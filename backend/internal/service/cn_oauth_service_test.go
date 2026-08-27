package service

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
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

// fakeCnAuthCodeExchanger 冒充换 token 窄接口的测试替身。
type fakeCnAuthCodeExchanger struct {
	result      *traework.AuthCodeResult
	err         error
	gotAuth     *auth.Auth
	gotCode     string
	gotVerifier string
}

func (f *fakeCnAuthCodeExchanger) ExchangeAuthCode(a *auth.Auth, authCode, codeVerifier string) (*traework.AuthCodeResult, error) {
	f.gotAuth, f.gotCode, f.gotVerifier = a, authCode, codeVerifier
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &traework.AuthCodeResult{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 123456, Host: "https://api.trae.cn"}, nil
}

func newTestCnOAuthSvc() (*CnOAuthService, *fakeCnAccountCreator, *fakeCnAuthCodeExchanger) {
	creator := &fakeCnAccountCreator{}
	exch := &fakeCnAuthCodeExchanger{}
	svc := NewCnOAuthService(creator, exch, NewInMemoryOAuthStateStore())
	return svc, creator, exch
}

func TestCnOAuthStart_GeneratesStateAndAuthorizeURL(t *testing.T) {
	svc, _, _ := newTestCnOAuthSvc()
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
	q := parsed.Query()
	if q.Get("client_id") != traework.ClientID {
		t.Fatalf("client_id = %q, want %q", q.Get("client_id"), traework.ClientID)
	}
	if q.Get("redirect_uri") != "http://localhost:3000/oauth/callback/traework" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("expected non-empty code_challenge")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("state") != state {
		t.Fatalf("url state=%q, want state=%q", q.Get("state"), state)
	}
}

func TestCnOAuthStart_UnsupportedPlatform(t *testing.T) {
	svc, _, _ := newTestCnOAuthSvc()
	for _, p := range []string{"workbuddy", "qoder", "unknown"} {
		if _, _, err := svc.Start(context.Background(), p, "http://localhost:3000"); err == nil {
			t.Fatalf("Start(%q) should fail for unsupported platform", p)
		}
	}
}

func TestCnOAuthStatus_PendingAndNotFound(t *testing.T) {
	svc, _, _ := newTestCnOAuthSvc()
	_, state, _ := svc.Start(context.Background(), "traework", "http://localhost:3000")
	if status, _ := svc.Status(context.Background(), state); status != oauthStateStatusPending {
		t.Fatalf("expected pending status, got %q", status)
	}
	if status, _ := svc.Status(context.Background(), "no-such-state"); status != oauthStateStatusNotFound {
		t.Fatalf("expected not_found status, got %q", status)
	}
}

func TestCnOAuthExchangeAndCreate_FakeStateFails(t *testing.T) {
	svc, _, _ := newTestCnOAuthSvc()
	if _, err := svc.ExchangeAndCreate(context.Background(), "totally-fake-state", "code1"); !errors.Is(err, ErrCnOAuthStateNotFound) {
		t.Fatalf("expected ErrCnOAuthStateNotFound, got %v", err)
	}
}

func TestCnOAuthExchangeAndCreate_ExpiredStateFails(t *testing.T) {
	svc, _, _ := newTestCnOAuthSvc()
	expired := oauthStateEntry{
		Platform:     PlatformTraeWork,
		CodeVerifier: "verifier",
		RedirectURI:  "http://localhost:3000/oauth/callback/traework",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	svc.store.Put("exp-state", expired)
	if _, err := svc.ExchangeAndCreate(context.Background(), "exp-state", "code1"); !errors.Is(err, ErrCnOAuthStateExpired) {
		t.Fatalf("expected ErrCnOAuthStateExpired, got %v", err)
	}
}

func TestCnOAuthExchangeAndCreate_SuccessAndReplayRejected(t *testing.T) {
	svc, creator, exch := newTestCnOAuthSvc()
	ctx := context.Background()
	_, state, err := svc.Start(ctx, "traework", "http://localhost:3000")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	account, err := svc.ExchangeAndCreate(ctx, state, "sso-code")
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
	creds := input.Credentials
	if creds["access_token"] != "at" {
		t.Fatalf("access_token = %v", creds["access_token"])
	}
	if creds["refresh_token"] != "rt" {
		t.Fatalf("refresh_token = %v", creds["refresh_token"])
	}
	if getInt64(creds["expires_at"]) != 123456 {
		t.Fatalf("expires_at = %v", creds["expires_at"])
	}
	if creds["api_host"] != "https://api.trae.cn" {
		t.Fatalf("api_host = %v", creds["api_host"])
	}
	// 交换器确实拿到 code 与配对 verifier。
	if exch.gotCode != "sso-code" {
		t.Fatalf("exchanger got code = %q", exch.gotCode)
	}
	if exch.gotVerifier == "" {
		t.Fatal("exchanger should receive non-empty code verifier")
	}

	// State 状态机：已用。
	if status, platform := svc.Status(ctx, state); status != oauthStateStatusUsed || platform != PlatformTraeWork {
		t.Fatalf("status = %q platform=%q, want used/traework", status, platform)
	}

	// 二次使用（重放）被拒绝。
	if _, err := svc.ExchangeAndCreate(ctx, state, "sso-code"); !errors.Is(err, ErrCnOAuthStateUsed) {
		t.Fatalf("expected ErrCnOAuthStateUsed on replay, got %v", err)
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
