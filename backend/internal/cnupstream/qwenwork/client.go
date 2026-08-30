// client.go 千问办公上游客户端：业务 API（dt- Bearer）+
// COSY 签名对话转发 + 错误分类，实现 provider.Upstream 接口。
package qwenwork

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
)

// Client 千问办公上游客户端。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 供 ChatStream 流式转发使用：不设 http.Client.Timeout 总超时，
	// 否则 SSE 流一旦持续超过 HTTP 的 Timeout（默认 180s）就会被 resp.Body 读取
	// 强制中断（长推理/长输出必炸）。类比 traework 的 StreamHTTP。
	// 为 nil 时回退到 HTTP。
	StreamHTTP *http.Client
	Gateway    string // 推理网关 + 业务 API，默认 https://gateway.qwenwork.cn

	// lastModel 最近一次 chat 请求的客户端模型名（含 qwenwork/ 前缀），
	// 注：仍是平台级共享——provider.Upstream.Stream/Aggregate 签名不带账号，
	// 无法按账号归属；它只影响响应里的 model 字段展示，不影响路由。
	// 供 Aggregate/Stream 覆盖响应中的 model 字段（上游恒为 auto）。
	lastModelMu sync.RWMutex
	lastModel   string
}

// New 生产默认。Qoder gateway 对 HTTP/2 不友好（stream INTERNAL_ERROR），强制 HTTP/1.1。
func New() *Client {
	return NewWithTimeout(180 * time.Second)
}

// NewWithTimeout 指定上游 HTTP 超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	return &Client{
		HTTP:       &http.Client{Timeout: timeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		Gateway:    GatewayBase,
	}
}

// NewWithGateway 测试用：覆盖网关域名。
func NewWithGateway(gateway string) *Client {
	c := New()
	c.Gateway = gateway
	return c
}

// setLastModel 记录最近一次 chat 的客户端模型名。
func (c *Client) setLastModel(m string) {
	c.lastModelMu.Lock()
	c.lastModel = m
	c.lastModelMu.Unlock()
}

// lastClientModel 读取最近一次 chat 的客户端模型名（含前缀）。
func (c *Client) lastClientModel() string {
	c.lastModelMu.RLock()
	defer c.lastModelMu.RUnlock()
	return c.lastModel
}

// modelMapTTL 账号动态模型表的重拉间隔：ChatStream 解析不到 key 且表已过期时懒刷新。
const modelMapTTL = 30 * time.Minute

// resolveModelKey 客户端模型名 → 上游 model key：**该账号**的动态映射优先，静态表兜底。
// 返回空串表示两级都没命中，由调用方决定懒刷新还是原样透传。
// 动态映射挂在 auth.Auth 上而非 Client：Client 是平台级单例，同平台不同套餐
// 账号可见的模型集不同，共享会互相覆盖 key。
func resolveModelKey(a *auth.Auth, clientName string) string {
	if a != nil {
		if k := a.ModelKey(clientName); k != "" {
			return k
		}
	}
	return staticModelKeys[clientName]
}

// refreshAccountModelMap 拉取该账号可见的动态模型表并写回其 Auth。
// 失败仅推进 attempt 时间戳（MarkModelMapAttempt），避免每个请求都重打上游。
func (c *Client) refreshAccountModelMap(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	dyn, err := c.fetchModels(a)
	if err != nil {
		a.MarkModelMapAttempt()
		return false
	}
	mm := make(map[string]string, len(dyn))
	for _, m := range dyn {
		if !m.Enable || m.Key == "" {
			continue
		}
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		mm[name] = m.Key
	}
	a.SetModelMap(mm)
	return true
}

func (c *Client) gatewayBase() string { return c.Gateway }
func (c *Client) base() string        { return c.Gateway }

