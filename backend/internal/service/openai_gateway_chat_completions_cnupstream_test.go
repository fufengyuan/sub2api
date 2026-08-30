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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"

	"github.com/gin-gonic/gin"
)

// OpenAIForwardResultToForwardResult 正确映射 token 与计费模型（供 GatewayHandler 复用旧计费链路）。
func TestOpenAIForwardResultToForwardResult(t *testing.T) {
	firstTokenMs := 123
	or := &OpenAIForwardResult{
		RequestID:     "req-1",
		Model:         "glm-5.3",
		BillingModel:  "deepseek-v4-flash",
		UpstreamModel: "glm-5.3",
		Usage:         OpenAIUsage{InputTokens: 12, OutputTokens: 8, CacheReadInputTokens: 3},
		Stream:        true,
		FirstTokenMs:  &firstTokenMs,
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

// cnTarget 构造注入用的假上游目标（可选携带 report 回调便于断言冷却回灌）。
func cnTarget(sse string, report func(error)) *CnChatTarget {
	return &CnChatTarget{
		RC:     io.NopCloser(strings.NewReader(sse)),
		Status: http.StatusOK,
		report: report,
	}
}

func TestForwardCnUpstreamChatCompletionsStreamWithUsage(t *testing.T) {
	svc := newCnGatewaySvc(func(platform string, accountID int64, _ []byte) (*CnChatTarget, error) {
		if platform != PlatformWorkBuddy {
			t.Fatalf("platform=%q", platform)
		}
		if accountID != 77 {
			t.Fatalf("accountID=%d, want 77（统一池选中账号必须透传到取号）", accountID)
		}
		return cnTarget(cnUpstreamSSEFixture, nil), nil
	})

	rec, res, err := cnGatewayForward(t, svc, &Account{ID: 77, Platform: PlatformWorkBuddy},
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
	svc := newCnGatewaySvc(func(_ string, _ int64, _ []byte) (*CnChatTarget, error) {
		return cnTarget(cnUpstreamSSEFixture, nil), nil
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
	svc := newCnGatewaySvc(func(_ string, _ int64, _ []byte) (*CnChatTarget, error) {
		return &CnChatTarget{Status: http.StatusServiceUnavailable, Body: []byte("upstream down")}, nil
	})

	rec, _, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected error for upstream 503")
	}
	// 未写出响应前统一包装为 failover 错误，原始上游细节保留在 Reason。
	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("want UpstreamFailoverError, got %T: %v", err, err)
	}
	if !strings.Contains(string(failoverErr.Reason), "http 503") {
		t.Errorf("reason=%v, want 保留 http 503 上游细节", failoverErr.Reason)
	}
	if failoverErr.Scope != GatewayFailureScopeRequest {
		t.Errorf("scope=%v, want request（传输层失败不惩罚账号健康度）", failoverErr.Scope)
	}
	if strings.Contains(rec.Body.String(), "upstream down") {
		t.Errorf("should not have written upstream body: %q", rec.Body.String())
	}
}

// TestForwardCnUpstreamChatCompletionsPoolExhaustedIs429 整池冷却时按 429 + Retry-After
// 交给 failover，而不是当成可立即重试的 502。
func TestForwardCnUpstreamChatCompletionsPoolExhaustedIs429(t *testing.T) {
	svc := newCnGatewaySvc(func(platform string, _ int64, _ []byte) (*CnChatTarget, error) {
		return nil, &CnPoolExhaustedError{Platform: platform, RetryAfter: 45 * time.Second, Accounts: 2}
	})

	_, _, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected error when whole pool cooling")
	}
	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("want UpstreamFailoverError, got %T: %v", err, err)
	}
	if failoverErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d, want 429", failoverErr.StatusCode)
	}
	if got := failoverErr.ResponseHeaders.Get("Retry-After"); got != "45" {
		t.Errorf("Retry-After=%q, want 45", got)
	}
}

func TestForwardCnUpstreamChatCompletionsNotWired(t *testing.T) {
	// cnUpstream 与 cnChatStream 均为 nil，命中「service not wired」守卫。
	_, _, err := cnGatewayForward(t, &OpenAIGatewayService{}, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("err=%v", err)
	}
}

