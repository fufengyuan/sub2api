package service

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/upstream"
)

// fakeCnAccountCreator 冒充 AdminAccountCreator 的测试替身。
type fakeCnAccountCreator struct {
	created []*CreateAccountInput
	acct    *Account
	err     error
}

func (f *fakeCnAccountCreator) CreateAccount(_ context.Context, input *CreateAccountInput) (*Account, error) {
	f.created = append(f.created, input)
	if f.err != nil {
		return nil, f.err
	}
	if f.acct != nil {
		a := *f.acct
		a.Name, a.Platform, a.Type, a.Credentials = input.Name, input.Platform, input.Type, input.Credentials
		return &a, nil
	}
	return &Account{ID: 1, Name: input.Name, Platform: input.Platform, Type: input.Type, Credentials: input.Credentials}, nil
}

// fakeCnTraeWorkLoginClient 冒充 traework 登录窄接口的测试替身。
type fakeCnTraeWorkLoginClient struct {
	refreshErr    error
	userInfoErr   error
	gotAuth       *auth.Auth
	uid           string
	nickname      string
	enterpriseID  string
	rotatedToken  string
	rotatedExpiry int64
}

func (f *fakeCnTraeWorkLoginClient) RefreshToken(a *auth.Auth) error {
	f.gotAuth = a
	if f.refreshErr != nil {
		return f.refreshErr
	}
	a.AccessToken = "at"
	if f.rotatedToken != "" {
		a.RefreshToken = f.rotatedToken
	}
	if f.rotatedExpiry > 0 {
		a.ExpiresAt = f.rotatedExpiry
	}
	return nil
}

func (f *fakeCnTraeWorkLoginClient) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	if f.userInfoErr != nil {
		return "", "", "", f.userInfoErr
	}
	return f.uid, f.nickname, f.enterpriseID, nil
}

// fakeCnWorkBuddyLoginClient 冒充 workbuddy 登录窄接口的测试替身。
type fakeCnWorkBuddyLoginClient struct {
	startErr    error
	pollResult  *upstream.LoginResult
	pollErr     error
	pollCount   int
	gotUpstream string
}

func (f *fakeCnWorkBuddyLoginClient) StartLogin() (state, authURL string, err error) {
	if f.startErr != nil {
		return "", "", f.startErr
	}
	return "upstream-state", "https://www.codebuddy.cn/login?foo=bar", nil
}

func (f *fakeCnWorkBuddyLoginClient) PollLogin(state string) (*upstream.LoginResult, error) {
	f.pollCount++
	f.gotUpstream = state
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	if f.pollResult != nil {
		return f.pollResult, nil
	}
	return nil, upstream.ErrLoginPending
}

func newTestCnOAuthSvc(trae cnTraeWorkLoginClient, wb cnWorkBuddyLoginClient) (*CnOAuthService, *fakeCnAccountCreator) {
	creator := &fakeCnAccountCreator{}
	svc := NewCnOAuthService(creator, trae, wb, NewInMemoryOAuthStateStore())
	return svc, creator
}

func TestCnOAuthStart_TraeWorkBuildsAuthorizationURL(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	authorizeURL, state, err := svc.Start(context.Background(), "traework", "http://localhost:3000")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if state == "" {
		t.Fatal("expected non-empty state")
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("authorizeURL is not a valid URL: %v", err)
	}
	if parsed.Scheme+"://"+parsed.Host != traework.ConsoleHost {
		t.Fatalf("authorize host = %q, want %q", parsed.Host, traework.ConsoleHost)
	}
	if parsed.Path != "/authorization" {
		t.Fatalf("authorize path = %q, want /authorization", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("client_id") != traework.ClientID {
		t.Fatalf("client_id = %q, want %q", q.Get("client_id"), traework.ClientID)
	}
	if q.Get("auth_type") != "local" {
		t.Fatalf("auth_type = %q, want local", q.Get("auth_type"))
	}
	if q.Get("auth_from") != "solo" {
		t.Fatalf("auth_from = %q, want solo", q.Get("auth_from"))
	}
	wantCallback := "http://localhost:3000/oauth/callback/traework/" + state
	if q.Get("auth_callback_url") != wantCallback {
		t.Fatalf("auth_callback_url = %q, want %q", q.Get("auth_callback_url"), wantCallback)
	}
	if q.Get("machine_id") == "" || q.Get("device_id") == "" {
		t.Fatal("expected non-empty machine_id/device_id")
	}
	if q.Get("code_challenge") != "" || q.Get("response_type") != "" {
		t.Fatal("traework flow must not use authorization-code/PKCE params")
	}
}

func TestCnOAuthStart_WorkBuddyReturnsUpstreamAuthURL(t *testing.T) {
	wb := &fakeCnWorkBuddyLoginClient{}
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, wb)
	authorizeURL, state, err := svc.Start(context.Background(), "workbuddy", "http://localhost:3000")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if authorizeURL != "https://www.codebuddy.cn/login?foo=bar" {
		t.Fatalf("authorizeURL = %q", authorizeURL)
	}
	if state == "" {
		t.Fatal("expected non-empty state")
	}
	// 后续轮询应使用上游签发的 state。
	if status, _ := svc.Status(context.Background(), state); status != oauthStateStatusPending {
		t.Fatalf("expected pending status, got %q", status)
	}
	if wb.gotUpstream != "upstream-state" {
		t.Fatalf("PollLogin got upstream state = %q, want upstream-state", wb.gotUpstream)
	}
}

func TestCnOAuthStart_WorkBuddyStartLoginFailed(t *testing.T) {
	wb := &fakeCnWorkBuddyLoginClient{startErr: errors.New("upstream down")}
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, wb)
	if _, _, err := svc.Start(context.Background(), "workbuddy", "http://localhost:3000"); err == nil {
		t.Fatal("Start should fail when upstream StartLogin fails")
	}
}

