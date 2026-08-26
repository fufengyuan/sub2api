package service

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/pool"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/qoder"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/upstream"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

// cnAccountRepo 是 CnUpstreamService 对账号仓储的最小依赖切片：只消费账号列表，
// 其余能力（如 Update 持久化刷新后的凭证）通过可选的接口断言注入。
type cnAccountRepo interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
}

// cnUpdater 可选接口：若仓储实现 Update，则刷新 token 后回写 credentials。
type cnUpdater interface {
	Update(ctx context.Context, account *Account) error
}

// cnUpstreamPlatform 单个平台在运行期的池与上游客户端。
type cnUpstreamPlatform struct {
	pool     *pool.Pool
	upstream provider.Upstream
}

// CnUpstreamService 多渠道上游服务：加载三渠道账号组池并提供 ChatStream / 签到 / 刷新等能力。
type CnUpstreamService struct {
	accountRepo cnAccountRepo
	cfg         *config.Config

	platforms map[string]*cnUpstreamPlatform
	// acctRefs uid -> *Account，RefreshToken 后回写凭证用（保留其余字段避免覆盖丢失）。
	acctRefs map[string]*Account
}

// NewCnUpstreamService 构建三个平台各自的 Pool + Upstream。
func NewCnUpstreamService(accountRepo cnAccountRepo, cfg *config.Config) *CnUpstreamService {
	svc := &CnUpstreamService{
		accountRepo: accountRepo,
		cfg:         cfg,
		platforms:   make(map[string]*cnUpstreamPlatform),
		acctRefs:    make(map[string]*Account),
	}
	svc.platforms[PlatformWorkBuddy] = &cnUpstreamPlatform{pool: pool.New(""), upstream: upstream.New()}
	svc.platforms[PlatformTraeWork] = &cnUpstreamPlatform{pool: pool.New(""), upstream: traework.New()}
	svc.platforms[PlatformQoder] = &cnUpstreamPlatform{pool: pool.New(""), upstream: qoder.New()}
	return svc
}

// PlatformPool 返回某平台的账号池；平台未知返回 nil。
func (s *CnUpstreamService) PlatformPool(platform string) *pool.Pool {
	if rt, ok := s.platforms[platform]; ok {
		return rt.pool
	}
	return nil
}

// PlatformUpstream 返回某平台的上游客户端；平台未知返回 nil。
func (s *CnUpstreamService) PlatformUpstream(platform string) provider.Upstream {
	if rt, ok := s.platforms[platform]; ok {
		return rt.upstream
	}
	return nil
}

// ReloadAccounts 从 ent 账号库加载三渠道账号并同步进各自的池。
func (s *CnUpstreamService) ReloadAccounts(ctx context.Context) error {
	for platform, rt := range s.platforms {
		accounts, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			return fmt.Errorf("cnupstream: list %s accounts: %w", platform, err)
		}
		auths := make([]*auth.Auth, 0, len(accounts))
		for i := range accounts {
			a := hydrateAuth(&accounts[i], platform)
			auths = append(auths, a)
			s.acctRefs[a.UID] = &accounts[i]
		}
		rt.pool.SyncToDir(auths)
	}
	return nil
}

// hydrateAuth 从 ent Account.Credentials 喂出 auth.Auth；仅填充存在且类型正确的字段。
func hydrateAuth(acct *Account, platform string) *auth.Auth {
	a := &auth.Auth{
		Kind:         platform,
		UID:          strconv.FormatInt(acct.ID, 10),
		AccessToken:  credString(acct.Credentials, "accessToken"),
		RefreshToken: credString(acct.Credentials, "refreshToken"),
		Domain:       credString(acct.Credentials, "domain"),
		ApiHost:      credString(acct.Credentials, "apiHost"),
		MachineID:    credString(acct.Credentials, "machineId"),
		DeviceID:     credString(acct.Credentials, "deviceId"),
		MachineToken: credString(acct.Credentials, "machineToken"),
		MachineType:  credString(acct.Credentials, "machineType"),
		EnterpriseID: credString(acct.Credentials, "enterpriseId"),
		Nickname:     acct.Name,
	}
	if uid := credString(acct.Credentials, "uid"); uid != "" {
		a.UID = uid
	}
	if nickname := credString(acct.Credentials, "nickname"); nickname != "" {
		a.Nickname = nickname
	}
	if exp, ok := numberToInt64(acct.Credentials["expiresAt"]); ok {
		a.ExpiresAt = exp
	}
	return a
}

