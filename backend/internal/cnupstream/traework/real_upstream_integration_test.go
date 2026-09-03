//go:build integration

package traework

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

// TestChatStream_RealUpstream 连真实 Trae 上游验证 llm_utils_chat 报文被接受。
// 凭证从环境变量读取（由外部从 DB 拉取注入，不落盘不打印）：
//
//	TW2A_TEST_ACCESS_TOKEN / TW2A_TEST_UID / TW2A_TEST_DEVICE_ID /
//	TW2A_TEST_MACHINE_ID / TW2A_TEST_APIHOST
//
//	用法：env=... go test -tags=integration ./internal/cnupstream/traework/ \
//		  -run TestChatStream_RealUpstream -v
func TestChatStream_RealUpstream(t *testing.T) {
	at := os.Getenv("TW2A_TEST_ACCESS_TOKEN")
	if at == "" {
		t.Skip("TW2A_TEST_ACCESS_TOKEN 未设置，跳过真实上游测试")
	}
	a := &auth.Auth{
		Kind:         "traework",
		AccessToken:  at,
		UID:          os.Getenv("TW2A_TEST_UID"),
		DeviceID:     os.Getenv("TW2A_TEST_DEVICE_ID"),
		MachineID:    os.Getenv("TW2A_TEST_MACHINE_ID"),
		ApiHost:      firstNonEmpty(os.Getenv("TW2A_TEST_APIHOST"), AgentHost),
		MachineToken: os.Getenv("TW2A_TEST_MACHINE_TOKEN"),
		MachineType:  os.Getenv("TW2A_TEST_MACHINE_TYPE"),
	}

	c := New()

	// 用 conf 模型 hl，steam:true，发一句话
	body, _ := json.Marshal(map[string]any{
		"model":    "DeepSeek-V4-Flash-Official",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		"stream":   true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("ChatStream err=%v", err)
	}
	defer rc.Close()
	_ = ctx

	t.Logf("upstream status=%d", status)
	if status >= 400 {
		t.Fatalf("upstream HTTP %d body=%s", status, truncate(string(respBody), 300))
	}

	// 读 SSE 流，确认拿到输出/业务错误
	raw, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
	s := string(raw)
	t.Logf("SSE response (first 400):\n%s", truncate(s, 400))
	if strings.Contains(s, `"code":4001`) || strings.Contains(s, `code=4001`) || strings.Contains(s, `"param is invalid"`) {
		t.Fatalf("上游返回 4001 参数非法（model_name/__dev 或 prompt_max_tokens 不被接受）")
	}
	if !strings.Contains(s, "data:") {
		t.Fatalf("未收到任何 SSE data")
	}
}

// TestProbePromptMaxTokens 探 prompt_max_tokens 上下文上限：把包级变量调大
// 发真实请求，观察上游是否接受（200）还是拒绝（4001/上下文超限）。
// 运行方式同 TestChatStream_RealUpstream。
func TestProbePromptMaxTokens(t *testing.T) {
	at := os.Getenv("TW2A_TEST_ACCESS_TOKEN")
	if at == "" {
		t.Skip("TW2A_TEST_ACCESS_TOKEN 未设置")
	}
	// 探 1M
	old := promptMaxTokens
	promptMaxTokens = 1000000
	defer func() { promptMaxTokens = old }()

	a := &auth.Auth{
		Kind: "traework", AccessToken: at,
		UID: os.Getenv("TW2A_TEST_UID"), DeviceID: os.Getenv("TW2A_TEST_DEVICE_ID"),
		MachineID: os.Getenv("TW2A_TEST_MACHINE_ID"), ApiHost: firstNonEmpty(os.Getenv("TW2A_TEST_APIHOST"), AgentHost),
	}
	c := New()
	body, _ := json.Marshal(map[string]any{
		"model":    "DeepSeek-V4-Flash-Official",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer rc.Close()
	t.Logf("prompt_max_tokens=1000000 → status=%d body=%s", status, truncate(string(respBody), 200))
	if status >= 400 {
		t.Logf("🎯 上游拒绝大 prompt_max_tokens: HTTP %d", status)
		return // 不算失败，是实测信息
	}
	raw, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
	s := string(raw)
	t.Logf("SSE(400):\n%s", truncate(s, 400))
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
