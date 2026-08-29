package qwenwork

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 千问办公 CN OAuth 设备授权（PKCE Device Flow）。
// 移植自原 workbuddy-wild 的 login_qwenwork：与桌面端 GUI 对齐的
// /device/selectAccounts 授权页 + /api/v1/deviceToken/poll 轮询。
// 与原实现的差异：PKCE 状态不落盘，由调用方（cn_oauth_service 的
// oauthStateStore）持有；机器指纹在登录完成后经 EnsureFingerprint 生成
// 并随 credentials 入库，不再写 ~/.qwenworkcn。

// ErrLoginPending 表示授权尚未完成（浏览器侧还没确认）。
var ErrLoginPending = errors.New("qwenwork login pending")

const (
	oauthDeviceSelect = "/device/selectAccounts"
	oauthDevicePoll   = "/api/v1/deviceToken/poll"
	oauthClientID     = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	oauthRedirect     = "qwenwork-cn://"
	oauthClientUA     = "Go-http-client/2.0"
)

// LoginState 一次设备授权流的 PKCE 状态（由调用方保存并在轮询时回传）。
type LoginState struct {
	Verifier  string `json:"verifier"`
	Nonce     string `json:"nonce"`
	MachineID string `json:"machine_id"`
}

// LoginResult 登录成功后拿到的凭证与账号信息。
type LoginResult struct {
	AccessToken  string // dt-
	RefreshToken string // drt-
	ExpiresIn    int64  // 秒
	UID          string
	Nickname     string
}

// StartLogin 生成 PKCE 状态与授权 URL。machineID 由调用方传入（必须与
// 建号时写入 credentials.machineId 的值一致，上游按 machine_id 识别设备）。
func startLogin(machineID string) (*LoginState, string, error) {
	if strings.TrimSpace(machineID) == "" {
		machineID = uuid4()
	}
	verifier, challenge := makePKCE()
	nonce := uuid4()
	authURL := fmt.Sprintf("%s%s?challenge=%s&challenge_method=S256&nonce=%s&machine_id=%s&client_id=%s&redirect_uri=%s",
		GatewayBase, oauthDeviceSelect, challenge, nonce, machineID, oauthClientID, urlQueryEscape(oauthRedirect))
	return &LoginState{Verifier: verifier, Nonce: nonce, MachineID: machineID}, authURL, nil
}

// PollLogin 单次轮询设备令牌：未完成返回 ErrLoginPending；成功返回 dt-/drt-。
func pollLogin(st *LoginState) (*LoginResult, error) {
	if st == nil || st.Verifier == "" || st.Nonce == "" {
		return nil, fmt.Errorf("qwenwork login state missing verifier/nonce")
	}
	url := fmt.Sprintf("%s%s?nonce=%s&verifier=%s&challenge_method=S256",
		GatewayBase, oauthDevicePoll, st.Nonce, st.Verifier)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", oauthClientUA)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 202/404 = 授权未完成（对齐原 login_qwenwork 的 pending 语义）。
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, ErrLoginPending
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qwenwork device poll http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parse qwenwork device poll: %w", err)
	}
	token := jsonString(tok, "token")
	if token == "" {
		token = jsonString(tok, "device_token")
	}
	if token == "" {
		return nil, ErrLoginPending
	}
	return &LoginResult{
		AccessToken:  token,
		RefreshToken: jsonString(tok, "refresh_token"),
		ExpiresIn:    int64(jsonFloat(tok, "expires_in")),
		UID:          firstNonEmpty(jsonString(tok, "user_id"), jsonString(tok, "uid")),
		Nickname:     jsonString(tok, "nickname"),
	}, nil
}

// makePKCE 生成 verifier + S256 challenge（verifier 长度 43~128 随机，对齐 GUI）。
func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	n := 43 + int(randInt(86))
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	sb.Grow(n)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func randInt(n int) uint32 {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if n <= 0 {
		return 0
	}
	return v % uint32(n)
}

// urlQueryEscape 简化的 query 编码（仅编码授权 URL 中的必需字符，对齐原实现）。
func urlQueryEscape(s string) string {
	return strings.NewReplacer(":", "%3A", "/", "%2F", "&", "%26", "=", "%3D", "?", "%3F").Replace(s)
}

func jsonString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func jsonFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// StartLogin/PollLogin 的方法包装：供 cn_oauth_service 的窄接口
// （cnQwenWorkLoginClient）以 Client 实例注入。
func (c *Client) StartLogin(machineID string) (*LoginState, string, error) {
	return startLogin(machineID)
}

func (c *Client) PollLogin(st *LoginState) (*LoginResult, error) { return pollLogin(st) }
