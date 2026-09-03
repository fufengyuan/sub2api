package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/pool"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/qoder"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/qwenwork"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/scheduler"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/upstream"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

// cnAccountRepo 是 CnUpstreamService 对账号仓储的最小依赖切片：只消费账号列表，
// 其余能力（如 Update 持久化刷新后的凭证）通过可选的接口断言注入。
type cnAccountRepo interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
}

// cnFilterAccountRepo 可选能力：按列表筛选条件返回全部国产渠道账号（不分页），
// 供「按查询条件汇总积分 / 一键刷新积分」使用。AccountRepository 实现该接口。
type cnFilterAccountRepo interface {
	ListAllWithFilters(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error)
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

// CnUpstreamService 多渠道上游服务：加载国产渠道（workbuddy/traework/qoder/qwenwork）
// 账号组池并提供 ChatStream / 签到 / 刷新等能力。
type CnUpstreamService struct {
	accountRepo cnAccountRepo
	cfg         *config.Config

	platforms map[string]*cnUpstreamPlatform
	// acctRefs uid -> *Account，RefreshToken 后回写凭证用（保留其余字段避免覆盖丢失）。
	// 调度器与管理端 HTTP 并发访问，mu 保护 map 读写。
	mu       sync.Mutex
	acctRefs map[string]*Account

	// creditRateCache 缓存上游模型积分倍率（按平台+账号），供积分折算计费读取。
	// 自带锁，独立于 mu 以避免与账号管理操作互相阻塞。
	creditRateCache *cnCreditRateCache
}

// NewCnUpstreamService 构建三个平台各自的 Pool + Upstream。
func NewCnUpstreamService(accountRepo cnAccountRepo, cfg *config.Config) *CnUpstreamService {
	svc := &CnUpstreamService{
		accountRepo:     accountRepo,
		cfg:             cfg,
		platforms:       make(map[string]*cnUpstreamPlatform),
		acctRefs:        make(map[string]*Account),
		creditRateCache: newCnCreditRateCache(),
	}
	svc.platforms[PlatformWorkBuddy] = &cnUpstreamPlatform{pool: pool.New(""), upstream: upstream.New()}
	svc.platforms[PlatformTraeWork] = &cnUpstreamPlatform{pool: pool.New(""), upstream: traework.New()}
	svc.platforms[PlatformQoder] = &cnUpstreamPlatform{pool: pool.New(""), upstream: qoder.New()}
	svc.platforms[PlatformQwenWork] = &cnUpstreamPlatform{pool: pool.New(""), upstream: qwenwork.New()}
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

// ReloadAccounts 从 ent 账号库加载国产渠道账号并同步进各自的池。
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
			s.mu.Lock()
			s.acctRefs[a.UID] = &accounts[i]
			s.mu.Unlock()
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
		AccountID:    acct.ID,
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

// CnChatTarget 一次 CN 上游 Chat 调用的结果：实际使用的池账号 + 上游响应 +
// 转换结束后的结果回灌回调。
type CnChatTarget struct {
	Platform  string
	UID       string
	AccountID int64
	RC        io.ReadCloser
	Status    int
	Body      []byte

	// report 把流转换/聚合阶段的结果回灌池状态机。SSE 流内业务错误（4008/4028
	// 配额超限、1005 权益不足）发生在 HTTP 200 之后，旧实现只在 status>=400 时
	// 冷却，导致被限流账号下一轮仍按「池内积分最高」被选中，用户侧同一个错误
	// 反复出现。
	report func(err error)
}

// Report 在流转换/聚合结束后回灌结果：err 为 nil 时不做处理（成功已在取号时
// NoteSuccess），否则按错误分类对实际使用的账号施加冷却/禁用。
func (t *CnChatTarget) Report(err error) {
	if t == nil || t.report == nil {
		return
	}
	t.report(err)
}

// CnPoolExhaustedError 表示该平台池内账号全部处于冷却/禁用状态。
// RetryAfter 取池内最早的冷却恢复时间，供网关向客户端下发 Retry-After。
type CnPoolExhaustedError struct {
	Platform   string
	RetryAfter time.Duration
	Accounts   int
}

func (e *CnPoolExhaustedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("cnupstream: all %s accounts cooling (retry after %s)", e.Platform, e.RetryAfter.Truncate(time.Second))
	}
	return fmt.Sprintf("cnupstream: all %s accounts unavailable", e.Platform)
}

// ChatStream 转发一次 Chat 请求：优先使用统一池选中的账号（accountID，<=0 时
// 按池策略挑积分最高的健康账号），失败账号按分类冷却/禁用并轮换。
// 返回的 *CnChatTarget 必须在流转换结束后调用 Report，以便把 SSE 流内业务错误
// 回灌到池冷却状态机。
func (s *CnUpstreamService) ChatStream(platform string, accountID int64, body []byte) (*CnChatTarget, error) {
	rt, ok := s.platforms[platform]
	if !ok {
		return nil, fmt.Errorf("cnupstream: unsupported platform %s", platform)
	}
	count := len(rt.pool.List())
	if count == 0 {
		return nil, fmt.Errorf("cnupstream: no %s accounts loaded", platform)
	}
	tried := map[string]bool{}
	for len(tried) < count {
		a := rt.pool.PickForAccount(accountID, tried)
		if a == nil {
			log.Printf("cnupstream chat_stream no schedulable account platform=%s tried=%d/%d",
				platform, len(tried), count)
			break
		}
		rc, status, respBody, err := rt.upstream.ChatStream(a, body)
		if err != nil {
			// 传输层失败：短冷却并尝试下一账号
			tried[a.UID] = true
			rt.pool.Cooldown(a.UID, pool.CoolErr, 30*time.Second, err.Error())
			log.Printf("cnupstream chat_stream transport error platform=%s uid=%s model=%s err=%s",
				platform, a.UID, gjson.GetBytes(body, "model").String(), err.Error())
			continue
		}
		if status >= 400 {
			kind := rt.upstream.Classify(status, string(respBody))
			tried[a.UID] = true
			applyCnCooldown(rt.pool, a.UID, kind)
			bodySnippet := strings.TrimSpace(string(respBody))
			if len(bodySnippet) > 200 {
				bodySnippet = bodySnippet[:200]
			}
			log.Printf("cnupstream chat_stream upstream http error platform=%s uid=%s status=%d kind=%s body=%s",
				platform, a.UID, status, kind, bodySnippet)
			continue
		}
		rt.pool.NoteSuccess(a.UID)
		uid := a.UID
		return &CnChatTarget{
			Platform:  platform,
			UID:       uid,
			AccountID: a.AccountID,
			RC:        rc,
			Status:    status,
			Body:      respBody,
			report:    func(streamErr error) { s.reportStreamOutcome(platform, uid, streamErr) },
		}, nil
	}
	retryAfter, _ := rt.pool.EarliestRecovery()
	log.Printf("cnupstream chat_stream pool exhausted platform=%s tried=%d/%d retry_after=%s",
		platform, len(tried), count, retryAfter)
	return nil, &CnPoolExhaustedError{Platform: platform, RetryAfter: retryAfter, Accounts: count}
}

// reportStreamOutcome 把流转换阶段的错误回灌池冷却：命中 SOLO 业务错误时按码表
// 分类（软限流短冷却 / 硬权益长冷却 / 登录态失效禁用），其余错误按传输层失败短冷却。
func (s *CnUpstreamService) reportStreamOutcome(platform, uid string, streamErr error) {
	rt, ok := s.platforms[platform]
	if !ok || uid == "" || streamErr == nil {
		return
	}
	var soloErr *traework.SOLOStreamError
	if errors.As(streamErr, &soloErr) {
		applyCnCooldown(rt.pool, uid, soloErr.Kind())
		log.Printf("cnupstream stream error cooling account platform=%s uid=%s kind=%s err=%s",
			platform, uid, soloErr.Kind(), soloErr.Error())
		return
	}
	rt.pool.Cooldown(uid, pool.CoolErr, 30*time.Second, streamErr.Error())
}

// applyCnCooldown 按错误分类对账号施以冷却/禁用；未命中明确类别时走错误计数阈值。
func applyCnCooldown(p *pool.Pool, uid string, kind provider.ErrKind) {
	switch kind {
	case provider.ErrSessionDead:
		p.Disable(uid, "session dead")
		log.Printf("cnupstream cooldown uid=%s kind=session_dead action=disable", uid)
	case provider.ErrHardCredit:
		p.Cooldown(uid, pool.CoolHard, 30*time.Minute, "credit exhausted")
		log.Printf("cnupstream cooldown uid=%s kind=hard_credit action=cooldown 30m", uid)
	case provider.ErrSoftRate:
		p.Cooldown(uid, pool.CoolSoft, 60*time.Second, "rate limited")
		log.Printf("cnupstream cooldown uid=%s kind=soft_rate action=cooldown 60s", uid)
	case provider.ErrServer:
		p.Cooldown(uid, pool.CoolErr, 2*time.Minute, "server error")
		log.Printf("cnupstream cooldown uid=%s kind=server action=cooldown 2m", uid)
	case provider.ErrRequest:
		// 请求本身有问题（上下文超限/参数非法）：账号是健康的，换号必然复现。
		// 记错会让池内账号逐个累积 errCount，最终把整个渠道拖进冷却——这正是
		// 「一个超长请求打挂整个平台」的成因，因此这里必须完全不动账号状态。
		log.Printf("cnupstream cooldown uid=%s kind=request action=none (request-scoped, account healthy)", uid)
	default:
		p.NoteError(uid, 3, 5*time.Minute)
		log.Printf("cnupstream cooldown uid=%s kind=default action=note_error 3x5m", uid)
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
	return s.PersistAuth(a)
}

// accountByUID 取 uid 对应的账号引用；无引用时按 uid 解析账号 ID 重建（仅 ID）。
func (s *CnUpstreamService) accountByUID(uid string) *Account {
	s.mu.Lock()
	acct := s.acctRefs[uid]
	s.mu.Unlock()
	if acct != nil {
		return acct
	}
	id, _ := strconv.ParseInt(uid, 10, 64)
	return &Account{ID: id}
}

// PersistAuth 把刷新后的 auth 凭证按 uid 回写 ent credentials。
// 在既有 credentials 上合并鉴权字段（COW），保留 model_mapping/creditsRemain
// 等业务键——仓储 Update 是整行覆盖，整包替换会抹掉它们。
// 仓储未实现 Update 时静默跳过（无持久化能力不阻断刷新流程）。
func (s *CnUpstreamService) PersistAuth(a *auth.Auth) error {
	upd, ok := s.accountRepo.(cnUpdater)
	if !ok {
		return nil
	}
	acct := s.accountByUID(a.UID)
	// COW：副本写，避免与并发读共享引用竞态。
	creds := copyCredentials(acct.Credentials)
	for k, v := range credsFromAuth(a) {
		creds[k] = v
	}
	cp := *acct
	cp.Credentials = creds
	return upd.Update(context.Background(), &cp)
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

// cnAccountGetter 单账号查询切片（AccountRepository 已实现）。
type cnAccountGetter interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// CnCreditsDetail 积分明细响应（总余额 + 各套餐条目）。
type CnCreditsDetail struct {
	CreditsRemain int64                   `json:"credits_remain"`
	Items         []provider.ResourceItem `json:"items"`
}

// CnCheckinResult 手动签到结果（业务失败如「已签到」也算成功响应，前端按 message 提示）。
type CnCheckinResult struct {
	Success       bool   `json:"success"`
	Message       string `json:"message"`
	CreditsRemain int64  `json:"credits_remain"`
}

// loadCnAccount 按账号 ID 取国产渠道账号并构造运行期 auth；非国产平台报错。
func (s *CnUpstreamService) loadCnAccount(ctx context.Context, accountID int64) (*cnUpstreamPlatform, *Account, *auth.Auth, error) {
	getter, ok := s.accountRepo.(cnAccountGetter)
	if !ok {
		return nil, nil, nil, infraerrors.Newf(http.StatusInternalServerError, "CN_UPSTREAM_REPO_UNSUPPORTED",
			"account repo does not support GetByID")
	}
	acct, err := getter.GetByID(ctx, accountID)
	if err != nil {
		return nil, nil, nil, err
	}
	if acct == nil {
		return nil, nil, nil, infraerrors.Newf(http.StatusNotFound, "CN_ACCOUNT_NOT_FOUND",
			"账号 %d 不存在", accountID)
	}
	rt, ok := s.platforms[acct.Platform]
	if !ok {
		return nil, nil, nil, infraerrors.Newf(http.StatusBadRequest, "CN_ACCOUNT_UNSUPPORTED_PLATFORM",
			"平台 %q 不支持积分/签到（仅 workbuddy/traework/qoder）", acct.Platform)
	}
	a := hydrateAuth(acct, acct.Platform)
	s.mu.Lock()
	s.acctRefs[a.UID] = acct
	s.mu.Unlock()
	return rt, acct, a, nil
}

// persistCreditsRemain 把余额写入账号 credentials（驼峰键 creditsRemain）并回写仓储。
// COW 副本写，避免直接改共享引用与并发读竞态。
func (s *CnUpstreamService) persistCreditsRemain(ctx context.Context, acct *Account, uid string, remain int64, p *pool.Pool) {
	creds := copyCredentials(acct.Credentials)
	creds["creditsRemain"] = remain
	cp := *acct
	cp.Credentials = creds
	if upd, ok := s.accountRepo.(cnUpdater); ok {
		if err := upd.Update(ctx, &cp); err != nil {
			log.Printf("cnupstream: persist credits for account %d failed: %v", cp.ID, err)
		}
	}
	if p != nil {
		p.SetCredits(uid, remain)
		p.ReenableIfCredits(uid, remain)
	}
}

// copyCredentials 浅拷贝 credentials map（COW 基础）。
func copyCredentials(src map[string]any) map[string]any {
	out := make(map[string]any, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

// PersistCheckinResult 把签到结果落库：credentials 写 lastCheckinAt（RFC3339）、
// lastCheckinResult（结果消息）与 lastCheckinOK（是否成功，含「已签到」视为成功），
// 携带余额时同步 creditsRemain，并刷新内存池（SetCredits + 按余额解冻）。
// 自动签到观察者与手动签到共用此口径。lastCheckinOK 供前端稳定判定成败，
// 避免仅靠 lastCheckinResult 文本猜测（如「已签到」误判失败）。
func (s *CnUpstreamService) PersistCheckinResult(platform string, r scheduler.CheckinResult) {
	rt, ok := s.platforms[platform]
	if !ok {
		return
	}
	acct := s.accountByUID(r.UID)
	creds := copyCredentials(acct.Credentials)
	creds["lastCheckinAt"] = time.Now().Format(time.RFC3339)
	creds["lastCheckinResult"] = r.Msg
	creds["lastCheckinOK"] = r.OK
	if r.HasRemain {
		creds["creditsRemain"] = r.Remain
	}
	cp := *acct
	cp.Credentials = creds
	if upd, ok := s.accountRepo.(cnUpdater); ok {
		if err := upd.Update(context.Background(), &cp); err != nil {
			log.Printf("cnupstream: persist checkin result for account %d failed: %v", cp.ID, err)
		}
	}
	if r.HasRemain {
		rt.pool.SetCredits(r.UID, r.Remain)
		rt.pool.ReenableIfCredits(r.UID, r.Remain)
	}
}

// RefreshCredits 刷新单账号积分余额：调上游 UserResource，写入账号
// credentials["creditsRemain"] 并同步池状态，返回余额。
func (s *CnUpstreamService) RefreshCredits(ctx context.Context, accountID int64) (int64, error) {
	rt, acct, a, err := s.loadCnAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	remain, err := rt.upstream.UserResource(a)
	if err != nil {
		return 0, infraerrors.Newf(http.StatusBadGateway, "CN_UPSTREAM_QUERY_FAILED", "查询积分失败: %v", err)
	}
	s.persistCreditsRemain(ctx, acct, a.UID, remain, rt.pool)
	return remain, nil
}

// cnCreditsRemainOf 取账号 credentials.creditsRemain 的数值；非 number 或缺失返回 (0, false)。
func cnCreditsRemainOf(acct *Account) (int64, bool) {
	if acct == nil || acct.Credentials == nil {
		return 0, false
	}
	switch n := acct.Credentials["creditsRemain"].(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// filterAccountRepo 返回具备 ListAllWithFilters 能力的仓储；不可用返回 nil。
func (s *CnUpstreamService) filterAccountRepo() cnFilterAccountRepo {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	fr, _ := s.accountRepo.(cnFilterAccountRepo)
	return fr
}

// CnCreditsFilter 一次国产渠道积分「按筛选条件」操作所需的查询条件（与账号列表筛选对齐）。
type CnCreditsFilter struct {
	Platform    string
	AccountType string
	Status      string
	Search      string
	GroupID     int64
	PrivacyMode string
}

// listCnAccountsByFilter 枚举满足筛选条件的国产渠道账号；仓储不支持筛选能力时
// 回退到按平台列举（此时忽略 platform 之外的其余筛选条件）。
func (s *CnUpstreamService) listCnAccountsByFilter(ctx context.Context, f CnCreditsFilter) ([]Account, error) {
	if fr := s.filterAccountRepo(); fr != nil {
		accounts, err := fr.ListAllWithFilters(ctx, f.Platform, f.AccountType, f.Status, f.Search, f.GroupID, f.PrivacyMode)
		if err != nil {
			return nil, err
		}
		return accounts, nil
	}
	// 兜底：无筛选能力时按平台列举（保持既有行为，仅能覆盖单平台维度）。
	platform := f.Platform
	if platform == "" {
		platform = PlatformWorkBuddy
	}
	return s.accountRepo.ListByPlatform(ctx, platform)
}

// isCnUpstreamPlatformAccount 判断账号是否属于国产多渠道（workbuddy/traework/qoder/qwenwork）。
func isCnUpstreamPlatformAccount(acct *Account) bool {
	if acct == nil {
		return false
	}
	return IsCnUpstreamPlatform(acct.Platform)
}

// SummarizeCreditsByFilter 按列表筛选条件对满足条件的全部国产渠道账号的
// credentials.creditsRemain 求和（全量口径，跨页），供积分列下方汇总展示。
func (s *CnUpstreamService) SummarizeCreditsByFilter(ctx context.Context, f CnCreditsFilter) (total, counted int64, err error) {
	accounts, err := s.listCnAccountsByFilter(ctx, f)
	if err != nil {
		return 0, 0, err
	}
	for i := range accounts {
		if !isCnUpstreamPlatformAccount(&accounts[i]) {
			continue
		}
		remain, ok := cnCreditsRemainOf(&accounts[i])
		if !ok {
			continue
		}
		total += remain
		counted++
	}
	return total, counted, nil
}

// RefreshCreditsByFilter 按列表筛选条件对满足条件的全部国产渠道账号逐个刷新积分，
// 返回成功/失败账号数。单账号刷新失败不中断整体流程。
func (s *CnUpstreamService) RefreshCreditsByFilter(ctx context.Context, f CnCreditsFilter) (success, failed int, err error) {
	accounts, err := s.listCnAccountsByFilter(ctx, f)
	if err != nil {
		return 0, 0, err
	}
	for i := range accounts {
		if ctx.Err() != nil {
			break
		}
		acct := &accounts[i]
		if !isCnUpstreamPlatformAccount(acct) {
			continue
		}
		if _, rerr := s.RefreshCredits(ctx, acct.ID); rerr != nil {
			failed++
			log.Printf("cnupstream: refresh credits by filter failed account=%d platform=%s err=%v",
				acct.ID, acct.Platform, rerr)
			continue
		}
		success++
	}
	return success, failed, nil
}

// ResourceDetail 查询单账号积分明细（实时查上游，不落库）。
func (s *CnUpstreamService) ResourceDetail(ctx context.Context, accountID int64) (*CnCreditsDetail, error) {
	rt, _, a, err := s.loadCnAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	remain, items, err := rt.upstream.UserResourceDetail(a)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_UPSTREAM_QUERY_FAILED", "查询积分明细失败: %v", err)
	}
	return &CnCreditsDetail{CreditsRemain: remain, Items: items}, nil
}

// CheckinNow 单账号立即签到（语义对齐原 workbuddy-wild scheduler.checkinOne）：
//   - 签到成功 → Success=true, Message="ok"，刷新余额；
//   - 上游返回「今日已签到」→ 视为成功，Message="已签到"，同样刷新余额；
//   - 其余业务失败（如 token 无效、无签到活动）→ Success=false + Message，不返回 error。
//
// 结果统一经 PersistCheckinResult 落库（lastCheckin 状态 + lastCheckinOK + 余额），
// 与自动签到同口径。
func (s *CnUpstreamService) CheckinNow(ctx context.Context, accountID int64) (*CnCheckinResult, error) {
	rt, acct, a, err := s.loadCnAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	msg := "ok"
	err = rt.upstream.DailyCheckin(a)
	if err != nil && !scheduler.IsAlready(err) {
		short := err.Error()
		rt.pool.RecordCheckin(a.UID, false, short)
		s.PersistCheckinResult(acct.Platform, scheduler.CheckinResult{UID: a.UID, OK: false, Msg: short})
		return &CnCheckinResult{Success: false, Message: short}, nil
	}
	if err != nil {
		// 已签到：视为成功状态（对齐 checkinOne）
		msg = "已签到"
	}
	rt.pool.RecordCheckin(a.UID, true, msg)
	// 签到成功（含已签到）后刷新余额，立即同步池与列表展示。
	remain := int64(0)
	hasRemain := false
	if r, rerr := rt.upstream.UserResource(a); rerr == nil {
		remain, hasRemain = r, true
	}
	s.PersistCheckinResult(acct.Platform, scheduler.CheckinResult{UID: a.UID, OK: true, Msg: msg, Remain: remain, HasRemain: hasRemain})
	return &CnCheckinResult{Success: true, Message: msg, CreditsRemain: remain}, nil
}

// FetchUpstreamModels 拉取国产渠道账号的上游真实模型列表（供模型映射配置辅助选择）。
func (s *CnUpstreamService) FetchUpstreamModels(ctx context.Context, accountID int64) ([]provider.ModelInfo, error) {
	rt, _, a, err := s.loadCnAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// 动态模型映射按账号存在 auth.Auth 上，必须写进**池内那个长期实例**：
	// 网关聊天链路取的是池内 Auth，若把映射写进 loadCnAccount 新建的临时
	// Auth，管理员拉完模型聊天时仍然解析不到 key。池内暂无该账号（新增后
	// 尚未 Reload）时退回临时 Auth，由 ChatStream 的按账号懒刷新兜住。
	if pooled := rt.pool.AuthByAccountID(accountID); pooled != nil {
		a = pooled
	}
	models, err := rt.upstream.FetchModels(a)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_UPSTREAM_QUERY_FAILED", "拉取上游模型列表失败: %v", err)
	}
	return models, nil
}
