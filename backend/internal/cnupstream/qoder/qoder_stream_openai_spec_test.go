package qoder

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// nest 把标准 OpenAI chunk 包装成 qoder 上游的嵌套 SSE 行:
// data:{"body":"<chunk-json>"}。qoder 的 parseNestedSSE 只认这种格式,
// 与 qwenwork(兼容嵌套 + 标准两种)不同,故测试输入必须走嵌套。
func nest(t *testing.T, chunk string) string {
	t.Helper()
	// 把 chunk 作为 JSON 字符串嵌入 body 字段。
	quoted, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal nested body: %v", err)
	}
	return "data:{\"body\":" + string(quoted) + "}\n\n"
}

// sseChunks 走 streamAsOpenAI 转写,再把输出按行切分提取 JSON payload。
func sseChunks(t *testing.T, in string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := streamAsOpenAI(&out, strings.NewReader(in), "deepseek-v4-flash", nil); err != nil {
		t.Fatalf("streamAsOpenAI: %v", err)
	}
	raw := out.String()
	t.Logf("SSE output:\n%s", raw)

	var chunks []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v body=%q", err, payload)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// deltaOfQ 取出 chunk 的 choices[0].delta。
func deltaOfQ(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chunk 无 choices: %v", chunk)
	}
	d, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	if d == nil {
		t.Fatalf("chunk 无 delta: %v", chunk)
	}
	return d
}

