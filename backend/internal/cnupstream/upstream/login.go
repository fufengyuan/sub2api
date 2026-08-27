// login.go WorkBuddy CN OAuth 登录（移植自原 workbuddy-wild internal/login，
// 端点/请求头/信封解析与原实现逐字一致）。
//
// 流程（服务端轮询，无浏览器回调）：
//
//	StartLogin → POST /v2/plugin/auth/state?platform=CLI 拿 state + 授权 URL
//	PollLogin  → GET  /v2/plugin/auth/token?state= 轮询登录状态（pending 返回 ErrLoginPending）
//	          → 成功后再 GET /v2/plugin/login/account?state= 拿 uid/nickname
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// 登录三端点（CN only，挂在 ChatBaseCN = copilot.tencent.com 下）。
const (
	loginEpAuthState = "/v2/plugin/auth/state?platform=CLI"
	loginEpAuthToken = "/v2/plugin/auth/token?state="
	loginEpLoginAcct = "/v2/plugin/login/account?state="
)

// ErrLoginPending 表示登录尚未完成（业务 code 非 0，浏览器还没登录完）。
var ErrLoginPending = errors.New("login pending")

// LoginResult 登录成功后拿到的凭证与账号信息。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// StartLogin 发起登录流：POST auth/state 拿上游签发的 state 与授权 URL。
// 调用方负责暂存 state 并在浏览器完成登录后轮询 PollLogin。
func (c *Client) StartLogin() (state, authURL string, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ChatBaseCN+loginEpAuthState, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	CommonHeaders(req, nil)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.State, st.AuthURL, nil
}

// PollLogin 轮询一次登录状态；未完成返回 ErrLoginPending，成功返回凭证并附带账号信息。
func (c *Client) PollLogin(state string) (*LoginResult, error) {
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("no state")
	}

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（"login ing"）
	req, err := http.NewRequest(http.MethodGet, c.ChatBaseCN+loginEpAuthToken+url.QueryEscape(state), nil)
	if err != nil {
		return nil, err
	}
	CommonHeaders(req, nil)
	tokRaw, err := c.doJSON(req)
	if err != nil {
		// pending 由业务 code 非 0（"login ing"）表达；判定须基于 *Error 的 Msg
		// 而非整个 error 字符串（后者带 "upstream client (http 200): " 前缀会误伤）。
		var upErr *Error
		if errors.As(err, &upErr) && isLoginPending(upErr.Msg) {
			return nil, ErrLoginPending
		}
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrLoginPending
	}

	// login/account 拿 uid/nickname（带 Bearer；失败不阻塞登录成功）
	res := &LoginResult{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
	}
	acctReq, err := http.NewRequest(http.MethodGet, c.ChatBaseCN+loginEpLoginAcct+url.QueryEscape(state), nil)
	if err == nil {
		CommonHeaders(acctReq, nil)
		acctReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if acctRaw, acctErr := c.doJSON(acctReq); acctErr == nil {
			var acct struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			}
			_ = json.Unmarshal(acctRaw, &acct)
			res.UID, res.EnterpriseID, res.Nickname = acct.UID, acct.EnterpriseID, acct.Nickname
		}
	}
	return res, nil
}

// isLoginPending 业务错误码（如 "login ing"）视为尚未完成。
// 入参应为 *Error.Msg（纯业务消息），不含 "http xxx" 等传输层前缀。
func isLoginPending(msg string) bool {
	return (strings.Contains(msg, "code=") || strings.Contains(msg, "login")) &&
		!strings.Contains(msg, "http ") && !strings.Contains(msg, "parse")
}