// TestForwardCnUpstreamNoCrossPlatformFallback 统一账号池后不再做跨平台 fallback：
// 只按选中账号的平台执行一次转发，失败包装成 failover 错误交给 handler 换号。
func TestForwardCnUpstreamNoCrossPlatformFallback(t *testing.T) {
	var calls []string
	svc := newCnGatewaySvc(func(platform string, _ int64, _ []byte) (*CnChatTarget, error) {
		calls = append(calls, platform)
		return nil, fmt.Errorf("fake: all %s accounts unavailable", platform)
	})

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	_, err := svc.forwardCnUpstreamChatCompletions(context.Background(), c, &Account{Platform: PlatformTraeWork},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected failover error")
	}
	if len(calls) != 1 || calls[0] != PlatformTraeWork {
		t.Fatalf("calls=%v, want 仅选中平台 traework 一次（候选平台不再自动尝试）", calls)
	}
	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("want UpstreamFailoverError, got %T", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("未写出响应前不应写 body: %q", rec.Body.String())
	}
}

// soloErrorOnlySSE 上游 SSE 流：未产出任何内容，第一时间返回 event:error（模拟 4008 间歇配额超限）。
const soloErrorOnlySSE string = "event:metadata\ndata:{}\n\n" +
	"event:error\ndata:{\"code\":4008,\"message\":\"Your requests have exceeded the quota.\"}\n\n"

// soloMidQuotaErrorSSE 已产出一段内容后才遇到 4028 频控：流要正常收尾（不再返回错误），
// 但账号仍必须被回灌冷却，否则下一轮会被「积分最高」再次选中。
const soloMidQuotaErrorSSE string = "event:metadata\ndata:{}\n\n" +
	"event:output\ndata:{\"response\":\"前半\"}\n\n" +
	"event:error\ndata:{\"code\":4028,\"message\":\"your requests are too frequent\"}\n\n"

// TestForwardCnUpstreamStreamErrorBeforeOutputReportsAndFailsover 验证：流内未产出内容
// 即业务错误时，① 不向客户端泄漏错误文本；② 按软限流回灌账号冷却（Report）；
// ③ 返回带换号退避的 failover 错误，由 handler 换下一个账号。
func TestForwardCnUpstreamStreamErrorBeforeOutputReportsAndFailsover(t *testing.T) {
	var reported error
	svc := newCnGatewaySvc(func(platform string, _ int64, _ []byte) (*CnChatTarget, error) {
		if platform != PlatformTraeWork {
			t.Fatalf("unexpected platform=%q", platform)
		}
		return cnTarget(soloErrorOnlySSE, func(err error) { reported = err }), nil
	})

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	_, err := svc.forwardCnUpstreamChatCompletions(context.Background(), c, &Account{Platform: PlatformTraeWork},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err == nil {
		t.Fatal("expected failover error on pre-output business error")
	}
	var failoverErr *UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		t.Fatalf("want UpstreamFailoverError, got %T", err)
	}
	if failoverErr.NextAccountRetryDelay != cnSoftRateSwitchBackoff {
		t.Errorf("backoff=%v, want %v（软限流换号前退避）", failoverErr.NextAccountRetryDelay, cnSoftRateSwitchBackoff)
	}
	if failoverErr.Scope != GatewayFailureScopeAccount {
		t.Errorf("scope=%v, want account", failoverErr.Scope)
	}
	if reported == nil {
		t.Fatal("必须把流内业务错误回灌上游池（Report），否则限流账号会被反复选中")
	}
	var soloErr *traework.SOLOStreamError
	if !errors.As(reported, &soloErr) || soloErr.Code != 4008 {
		t.Errorf("reported=%v, want solo 4008", reported)
	}
	if strings.Contains(rec.Body.String(), "exceeded the quota") || strings.Contains(rec.Body.String(), "solo error") {
		t.Errorf("should not leak solo business error to client: %q", rec.Body.String())
	}
}

// TestForwardCnUpstreamMidStreamErrorCoolsAccount 已产出内容的流内业务错误：客户端已拿到
// 前半段并正常收尾（不报错），但账号仍须被回灌冷却。
func TestForwardCnUpstreamMidStreamErrorCoolsAccount(t *testing.T) {
	var reported error
	svc := newCnGatewaySvc(func(_ string, _ int64, _ []byte) (*CnChatTarget, error) {
		return cnTarget(soloMidQuotaErrorSSE, func(err error) { reported = err }), nil
	})

	rec, res, err := cnGatewayForward(t, svc, &Account{Platform: PlatformTraeWork},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("已产出内容时不应报错: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if reported == nil {
		t.Fatal("mid-stream 业务错误也必须回灌冷却")
	}
	var soloErr *traework.SOLOStreamError
	if !errors.As(reported, &soloErr) || soloErr.Kind() != provider.ErrSoftRate {
		t.Errorf("reported=%v, want solo 4028 soft rate", reported)
	}
	if !strings.Contains(rec.Body.String(), "前半") {
		t.Errorf("已产出内容需保留: %q", rec.Body.String())
	}
}

// TestForwardCnUpstreamHardCreditErrorHasNoBackoff 1005 权益不足是账号级硬错误：
// 标记账号作用域但不做换号退避（等待不会让权益恢复）。
func TestForwardCnUpstreamHardCreditErrorHasNoBackoff(t *testing.T) {
	err := cnUpstreamFailoverError(&Account{Platform: PlatformTraeWork},
		&traework.StreamBeforeOutputError{Se: &traework.SOLOStreamError{Code: 1005, Msg: "plan expired"}})
	if err.StatusCode != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", err.StatusCode)
	}
	if err.Scope != GatewayFailureScopeAccount {
		t.Errorf("scope=%v, want account", err.Scope)
	}
	if err.NextAccountRetryDelay != 0 {
		t.Errorf("backoff=%v, want 0（硬权益不等）", err.NextAccountRetryDelay)
	}
}

