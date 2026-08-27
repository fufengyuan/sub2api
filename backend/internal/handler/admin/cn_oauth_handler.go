package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/traework"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CnOAuthHandler 三渠道（workbuddy/traework/qoder）一键 OAuth 授权建号。
//
//	POST /admin/cn-oauth/start   发起授权（返回 authorize_url + state）
//	GET  /admin/cn-oauth/status  查询 state 状态
//	GET  /authorize              traework 授权页真实回调（127.0.0.1 本机协议）
//	GET  /oauth/callback/:platform/:state 显式 state 回调（保留的兼容路径）
type CnOAuthHandler struct {
	svc *service.CnOAuthService
}

// NewCnOAuthHandler 构造 CnOAuthHandler。
func NewCnOAuthHandler(svc *service.CnOAuthService) *CnOAuthHandler {
	return &CnOAuthHandler{svc: svc}
}

// StartRequest 发起授权请求体。
type StartRequest struct {
	Platform     string `json:"platform"`
	RedirectBase string `json:"redirect_base"`
}

// Start 发起一次 OAuth 授权流。
func (h *CnOAuthHandler) Start(c *gin.Context) {
	var req StartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn oauth service is not enabled")
		return
	}
	authorizeURL, state, err := h.svc.Start(c.Request.Context(), req.Platform, req.RedirectBase)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"authorize_url": authorizeURL,
		"state":         state,
	})
}

// Status 查询一次授权流 state 的状态。
func (h *CnOAuthHandler) Status(c *gin.Context) {
	state := c.Query("state")
	if h == nil || h.svc == nil {
		response.InternalError(c, "cn oauth service is not enabled")
		return
	}
	status, platform := h.svc.Status(c.Request.Context(), state)
	response.Success(c, gin.H{
		"status":   status,
		"platform": platform,
	})
}

// Authorize traework 授权页真实回调（GET/POST /authorize）。
// 授权页硬校验 callback 必须形如 http://127.0.0.1:{port}/authorize（不得带 state），
// 后端用最近一条 pending 的 traework state 关联本次授权流并建号。
// 凭证解析对齐原 workbuddy-wild：GET 走 query，POST（新版授权页 redirect=1）
// 走 JSON body 合并进 query 后统一解析（refreshToken > userJwt > authCodeInfo）。
func (h *CnOAuthHandler) Authorize(c *gin.Context) {
	if h == nil || h.svc == nil {
		writeCallbackResult(c, false, "cn oauth service is not enabled")
		return
	}
	q, host := callbackParams(c)
	info := traework.ParseCallbackValues(q)
	account, err := h.svc.ExchangeLatestTraeWork(c.Request.Context(), info, host)
	if err != nil {
		writeCallbackResult(c, false, err.Error())
		return
	}
	name := account.Name
	if name == "" {
		name = account.Platform
	}
	writeCallbackResult(c, true, fmt.Sprintf("账号授权成功：%s（platform=traework）", name))
}

// Callback 显式 state 公开回调（仅 traework）：授权页回跳携带路径级 state 与
// 凭证查询参数；workbuddy 走服务端轮询无回调，qoder 不支持。
func (h *CnOAuthHandler) Callback(c *gin.Context) {
	platform := c.Param("platform")
	state := c.Param("state")
	if h == nil || h.svc == nil {
		writeCallbackResult(c, false, "cn oauth service is not enabled")
		return
	}
	if platform != "traework" {
		writeCallbackResult(c, false, fmt.Sprintf("平台 %s 无需回调（workbuddy 由服务端轮询建号，qoder 请粘贴 auth JSON）", platform))
		return
	}
	q, host := callbackParams(c)
	info := traework.ParseCallbackValues(q)
	account, err := h.svc.ExchangeAndCreate(c.Request.Context(), state, info, host)
	if err != nil {
		writeCallbackResult(c, false, err.Error())
		return
	}
	name := account.Name
	if name == "" {
		name = account.Platform
	}
	writeCallbackResult(c, true, fmt.Sprintf("账号授权成功：%s（platform=%s）", name, platform))
}

// callbackParams 汇总回调请求的全部参数（query + POST JSON body 字段合并，
// query 已有的键不被覆盖），并提取 host。对齐原版 login_trae 的 GET/POST 双协议。
func callbackParams(c *gin.Context) (q url.Values, host string) {
	q = c.Request.URL.Query()
	if c.Request.Method == "POST" && c.Request.Body != nil {
		if body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10)); len(body) > 0 {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				for k, v := range m {
					if s, ok := v.(string); ok && q.Get(k) == "" {
						q.Set(k, s)
					}
				}
			}
		}
	}
	return q, q.Get("host")
}

// writeCallbackResult 以简单可读 HTML 文本回写回调结果（浏览器直接可见）。
func writeCallbackResult(c *gin.Context, ok bool, message string) {
	statusText := "授权成功"
	statusColor := "#16a34a"
	if !ok {
		statusText = "授权失败"
		statusColor = "#dc2626"
	}
	c.Data(200, "text/html; charset=utf-8", []byte(fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family: -apple-system, sans-serif; display:flex; min-height:90vh; align-items:center; justify-content:center;">
<div style="text-align:center;"><div style="font-size:40px;">%s</div>
<div style="font-size:20px; color:%s; margin-top:12px;">%s</div>
</div></body></html>`, statusText, statusColor, htmlEscape(message))))
}

func htmlEscape(s string) string {
	escaped := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			escaped = append(escaped, "&amp;"...)
		case '<':
			escaped = append(escaped, "&lt;"...)
		case '>':
			escaped = append(escaped, "&gt;"...)
		case '"':
			escaped = append(escaped, "&quot;"...)
		case '\'':
			escaped = append(escaped, "&#39;"...)
		default:
			escaped = append(escaped, s[i])
		}
	}
	return string(escaped)
}
