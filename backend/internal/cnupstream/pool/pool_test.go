package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "余额不足" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestRecordCheckinAndRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordCheckin("u1", true, "ok")
	st, _ := p.Status("u1")
	if !st.LastCheckinOK || st.LastCheckinAt.IsZero() {
		t.Errorf("status=%+v", st)
	}
	p.Remove("u1")
	if _, ok := p.Status("u1"); ok {
		t.Error("account should be removed")
	}
}

func TestDailyUsageTracking(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 消耗：余额从 1000 降到 800 → 今日消耗 200
	p.SetCredits("u1", 1000)
	p.SetCredits("u1", 800)
	p.NoteCall("u1", 1500)
	p.NoteCall("u1", 500)
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if st.TodayConsumed != 200 {
		t.Fatalf("consumed=%d want 200", st.TodayConsumed)
	}
	if st.TodayCalls != 2 {
		t.Fatalf("calls=%d want 2", st.TodayCalls)
	}
	if st.TodayTokens != 2000 {
		t.Fatalf("tokens=%d want 2000", st.TodayTokens)
	}
	if st.TodayDate == "" {
		t.Fatal("today_date empty")
	}
	// 余额上涨（如签到）不计消耗
	p.SetCredits("u1", 1200)
	st, _ = p.Status("u1")
	if st.TodayConsumed != 200 {
		t.Fatalf("consumed after topup=%d want 200", st.TodayConsumed)
	}
}

func TestDailyUsagePersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500)
	p.SetCredits("u1", 480)
	p.NoteCall("u1", 3000)
	// 重新加载应保留今日统计
	p2 := New(fp)
	st, _ := p2.Status("u1")
	if st.TodayConsumed != 20 || st.TodayCalls != 1 || st.TodayTokens != 3000 {
		t.Fatalf("reload stats=%+v", st)
	}
}

func TestPickPrefersPreferredAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "normal-high"})
	p.Add(&auth.Auth{UID: "pref-low"})
	p.SetCredits("normal-high", 9000)
	p.SetCredits("pref-low", 10)
	// 未标记：余额高的 normal-high
	if got := p.Pick(); got == nil || got.UID != "normal-high" {
		t.Fatalf("pick=%v want normal-high", got)
	}
	// 标记 pref-low 优先：即使余额低也先选
	p.SetPreferred("pref-low", true)
	if got := p.Pick(); got == nil || got.UID != "pref-low" {
		t.Fatalf("pick=%v want pref-low (preferred)", got)
	}
	// 优先组内有多个：积分高者优先
	p.Add(&auth.Auth{UID: "pref-high"})
	p.SetCredits("pref-high", 500)
	p.SetPreferred("pref-high", true)
	if got := p.Pick(); got == nil || got.UID != "pref-high" {
		t.Fatalf("pick=%v want pref-high", got)
	}
	// 取消全部优先：回退余额最高
	p.SetPreferred("pref-low", false)
	p.SetPreferred("pref-high", false)
	if got := p.Pick(); got == nil || got.UID != "normal-high" {
		t.Fatalf("pick=%v want normal-high after unpin", got)
	}
}

func TestPreferredPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetPreferred("u1", true)
	p2 := New(fp)
	if got := p2.PreferredUIDs(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("preferred=%v want [u1]", got)
	}
}
