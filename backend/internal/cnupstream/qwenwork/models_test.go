package qwenwork

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

// 测试用假公钥：CosySession 会用 serverPubKey 加密临时密钥，
// 单测只需签名链路能跑通，不校验真实服务端公钥。
func testAuth() *auth.Auth {
	return &auth.Auth{
		Kind:         "qwenwork",
		UID:          "uid-1",
		Nickname:     "tester",
		AccessToken:  "dt-test",
		RefreshToken: "drt-test",
		MachineID:    "machine-test",
		MachineToken: strings.Repeat("a", 50),
		MachineType:  "0123456789abcdefgh",
	}
}

// FetchModels 必须优先解析 qwork 场景（千问办公模型挂在 qwork，
// qoder 挂 chat），并带 x-qw-request-id 头；contextWindow 取 max_input_tokens。
func TestFetchModelsPrefersQworkScene(t *testing.T) {
	var gotReqID, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReqID = r.Header.Get("x-qw-request-id")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"qwork": []any{
				map[string]any{"key": "qmodel_38max", "display_name": "Qwen3.8-Max", "enable": true, "max_input_tokens": 262144, "price_factor": 1.5},
				map[string]any{"key": "offmodel", "display_name": "Disabled Model", "enable": false},
			},
			"chat": []any{
				map[string]any{"key": "chatonly", "display_name": "Chat Only", "enable": true},
			},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Gateway: srv.URL}
	models, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotPath != EpModels {
		t.Fatalf("path = %q, want %q", gotPath, EpModels)
	}
	if gotReqID == "" {
		t.Fatal("缺少 x-qw-request-id 头")
	}
	// 模型列表走 COSY 签名 token（不是裸 dt- Bearer），payload 内含 requestId/info。
	if !strings.HasPrefix(gotAuth, "Bearer COSY.") {
		t.Fatalf("Authorization = %q, want COSY 签名 token", gotAuth)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v, want 仅 qwork 场景 1 个启用模型（disabled 与 chat 场景都要排除）", models)
	}
	if models[0].ID != "qwen3.8-max" || models[0].Name != "Qwen3.8-Max" {
		t.Fatalf("模型名解析错误: %+v", models[0])
	}
	if models[0].ContextWindow != 262144 {
		t.Fatalf("ContextWindow = %d, want 262144", models[0].ContextWindow)
	}
	// 拉到的 key 映射必须缓存进 Client 供 ChatStream 路由（否则只能靠静态表）。
	if key := c.modelKey("qwen3.8-max"); key != "qmodel_38max" {
		t.Fatalf("modelKey(qwen3.8-max) = %q, want qmodel_38max", key)
	}
}

// 无 qwork 场景时回退 chat（老网关行为），保证不同账号套餐下模型列表仍可解析。
func TestFetchModelsFallsBackToChatScene(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chat": []any{map[string]any{"key": "qmodel", "display_name": "Qwen3.7-Plus", "enable": true}},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Gateway: srv.URL}
	models, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qwen3.7-plus" {
		t.Fatalf("models = %+v", models)
	}
}

// 缺机器指纹必须明确报错（粘贴凭证建号时必须带 machineId/machineToken/machineType）。
func TestFetchModelsRequiresMachineFingerprint(t *testing.T) {
	a := testAuth()
	a.MachineToken = ""
	c := &Client{HTTP: http.DefaultClient, Gateway: "http://127.0.0.1:1"}
	if _, err := c.FetchModels(a); err == nil || !strings.Contains(err.Error(), "machine fingerprint") {
		t.Fatalf("err = %v, want missing machine fingerprint", err)
	}
}

// FetchModelPricing 复用同一动态接口，price_factor 要落到倍率。
func TestFetchModelPricingUsesPriceFactor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"qwork": []any{map[string]any{"key": "dfmodel", "display_name": "DeepSeek-V4-Flash", "enable": true, "price_factor": 0.3}},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Gateway: srv.URL}
	pricing, err := c.FetchModelPricing(testAuth())
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	if len(pricing) != 1 || pricing[0].Rate != 0.3 {
		t.Fatalf("pricing = %+v", pricing)
	}
	if pricing[0].Model != "deepseek-v4-flash" {
		t.Fatalf("model = %q, want 规范化客户端名", pricing[0].Model)
	}
}

// EnsureFingerprint 用于一键授权建号时补齐三件套；已有值不得被覆盖。
func TestEnsureFingerprintKeepsExisting(t *testing.T) {
	_ = rsa.GenerateKey // 仅确保测试依赖被引用（cosy 使用 RSA 公钥加密）
	a := &auth.Auth{MachineID: "keep-me"}
	EnsureFingerprint(a)
	if a.MachineID != "keep-me" {
		t.Fatalf("MachineID = %q, want 保留原值", a.MachineID)
	}
	if len(a.MachineToken) == 0 || len(a.MachineType) != 18 {
		t.Fatalf("fingerprint incomplete: token=%q type=%q", a.MachineToken, a.MachineType)
	}
	b := &auth.Auth{}
	EnsureFingerprint(b)
	if len(b.MachineID) != 36 || b.MachineToken == "" {
		t.Fatalf("auto fingerprint missing: %+v", b)
	}
	_ = rand.Reader
}
