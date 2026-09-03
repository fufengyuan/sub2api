package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamChunks 走 Stream 转写,再把输出按行切分提取 JSON payload。
func streamChunks(t *testing.T, in string) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(in)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := rec.Body.String()
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

func deltaOfU(t *testing.T, chunk map[string]any) map[string]any {
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

// TestStream_FirstChunkHasRole
// 回归:首 chunk 必须带 delta.role="assistant"(上游未给时网关兜底)。
// workbuddy 此前是逐行原样透传,上游缺 role 时客户端(zcode 等)会把空 delta
// 渲染成"思考持续了几秒"独立块。
func TestStream_FirstChunkHasRole(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	chunks := streamChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3", len(chunks))
	}
	if first := deltaOfU(t, chunks[0]); first["role"] != "assistant" {
		t.Errorf("首 chunk delta.role = %v, want assistant", first["role"])
	}
	if mid := deltaOfU(t, chunks[1]); mid != nil {
		if _, hasRole := mid["role"]; hasRole {
			t.Errorf("中间 chunk 不应再带 role: %v", mid)
		}
	}
	if last := deltaOfU(t, chunks[2]); last != nil {
		if _, hasRole := last["role"]; hasRole {
			t.Errorf("末 chunk 不应带 role: %v", last)
		}
	}
}

// TestStream_MiddleChunksHaveExplicitNullFinishReason
// 回归:中间 chunk 的 finish_reason 必须显式存在且为 null(缺字段会让客户端
// 误判流状态)。
func TestStream_MiddleChunksHaveExplicitNullFinishReason(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	chunks := streamChunks(t, in)
	if len(chunks) != 2 {
		t.Fatalf("chunks len = %d, want 2", len(chunks))
	}
	first, _ := chunks[0]["choices"].([]any)[0].(map[string]any)
	fr, has := first["finish_reason"]
	if !has {
		t.Fatalf("中间 chunk finish_reason 缺字段: %v", chunks[0])
	}
	if fr != nil {
		t.Errorf("中间 chunk finish_reason = %v, want null", fr)
	}
	second, _ := chunks[1]["choices"].([]any)[0].(map[string]any)
	if second["finish_reason"] != "stop" {
		t.Errorf("末 chunk finish_reason = %v, want stop", second["finish_reason"])
	}
}

// TestStream_UsageIsSeparateChunkWithEmptyChoices
// 回归:usage 必须是 finish chunk 之后的独立 chunk,choices 为空数组。
func TestStream_UsageIsSeparateChunkWithEmptyChoices(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":3,\"total_tokens\":11}}\n\n"

	chunks := streamChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (content + finish + usage)", len(chunks))
	}
	finish := chunks[1]
	if _, hasUsage := finish["usage"]; hasUsage {
		t.Errorf("finish chunk 不应携带 usage(应独立成 chunk): %v", finish)
	}
	usageChunk := chunks[2]
	u, ok := usageChunk["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage chunk 缺少 usage: %v", usageChunk)
	}
	if int(u["prompt_tokens"].(float64)) != 8 {
		t.Errorf("usage prompt_tokens = %v, want 8", u["prompt_tokens"])
	}
	ch, ok := usageChunk["choices"].([]any)
	if !ok {
		t.Fatalf("usage chunk choices 应为数组, got %T", usageChunk["choices"])
	}
	if len(ch) != 0 {
		t.Errorf("usage chunk choices 应为空数组, got len=%d", len(ch))
	}
}

// TestStream_ReasoningAndContentSplitIntoSeparateChunks
// 回归:上游把 reasoning_content 与 content 塞进同一个 delta 时必须拆成两个 chunk。
func TestStream_ReasoningAndContentSplitIntoSeparateChunks(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\",\"content\":\"正文\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	chunks := streamChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (reasoning + content + finish)", len(chunks))
	}
	first := deltaOfU(t, chunks[0])
	if first["reasoning_content"] != "思考" {
		t.Errorf("chunk0 reasoning_content = %v, want 思考", first["reasoning_content"])
	}
	if _, hasContent := first["content"]; hasContent {
		t.Errorf("chunk0 不应同时携带 content: %v", first)
	}
	second := deltaOfU(t, chunks[1])
	if second["content"] != "正文" {
		t.Errorf("chunk1 content = %v, want 正文", second["content"])
	}
	if _, hasReasoning := second["reasoning_content"]; hasReasoning {
		t.Errorf("chunk1 不应携带 reasoning_content: %v", second)
	}
}

// TestStream_NonJSONLinesPassThrough
// 回归:非 JSON 行(注释、event: 行、空行)必须原样透传,不能因规范化改造丢数据。
func TestStream_NonJSONLinesPassThrough(t *testing.T) {
	in := "" +
		": keepalive\n\n" +
		"event: ping\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(in)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := rec.Body.String()
	for _, want := range []string{": keepalive", "event: ping"} {
		if !strings.Contains(raw, want) {
			t.Errorf("输出丢失非 JSON 行 %q:\n%s", want, raw)
		}
	}
}

// TestStream_UpstreamEndsWithoutFinish_StillEmitsFinishAndUsage
// 回归:上游未给 finish_reason 就结束时,兜底必须补 finish chunk + usage。
func TestStream_UpstreamEndsWithoutFinish_StillEmitsFinishAndUsage(t *testing.T) {
	in := "" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"

	chunks := streamChunks(t, in)
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
