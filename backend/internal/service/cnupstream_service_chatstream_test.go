//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
)

// cnTwoAccountPool 构造 traework 平台两账号的池：21 积分低、22 积分高，
// 便于区分「按选中账号取号」与旧的「按积分最高取号」。
func cnTwoAccountPool(t *testing.T, up *fakeCnUpstream) *CnUpstreamService {
	t.Helper()
	repo := &fakeCnRepo{accounts: map[int64]*Account{
		21: cnTestAccount(21, PlatformTraeWork),
		22: cnTestAccount(22, PlatformTraeWork),
	}}
	svc := newCnUpstreamTestSvc(repo, up)
	if err := svc.ReloadAccounts(context.Background()); err != nil {
		t.Fatalf("ReloadAccounts: %v", err)
	}
	p := svc.PlatformPool(PlatformTraeWork)
	p.SetCredits("21", 100)
	p.SetCredits("22", 900)
	return svc
}

func okSSE() io.ReadCloser {
	return io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"))
}

// 统一池选中的账号必须成为实际转发账号（旧实现按池内积分最高挑号，
// 调度层的账号级选号/排除集对上游池完全无效）。
func TestCnUpstreamChatStreamPrefersSelectedAccount(t *testing.T) {
	up := &fakeCnUpstream{chatFn: func(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
		return okSSE(), http.StatusOK, nil, nil
	}}
	svc := cnTwoAccountPool(t, up)

	target, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = target.RC.Close() }()
	if target.AccountID != 21 || target.UID != "21" {
		t.Fatalf("target = %+v, want 选中账号 21", target)
	}
	if len(up.chatCalls) != 1 || up.chatCalls[0] != 21 {
		t.Fatalf("chatCalls = %v, want [21]", up.chatCalls)
	}
}

// 上游以 HTTP 400 + 业务码 4028 下发间歇限流：该账号短冷却并轮换到下一个账号。
func TestCnUpstreamChatStreamCoolsSoftRateAndRotates(t *testing.T) {
	up := &fakeCnUpstream{chatFn: func(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
		if a.AccountID == 21 {
			return nil, http.StatusBadRequest, []byte(`{"code":4028,"message":"Your requests have exceeded the quota."}`), nil
		}
		return okSSE(), http.StatusOK, nil, nil
	}}
	svc := cnTwoAccountPool(t, up)

	target, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = target.RC.Close() }()
	if target.AccountID != 22 {
		t.Fatalf("target.AccountID = %d, want 22（限流账号轮换）", target.AccountID)
	}
	st, ok := svc.PlatformPool(PlatformTraeWork).Status("21")
	if !ok || !st.Cooling {
		t.Fatalf("账号 21 应进入冷却, got %+v", st)
	}
	if st.Reason != "rate limited" {
		t.Fatalf("cooldown reason = %q, want rate limited（4028 归软限流）", st.Reason)
	}
	// 冷却时长应在软限流区间（60s），而不是硬权益的 30min。
	if remain := time.Until(st.Until); remain > 2*time.Minute || remain <= 0 {
		t.Fatalf("cooldown remain = %v, want ≤2min", remain)
	}
}

// 池内账号全部撞上软限流：返回带 Retry-After 的整池冷却错误，供上层下发 429。
func TestCnUpstreamChatStreamPoolExhaustedCarriesRetryAfter(t *testing.T) {
	up := &fakeCnUpstream{chatFn: func(_ *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
		return nil, http.StatusBadRequest, []byte(`{"code":4008,"message":"Your requests have exceeded the quota."}`), nil
	}}
	svc := cnTwoAccountPool(t, up)

	if _, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`)); err == nil {
		t.Fatal("expected error on first round too")
	}
	if len(up.chatCalls) == 0 {
		t.Fatal("expected upstream attempts")
	}
	_, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`))
	if err == nil {
		t.Fatal("expected pool exhausted error")
	}
	var exhausted *CnPoolExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("want *CnPoolExhaustedError, got %T: %v", err, err)
	}
	if exhausted.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want >0（供 Retry-After 下发）", exhausted.RetryAfter)
	}
	if !strings.Contains(err.Error(), "retry after") {
		t.Errorf("err = %v, want 文案含恢复时间", err)
	}
}

// SSE 流内业务错误（HTTP 200 之后才出现）必须通过 Report 回灌冷却——
// 这正是「同一个限流错误反复出现、根本没法用」的根因。
func TestCnUpstreamChatStreamReportCoolsAccountOnStreamError(t *testing.T) {
	up := &fakeCnUpstream{chatFn: func(_ *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
		return io.NopCloser(strings.NewReader(soloErrorOnlySSE)), http.StatusOK, nil, nil
	}}
	svc := cnTwoAccountPool(t, up)

	target, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = target.RC.Close() }()

	// 未产出内容首遇 4008：转换层返回携带业务码的 sentinel，Report 须短冷却。
	target.Report(&traework.StreamBeforeOutputError{Se: &traework.SOLOStreamError{Code: 4008, Msg: "Your requests have exceeded the quota."}})
	st, ok := svc.PlatformPool(PlatformTraeWork).Status("21")
	if !ok || !st.Cooling || st.Reason != "rate limited" {
		t.Fatalf("4008 后账号状态 = %+v, want cooling/rate limited", st)
	}

	// 1005 权益不足：长冷却（30min 量级）。
	svc2 := cnTwoAccountPool(t, up)
	target2, err := svc2.ChatStream(PlatformTraeWork, 22, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_ = target2.RC.Close()
	target2.Report(&traework.SOLOStreamError{Code: 1005, Msg: "plan expired"})
	st2, ok := svc2.PlatformPool(PlatformTraeWork).Status("22")
	if !ok || !st2.Cooling || st2.Reason != "credit exhausted" {
		t.Fatalf("1005 后账号状态 = %+v, want cooling/credit exhausted", st2)
	}
	if remain := time.Until(st2.Until); remain < 25*time.Minute {
		t.Fatalf("硬权益冷却时长 = %v, want ≈30min", remain)
	}
}

// Report(nil) 不改变账号状态（成功路径不应被误冷却）。
func TestCnUpstreamChatStreamReportNilKeepsAccountHealthy(t *testing.T) {
	up := &fakeCnUpstream{chatFn: func(_ *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
		return okSSE(), http.StatusOK, nil, nil
	}}
	svc := cnTwoAccountPool(t, up)
	target, err := svc.ChatStream(PlatformTraeWork, 21, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_ = target.RC.Close()
	target.Report(nil)
	if st, _ := svc.PlatformPool(PlatformTraeWork).Status("21"); st.Cooling {
		t.Fatalf("成功不应冷却: %+v", st)
	}
}