func billingHeaders(req *http.Request, dt string) {
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

func (c *Client) get(path, dt string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func (c *Client) post(path, dt string, body []byte) (*http.Response, error) {
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 用 drt- 走 /api/v1/deviceToken/refresh 轮换 dt/drt。
// 401/403 → ErrSessionDead（需重新 OAuth 登录）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	log.Printf("qwenwork refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no drt- available")
		log.Printf("qwenwork refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": a.RefreshToken})
	req, err := http.NewRequest(http.MethodPost, c.base()+EpDTRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// refresh 用独立短超时客户端
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("qwenwork refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// drt 失效 → 需重新登录
		err := &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		log.Printf("qwenwork refresh session-dead uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("deviceToken refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		log.Printf("qwenwork refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("qwenwork refresh parse failed uid=%s err=%v", a.UID, err)
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	a.AccessToken = dt
	a.RefreshToken = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	log.Printf("qwenwork refresh success uid=%s expires_at=%d", a.UID, a.ExpiresAt)
	return nil
}

// ChatStream 发 chat 请求并返回原始嵌套 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体、err 为 nil；只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// body 是 server 侧改写后的 OpenAI 请求；qoder 需要 messages/model/tools/reasoning_effort。
	var reqOpenAI struct {
		Model           string           `json:"model"`
		Messages        []map[string]any `json:"messages"`
		Tools           []any            `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
		Thinking        *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &reqOpenAI); err != nil {
		return nil, 0, nil, fmt.Errorf("parse chat body: %w", err)
	}
	c.setLastModel(reqOpenAI.Model)
	modelKey := resolveModelKey(a, reqOpenAI.Model)
	// 两级映射都没命中且该账号的动态表还没拉过（或已过期）：懒刷新一次。
	// 映射按账号隔离后，管理员只为 A 账号点过「拉取上游模型」不再顺带惠及
	// B 账号，这里补一次按需拉取，避免 B 账号只能靠静态表。
	if modelKey == "" && a.ModelMapsStale(modelMapTTL) {
		if c.refreshAccountModelMap(a) {
			modelKey = resolveModelKey(a, reqOpenAI.Model)
		}
	}
	if modelKey == "" {
		modelKey = reqOpenAI.Model
	}

	// 思考开关：reasoning_effort 或 thinking:{type:"enabled"} → 启用推理
	enableReasoning := false
	reasoningEffort := ""
	if reqOpenAI.ReasoningEffort != "" {
		enableReasoning = true
		reasoningEffort = reqOpenAI.ReasoningEffort
	} else if reqOpenAI.Thinking != nil && reqOpenAI.Thinking.Type == "enabled" {
		enableReasoning = true
	}

	rawBody, err := buildAgentBody(reqOpenAI.Messages, modelKey, reqOpenAI.Tools, enableReasoning, reasoningEffort)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build qwenwork body: %w", err)
	}
	// 千问办公 chat 端点 Encode=0：body 直接发原始 JSON（不用 QoderEncoding）
	url := c.gatewayBase() + EpChat
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, string(rawBody), url, a.UID, true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	// 千问办公网关要求 x-qw-request-id（request_id is required 否则 400）
	req.Header.Set("x-qw-request-id", uuidNoDash())
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("qwenwork chat_stream uid=%s model=%s: transport error: %v", a.UID, modelKey, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qwenwork chat_stream uid=%s model=%s: upstream %d body=%s",
			a.UID, modelKey, resp.StatusCode, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// UserResource 查询账号当前可花费积分（基础 + 赠送聚合）。
// accountContextResp /api/v1/adapter/user/account-context 的 quota 段。
type accountContextResp struct {
	Data struct {
		Quota struct {
			Total     *float64 `json:"total"`
			Used      *float64 `json:"used"`
			Remaining *float64 `json:"remaining"`
			Exceeded  bool     `json:"exceeded"`
		} `json:"quota"`
		Plan struct {
			Pid      string `json:"pid"`
			Name     string `json:"name"`
			Tier     string `json:"tier"`
			Sessions int    `json:"sessions"`
		} `json:"plan"`
	} `json:"data"`
}

// accountContext 拉取账户上下文积分。
func (c *Client) accountContext(a *auth.Auth) (*accountContextResp, error) {
	dt := a.JWT()
	if dt == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	resp, err := c.get(EpAccountContext, dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, c.classifyError(resp.StatusCode, string(raw))
	}
	var q accountContextResp
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("account-context parse: %w", err)
	}
	return &q, nil
}

func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	q, err := c.accountContext(a)
	if err != nil {
		return 0, err
	}
	if q.Data.Quota.Remaining == nil {
		return 0, nil
	}
	return int64(*q.Data.Quota.Remaining), nil
}

// UserResourceDetail 查询积分明细：userQuota + addOnQuota 两个条目。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	q, err := c.accountContext(a)
	if err != nil {
		return 0, nil, err
	}
	quota := q.Data.Quota
	var total, used, remain int64
	if quota.Total != nil {
		total = int64(*quota.Total)
	}
	if quota.Used != nil {
		used = int64(*quota.Used)
	}
	if quota.Remaining != nil {
		remain = int64(*quota.Remaining)
	}
	items := []provider.ResourceItem{{
		Name:   "积分额度",
		Total:  total,
		Used:   used,
		Remain: remain,
	}}
	return remain, items, nil
}

func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("qwenwork 无需签到，积分每日自动重置")
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（嵌套 SSE → 标准 OpenAI SSE 透传）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error {
	return Stream(w, r, c.lastClientModel())
}

// Aggregate 实现 provider.Upstream（嵌套 SSE 聚合）。
func (c *Client) Aggregate(r io.Reader) (map[string]any, error) {
	return aggregate(r, c.lastClientModel())
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	// TOKEN_EXPIRE 优先于通用 401
	if status == http.StatusUnauthorized && strings.Contains(body, "TOKEN_EXPIRE") {
		return provider.ErrSessionDead // 无有效 dt → 需重新登录（dt 刷新由 RefreshToken 处理）
	}
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	// 业务码表优先于「4xx 一律 ErrClient」兜底：间歇配额/频控类业务码按码表
	// 短冷却换号，避免被反复优先选中（见 provider.errcode.go，与 qoder 对齐）。
	if code, ok := provider.ExtractBusinessCode(body); ok {
		if kind := provider.ClassifyBusinessCode(code, body); kind != provider.ErrClient {
			return kind
		}
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}

// classifyError 转成 provider.Error。
func (c *Client) classifyError(status int, body string) error {
	return &provider.Error{Kind: Classify(status, body), Status: status, Msg: truncate(body, 200)}
}

// EnsureFingerprint 为账号生成/保持 COSY 机器指纹（MachineID/Token/Type）。
// 幂等：已有则保留。与 qoderwork2api EnsureMachineFingerprint 对齐。
func EnsureFingerprint(a *auth.Auth) {
	if a.MachineID == "" {
		a.MachineID = uuid4()
	}
	if a.MachineToken == "" {
		seed := []byte(uuid4() + uuid4())
		if len(seed) > 50 {
			seed = seed[:50]
		}
		a.MachineToken = base64.RawURLEncoding.EncodeToString(seed)
	}
	if a.MachineType == "" {
		a.MachineType = strings.ReplaceAll(uuid4(), "-", "")[:18]
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ChatStream2 探针测试：显式 model key（绕过 body 的 model 字段解析）。
func (c *Client) ChatStream2(a *auth.Auth, body []byte, modelKey string) (io.ReadCloser, int, []byte, error) {
	rawBody := body
	url := c.gatewayBase() + EpChat
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, string(rawBody), url, a.UID, true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	req.Header.Set("x-qw-request-id", uuidNoDash())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, rb, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}