// TestCnUpstreamFailoverErrorSoftRateFromBody HTTP 4xx + JSON 业务码（4028）也按软限流退避。
// 软限流应映射为 429 + Retry-After，让 OpenAI 网关路径 mapUpstreamError(429) 返回
// rate_limit_error，市面上 agent 客户端遇到 429 会自动重试（而非 502 不重试）。
func TestCnUpstreamFailoverErrorSoftRateFromBody(t *testing.T) {
	err := cnUpstreamFailoverError(&Account{Platform: PlatformTraeWork},
		fmt.Errorf(`cnupstream: %s upstream error (http 400): {"code":4028,"message":"Your requests have exceeded the quota."}`, PlatformTraeWork))
	if err.Scope != GatewayFailureScopeAccount {
		t.Errorf("scope=%v, want account", err.Scope)
	}
	if err.NextAccountRetryDelay != cnSoftRateSwitchBackoff {
		t.Errorf("backoff=%v, want %v", err.NextAccountRetryDelay, cnSoftRateSwitchBackoff)
	}
	if err.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d, want 429（上游频控应映射为通用 429 让 agent 自动重试）", err.StatusCode)
	}
	if retry := err.ResponseHeaders.Get("Retry-After"); retry == "" {
		t.Errorf("429 必须携带 Retry-After，got %q", retry)
	}
}

// TestForwardCnUpstreamStreamWritesKeepaliveBeforeFirstOutput 国产渠道流式路径在
// 首个可见输出前必须向下游写 keepalive 心跳：长推理（思考数分钟无输出）期间若
// 无任何字节写出，中间 nginx 的 proxy_read_timeout（600s）会按空闲把连接判死回
// 504。这是本次 504 修复的核心断言——此前该路径完全没有 keepalive。
func TestForwardCnUpstreamStreamWritesKeepaliveBeforeFirstOutput(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// 首个 chunk 延迟 1.2s，超过 keepalive interval（1s）的首拍，让心跳先写出。
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.Close()
	}()

	svc := newCnGatewaySvc(func(_ string, _ int64, _ []byte) (*CnChatTarget, error) {
		return &CnChatTarget{RC: pr, Status: http.StatusOK}, nil
	})
	svc.cfg = &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}

	rec, _, err := cnGatewayForward(t, svc, &Account{Platform: PlatformWorkBuddy},
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Errorf("首个可见输出前未写出 keepalive 心跳（长推理会被 nginx 空闲超时判 504）: %q", body)
	}
	if !strings.Contains(body, "你好") {
		t.Errorf("实际输出缺失: %q", body)
	}
}

// TestCnUpstreamFailoverErrorSSEStreamSoftRate 未产出内容的 SSE 流内业务错误 4028
// （ErrSoftRate）同样映射为 429 + Retry-After。这是国产渠道最常触发的频控路径。
func TestCnUpstreamFailoverErrorSSEStreamSoftRate(t *testing.T) {
	err := cnUpstreamFailoverError(&Account{Platform: PlatformTraeWork},
		&traework.StreamBeforeOutputError{Se: &traework.SOLOStreamError{Code: 4028, Msg: "your requests are too frequent"}})
	if err.Scope != GatewayFailureScopeAccount {
		t.Errorf("scope=%v, want account", err.Scope)
	}
	if err.NextAccountRetryDelay != cnSoftRateSwitchBackoff {
		t.Errorf("backoff=%v, want %v", err.NextAccountRetryDelay, cnSoftRateSwitchBackoff)
	}
	if err.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d, want 429（SSE 流内 4028 频控应映射为通用 429）", err.StatusCode)
	}
	if retry := err.ResponseHeaders.Get("Retry-After"); retry == "" {
		t.Errorf("429 必须携带 Retry-After，got %q", retry)
	}
}
