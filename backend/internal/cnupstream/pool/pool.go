// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard       CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                       // 429 → 短冷却
	CoolErr                        // 连续错误 → 中冷却
	CoolLowBalance                 // 余额低于平均 60% → 15min 冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	case CoolLowBalance:
		return "low_balance"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID            string    `json:"uid"`
	Nickname       string    `json:"nickname,omitempty"`
	Credits        int64     `json:"credits"`
	Cooling        bool      `json:"cooling"`
	Until          time.Time `json:"until,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Disabled       bool      `json:"disabled"`
	ErrCount       int       `json:"err_count,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`
	TodayCalls     int       `json:"today_calls"`
	TodayTokens    int64     `json:"today_tokens"`
	TodayConsumed  int64     `json:"today_consumed"`
	TodayDate      string    `json:"today_date,omitempty"`
	Preferred      bool      `json:"preferred"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string

	todayCalls    int
	todayTokens   int64
	todayConsumed int64
	todayDate     string
	preferred     bool
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type accountState struct {
	Credits        int64     `json:"credits"`
	Disabled       bool      `json:"disabled"`
	Reason         string    `json:"reason,omitempty"`
	Until          time.Time `json:"until,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`
	TodayCalls     int       `json:"today_calls,omitempty"`
	TodayTokens    int64     `json:"today_tokens,omitempty"`
	TodayConsumed  int64     `json:"today_consumed,omitempty"`
	TodayDate      string    `json:"today_date,omitempty"`
	Preferred      bool      `json:"preferred,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 优先返回标记了「优先使用」的 healthy 账号（组内积分最高），无优先或全失败再回退普通账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var bestPref, bestNormal *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if e.preferred {
			if bestPref == nil || e.credits > bestPref.credits {
				bestPref = e
			}
		} else {
			if bestNormal == nil || e.credits > bestNormal.credits {
				bestNormal = e
			}
		}
	}
	if bestPref != nil {
		return bestPref.a
	}
	if bestNormal != nil {
		return bestNormal.a
	}
	return nil
}

// PickForAccount 优先返回网关统一池选中的账号（preferredAccountID = ent 账号主键）：
// 该账号健康且未被 tried 排除时直接命中；否则回落到 PickExcluding 的「健康账号中
// 积分最高者」。让「调度选中的账号」真正成为「实际转发用的账号」，账号级冷却与
// failover 排除集才能对上游池生效。
func (p *Pool) PickForAccount(preferredAccountID int64, tried map[string]bool) *auth.Auth {
	if preferredAccountID <= 0 {
		return p.PickExcluding(tried)
	}
	p.mu.RLock()
	now := time.Now()
	for uid, e := range p.byUID {
		if e.a == nil || e.a.AccountID != preferredAccountID {
			continue
		}
		if tried != nil && tried[uid] {
			break
		}
		if e.healthy(now) {
			a := e.a
			p.mu.RUnlock()
			return a
		}
		break
	}
	p.mu.RUnlock()
	return p.PickExcluding(tried)
}

// EarliestRecovery 返回池内未禁用账号中最早的冷却恢复剩余时间。
// 全部健康（无冷却中账号）或池为空时返回 (0, false)。供上层在「整池不可用」时
// 给客户端下发 Retry-After，避免用户在冷却窗口内反复重试打爆上游。
func (p *Pool) EarliestRecovery() (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var earliest time.Time
	for _, e := range p.byUID {
		if e.disabled {
			continue
		}
		if e.until.IsZero() || !now.Before(e.until) {
			continue
		}
		if earliest.IsZero() || e.until.Before(earliest) {
			earliest = e.until
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	return time.Until(earliest), true
}

// SetPreferred 设置账号是否优先使用（持久化到 state）。
func (p *Pool) SetPreferred(uid string, pref bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.preferred = pref
		p.saveLocked()
	}
}

// PreferredUIDs 返回全部标记为优先使用的账号 UID。
func (p *Pool) PreferredUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for uid, e := range p.byUID {
		if e.preferred {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

// SetCredits 更新账号余额；余额下降的差值计入该账号今日消耗。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if e.credits > credits {
			p.ensureTodayLocked(e)
			e.todayConsumed += e.credits - credits
		}
		e.credits = credits
	}
	p.saveLocked()
}

// NoteCall 记录一次成功的调用（今日调用次数 + token 量）。
func (p *Pool) NoteCall(uid string, tokens int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.ensureTodayLocked(e)
		e.todayCalls++
		if tokens > 0 {
			e.todayTokens += tokens
		}
		p.saveLocked()
	}
}

// ensureTodayLocked 跨天时清空今日桶（调用方需已持锁）。
func (p *Pool) ensureTodayLocked(e *entry) {
	today := time.Now().Format("2006-01-02")
	if e.todayDate == "" {
		e.todayDate = today
	}
	if e.todayDate != today {
		e.todayDate = today
		e.todayCalls = 0
		e.todayTokens = 0
		e.todayConsumed = 0
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// SetDisabled 设置账号禁用/启用状态。
func (p *Pool) SetDisabled(uid string, d bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = d
		if d {
			e.reason = "手动停用"
		} else {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
	}
	p.saveLocked()
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AuthByAccountID 按 ent 账号主键取池内长期存活的凭证对象。
// 动态模型映射等运行期状态挂在 auth.Auth 上，必须落到池内这一个实例，
// 管理端「拉取上游模型」与网关聊天才会读到同一份数据。
func (p *Pool) AuthByAccountID(accountID int64) *auth.Auth {
	if accountID <= 0 {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		if e.a != nil && e.a.AccountID == accountID {
			return e.a
		}
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

// MaxHealthyCredits 返回该池 healthy 账号中最大的剩余积分；无可用账号时返回 0。
// 用于跨平台模型映射时按余额高优先决定候选池顺序。
func (p *Pool) MaxHealthyCredits() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var max int64
	for _, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if e.credits > max {
			max = e.credits
		}
	}
	return max
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:            uid,
		Nickname:       e.a.Nickname,
		Credits:        e.credits,
		Cooling:        !e.until.IsZero() && now.Before(e.until),
		Until:          e.until,
		Reason:         e.reason,
		Disabled:       e.disabled,
		ErrCount:       e.errCount,
		LastCheckinOK:  e.lastCheckinOK,
		LastCheckinAt:  e.lastCheckinAt,
		LastCheckinMsg: e.lastCheckinMsg,
		TodayCalls:     e.todayCalls,
		TodayTokens:    e.todayTokens,
		TodayConsumed:  e.todayConsumed,
		TodayDate:      e.todayDate,
		Preferred:      e.preferred,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:              &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:        s.Credits,
			disabled:       s.Disabled,
			reason:         s.Reason,
			until:          s.Until,
			lastCheckinOK:  s.LastCheckinOK,
			lastCheckinAt:  s.LastCheckinAt,
			lastCheckinMsg: s.LastCheckinMsg,
			todayCalls:     s.TodayCalls,
			todayTokens:    s.TodayTokens,
			todayConsumed:  s.TodayConsumed,
			todayDate:      s.TodayDate,
			preferred:      s.Preferred,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]accountState{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = accountState{
			Credits:        e.credits,
			Disabled:       e.disabled,
			Reason:         e.reason,
			Until:          e.until,
			LastCheckinOK:  e.lastCheckinOK,
			LastCheckinAt:  e.lastCheckinAt,
			LastCheckinMsg: e.lastCheckinMsg,
			TodayCalls:     e.todayCalls,
			TodayTokens:    e.todayTokens,
			TodayConsumed:  e.todayConsumed,
			TodayDate:      e.todayDate,
			Preferred:      e.preferred,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
