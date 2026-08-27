package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/upstream"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// CnOAuthService 三渠道（workbuddy/traework/qoder）的一键 OAuth 授权建号服务。
//
// 平台流程（对齐原 workbuddy-wild 的真实协议，见 docs/oauth-account-add-design.md）：
//   - traework：浏览器回调流。授权页 www.trae.cn/authorization?auth_type=local，
//     回调 URL 携带一次性 state（路径），授权后回跳时带 refreshToken+host，
//     后端 RefreshToken 换 accessToken + GetUserInfo 建号。
//   - workbuddy：服务端轮询流。StartLogin 拿上游 state+授权 URL，浏览器登录后
//     后端轮询 PollLogin，成功即自动建号（无浏览器回调）。
//   - qoder：原项目无登录实现，Start 返回明确错误（仅支持粘贴 auth JSON 建号）。
type CnOAuthService struct {
	creator   AdminAccountCreator // 建号器，由 adminServiceImpl 提供
	traework  cnTraeWorkLoginClient
	workbuddy cnWorkBuddyLoginClient
	store     oauthStateStore // state 一次性占用存储（本地内存实现）
}

// AdminAccountCreator 仅暴露建号能力，避免 CnOAuthService 依赖完整 AdminService。
type AdminAccountCreator interface {
	CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error)
}

// cnTraeWorkLoginClient 与 traework.Client 方法签名一致的窄接口（测试可注入 stub）。
type cnTraeWorkLoginClient interface {
	RefreshToken(a *auth.Auth) error
	GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error)
}

// cnWorkBuddyLoginClient 与 upstream.Client 登录方法签名一致的窄接口（测试可注入 stub）。
type cnWorkBuddyLoginClient interface {
	StartLogin() (state, authURL string, err error)
	PollLogin(state string) (*upstream.LoginResult, error)
}

// oauthStateStore 是 OAuth state 的存储接口（本地内存实现，便于测试注入替身）。
type oauthStateStore interface {
	// Put 存入（或覆盖）一个一次性 state。
	Put(state string, entry oauthStateEntry)
	// Get 非占用读取（Status 用）；state 不存在返回 false。
	Get(state string) (oauthStateEntry, bool)
	// Consume 一次性占用读取（建号路径用）：
	// 仅当存在、未过期、未使用才标记为已用并返回 true；否则返回 false（同时 lazily 清理过期条目）。
	Consume(state string) (oauthStateEntry, bool)
}

// oauthStateEntry 一次 OAuth 授权流中的 state 上下文。
type oauthStateEntry struct {
	Platform string
	// MachineID/DeviceID 仅 traework：授权 URL 配对的设备指纹，回调建号时回填。
	MachineID string
	DeviceID  string
	// UpstreamState 仅 workbuddy：上游 auth/state 签发的登录 state。
	UpstreamState string
	ExpiresAt     time.Time
	Used          bool
}

// oauthStateStatus 状态机值，供 Status 返回。
const (
	oauthStateStatusPending  = "pending"
	oauthStateStatusExpired  = "expired"
	oauthStateStatusUsed     = "used"
	oauthStateStatusNotFound = "not_found"
)

// cnOAuthStateTTL OAuth state 有效期（workbuddy 上游 state 亦受此约束收敛）。
const cnOAuthStateTTL = 10 * time.Minute

// cnOAuthStateRetention 过期/已用条目在内存中的保留窗口：保留期内 Status 仍能观测到
// 「expired/used」状态，超过后会被 lazily 回收以防内存无限增长。
const cnOAuthStateRetention = 1 * time.Hour

var (
	// ErrCnOAuthStateNotFound state 不存在或已被清理。
	ErrCnOAuthStateNotFound = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_NOT_FOUND", "oauth state invalid or missing")
	// ErrCnOAuthStateExpired state 已过期。
	ErrCnOAuthStateExpired = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_EXPIRED", "oauth state has expired")
	// ErrCnOAuthStateUsed state 已被使用（防止重放）。
	ErrCnOAuthStateUsed = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_USED", "oauth state already used")
)

// NewCnOAuthService 构造 CnOAuthService。
func NewCnOAuthService(creator AdminAccountCreator, trae cnTraeWorkLoginClient, wb cnWorkBuddyLoginClient, store oauthStateStore) *CnOAuthService {
	return &CnOAuthService{
		creator:   creator,
		traework:  trae,
		workbuddy: wb,
		store:     store,
	}
}

