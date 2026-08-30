package qoder

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"testing"
)

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max":      "qwen3.8-max",
		"DeepSeek-V4-Pro":  "deepseek-v4-pro",
		"GLM-5.3":          "glm-5.3",
		"Kimi-K2.7-Code":   "kimi-k2.7-code",
		"MiniMax-M2.7":     "minimax-m2.7",
		"Auto":             "auto",
		"Qwen3.8 Max Test": "qwen3.8-max-test",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelKeyStatic(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4-pro": "dmodel",
		"glm-5.3":         "gmodel",
		"glm-5.2":         "gm51model",
		"qwen3.8-max":     "qmodel_38max",
		"kimi-k2.7-code":  "kmodel",
		"unknown-model":   "",
	}
	for name, want := range cases {
		if got := ModelKey(name); got != want {
			t.Errorf("ModelKey(%q) = %q, want %q", name, got, want)
		}
	}
}

// 动态映射优先于静态表，且**按账号隔离**：A 账号的映射不得影响 B 账号。
func TestModelKeyDynamicPrecedence(t *testing.T) {
	a := &auth.Auth{UID: "acct-a"}
	a.SetModelMap(map[string]string{"glm-5.3": "gmodel-new", "custom": "ckey"})
	b := &auth.Auth{UID: "acct-b"} // 从未拉取过模型表

	if got := resolveModelKey(a, "glm-5.3"); got != "gmodel-new" {
		t.Errorf("账号动态映射应优先于静态表, got %q", got)
	}
	if got := resolveModelKey(a, "custom"); got != "ckey" {
		t.Errorf("账号自定义模型名应可解析, got %q", got)
	}
	if got := resolveModelKey(a, "deepseek-v4-pro"); got != "dmodel" { // 动态缺失 → 静态兜底
		t.Errorf("static fallback got %q", got)
	}
	if got := resolveModelKey(b, "glm-5.3"); got != "gmodel" {
		t.Fatalf("B 账号必须走静态表而非 A 账号的 %q", got)
	}
	if got := resolveModelKey(b, "custom"); got != "" {
		t.Fatalf("B 账号不应看到 A 账号独有的映射, got %q", got)
	}
	if got := resolveModelKey(nil, "glm-5.3"); got != "gmodel" {
		t.Fatalf("nil Auth 应退回静态表, got %q", got)
	}
}

// 拉取失败只推进 attempt 时间戳：短期内不再重打上游，超过 TTL 后仍会再试。
func TestModelMapAttemptThrottle(t *testing.T) {
	a := &auth.Auth{}
	if !a.ModelMapsStale(0) {
		t.Fatal("从未拉取的账号应判定为 stale")
	}
	c := New()
	if c.refreshAccountModelMap(a) {
		t.Fatal("上游不可达时刷新应失败")
	}
	if a.ModelMapsStale(0) {
		t.Fatal("失败后应记录 attempt，避免每个请求都重打上游")
	}
}

func TestEncodeDecodeRoundtrip(t *testing.T) {
	plain := []byte(`{"a":1,"b":"中文内容 😀","c":[true,null,3.14]}`)
	enc := qoderEncode(plain)
	if enc == "" {
		t.Fatal("empty encode")
	}
	dec, err := qoderDecode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(plain) {
		t.Errorf("roundtrip mismatch:\n in=%s\nout=%s", plain, dec)
	}
}

func TestDecodeInvalidChar(t *testing.T) {
	if _, err := qoderDecode("!!!"); err == nil {
		t.Error("expected error for invalid char")
	}
}

func TestBuildAgentBodyDeveloperToSystem(t *testing.T) {
	// developer 角色应改写为 system；其余消息不受影响；原数据不被污染。
	msgs := []map[string]any{
		{"role": "developer", "content": "你是助手"},
		{"role": "system", "content": "保持简洁"},
		{"role": "user", "content": "你好"},
	}
	raw, err := buildAgentBody(msgs, "dmodel", nil, false, "")
	if err != nil {
		t.Fatalf("buildAgentBody: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(body.Messages))
	}
	if body.Messages[0]["role"] != "system" {
		t.Errorf("developer should become system, got %v", body.Messages[0]["role"])
	}
	if body.Messages[1]["role"] != "system" {
		t.Errorf("system unchanged, got %v", body.Messages[1]["role"])
	}
	if body.Messages[2]["role"] != "user" {
		t.Errorf("user unchanged, got %v", body.Messages[2]["role"])
	}
	// 原数据不被污染
	if msgs[0]["role"] != "developer" {
		t.Errorf("source mutated: role=%v", msgs[0]["role"])
	}
	// prompt 取最后一条 user 内容
	var prompt struct {
		ChatContext struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"chat_context"`
	}
	_ = json.Unmarshal(raw, &prompt)
	if prompt.ChatContext.Text.Text != "你好" {
		t.Errorf("prompt = %q, want 你好", prompt.ChatContext.Text.Text)
	}
}