func TestCnOAuthStart_UnsupportedPlatform(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	for _, p := range []string{"qoder", "unknown"} {
		if _, _, err := svc.Start(context.Background(), p, "http://localhost:3000"); err == nil {
			t.Fatalf("Start(%q) should fail for unsupported platform", p)
		}
	}
}

func TestCnOAuthStatus_NotFound(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	if status, _ := svc.Status(context.Background(), "no-such-state"); status != oauthStateStatusNotFound {
		t.Fatalf("expected not_found status, got %q", status)
	}
}

func TestCnOAuthExchangeAndCreate_FakeStateFails(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	if _, err := svc.ExchangeAndCreate(context.Background(), "totally-fake-state", "rt", ""); !errors.Is(err, ErrCnOAuthStateNotFound) {
		t.Fatalf("expected ErrCnOAuthStateNotFound, got %v", err)
	}
}

func TestCnOAuthExchangeAndCreate_ExpiredStateFails(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	svc.store.Put("exp-state", oauthStateEntry{
		Platform:  PlatformTraeWork,
		MachineID: "m",
		DeviceID:  "d",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if _, err := svc.ExchangeAndCreate(context.Background(), "exp-state", "rt", ""); !errors.Is(err, ErrCnOAuthStateExpired) {
		t.Fatalf("expected ErrCnOAuthStateExpired, got %v", err)
	}
}

func TestCnOAuthExchangeAndCreate_WorkBuddyStateRejected(t *testing.T) {
	svc, _ := newTestCnOAuthSvc(&fakeCnTraeWorkLoginClient{}, &fakeCnWorkBuddyLoginClient{})
	svc.store.Put("wb-state", oauthStateEntry{
		Platform:      PlatformWorkBuddy,
		UpstreamState: "up",
		ExpiresAt:     time.Now().Add(time.Minute),
	})
	if _, err := svc.ExchangeAndCreate(context.Background(), "wb-state", "rt", ""); err == nil {
		t.Fatal("workbuddy state must not go through callback exchange")
	}
}

func TestCnOAuthExchangeAndCreate_SuccessAndReplayRejected(t *testing.T) {
	trae := &fakeCnTraeWorkLoginClient{uid: "u-1", nickname: "张三", enterpriseID: "ent-1", rotatedExpiry: 123456}
	svc, creator := newTestCnOAuthSvc(trae, &fakeCnWorkBuddyLoginClient{})
	ctx := context.Background()
	_, state, err := svc.Start(ctx, "traework", "http://localhost:3000")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	account, err := svc.ExchangeAndCreate(ctx, state, "refresh-token-1", "https://api.trae.cn")
	if err != nil {
		t.Fatalf("ExchangeAndCreate returned error: %v", err)
	}
	if account == nil {
		t.Fatal("expected account, got nil")
	}
	if len(creator.created) != 1 {
		t.Fatalf("expected creator called once, got %d", len(creator.created))
	}
	input := creator.created[0]
	if input.Type != AccountTypeOAuth {
		t.Fatalf("account type = %q, want oauth", input.Type)
	}
	if input.Platform != PlatformTraeWork {
		t.Fatalf("account platform = %q, want traework", input.Platform)
	}
	if input.Name != "张三" {
		t.Fatalf("account name = %q, want nickname 张三", input.Name)
	}
	creds := input.Credentials
	if creds["accessToken"] != "at" {
		t.Fatalf("accessToken = %v", creds["accessToken"])
	}
	if creds["refreshToken"] != "refresh-token-1" {
		t.Fatalf("refreshToken = %v", creds["refreshToken"])
	}
	if getInt64(creds["expiresAt"]) != 123456 {
		t.Fatalf("expiresAt = %v", creds["expiresAt"])
	}
	if creds["apiHost"] != "https://api.trae.cn" {
		t.Fatalf("apiHost = %v", creds["apiHost"])
	}
	if creds["uid"] != "u-1" || creds["enterpriseId"] != "ent-1" {
		t.Fatalf("uid/enterpriseId = %v/%v", creds["uid"], creds["enterpriseId"])
	}
	// 交换器确实拿到回调携带的 refreshToken 与 state 配对的设备指纹。
	if trae.gotAuth.RefreshToken != "refresh-token-1" {
		t.Fatalf("exchanger got refreshToken = %q", trae.gotAuth.RefreshToken)
	}
	if trae.gotAuth.MachineID == "" || trae.gotAuth.DeviceID == "" {
		t.Fatal("exchanger should receive state-paired machine/device id")
	}

	// State 状态机：已用。
	if status, platform := svc.Status(ctx, state); status != oauthStateStatusUsed || platform != PlatformTraeWork {
		t.Fatalf("status = %q platform=%q, want used/traework", status, platform)
	}

	// 二次使用（重放）被拒绝。
	if _, err := svc.ExchangeAndCreate(ctx, state, "refresh-token-1", ""); !errors.Is(err, ErrCnOAuthStateUsed) {
		t.Fatalf("expected ErrCnOAuthStateUsed on replay, got %v", err)
	}
}

func TestCnOAuthExchangeAndCreate_HostFallback(t *testing.T) {
	trae := &fakeCnTraeWorkLoginClient{}
	svc, _ := newTestCnOAuthSvc(trae, &fakeCnWorkBuddyLoginClient{})
	ctx := context.Background()
	_, state, _ := svc.Start(ctx, "traework", "http://localhost:3000")
	if _, err := svc.ExchangeAndCreate(ctx, state, "rt", ""); err != nil {
		t.Fatalf("ExchangeAndCreate returned error: %v", err)
	}
	if trae.gotAuth.ApiHost != traework.OAuthHost {
		t.Fatalf("apiHost = %q, want fallback %q", trae.gotAuth.ApiHost, traework.OAuthHost)
	}
}

func TestCnOAuthStatus_WorkBuddyPollCreatesAccount(t *testing.T) {
	wb := &fakeCnWorkBuddyLoginClient{}
	trae := &fakeCnTraeWorkLoginClient{}
	svc, creator := newTestCnOAuthSvc(trae, wb)
	ctx := context.Background()
	_, state, err := svc.Start(ctx, "workbuddy", "http://localhost:3000")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// 第一轮：未登录完成 → pending，不建号。
	if status, _ := svc.Status(ctx, state); status != oauthStateStatusPending {
		t.Fatalf("expected pending status, got %q", status)
	}
	if len(creator.created) != 0 {
		t.Fatalf("expected no account created while pending, got %d", len(creator.created))
	}

	// 第二轮：登录完成 → 自动建号并置 used。
	wb.pollResult = &upstream.LoginResult{
		AccessToken:  "wb-at",
		RefreshToken: "wb-rt",
		ExpiresIn:    3600,
		Domain:       "codebuddy.cn",
		UID:          "wb-uid",
		EnterpriseID: "wb-ent",
		Nickname:     "李四",
	}
	if status, platform := svc.Status(ctx, state); status != oauthStateStatusUsed || platform != PlatformWorkBuddy {
		t.Fatalf("status = %q platform=%q, want used/workbuddy", status, platform)
	}
	if len(creator.created) != 1 {
		t.Fatalf("expected creator called once, got %d", len(creator.created))
	}
	input := creator.created[0]
	if input.Platform != PlatformWorkBuddy || input.Type != AccountTypeOAuth || input.Name != "李四" {
		t.Fatalf("unexpected account input: %+v", input)
	}
	creds := input.Credentials
	if creds["accessToken"] != "wb-at" || creds["refreshToken"] != "wb-rt" {
		t.Fatalf("accessToken/refreshToken = %v/%v", creds["accessToken"], creds["refreshToken"])
	}
	if creds["uid"] != "wb-uid" || creds["enterpriseId"] != "wb-ent" {
		t.Fatalf("uid/enterpriseId = %v/%v", creds["uid"], creds["enterpriseId"])
	}
	if getInt64(creds["expiresAt"]) <= time.Now().Unix() {
		t.Fatalf("expiresAt = %v, want future unix timestamp", creds["expiresAt"])
	}

	// 第三轮：used 后不再轮询上游、不再建号。
	polls := wb.pollCount
	if status, _ := svc.Status(ctx, state); status != oauthStateStatusUsed {
		t.Fatalf("expected used status, got %q", status)
	}
	if wb.pollCount != polls || len(creator.created) != 1 {
		t.Fatal("used state must not poll upstream or create account again")
	}
}

func TestCnOAuthStatus_WorkBuddyHardErrorKeepsPending(t *testing.T) {
	wb := &fakeCnWorkBuddyLoginClient{pollErr: errors.New("upstream 500")}
	trae := &fakeCnTraeWorkLoginClient{}
	svc, creator := newTestCnOAuthSvc(trae, wb)
	ctx := context.Background()
	_, state, _ := svc.Start(ctx, "workbuddy", "http://localhost:3000")
	if status, _ := svc.Status(ctx, state); status != oauthStateStatusPending {
		t.Fatalf("expected pending status on hard error, got %q", status)
	}
	if len(creator.created) != 0 {
		t.Fatalf("expected no account created on hard error, got %d", len(creator.created))
	}
}

func getInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}