// Start 校验平台并构造一次 OAuth 授权流，返回可跳转的授权 URL 与本地 state。
//
//   - traework：生成一次性 state + 设备指纹，构造 www.trae.cn/authorization 授权 URL
//     （auth_callback_url 含路径级 state，回调见 ExchangeAndCreate）。
//   - workbuddy：调上游 StartLogin 拿授权 URL，暂存「本地 state ↔ 上游 state」映射
//     （轮询建号见 Status）。
func (s *CnOAuthService) Start(ctx context.Context, platform, redirectBase string) (authorizeURL, state string, err error) {
	platform = strings.TrimSpace(strings.ToLower(platform))
	switch platform {
	case PlatformTraeWork, PlatformWorkBuddy:
	case PlatformQoder:
		return "", "", infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_UNSUPPORTED_PLATFORM",
			"platform %q 暂不支持 OAuth 授权建号（原项目无 qoder 登录实现，请使用粘贴 auth JSON）", platform)
	default:
		return "", "", infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_UNSUPPORTED_PLATFORM",
			"unknown OAuth platform %q（可选值: traework, workbuddy）", platform)
	}

	state = randomHex(32)
	switch platform {
	case PlatformTraeWork:
		if strings.TrimSpace(redirectBase) == "" {
			return "", "", infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_INVALID_REDIRECT", "redirect_base required")
		}
		machineID, deviceID := randomHex(16), randomHex(16)
		callbackURL := strings.TrimRight(redirectBase, "/") + "/oauth/callback/traework/" + state
		s.store.Put(state, oauthStateEntry{
			Platform:  platform,
			MachineID: machineID,
			DeviceID:  deviceID,
			ExpiresAt: time.Now().Add(cnOAuthStateTTL),
		})
		return traework.BuildAuthorizeURL(callbackURL, machineID, deviceID), state, nil
	default: // workbuddy
		upstreamState, authURL, err := s.workbuddy.StartLogin()
		if err != nil {
			return "", "", infraerrors.Newf(http.StatusBadGateway, "CN_OAUTH_START_FAILED",
				"workbuddy auth state failed: %v", err)
		}
		s.store.Put(state, oauthStateEntry{
			Platform:      platform,
			UpstreamState: upstreamState,
			ExpiresAt:     time.Now().Add(cnOAuthStateTTL),
		})
		return authURL, state, nil
	}
}

// ExchangeAndCreate traework 回调：校验并消费 state（防重放），用回调携带的
// refreshToken 换 accessToken（RefreshToken），再取用户信息建 type=oauth 账号。
func (s *CnOAuthService) ExchangeAndCreate(ctx context.Context, state, refreshToken, host string) (*Account, error) {
	state = strings.TrimSpace(state)
	refreshToken = strings.TrimSpace(refreshToken)
	if state == "" || refreshToken == "" {
		return nil, infraerrors.BadRequest("CN_OAUTH_MISSING_PARAMS", "state and refreshToken are required")
	}

	entry, ok := s.store.Get(state)
	if !ok {
		return nil, ErrCnOAuthStateNotFound
	}
	if entry.Platform != PlatformTraeWork {
		return nil, infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_STATE_PLATFORM_MISMATCH",
			"state platform %q does not support callback exchange", entry.Platform)
	}
	if time.Now().After(entry.ExpiresAt) {
		return nil, ErrCnOAuthStateExpired
	}
	if entry.Used {
		return nil, ErrCnOAuthStateUsed
	}

	// 一次性占用（并发竞态下 Consume 可能失败，视为重放拒绝）。
	consumed, ok := s.store.Consume(state)
	if !ok {
		return nil, ErrCnOAuthStateUsed
	}

	if host == "" {
		host = traework.OAuthHost
	}
	a := &auth.Auth{
		Kind:         "traework",
		RefreshToken: refreshToken,
		ApiHost:      host,
		MachineID:    consumed.MachineID,
		DeviceID:     consumed.DeviceID,
		Domain:       "trae.cn",
	}
	if err := s.traework.RefreshToken(a); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_OAUTH_EXCHANGE_FAILED",
			"traework refresh token failed: %v", err)
	}
	uid, nickname, enterpriseID, err := s.traework.GetUserInfo(a)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_OAUTH_USERINFO_FAILED",
			"traework get user info failed: %v", err)
	}
	a.UID, a.Nickname, a.EnterpriseID = uid, nickname, enterpriseID

	// 键名必须与 hydrateAuth（cnupstream_service.go）读取的驼峰字段一致，
	// 否则运行时凭证解析不到 token，三渠道账号无法使用。
	credentials := map[string]any{
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
		"nickname":     a.Nickname,
		"enterpriseId": a.EnterpriseID,
	}

	name := strings.TrimSpace(a.Nickname)
	if name == "" {
		name = platformAccountName(consumed.Platform)
	}
	account, err := s.creator.CreateAccount(ctx, &CreateAccountInput{
		Name:        name,
		Platform:    consumed.Platform,
		Type:        AccountTypeOAuth,
		Credentials: credentials,
		Concurrency: 1,
	})
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "CN_OAUTH_CREATE_ACCOUNT_FAILED",
			"create oauth account failed: %v", err)
	}
	return account, nil
}

