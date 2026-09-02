package qwenwork

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamAsOpenAI_NormalizesEmptyFinishReason
// 回归：国产渠道（qwenwork/qoder）上游流式 chunk 的 finish_reason 恒为 ""（空串）。
// 网关若原样透传，客户端按 finish_reason 判「流已结束」会把第一个 chunk 当作
// 结束标志，正文只收 1 个分片就中断（文档 P0-6「空回复 + 无限重试」）。
// 修复后：中间 chunk 的 finish_reason 应为 null（或缺省），末 chunk 保留 "stop"。
func TestStreamAsOpenAI_NormalizesEmptyFinishReason(t *testing.T) {
	// 模拟上游：第 1 个 chunk finish_reason=""（中间片），第 2 个 chunk "stop"（末片）。
	in := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"

	var out bytes.Buffer
	if err := streamAsOpenAI(&out, strings.NewReader(in), "deepseek-v4-flash", nil); err != nil {
		t.Fatalf("streamAsOpenAI: %v", err)
	}

	// 解析每行 data: 的 chunk。
	var chunks []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
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

	if len(chunks) != 2 {
		t.Fatalf("chunks len = %d, want 2", len(chunks))
	}

	// 第 1 个 chunk（中间片）：finish_reason 不得是空串，应为 nil（null）。
	first := chunks[0]
	choices, _ := first["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("first chunk choices len = %d, want 1", len(choices))
	}
	firstChoice, _ := choices[0].(map[string]any)
	if firstChoice == nil {
		t.Fatal("first chunk choice missing")
	}
	fr, has := firstChoice["finish_reason"]
	if fr == "" {
		t.Errorf("中间 chunk finish_reason = %q（空串会让客户端误判流结束，只收 1 个分片）: %q", fr, out.String())
	}
	_ = has

	// 第 2 个 chunk（末片）：finish_reason 应为 "stop"。
	second := chunks[1]
	schoices, _ := second["choices"].([]any)
	if len(schoices) != 1 {
		t.Fatalf("second chunk choices len = %d, want 1", len(schoices))
	}
	secondChoice, _ := schoices[0].(map[string]any)
	if secondChoice["finish_reason"] != "stop" {
		t.Errorf("末 chunk finish_reason = %v, want stop", secondChoice["finish_reason"])
	}
}
