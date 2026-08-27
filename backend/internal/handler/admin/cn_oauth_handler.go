package admin

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CnOAuthHandler 三渠道（workbuddy/traework/qoder）一键 OAuth 授权建号。
//
//	POST /admin/cn-oauth/start   发起授权（返回 authorize_url + state）
//	GET  /admin/cn-oauth/status  查询 state 状态
//	GET  /oauth/callback/:platform 公开回调（用户授权后跳回）
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

// Callback 公开回调（仅 traework）：授权页回跳携带路径级 state 与
// refreshToken/host 查询参数；workbuddy 走服务端轮询无回调，qoder 不支持。
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
	refreshToken := c.Query("refreshToken")
	host := c.Query("host")
	account, err := h.svc.ExchangeAndCreate(c.Request.Context(), state, refreshToken, host)
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
