// sse.go 处理 QoderWork 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。
// 移植自 qoderwork2api internal/upstream/sse.go，含聚合与流式转写。
package qoder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// parseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - "event:finish" 行 → 忽略
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
func parseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			var env struct {
				Body string `json:"body"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(env.Body), &chunk) == nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 参数覆盖响应中的 model 字段（上游恒为 "auto"）。
// tool_calls 以流式 delta 到达（按 index 合并）。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
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
		}
		return nil
	})
	if err != nil {
		return nil, err
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
// id/type/function.name 直覆盖，function.arguments 拼接。
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

// streamAsOpenAI 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端。
// 每个 chunk 重写 model 字段为客户端模型名；末尾补 data: [DONE]。
//
// 输出严格对齐 OpenAI Chat Completions 流式规范（对照 apicompat 参考实现）：
//  1. 首 chunk 必须带 delta.role="assistant"（上游未给则补，且只出现一次）
//  2. choices[].finish_reason 恒定存在：中间 chunk 为 null，末 chunk 为具体原因
//  3. reasoning_content 与 content 不同处一个 delta（上游混发时拆成两个 chunk）
//  4. usage 是 finish chunk 之后的独立 chunk，choices 为空数组
//     （挂在 finish chunk 上会让客户端收到 finish_reason 就停止解析而丢 usage）
func streamAsOpenAI(w io.Writer, r io.Reader, model string, flush func()) error {
	sawAnyChunk := false
	// pendingUsage 暂存上游 usage：规范要求在 finish chunk 之后单独下发。
	var pendingUsage map[string]any

	// write 序列化并写出一个 SSE 事件。
	// 首 chunk（带 delta 的第一个 chunk）在此统一注入 delta.role="assistant"，
	// 覆盖全部分支（普通 delta、拆分的 reasoning delta、兜底 finish chunk），
	// 避免只在某条路径注入导致漏补。
	write := func(chunk map[string]any) error {
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
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		sawAnyChunk = true
		return nil
	}
	// writeDelta 写出一个普通 delta chunk（中间片，finish_reason 恒为 null）。
	// role 注入由上面的 write 统一处理。
	writeDelta := func(delta map[string]any) error {
		chunk := map[string]any{
			"model": model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		}
		return write(chunk)
	}
	// writeUsage 写出独立 usage chunk（choices 为空数组）。
	writeUsage := func() error {
		if pendingUsage == nil {
			return nil
		}
		err := write(map[string]any{
			"model":   model,
			"choices": []any{},
			"usage":   pendingUsage,
		})
		pendingUsage = nil
		return err
	}

	err := parseNestedSSE(r, func(chunk map[string]any) error {
		// usage 先抽出来暂存，绝不与 finish_reason 同处一个 chunk。
		if u, ok := chunk["usage"].(map[string]any); ok {
			pendingUsage = u
			delete(chunk, "usage")
		}
		chunk["model"] = model

		choices, _ := chunk["choices"].([]any)
		// usage-only chunk（无 choices 或 choices 为空）：不再单独下发，
		// 其 usage 已在上面暂存，将由末尾的独立 usage chunk 输出。
		if len(choices) == 0 {
			return nil
		}

		// 逐个 choice 规范化。当前上游只会有 index=0 单 choice，
		// 但按数组遍历以免将来上游变化被漏掉。
		var finishReason string
		for _, ci := range choices {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			// finish_reason 恒定存在：末 chunk 保留具体原因，其余为 null。
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
			// reasoning 与 content 同处一个 delta 时拆成两个 chunk 顺序下发，
			// 与 DeepSeek-reasoner 形态一致（先思考后正文）。
			if hasReasoning && reasoning != "" && hasContent {
				split := map[string]any{"reasoning_content": reasoning}
				if err := writeDelta(split); err != nil {
					return err
				}
				delete(delta, "reasoning_content")
			}
		}

		if err := write(chunk); err != nil {
			return err
		}
		// finish chunk 之后立即输出独立 usage chunk。
		if finishReason != "" {
			return writeUsage()
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 上游未给 finish_reason 就结束（中断/无末片）时兜底：
	// 补 finish chunk + usage，否则客户端判定流被截断且 usage 丢失。
	if pendingUsage != nil || !sawAnyChunk {
		if err := write(map[string]any{
			"model": model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				},
			},
		}); err != nil {
			return err
		}
		if err := writeUsage(); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// Stream 实现 provider.Upstream：嵌套 SSE → 标准 OpenAI SSE 透传。
func Stream(w http.ResponseWriter, r io.Reader, model string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	return streamAsOpenAI(w, r, model, flush)
}
