package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginFixtureServer 模拟 WorkBuddy 登录三端点（auth/state / auth/token / login/account）。
func loginFixtureServer(t *testing.T, pendingFirst int) (*httptest.Server, *int, *[]string) {
	t.Helper()
	tokenCalls := 0
	bearers := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			if r.Method != http.MethodPost {
				t.Errorf("auth/state method = %s, want POST", r.Method)
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"up-st","authUrl":"https://www.codebuddy.cn/login?x=1"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			tokenCalls++
			if tokenCalls <= pendingFirst {
				_, _ = w.Write([]byte(`{"code":40001,"msg":"login ing","data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600,"domain":"codebuddy.cn"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			bearers = append(bearers, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"uid":"u-9","enterpriseId":"ent-9","nickname":"王五"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return srv, &tokenCalls, &bearers
}

func TestStartLogin_ReturnsStateAndAuthURL(t *testing.T) {
	srv, _, _ := loginFixtureServer(t, 0)
	defer srv.Close()
	c := New()
	c.ChatBaseCN = srv.URL

	state, authURL, err := c.StartLogin()
	if err != nil {
		t.Fatalf("StartLogin returned error: %v", err)
	}
	if state != "up-st" {
		t.Fatalf("state = %q, want up-st", state)
	}
	if authURL != "https://www.codebuddy.cn/login?x=1" {
		t.Fatalf("authURL = %q", authURL)
	}
}

func TestPollLogin_PendingThenSuccess(t *testing.T) {
	srv, _, bearers := loginFixtureServer(t, 1)
	defer srv.Close()
	c := New()
	c.ChatBaseCN = srv.URL

	// 第一轮：业务 code 非 0（"login ing"）→ ErrLoginPending。
	if _, err := c.PollLogin("up-st"); err != ErrLoginPending {
		t.Fatalf("expected ErrLoginPending, got %v", err)
	}

	// 第二轮：成功返回 token + 账号信息。
	res, err := c.PollLogin("up-st")
	if err != nil {
		t.Fatalf("PollLogin returned error: %v", err)
	}
	if res.AccessToken != "at-1" || res.RefreshToken != "rt-1" {
		t.Fatalf("tokens = %v/%v", res.AccessToken, res.RefreshToken)
	}
	if res.ExpiresIn != 3600 || res.Domain != "codebuddy.cn" {
		t.Fatalf("expiresIn/domain = %v/%v", res.ExpiresIn, res.Domain)
	}
	if res.UID != "u-9" || res.Nickname != "王五" || res.EnterpriseID != "ent-9" {
		t.Fatalf("account info = %v/%v/%v", res.UID, res.Nickname, res.EnterpriseID)
	}
	if len(*bearers) != 1 || (*bearers)[0] != "Bearer at-1" {
		t.Fatalf("login/account bearers = %v", *bearers)
	}
}

func TestPollLogin_HardErrorNotPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New()
	c.ChatBaseCN = srv.URL

	if _, err := c.PollLogin("up-st"); err == nil || err == ErrLoginPending {
		t.Fatalf("expected hard error, got %v", err)
	}
}
