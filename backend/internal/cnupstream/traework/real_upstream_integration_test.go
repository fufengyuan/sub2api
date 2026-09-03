//go:build integration

package traework

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
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

// completionTokens 从 SOLO SSE 流里提取 usage.completion_tokens（lic 空）。
// 无 usage 事件时返回 -1。
func completionTokens(s string) int {
	// token_usage 事件携带 usage，形如 data:{"completion_tokens":N,...} 或
	// data:{"usage":{"completion_tokens":N}}。这里同时匹配两种。
	// 简单做法：找最后一个 "completion_tokens" 值。
	var last = -1
	re := regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				last = n
			}
		}
	}
	return last
}

// responseLen 统计 SSE 里 output 事件 content 的累计长度（粗略，用于对照）。
func responseLen(s string) int {
	// SOLO output 事件: data:{"response":"...","reasoning_content":"..."}
	re := regexp.MustCompile(`"response"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	n := 0
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if len(m) == 2 {
			n += len(m[1])
		}
	}
	return n
}

// TestMaxTokensEffective 验证 max_tokens 是否被上游处理：同一 prompt（要求长文本）
// 分别用 max_tokens=20 与 max_tokens=3000 发送两次，比较 usage.completion_tokens
// 与内容长度。若小值显著短、大值明显长，说明字段生效。
func TestMaxTokensEffective(t *testing.T) {
	at := os.Getenv("TW2A_TEST_ACCESS_TOKEN")
	if at == "" {
		t.Skip("TW2A_TEST_ACCESS_TOKEN 未设置")
	}
	a := &auth.Auth{
		Kind: "traework", AccessToken: at,
		UID: os.Getenv("TW2A_TEST_UID"), DeviceID: os.Getenv("TW2A_TEST_DEVICE_ID"),
		MachineID: os.Getenv("TW2A_TEST_MACHINE_ID"), ApiHost: firstNonEmpty(os.Getenv("TW2A_TEST_APIHOST"), AgentHost),
	}
	c := New()
	prompt := "请回复一篇不少于2000字的文章，主题是中国美食。"

	run := func(maxTokens int) (completion int, contentLen int) {
		body, _ := json.Marshal(map[string]any{
			"model":      "DeepSeek-V4-Flash-Official",
			"messages":   []any{map[string]any{"role": "user", "content": prompt}},
			"stream":     true,
			"max_tokens": maxTokens,
		})
		rc, status, respBody, err := c.ChatStream(a, body)
		if err != nil {
			t.Fatalf("max_tokens=%d err=%v", maxTokens, err)
		}
		defer rc.Close()
		if status >= 400 {
			t.Fatalf("max_tokens=%d upstream HTTP %d body=%s", maxTokens, status, truncate(string(respBody), 200))
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		s := string(raw)
		return completionTokens(s), responseLen(s)
	}

	smallC, smallL := run(20)
	largeC, largeL := run(3000)
	t.Logf("max_tokens=20   → completion_tokens=%d content_len=%d", smallC, smallL)
	t.Logf("max_tokens=3000 → completion_tokens=%d content_len=%d", largeC, largeL)

	// 小值应显著小于大值（低到接近 0，大值明显更大）。宽松断言：大值 completion ＞ 小值 completion。
	if largeC <= smallC {
		t.Logf("⚠️ max_tokens 看似未生效：小值 completion=%d 大值 completion=%d（可能上游不读此字段或阈值不同）", smallC, largeC)
	} else {
		t.Logf("✅ max_tokens 生效：completion_tokens 随 max_tokens 增长（%d → %d）", smallC, largeC)
	}
}

// TestPromptMaxTokensEffective 验证 prompt_max_tokens（输入上下文上限）是否被上游
// 处理：构造一个远超极小 prompt_max_tokens 的大输入，分别用 prompt_max_tokens=200
// 与 prompt_max_tokens=1000000 发送两次。
//   - 若小值触发上下文超限、大值成功 → 字段生效（严格限制输入）
//   - 若两者都成功 → 字段可能只作提示，或上游阈值更高
//   - 若都失败 → 字段不生效/另有约束
//
// 这是验证"1M 上下文是否真的可用"的关键测试。
func TestPromptMaxTokensEffective(t *testing.T) {
	at := os.Getenv("TW2A_TEST_ACCESS_TOKEN")
	if at == "" {
		t.Skip("TW2A_TEST_ACCESS_TOKEN 未设置")
	}
	a := &auth.Auth{
		Kind: "traework", AccessToken: at,
		UID: os.Getenv("TW2A_TEST_UID"), DeviceID: os.Getenv("TW2A_TEST_DEVICE_ID"),
		MachineID: os.Getenv("TW2A_TEST_MACHINE_ID"), ApiHost: firstNonEmpty(os.Getenv("TW2A_TEST_APIHOST"), AgentHost),
	}
	c := New()

	// 构造大输入：约 ~4000 token（远超 prompt_max_tokens=200，够触达上下文校验）
	longContent := strings.Repeat("这是一段用于测试上下文窗口上限的重复文本，包含中英文字符 abcdefg 123。 ", 200)

	run := func(pmt int) (status int, respBody string) {
		body, _ := json.Marshal(map[string]any{
			"model":             "DeepSeek-V4-Flash-Official",
			"messages":          []any{map[string]any{"role": "user", "content": longContent}},
			"stream":            true,
			"prompt_max_tokens": pmt,
		})
		rc, st, rb, err := c.ChatStream(a, body)
		if err != nil {
			t.Fatalf("prompt_max_tokens=%d err=%v", pmt, err)
		}
		defer rc.Close()
		if st >= 400 {
			return st, string(rb)
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		return st, string(raw)
	}

	smallStatus, smallBody := run(200)
	largeStatus, largeBody := run(1000000)
	t.Logf("prompt_max_tokens=200   → status=%d", smallStatus)
	t.Logf("prompt_max_tokens=200  SSE含event:error=%v 含code4026=%v 含finish_length=%v 含output=%v",
		strings.Contains(smallBody, "event:error"), strings.Contains(smallBody, `"code":4026`),
		strings.Contains(smallBody, `"finish_reason":"length"`), strings.Contains(smallBody, "event:output"))
	t.Logf("prompt_max_tokens=200 SSE尾部(600):\n%s", truncate(smallBody, 600))
	t.Logf("prompt_max_tokens=1M   → status=%d SSE含output=%v 尾部(300):\n%s", largeStatus, strings.Contains(largeBody, "event:output"), truncate(largeBody, 300))

	// 判定：小值被"截断/拒绝" = 状态非200，或 SSE 明确 event:error / code 4026 /
	// finish_reason=length（输入超出限制被截断）。
	smallErr := smallStatus >= 400 || strings.Contains(smallBody, "event:error") ||
		strings.Contains(smallBody, `"code":4026`) || strings.Contains(smallBody, `"finish_reason":"length"`)
	largeOK := largeStatus < 400 && strings.Contains(largeBody, "event:output")
	switch {
	case smallErr && largeOK:
		t.Logf("✅ prompt_max_tokens 生效：小值(200)输入超限被截断/拒绝，大值(1M)正常输出——1M 上下文可用")
	case !smallErr && largeOK:
		t.Logf("⚠️ prompt_max_tokens 未严格限制：小值(200)也正常输出——上游是软提示或阈值更高，需更大输入实测 1M 边界")
	default:
		t.Logf("⚠️ 结果待判：小值 err=%v 大值 ok=%v", smallErr, largeOK)
	}
}