// TestStreamAsOpenAI_NormalizesEmptyFinishReason
// 回归:上游(qoder)流式 chunk 的 finish_reason 恒为 ""(空串),原样透传会让客户端
// 误判「流已结束」而只收 1 个分片。修复后中间 chunk 为 null,末 chunk 保留 "stop"。
func TestStreamAsOpenAI_NormalizesEmptyFinishReason(t *testing.T) {
	in := nest(t, `{"choices":[{"delta":{"content":"你好"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	chunks := sseChunks(t, in)
	if len(chunks) != 2 {
		t.Fatalf("chunks len = %d, want 2", len(chunks))
	}

	first, _ := chunks[0]["choices"].([]any)[0].(map[string]any)
	fr, has := first["finish_reason"]
	if !has {
		t.Errorf("中间 chunk finish_reason 缺字段: %v", chunks[0])
	} else if fr != nil {
		t.Errorf("中间 chunk finish_reason = %v, want null", fr)
	}
	second, _ := chunks[1]["choices"].([]any)[0].(map[string]any)
	if second["finish_reason"] != "stop" {
		t.Errorf("末 chunk finish_reason = %v, want stop", second["finish_reason"])
	}
}

// TestStreamAsOpenAI_FirstChunkHasRole
// 回归:首 chunk 必须带 delta.role="assistant"(上游未给时网关兜底);中间/末 chunk
// 不应再带 role。缺 role 时 zcode 等客户端会把空 delta 渲染成"思考持续了几秒"独立块。
func TestStreamAsOpenAI_FirstChunkHasRole(t *testing.T) {
	in := nest(t, `{"choices":[{"delta":{"content":"你好"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[{"delta":{"content":"呀"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	chunks := sseChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3", len(chunks))
	}

	if first := deltaOfQ(t, chunks[0]); first["role"] != "assistant" {
		t.Errorf("首 chunk delta.role = %v, want assistant", first["role"])
	}
	if mid := deltaOfQ(t, chunks[1]); mid != nil {
		if _, hasRole := mid["role"]; hasRole {
			t.Errorf("中间 chunk 不应再带 role: %v", mid)
		}
	}
	if last := deltaOfQ(t, chunks[2]); last != nil {
		if _, hasRole := last["role"]; hasRole {
			t.Errorf("末 chunk 不应带 role: %v", last)
		}
	}
}

// TestStreamAsOpenAI_UsageIsSeparateChunkWithEmptyChoices
// 回归:usage 必须是 finish chunk 之后的独立 chunk,choices 为空数组
// (参考 apicompat.resToChatHandleCompleted)。与 finish_reason 同处一个 chunk 会让
// 客户端提前停止解析而丢 usage。
func TestStreamAsOpenAI_UsageIsSeparateChunkWithEmptyChoices(t *testing.T) {
	in := nest(t, `{"choices":[{"delta":{"content":"你好"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`)

	chunks := sseChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (content + finish + usage)", len(chunks))
	}

	finish := chunks[1]
	fc, _ := finish["choices"].([]any)[0].(map[string]any)
	if fc["finish_reason"] != "stop" {
		t.Errorf("finish chunk finish_reason = %v, want stop", fc["finish_reason"])
	}
	if _, hasUsage := finish["usage"]; hasUsage {
		t.Errorf("finish chunk 不应携带 usage(应独立成 chunk): %v", finish)
	}

	usageChunk := chunks[2]
	u, ok := usageChunk["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage chunk 缺少 usage: %v", usageChunk)
	}
	if int(u["prompt_tokens"].(float64)) != 9 {
		t.Errorf("usage prompt_tokens = %v, want 9", u["prompt_tokens"])
	}
	ch, ok := usageChunk["choices"].([]any)
	if !ok {
		t.Fatalf("usage chunk choices 应为数组, got %T", usageChunk["choices"])
	}
	if len(ch) != 0 {
		t.Errorf("usage chunk choices 应为空数组, got len=%d", len(ch))
	}
}

// TestStreamAsOpenAI_ReasoningAndContentSplitIntoSeparateChunks
// 回归:上游把 reasoning_content 与 content 塞进同一个 delta 时必须拆成两个 chunk。
func TestStreamAsOpenAI_ReasoningAndContentSplitIntoSeparateChunks(t *testing.T) {
	in := nest(t, `{"choices":[{"delta":{"reasoning_content":"思考","content":"正文"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	chunks := sseChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (reasoning + content + finish)", len(chunks))
	}

	first := deltaOfQ(t, chunks[0])
	if first["reasoning_content"] != "思考" {
		t.Errorf("chunk0 reasoning_content = %v, want 思考", first["reasoning_content"])
	}
	if _, hasContent := first["content"]; hasContent {
		t.Errorf("chunk0 不应同时携带 content: %v", first)
	}
	if first["role"] != "assistant" {
		t.Errorf("chunk0 role = %v, want assistant", first["role"])
	}

	second := deltaOfQ(t, chunks[1])
	if second["content"] != "正文" {
		t.Errorf("chunk1 content = %v, want 正文", second["content"])
	}
	if _, hasReasoning := second["reasoning_content"]; hasReasoning {
		t.Errorf("chunk1 不应携带 reasoning_content: %v", second)
	}
}

// TestStreamAsOpenAI_UpstreamEndsWithoutFinish_StillEmitsFinishAndUsage
// 回归:上游未给 finish_reason 就结束时,兜底必须补 finish chunk + usage。
func TestStreamAsOpenAI_UpstreamEndsWithoutFinish_StillEmitsFinishAndUsage(t *testing.T) {
	in := nest(t, `{"choices":[{"delta":{"content":"半截"},"finish_reason":""}]}`) +
		nest(t, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)

	chunks := sseChunks(t, in)
	if len(chunks) < 3 {
		t.Fatalf("chunks len = %d, want >= 3 (content + finish + usage)", len(chunks))
	}

	last := chunks[len(chunks)-1]
	if _, hasUsage := last["usage"]; !hasUsage {
		t.Errorf("兜底路径应补发 usage chunk, 末 chunk = %v", last)
	}
	prev := chunks[len(chunks)-2]
	pc, _ := prev["choices"].([]any)
	if len(pc) != 1 {
		t.Fatalf("finish chunk choices len = %d, want 1: %v", len(pc), prev)
	}
	if pc[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("兜底 finish_reason = %v, want stop", pc[0].(map[string]any)["finish_reason"])
	}
}
