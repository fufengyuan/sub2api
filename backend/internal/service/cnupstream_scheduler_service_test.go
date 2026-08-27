//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/scheduler"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

func testCheckinConfig(enabled bool) *config.Config {
	return &config.Config{
		Checkin: config.CheckinConfig{
			Enabled:        enabled,
			Times:          []string{"09:00", "21:00"},
			KeepaliveHours: []int{22},
			BackoffSeconds: 5,
		},
	}
}

// 配置关闭时 Start 直接跳过，不构造任何调度器。
func TestSchedulerServiceDisabled(t *testing.T) {
	svc := NewCnUpstreamSchedulerService(nil, testCheckinConfig(false))
	svc.Start(context.Background()) // 不应 panic
	svc.Stop()
}

// schedulerConfig 注入 PersistAuth / Reload 钩子与 BackoffSeconds 退避。
func TestSchedulerServiceConfigHooks(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{
		20: cnTestAccount(20, PlatformWorkBuddy),
	}}
	cn := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})
	svc := NewCnUpstreamSchedulerService(cn, testCheckinConfig(true))

	cfg := svc.schedulerConfig(PlatformWorkBuddy, context.Background())
	if cfg.PersistAuth == nil {
		t.Fatal("PersistAuth hook must be injected")
	}
	if cfg.Reload == nil {
		t.Fatal("Reload hook must be injected")
	}
	if cfg.CheckinBackoff != 5*time.Second {
		t.Fatalf("CheckinBackoff = %v, want 5s", cfg.CheckinBackoff)
	}
	// Reload 钩子触发账号加载。
	cfg.Reload()
	if _, ok := cn.PlatformPool(PlatformWorkBuddy).Status("20"); !ok {
		t.Fatal("Reload hook should load accounts into pool")
	}
	// PersistAuth 钩子把凭证回写 ent。
	if err := cfg.PersistAuth(&auth.Auth{UID: "20", AccessToken: "new-at", RefreshToken: "new-rt"}); err != nil {
		t.Fatalf("PersistAuth: %v", err)
	}
	if len(repo.updated) != 1 || repo.updated[0].Credentials["accessToken"] != "new-at" {
		t.Fatalf("persist via hook failed: %+v", repo.updated)
	}
}

// checkinObserver 把签到结果落库（积分 + lastCheckin 状态）。
func TestSchedulerServiceCheckinObserver(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{
		21: cnTestAccount(21, PlatformTraeWork),
	}}
	cn := newCnUpstreamTestSvc(repo, &fakeCnUpstream{})
	svc := NewCnUpstreamSchedulerService(cn, testCheckinConfig(true))

	obs := svc.checkinObserver(PlatformTraeWork)
	obs(scheduler.CheckinResult{UID: "21", OK: true, Msg: "ok", Remain: 321, HasRemain: true})
	if len(repo.updated) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updated))
	}
	creds := repo.updated[0].Credentials
	if creds["creditsRemain"] != int64(321) {
		t.Fatalf("creditsRemain = %v, want 321", creds["creditsRemain"])
	}
	if res, _ := creds["lastCheckinResult"].(string); res != "ok" {
		t.Fatalf("lastCheckinResult = %v, want ok", creds["lastCheckinResult"])
	}
}