// Status 返回一次授权流 state 的当前状态。
//
// workbuddy 的 pending 条目会在每次查询时轮询上游登录状态，登录完成即自动建号
// 并转为 used；上游未完成（ErrLoginPending）或出错（建号失败/网络错误）时保持
// pending 并记日志，由 TTL 收敛。
func (s *CnOAuthService) Status(ctx context.Context, state string) (status string, platform string) {
	entry, ok := s.store.Get(state)
	if !ok {
		return oauthStateStatusNotFound, ""
	}
	if time.Now().After(entry.ExpiresAt) {
		return oauthStateStatusExpired, entry.Platform
	}
	if entry.Used {
		return oauthStateStatusUsed, entry.Platform
	}
	if entry.Platform == PlatformWorkBuddy && s.workbuddy != nil {
		res, err := s.workbuddy.PollLogin(entry.UpstreamState)
		if err != nil {
			if err != upstream.ErrLoginPending {
				log.Printf("cn oauth workbuddy poll failed state=%s err=%v", state, err)
			}
			return oauthStateStatusPending, entry.Platform
		}
		// 登录完成：一次性占用后建号。占用失败说明并发轮询已建过号。
		consumed, ok := s.store.Consume(state)
		if !ok {
			return oauthStateStatusUsed, entry.Platform
		}
		if _, err := s.creator.CreateAccount(ctx, s.workBuddyCreateInput(consumed, res)); err != nil {
			log.Printf("cn oauth workbuddy create account failed state=%s err=%v", state, err)
			return oauthStateStatusPending, entry.Platform
		}
		return oauthStateStatusUsed, entry.Platform
	}
	return oauthStateStatusPending, entry.Platform
}

// workBuddyCreateInput 由登录结果组装建号入参（凭证键与 hydrateAuth 驼峰字段一致）。
func (s *CnOAuthService) workBuddyCreateInput(entry oauthStateEntry, res *upstream.LoginResult) *CreateAccountInput {
	expiresAt := int64(0)
	if res.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Unix()
	}
	credentials := map[string]any{
		"accessToken":  res.AccessToken,
		"refreshToken": res.RefreshToken,
		"expiresAt":    expiresAt,
		"domain":       res.Domain,
		"uid":          res.UID,
		"nickname":     res.Nickname,
		"enterpriseId": res.EnterpriseID,
	}
	name := strings.TrimSpace(res.Nickname)
	if name == "" {
		name = platformAccountName(entry.Platform)
	}
	return &CreateAccountInput{
		Name:        name,
		Platform:    entry.Platform,
		Type:        AccountTypeOAuth,
		Credentials: credentials,
		Concurrency: 1,
	}
}

// platformAccountName 生成默认账号名（无昵称时的兜底）。
func platformAccountName(platform string) string {
	return fmt.Sprintf("%s-oauth-%s", platform, time.Now().Format("20060102150405"))
}

// inMemoryOAuthStateStore 本地内存 state 存储（map + mutex + TTL lazily 清理）。
type inMemoryOAuthStateStore struct {
	mu    sync.Mutex
	items map[string]oauthStateEntry
}

// NewInMemoryOAuthStateStore 构造本地内存 state 存储。
func NewInMemoryOAuthStateStore() *inMemoryOAuthStateStore {
	return &inMemoryOAuthStateStore{items: map[string]oauthStateEntry{}}
}

func (s *inMemoryOAuthStateStore) Put(state string, entry oauthStateEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.items[state] = entry
}

func (s *inMemoryOAuthStateStore) Get(state string) (oauthStateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	entry, ok := s.items[state]
	return entry, ok
}

func (s *inMemoryOAuthStateStore) Consume(state string) (oauthStateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	entry, ok := s.items[state]
	if !ok {
		return oauthStateEntry{}, false
	}
	if time.Now().After(entry.ExpiresAt) {
		// 已过期：视为不可消费，并删除（调用方基于 Get 已抛出 ErrCnOAuthStateExpired）。
		delete(s.items, state)
		return oauthStateEntry{}, false
	}
	if entry.Used {
		return oauthStateEntry{}, false
	}
	entry.Used = true
	s.items[state] = entry
	return entry, true
}

// pruneLocked 回收「已超过保留窗口」的条目（过期/已用均会在 TTL+Retention 后被清理）。
// 调用方必须已持锁。保留窗口内的过期/已用条目故意保留，供 Status/重复请求观测状态。
func (s *inMemoryOAuthStateStore) pruneLocked() {
	now := time.Now()
	for k, v := range s.items {
		if now.After(v.ExpiresAt.Add(cnOAuthStateRetention)) {
			delete(s.items, k)
		}
	}
}
