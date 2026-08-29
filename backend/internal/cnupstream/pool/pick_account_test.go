package pool

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

func newAuth(accountID int64, uid string) *auth.Auth {
	return &auth.Auth{Kind: "traework", UID: uid, AccountID: accountID, AccessToken: "at"}
}

func TestPickForAccountPrefersSelected(t *testing.T) {
	p := New("")
	p.Add(newAuth(21, "21"))
	p.Add(newAuth(22, "22"))
	p.SetCredits("21", 100)
	p.SetCredits("22", 900)

	// 选中低积分账号时必须用它：证明网关统一池的账号级选号对上游池生效。
	got := p.PickForAccount(21, nil)
	if got == nil || got.AccountID != 21 {
		t.Fatalf("PickForAccount(21) = %+v, want 账号 21", got)
	}
	// 未指定账号时回落到积分最高的健康账号。
	if got := p.PickForAccount(0, nil); got == nil || got.AccountID != 22 {
		t.Fatalf("PickForAccount(0) = %+v, want 账号 22", got)
	}
	// 选中账号在请求级排除集内时回落。
	if got := p.PickForAccount(21, map[string]bool{"21": true}); got == nil || got.AccountID != 22 {
		t.Fatalf("PickForAccount(21, excluded) = %+v, want 账号 22", got)
	}
}

func TestPickForAccountFallsBackWhenSelectedCooling(t *testing.T) {
	p := New("")
	p.Add(newAuth(21, "21"))
	p.Add(newAuth(22, "22"))
	p.SetCredits("21", 900)
	p.SetCredits("22", 100)

	p.Cooldown("21", CoolSoft, 60*time.Second, "rate limited")
	got := p.PickForAccount(21, nil)
	if got == nil || got.AccountID != 22 {
		t.Fatalf("冷却中的选中账号应回落到健康账号, got %+v", got)
	}
}

func TestEarliestRecovery(t *testing.T) {
	p := New("")
	p.Add(newAuth(21, "21"))
	p.Add(newAuth(22, "22"))

	// 无冷却：不可计算恢复时间。
	if d, ok := p.EarliestRecovery(); ok || d != 0 {
		t.Fatalf("EarliestRecovery() = (%v,%v), want (0,false)", d, ok)
	}
	p.Cooldown("21", CoolSoft, 60*time.Second, "rate limited")
	p.Cooldown("22", CoolHard, 30*time.Minute, "credit exhausted")
	d, ok := p.EarliestRecovery()
	if !ok {
		t.Fatal("expected a recoverable cooling account")
	}
	// 取最早恢复的那个（软限流 60s），而不是最长冷却。
	if d <= 50*time.Second || d > 60*time.Second {
		t.Fatalf("EarliestRecovery = %v, want ≈60s", d)
	}
	// 禁用的账号不参与恢复时间计算，也不会把窗口拉长。
	p.Add(newAuth(23, "23"))
	p.Disable("23", "session dead")
	if d2, _ := p.EarliestRecovery(); d2 <= 50*time.Second || d2 > 60*time.Second {
		t.Fatalf("禁用账号不应影响恢复时间: %v, want ≈60s", d2)
	}
}
