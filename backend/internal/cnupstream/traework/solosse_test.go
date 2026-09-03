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

// TestStream_UsageIsSeparateChunkWithEmptyChoices
// 回归:usage 必须是 finish chunk 之后的**独立** chunk,且 choices 为空数组。
// OpenAI 规范(参考 apicompat.resToChatHandleCompleted):finish chunk 与 usage chunk
// 是两个 SSE 事件。把 usage 挂在 finish chunk 上会让部分客户端收到 finish_reason
// 就停止解析,导致 usage 丢失(表现为 token 数恒为 0、计费异常)。
func TestStream_UsageIsSeparateChunkWithEmptyChoices(t *testing.T) {
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"hello\"}\n" +
		"\n" +
		"event: token_usage\n" +
		"data: {\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	// 期望:content chunk + finish chunk + usage chunk = 3
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (content + finish + usage)", len(chunks))
	}

	finishChunk := chunks[1]
	finishChoices, _ := finishChunk["choices"].([]any)
	fc, _ := finishChoices[0].(map[string]any)
	if fc["finish_reason"] != "stop" {
		t.Errorf("finish chunk finish_reason = %v, want stop", fc["finish_reason"])
	}
	if _, hasUsage := finishChunk["usage"]; hasUsage {
		t.Errorf("finish chunk 不应携带 usage(应独立成 chunk): %v", finishChunk)
	}

	usageChunk := chunks[2]
	if _, hasUsage := usageChunk["usage"]; !hasUsage {
		t.Fatalf("usage chunk 缺少 usage 字段: %v", usageChunk)
	}
	// choices 必须是空数组(不是缺字段、不是带一个空 choice)。
	ch, ok := usageChunk["choices"].([]any)
	if !ok {
		t.Fatalf("usage chunk choices 应为数组, got %T", usageChunk["choices"])
	}
	if len(ch) != 0 {
		t.Errorf("usage chunk choices 应为空数组, got len=%d: %v", len(ch), ch)
	}
	u, _ := usageChunk["usage"].(map[string]any)
	if int(u["prompt_tokens"].(float64)) != 11 || int(u["completion_tokens"].(float64)) != 7 {
		t.Errorf("usage 内容不符: %v", u)
	}
}

// TestStream_ReasoningAndContentSplitIntoSeparateChunks
// 回归:SOLO 上游同一个 output 事件同时给 reasoning_content 与 response 时,
// 必须拆成两个 delta 分发的 chunk —— OpenAI 官方流里二者不会同处一个 delta,
// 混在一起客户端无法判断"思考在哪结束、正文从哪开始",zcode 会渲染成零散小块。
func TestStream_ReasoningAndContentSplitIntoSeparateChunks(t *testing.T) {
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"hi\",\"reasoning_content\":\"think\"}\n" +
		"\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	// 期望:reasoning chunk + content chunk + finish chunk = 3
	if len(chunks) != 3 {
		t.Fatalf("chunks len = %d, want 3 (reasoning + content + finish)", len(chunks))
	}

	// 第 1 个:只有 reasoning_content,不能同时有 content。
	firstDelta := deltaOf(t, chunks[0])
	if firstDelta["reasoning_content"] != "think" {
		t.Errorf("chunk0 reasoning_content = %v, want think", firstDelta["reasoning_content"])
	}
	if _, hasContent := firstDelta["content"]; hasContent {
		t.Errorf("chunk0 不应同时携带 content(应与 reasoning 拆分): %v", firstDelta)
	}
	// 首 chunk 带 role。
	if firstDelta["role"] != "assistant" {
		t.Errorf("chunk0 role = %v, want assistant", firstDelta["role"])
	}

	// 第 2 个:只有 content。
	secondDelta := deltaOf(t, chunks[1])
	if secondDelta["content"] != "hi" {
		t.Errorf("chunk1 content = %v, want hi", secondDelta["content"])
	}
	if _, hasReasoning := secondDelta["reasoning_content"]; hasReasoning {
		t.Errorf("chunk1 不应携带 reasoning_content: %v", secondDelta)
	}
}

// TestStream_UpstreamEOFWithoutDone_StillEmitsFinishAndUsage
// 回归:上游未发 done 就 EOF(连接中断)时,兜底路径也必须补 finish chunk + usage,
// 否则客户端判定流被截断,且已收到的 token_usage 会丢失(计费侧丢 usage)。
func TestStream_UpstreamEOFWithoutDone_StillEmitsFinishAndUsage(t *testing.T) {
	in := "" +
		"event: output\n" +
		"data: {\"response\":\"partial\"}\n" +
		"\n" +
		"event: token_usage\n" +
		"data: {\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n" +
		"\n"

	chunks := streamToChunks(t, in)
	if len(chunks) < 3 {
		t.Fatalf("chunks len = %d, want >= 3 (content + finish + usage)", len(chunks))
	}

	last := chunks[len(chunks)-1]
	if _, hasUsage := last["usage"]; !hasUsage {
		t.Errorf("EOF 兜底路径应补发 usage chunk, 末 chunk = %v", last)
	}
	// 倒数第二个应是 finish chunk。
	prev := chunks[len(chunks)-2]
	pc, _ := prev["choices"].([]any)
	if len(pc) != 1 {
		t.Fatalf("finish chunk choices len = %d, want 1: %v", len(pc), prev)
	}
	if pc[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("EOF 兜底 finish_reason = %v, want stop", pc[0].(map[string]any)["finish_reason"])
	}
}

// deltaOf 取出 chunk 的 choices[0].delta。
func deltaOf(t *testing.T, chunk map[string]any) map[string]any {
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
