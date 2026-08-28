package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// OpenAIForwardResultToForwardResult 正确映射 token 与计费模型（供 GatewayHandler 复用旧计费链路）。
func TestOpenAIForwardResultToForwardResult(t *testing.T) {
	firstTokenMs := 123
	or := &OpenAIForwardResult{
		RequestID:    "req-1",
		Model:        "glm-5.3",
		BillingModel: "deepseek-v4-flash",
		UpstreamModel: "glm-5.3",
		Usage: OpenAIUsage{InputTokens: 12, OutputTokens: 8, CacheReadInputTokens: 3},
		Stream:       true,
		FirstTokenMs: &firstTokenMs,
	}
	fr := OpenAIForwardResultToForwardResult(or)
	if fr == nil {
		t.Fatal("expected non-nil ForwardResult")
	}
	if fr.Model != "glm-5.3" {
		t.Fatalf("Model = %v, want glm-5.3", fr.Model)
	}
	if fr.UpstreamModel != "deepseek-v4-flash" {
		t.Fatalf("UpstreamModel = %v, want BillingModel (计费模型)", fr.UpstreamModel)
	}
	if fr.Usage.InputTokens != 12 || fr.Usage.OutputTokens != 8 || fr.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("Usage = %+v", fr.Usage)
	}
	if !fr.Stream || fr.FirstTokenMs == nil || *fr.FirstTokenMs != 123 {
		t.Fatalf("Stream/FirstTokenMs = %v/%v", fr.Stream, fr.FirstTokenMs)
	}
	if OpenAIForwardResultToForwardResult(nil) != nil {
		t.Fatal("nil 输入应返回 nil")
	}
	// BillingModel 为空时回退 UpstreamModel。
	or.BillingModel = ""
	fr2 := OpenAIForwardResultToForwardResult(or)
	if fr2.UpstreamModel != "glm-5.3" {
		t.Fatalf("UpstreamModel = %v, want glm-5.3 (回退)", fr2.UpstreamModel)
	}
}

// cnUpstreamSSEFixture 三渠道上游返回的 OpenAI 形 SSE（含 usage 与 finish_reason）。
const cnUpstreamSSEFixture string = "data: {\"id\":\"chatcmpl-cn1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-cn1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"total_tokens\":20}}\n\n" +
	"data: [DONE]\n\n"

// cnGatewayForward 以真实 CnUpstreamService（内置平台默认上游客户端）+ 注入假 ChatStream
// 的场景，白盒调用 forwardCnUpstreamChatCompletions 并返回响应记录器与结果。
func cnGatewayForward(t *testing.T, svc *OpenAIGatewayService, account *Account, body []byte) (*httptest.ResponseRecorder, *OpenAIForwardResult, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	res, err := svc.forwardCnUpstreamChatCompletions(context.Background(), c, account, body)
	return rec, res, err
}

// newCnGatewaySvc 构造测试用网关：真实 CnUpstreamService（仅用于 PlatformUpstream）
// + 注入 fakeFn 作为 cnChatStream。
func newCnGatewaySvc(fakeFn cnChatStreamFn) *OpenAIGatewayService {
	// 只用 PlatformUpstream + 平台默认 upstream；不触发 ReloadAccounts。
	return &OpenAIGatewayService{
		cnUpstream:   NewCnUpstreamService(nil, nil),
		cnChatStream: fakeFn,
	}
}