// credString 返回 credentials 中 string 类型字段（去空白），非 string 或缺失返回空串。
func credString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// numberToInt64 兼容 JSON 解析后的 float64 与 int64 表示的时间戳。
func numberToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// ChatStream 对指定平台按池内账号健康度挑号转发；失败账号按分类冷却/禁用并轮换，
// 成功后返回 SSE 流。无健康账号时返回错误。
func (s *CnUpstreamService) ChatStream(platform string, body []byte) (io.ReadCloser, int, []byte, error) {
	rt, ok := s.platforms[platform]
	if !ok {
		return nil, 0, nil, fmt.Errorf("cnupstream: unsupported platform %s", platform)
	}
	count := len(rt.pool.List())
	if count == 0 {
		return nil, 0, nil, fmt.Errorf("cnupstream: no %s accounts loaded", platform)
	}
	tried := map[string]bool{}
	for len(tried) < count {
		a := rt.pool.PickExcluding(tried)
		if a == nil {
			break
		}
		rc, status, respBody, err := rt.upstream.ChatStream(a, body)
		if err != nil {
			// 传输层失败：短冷却并尝试下一账号
			tried[a.UID] = true
			rt.pool.Cooldown(a.UID, pool.CoolErr, 30*time.Second, err.Error())
			continue
		}
		if status >= 400 {
			kind := rt.upstream.Classify(status, string(respBody))
			tried[a.UID] = true
			applyCnCooldown(rt.pool, a.UID, kind)
			continue
		}
		rt.pool.NoteSuccess(a.UID)
		return rc, status, respBody, nil
	}
	return nil, 0, nil, fmt.Errorf("cnupstream: all %s accounts unavailable", platform)
}

// applyCnCooldown 按错误分类对账号施以冷却/禁用；未命中明确类别时走错误计数阈值。
func applyCnCooldown(p *pool.Pool, uid string, kind provider.ErrKind) {
	switch kind {
	case provider.ErrSessionDead:
		p.Disable(uid, "session dead")
	case provider.ErrHardCredit:
		p.Cooldown(uid, pool.CoolHard, 30*time.Minute, "credit exhausted")
	case provider.ErrSoftRate:
		p.Cooldown(uid, pool.CoolSoft, 60*time.Second, "rate limited")
	case provider.ErrServer:
		p.Cooldown(uid, pool.CoolErr, 2*time.Minute, "server error")
	default:
		p.NoteError(uid, 3, 5*time.Minute)
	}
}

// RefreshToken 刷新指定平台账号 token；若仓储实现 Update 则回写 credentials。
func (s *CnUpstreamService) RefreshToken(platform, uid string) error {
	rt, ok := s.platforms[platform]
	if !ok {
		return fmt.Errorf("cnupstream: unsupported platform %s", platform)
	}
	a := rt.pool.AuthByUID(uid)
	if a == nil {
		return fmt.Errorf("cnupstream: unknown account %s on %s", uid, platform)
	}
	if err := rt.upstream.RefreshToken(a); err != nil {
		return err
	}
	upd, ok := s.accountRepo.(cnUpdater)
	if !ok {
		return nil
	}
	acct := s.acctRefs[uid]
	if acct == nil {
		// 未知引用时以 uid 重建，仅回写 credentials。
		id, _ := strconv.ParseInt(uid, 10, 64)
		acct = &Account{ID: id}
	}
	acct.Credentials = credsFromAuth(a)
	return upd.Update(context.Background(), acct)
}

// credsFromAuth 将 auth.Auth 序列化为 credentials map（刷新后回写用）。
func credsFromAuth(a *auth.Auth) map[string]any {
	return map[string]any{
		"accessToken":  a.AccessToken,
		"refreshToken": a.RefreshToken,
		"expiresAt":    a.ExpiresAt,
		"domain":       a.Domain,
		"apiHost":      a.ApiHost,
		"machineId":    a.MachineID,
		"deviceId":     a.DeviceID,
		"machineToken": a.MachineToken,
		"machineType":  a.MachineType,
		"uid":          a.UID,
		"enterpriseId": a.EnterpriseID,
		"nickname":     a.Nickname,
	}
}

// DailyCheckin 对指定平台池内所有未禁用账号执行每日签到并记录结果。
func (s *CnUpstreamService) DailyCheckin(platform string) error {
	rt, ok := s.platforms[platform]
	if !ok {
		return fmt.Errorf("cnupstream: unsupported platform %s", platform)
	}
	var firstErr error
	for _, st := range rt.pool.List() {
		if st.Disabled {
			continue
		}
		a := rt.pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if err := rt.upstream.DailyCheckin(a); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			rt.pool.RecordCheckin(st.UID, false, err.Error())
		} else {
			rt.pool.RecordCheckin(st.UID, true, "ok")
		}
	}
	return firstErr
}

// PoolStatus 返回指定平台的账号池状态快照；平台未知返回 nil。
func (s *CnUpstreamService) PoolStatus(platform string) []pool.Status {
	if rt, ok := s.platforms[platform]; ok {
		return rt.pool.List()
	}
	return nil
}
