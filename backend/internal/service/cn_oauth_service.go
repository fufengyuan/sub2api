package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// CnOAuthService 三渠道（workbuddy/traework/qoder）的一键 OAuth 授权建号服务。
// 目前仅 traework 支持真实流程（复用 traework 包的 PKCE/AuthCode 交换），
// workbuddy / qoder 暂不支持，Start 返回明确错误。
type CnOAuthService struct {
	creator   AdminAccountCreator // 建号器，由 adminServiceImpl 提供
	exchanger cnAuthCodeExchanger // AuthCode 换 token（真实实现为 *traework.Client，测试可注入 stub）
	store     oauthStateStore     // state 一次性占用存储（本地内存实现）
}

// AdminAccountCreator 仅暴露建号能力，避免 CnOAuthService 依赖完整 AdminService。
type AdminAccountCreator interface {
	CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error)
}

// cnAuthCodeExchanger 与 traework.Client.ExchangeAuthCode 签名一致的换 token 窄接口。
type cnAuthCodeExchanger interface {
	ExchangeAuthCode(a *auth.Auth, authCode, codeVerifier string) (*traework.AuthCodeResult, error)
}

// oauthStateStore 是 OAuth state 的存储接口（本地内存实现，便于测试注入替身）。
type oauthStateStore interface {
	// Put 存入（或覆盖）一个一次性 state。
	Put(state string, entry oauthStateEntry)
	// Get 非占用读取（Status 用）；state 不存在返回 false。
	Get(state string) (oauthStateEntry, bool)
	// Consume 一次性占用读取（ExchangeAndCreate 用）：
	// 仅当存在、未过期、未使用才标记为已用并返回 true；否则返回 false（同时 lazily 清理过期条目）。
	Consume(state string) (oauthStateEntry, bool)
}

// oauthStateEntry 一次 OAuth 授权流中的 state 上下文。
type oauthStateEntry struct {
	Platform     string
	CodeVerifier string
	RedirectURI  string
	ExpiresAt    time.Time
	Used         bool
}

// oauthStateStatus 状态机值，供 Status 返回。
const (
	oauthStateStatusPending  = "pending"
	oauthStateStatusExpired  = "expired"
	oauthStateStatusUsed     = "used"
	oauthStateStatusNotFound = "not_found"
)

// cnOAuthStateTTL OAuth state 有效期。
const cnOAuthStateTTL = 10 * time.Minute

// cnOAuthStateRetention 过期/已用条目在内存中的保留窗口：保留期内 Status/ExchangeAndCreate
// 仍能观测到「expired/used」状态，超过后会被 lazily 回收以防内存无限增长。
const cnOAuthStateRetention = 1 * time.Hour

// cnOAuthAuthorizePath TraeWork 授权端点相对路径。
// TODO: 真实授权 URL 需联调校准（当前基于 ConsoleHost 的合理默认，未与真实上游核对）。
const cnOAuthAuthorizePath = "/oauth/authorize"

var (
	// ErrCnOAuthStateNotFound state 不存在或已被清理。
	ErrCnOAuthStateNotFound = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_NOT_FOUND", "oauth state invalid or missing")
	// ErrCnOAuthStateExpired state 已过期。
	ErrCnOAuthStateExpired = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_EXPIRED", "oauth state has expired")
	// ErrCnOAuthStateUsed state 已被使用（防止重放）。
	ErrCnOAuthStateUsed = infraerrors.New(http.StatusBadRequest, "CN_OAUTH_STATE_USED", "oauth state already used")
)

// NewCnOAuthService 构造 CnOAuthService。
func NewCnOAuthService(creator AdminAccountCreator, exchanger cnAuthCodeExchanger, store oauthStateStore) *CnOAuthService {
	return &CnOAuthService{
		creator:   creator,
		exchanger: exchanger,
		store:     store,
	}
}

// Start 校验平台并构造一次 OAuth 授权流：生成一次性 state + PKCE 并暂存，
// 返回可跳转的授权 URL 与 state。
func (s *CnOAuthService) Start(ctx context.Context, platform, redirectBase string) (authorizeURL, state string, err error) {
	platform = strings.TrimSpace(strings.ToLower(platform))
	switch platform {
	case PlatformTraeWork:
	case PlatformWorkBuddy, PlatformQoder:
		return "", "", infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_UNSUPPORTED_PLATFORM",
			"platform %q 暂不支持 OAuth 授权建号（当前仅支持 traework）", platform)
	default:
		return "", "", infraerrors.Newf(http.StatusBadRequest, "CN_OAUTH_UNSUPPORTED_PLATFORM",
			"unknown OAuth platform %q（可选值: traework）", platform)
	}
	if strings.TrimSpace(redirectBase) == "" {
		return "", "", infraerrors.New(http.StatusBadRequest, "CN_OAUTH_INVALID_REDIRECT", "redirect_base required")
	}

	state = randomHex(32)
	verifier, challenge := traework.GenPKCE()
	redirectURI := strings.TrimRight(redirectBase, "/") + "/oauth/callback/" + platform

	s.store.Put(state, oauthStateEntry{
		Platform:     platform,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		ExpiresAt:    time.Now().Add(cnOAuthStateTTL),
	})

	u, err := url.Parse(traework.ConsoleHost + cnOAuthAuthorizePath)
	if err != nil {
		return "", "", infraerrors.Newf(http.StatusInternalServerError, "CN_OAUTH_URL_BUILD_FAILED", "build authorize url: %v", err)
	}
	q := u.Query()
	q.Set("client_id", traework.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "read")
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), state, nil
}

// ExchangeAndCreate 校验并消费 state（防重放），换 token 后调用建号器创建 type=oauth 账号。
func (s *CnOAuthService) ExchangeAndCreate(ctx context.Context, state, code string) (*Account, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" || code == "" {
		return nil, infraerrors.BadRequest("CN_OAUTH_MISSING_PARAMS", "code and state are required")
	}

	entry, ok := s.store.Get(state)
	if !ok {
		return nil, ErrCnOAuthStateNotFound
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

	a := &auth.Auth{
		Kind:         "traework",
		MachineID:    randomHex(16),
		DeviceID:     randomHex(16),
		MachineToken: "",
		MachineType:  "",
	}
	result, err := s.exchanger.ExchangeAuthCode(a, code, consumed.CodeVerifier)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "CN_OAUTH_EXCHANGE_FAILED",
			"traework auth code exchange failed: %v", err)
	}

	a.AccessToken = result.AccessToken
	a.RefreshToken = result.RefreshToken
	a.ExpiresAt = result.ExpiresAt
	if result.Host != "" {
		a.ApiHost = result.Host
	}

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
	return oauthStateStatusPending, entry.Platform
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
		// 已过期：视为不可消费，并删除（ExchangeAndCreate 基于 Get 已抛出 ErrCnOAuthStateExpired）。
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
