// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// drain nothing; done
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// Stream 把上游 SSE 规范化后写给客户端（每行 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
//
// 上游（workbuddy）已是标准 OpenAI 兼容流，此前是逐行原样透传。但实测上游
// 并不保证每条规范都遵守（缺 delta.role、finish_reason 给空串等），客户端
// （zcode 等）会因此把 reasoning_content 切成零散小块。这里改为逐行解析后
// 规范化再写出，与 qoder/qwenwork/traework 保持一致的输出契约：
//  1. 首 chunk 带 delta.role="assistant"（上游未给则补，且只出现一次）
//  2. choices[].finish_reason 恒定存在：中间 chunk 为 null，末 chunk 为具体原因
//  3. reasoning_content 与 content 不同处一个 delta（上游混发时拆成两个 chunk）
//  4. usage 是 finish chunk 之后的独立 chunk，choices 为空数组
//
// 解析失败的行原样透传（不因个别异常行丢数据）。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64*1024)
	sawDone := false
	sawAnyChunk := false
	var pendingUsage map[string]any

	writeLine := func(s string) error {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	// writeChunk 序列化并写出一个 SSE 事件；首 chunk 统一注入 delta.role。
	writeChunk := func(chunk map[string]any) error {
		if !sawAnyChunk {
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				if c, ok := choices[0].(map[string]any); ok {
					if delta, ok := c["delta"].(map[string]any); ok {
						if _, hasRole := delta["role"]; !hasRole {
							delta["role"] = "assistant"
						}
					}
				}
			}
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if err := writeLine("data: " + string(raw) + "\n\n"); err != nil {
			return err
		}
		sawAnyChunk = true
		return nil
	}
	writeUsage := func() error {
		if pendingUsage == nil {
			return nil
		}
		err := writeChunk(map[string]any{
			"choices": []any{},
			"usage":   pendingUsage,
		})
		pendingUsage = nil
		return err
	}
	// emitNormalized 处理一个已解析的上游 chunk：暂存 usage、规范化 finish_reason、
	// 拆分混发的 reasoning/content，然后写出；finish chunk 之后补独立 usage chunk。
	emitNormalized := func(chunk map[string]any) error {
		if u, ok := chunk["usage"].(map[string]any); ok {
			pendingUsage = u
			delete(chunk, "usage")
		}
		choices, _ := chunk["choices"].([]any)
		// usage-only chunk（choices 为空）：usage 已暂存，末尾由独立 chunk 输出。
		if len(choices) == 0 {
			return nil
		}
		var finishReason string
		for _, ci := range choices {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if finishReason == "" {
				c["finish_reason"] = nil
			}
		}
		var delta map[string]any
		if len(choices) > 0 {
			delta, _ = choices[0].(map[string]any)["delta"].(map[string]any)
		}
		if delta != nil {
			reasoning, hasReasoning := delta["reasoning_content"].(string)
			_, hasContent := delta["content"]
			if hasReasoning && reasoning != "" && hasContent {
				if err := writeChunk(map[string]any{
					"choices": []any{map[string]any{
						"index":         0,
						"delta":         map[string]any{"reasoning_content": reasoning},
						"finish_reason": nil,
					}},
				}); err != nil {
					return err
				}
				delete(delta, "reasoning_content")
			}
		}
		if err := writeChunk(chunk); err != nil {
			return err
		}
		if finishReason != "" {
			return writeUsage()
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: [DONE]") {
				sawDone = true
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			var chunk map[string]any
			// 非 JSON 行（注释行、event: 行、空行、[DONE]）原样透传。
			if payload == "" || payload == "[DONE]" || json.Unmarshal([]byte(payload), &chunk) != nil || chunk == nil {
				if werr := writeLine(line); werr != nil {
					return werr
				}
			} else if werr := emitNormalized(chunk); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// 上游未给 finish_reason 就结束：补 finish chunk + usage，否则客户端判截断。
	if pendingUsage != nil {
		if err := writeChunk(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		}); err != nil {
			return err
		}
		if err := writeUsage(); err != nil {
			return err
		}
	}
	if !sawDone {
		if err := writeLine("data: [DONE]\n\n"); err != nil {
			return err
		}
	}
	return nil
}
