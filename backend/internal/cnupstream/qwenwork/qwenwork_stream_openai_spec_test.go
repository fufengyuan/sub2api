package qwenwork

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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

// TestStreamAsOpenAI_FirstChunkHasRole
// 回归:首 chunk 必须带 delta.role="assistant"(上游未给时网关兜底),让客户端
// 识别消息起点;zcode 等客户端在缺 role 时会把字段空 delta 渲染成"思考持续了几秒"
// 独立块。中间/末 chunk 不应再带 role。
func TestStreamAsOpenAI_FirstChunkHasRole(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"呀\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	chunks := sseChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3", len(chunks))
	}

	first := deltaOfQ(t, chunks[0])
	if first["role"] != "assistant" {
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
// 回归:usage 必须是 finish chunk 之后的独立 chunk,choices 为空数组。
// OpenAI 规范(参考 apicompat.resToChatHandleCompleted)中 finish chunk 与 usage
// chunk 是两个独立 SSE 事件;挂在同一 chunk 上会让客户端收到 finish_reason 就
// 停止解析,导致 usage 丢失(表现为 token 数恒为 0)。
func TestStreamAsOpenAI_UsageIsSeparateChunkWithEmptyChoices(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n"

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
	if int(u["prompt_tokens"].(float64)) != 9 || int(u["completion_tokens"].(float64)) != 4 {
		t.Errorf("usage 内容不符: %v", u)
	}
	ch, ok := usageChunk["choices"].([]any)
	if !ok {
		t.Fatalf("usage chunk choices 应为数组, got %T", usageChunk["choices"])
	}
	if len(ch) != 0 {
		t.Errorf("usage chunk choices 应为空数组, got len=%d: %v", len(ch), ch)
	}
}

// TestStreamAsOpenAI_ReasoningAndContentSplitIntoSeparateChunks
// 回归:上游把 reasoning_content 与 content 塞进同一个 delta 时必须拆成两个 chunk。
// OpenAI 官方流里二者不同处一个 delta,混在一起客户端无法判断"思考在哪结束",
// zcode 会渲染成零散小段。
func TestStreamAsOpenAI_ReasoningAndContentSplitIntoSeparateChunks(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考\",\"content\":\"正文\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	chunks := sseChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (reasoning + content + finish)", len(chunks))
	}

	first := deltaOfQ(t, chunks[0])
	if first["reasoning_content"] != "思考" {
		t.Errorf("chunk0 reasoning_content = %v, want 思考", first["reasoning_content"])
	}
	if _, hasContent := first["content"]; hasContent {
		t.Errorf("chunk0 不应同时携带 content(应与 reasoning 拆分): %v", first)
	}
	if first["role"] != "assistant" {
		t.Errorf("chunk0 是首 chunk, role = %v, want assistant", first["role"])
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
// 回归:上游未给 finish_reason 就结束(中断/无末片)时,兜底必须补 finish chunk +
// usage,否则客户端判定流被截断,且已收到的 usage 会丢失。
func TestStreamAsOpenAI_UpstreamEndsWithoutFinish_StillEmitsFinishAndUsage(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"半截\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n"

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
