package traework

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamToChunks 走 Stream 把 SOLO SSE 转成 OpenAI SSE,再把输出按行切分,
// 提取每个 data: 行的 JSON payload。
func streamToChunks(t *testing.T, in string) []map[string]any {
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

// TestStream_ReasoningContent_MiddleChunksHaveExplicitNullFinishReason
// 回归:traework 中间 chunk 的 finish_reason 必须显式为 null,不能缺字段。
// 背景:zcode 等客户端按"字段切换"切分 reasoning_content 时,缺字段的中间 chunk
// 会被渲染成"思考·持续了几秒"独立块(一段一段);显式 null 与 OpenAI DeepSeek-reasoner
// 标准流一致,客户端据此明确知道"消息未结束",避免误切段。
func TestStream_ReasoningContent_MiddleChunksHaveExplicitNullFinishReason(t *testing.T) {
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\"The\"}\n" +
		"\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\" user\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3", len(chunks))
	}

	// 第 1/2 个 chunk(中间片):finish_reason 必须存在且为 nil(JSON 序列化为 null),
	// 不能是空串、也不能缺字段。
	for i := 0; i < 2; i++ {
		choices, _ := chunks[i]["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("chunk %d choices len = %d, want 1", i, len(choices))
		}
		c, _ := choices[0].(map[string]any)
		if c == nil {
			t.Fatalf("chunk %d choice missing", i)
		}
		fr, ok := c["finish_reason"]
		if !ok {
			t.Errorf("chunk %d finish_reason 缺字段(zcode 会按字段切换误切段): %v", i, chunks[i])
			continue
		}
		if fr == "" {
			t.Errorf("chunk %d finish_reason = 空串(同上问题): %v", i, chunks[i])
		}
		if fr != nil {
			t.Errorf("chunk %d finish_reason = %v, want null", i, fr)
		}
	}

	// 第 3 个 chunk(末片):finish_reason 应为 "stop"。
	lastChoices, _ := chunks[2]["choices"].([]any)
	lastChoice, _ := lastChoices[0].(map[string]any)
	if lastChoice["finish_reason"] != "stop" {
		t.Errorf("末 chunk finish_reason = %v, want stop", lastChoice["finish_reason"])
	}
	// 末 chunk 不应再带 delta.role(role 只在首 chunk 出现一次,符合 OpenAI 规范)。
	if lastDelta, _ := lastChoice["delta"].(map[string]any); lastDelta != nil {
		if _, hasRole := lastDelta["role"]; hasRole {
			t.Errorf("末 chunk delta 不应再带 role(role 只在首 chunk 出现): %v", lastDelta)
		}
	}
}

// TestStream_FirstChunk_HasDeltaRoleAssistant
// 回归:首 chunk 必须带 delta.role="assistant",让客户端识别"消息起点"。
// 缺 role 时部分客户端会按字段空 delta 渲染为"思考持续了几秒"独立块。
func TestStream_FirstChunk_HasDeltaRoleAssistant(t *testing.T) {
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\"x\"}\n" +
		"\n" +
		"event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\"y\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	if len(chunks) < 2 {
		t.Fatalf("chunks len = %d, want >= 2", len(chunks))
	}

	firstChoices, _ := chunks[0]["choices"].([]any)
	firstChoice, _ := firstChoices[0].(map[string]any)
	delta, _ := firstChoice["delta"].(map[string]any)
	if delta == nil {
		t.Fatalf("首 chunk delta 缺失: %v", chunks[0])
	}
	if delta["role"] != "assistant" {
		t.Errorf("首 chunk delta.role = %v, want assistant(客户端识别消息起点)", delta["role"])
	}

	// 中间 chunk 不应再带 role(只首 chunk 带,避免客户端误以为是新一轮消息)。
	midChoices, _ := chunks[1]["choices"].([]any)
	midChoice, _ := midChoices[0].(map[string]any)
	midDelta, _ := midChoice["delta"].(map[string]any)
	if _, hasRole := midDelta["role"]; hasRole {
		t.Errorf("中间 chunk delta 不应再带 role(已识别为同一消息): %v", midDelta)
	}
}

// TestStream_DoneWithoutAnyOutput_FirstChunkStillHasRole
// 边界:上游未产出任何 output 就直接 done(极短/空响应)时,写出的唯一 chunk 同时
// 是首 chunk 与末 chunk,也必须带 delta.role="assistant",否则客户端从未收到 role。
func TestStream_DoneWithoutAnyOutput_FirstChunkStillHasRole(t *testing.T) {
	in := "" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	if len(chunks) != 1 {
		t.Fatalf("chunks len = %d, want 1", len(chunks))
	}

	choices, _ := chunks[0]["choices"].([]any)
	c, _ := choices[0].(map[string]any)
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		t.Fatalf("delta 缺失: %v", chunks[0])
	}
	if delta["role"] != "assistant" {
		t.Errorf("无 output 时的唯一 chunk 也应带 role=assistant, got %v", delta["role"])
	}
	if c["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", c["finish_reason"])
	}
}

// TestStream_DoneAndErrorChunks_AlsoHaveExplicitNullFinishReason
// 回归:done/error chunk 在 finish_reason=="" 时(异常路径)同样写 null。
func TestStream_DoneAndErrorChunks_AlsoHaveExplicitNullFinishReason(t *testing.T) {
	// done 事件 finish_reason 为空串 —— 与上游协议在 done 前未携带 finish_reason 时一致。
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\"x\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	if len(chunks) < 2 {
		t.Fatalf("chunks len = %d, want >= 2", len(chunks))
	}

	lastChoices, _ := chunks[len(chunks)-1]["choices"].([]any)
	lastChoice, _ := lastChoices[0].(map[string]any)
	fr, ok := lastChoice["finish_reason"]
	if !ok {
		t.Errorf("末 chunk finish_reason 缺字段: %v", lastChoice)
	}
	if fr != nil {
		t.Errorf("末 chunk(空 finish_reason)应规范化为 null, got %v", fr)
	}
}