func TestForwardCnUpstreamChatCompletionsStreamWithUsage(t *testing.T) {
	svc := newCnGatewaySvc(func(platform string, body []byte) (io.ReadCloser, int, []byte, error) {
		if platform != PlatformWorkBuddy {
			t.Fatalf("platform=%q", platform)
		}
		return io.NopCloser(strings.NewReader(cnUpstreamSSEFixture)), 200, nil, nil
	})

	rec, res, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if !res.Stream {
		t.Errorf("Stream=false")
	}
	// usage 从流中抽取，按 tokens 口径计费。
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 8 {
		t.Errorf("usage=%+v", res.Usage)
	}
	if res.Model != "glm-5.2" || res.BillingModel != "glm-5.2" || res.UpstreamModel != "glm-5.2" {
		t.Errorf("model=%q billing=%q upstream=%q", res.Model, res.BillingModel, res.UpstreamModel)
	}
	body := rec.Body.String()
	for _, want := range []string{"你好", "，世界", "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream output missing %q: %q", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

func TestForwardCnUpstreamChatCompletionsNonStreamAggregate(t *testing.T) {
	svc := newCnGatewaySvc(func(_ string, _ []byte) (io.ReadCloser, int, []byte, error) {
		return io.NopCloser(strings.NewReader(cnUpstreamSSEFixture)), 200, nil, nil
	})

	rec, res, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if res.Stream {
		t.Errorf("Stream should be false for non-stream request")
	}
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 8 {
		t.Errorf("usage=%+v", res.Usage)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("aggregated content=%q", msg["content"])
	}
}

func TestForwardCnUpstreamChatCompletionsUpstreamHTTPError(t *testing.T) {
	svc := newCnGatewaySvc(func(_ string, _ []byte) (io.ReadCloser, int, []byte, error) {
		return nil, http.StatusServiceUnavailable, []byte("upstream down"), nil
	})

	rec, _, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected error for upstream 503")
	}
	if !strings.Contains(err.Error(), "http 503") {
		t.Errorf("err=%v", err)
	}
	if strings.Contains(rec.Body.String(), "upstream down") {
		t.Errorf("should not have written upstream body: %q", rec.Body.String())
	}
}

func TestForwardCnUpstreamChatCompletionsAllAccountsUnavailable(t *testing.T) {
	svc := newCnGatewaySvc(func(_ string, _ []byte) (io.ReadCloser, int, []byte, error) {
		return nil, 0, nil, errors.New("fake: all accounts unavailable")
	})
	if _, _, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`)); err == nil {
		t.Fatal("expected error when all accounts unavailable")
	}
}

func TestForwardCnUpstreamChatCompletionsNotWired(t *testing.T) {
	// cnUpstream 与 cnChatStream 均为 nil，命中第 45 行守卫。
	_, _, err := cnGatewayForward(t, &OpenAIGatewayService{}, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("err=%v", err)
	}
}

// TestForwardCnUpstreamCrossPlatformFallback 验证 composite 组下，主平台池内全部失败时
// 会按候选平台顺序切到下一个国产渠道平台（同一请求跨平台 fallback）。
func TestForwardCnUpstreamCrossPlatformFallback(t *testing.T) {
	svc := newCnGatewaySvc(func(platform string, _ []byte) (io.ReadCloser, int, []byte, error) {
		if platform == PlatformTraeWork {
			// 主平台全部账号不可用
			return nil, 0, nil, errors.New("fake: all traework accounts unavailable")
		}
		if platform == PlatformWorkBuddy {
			// 候选平台票足、健康，返回正常 SSE
			return io.NopCloser(strings.NewReader(cnUpstreamSSEFixture)), 200, nil, nil
		}
		t.Fatalf("unexpected platform=%q", platform)
		return nil, 0, nil, errors.New("unexpected platform")
	})

	// primary 为 traework（失败），composite 候选把 workbuddy 作为后备。
	ctx := WithCompositeCandidates(context.Background(), []CompositeRouteCandidate{
		{Platform: PlatformTraeWork},
		{Platform: PlatformWorkBuddy},
	})

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	res, err := svc.forwardCnUpstreamChatCompletions(ctx, c, &Account{Platform: PlatformTraeWork},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("forward should fallback to workbuddy, got err: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// 从候选 workbuddy 成功返回 usage 应被抽取计费。
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 8 {
		t.Errorf("usage=%+v", res.Usage)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") {
		t.Errorf("stream output missing fallback content: %q", body)
	}
}

// TestForwardCnUpstreamFallbackAllPlatformsUnavailable 验证所有候选平台都失败时返回错误。
func TestForwardCnUpstreamFallbackAllPlatformsUnavailable(t *testing.T) {
	svc := newCnGatewaySvc(func(platform string, _ []byte) (io.ReadCloser, int, []byte, error) {
		return nil, 0, nil, fmt.Errorf("fake: all %s accounts unavailable", platform)
	})

	ctx := WithCompositeCandidates(context.Background(), []CompositeRouteCandidate{
		{Platform: PlatformTraeWork},
		{Platform: PlatformWorkBuddy},
		{Platform: PlatformQoder},
	})

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	_, err := svc.forwardCnUpstreamChatCompletions(ctx, c, &Account{Platform: PlatformTraeWork},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected error when all candidate platforms unavailable")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("err should reference unavailability, got: %v", err)
	}
}
