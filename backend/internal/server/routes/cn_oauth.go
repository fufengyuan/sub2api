package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// RegisterCnOAuthPublicRoutes 注册三渠道 OAuth 授权后的公开回调路由
// （用户浏览器授权后跳回，非 admin 鉴权）。
// 仅 traework 使用回调（workbuddy 服务端轮询、qoder 无授权流程）。
// 授权页硬校验 callback 必须形如 http://127.0.0.1:{port}/authorize（本机 IDE 协议，
// 域名/localhost/带 query 一律拒绝），真实回调走 /authorize（不带 state，
// 后端用最近 pending 的 traework state 关联；新版授权页 redirect=1 时为 POST，
// 凭证在 JSON body，GET/POST 都注册）；/oauth/callback/:platform/:state
// 为保留的显式 state 兼容路径。
func RegisterCnOAuthPublicRoutes(r *gin.Engine, h *handler.Handlers) {
	r.GET("/authorize", h.Admin.CNOAuth.Authorize)
	r.POST("/authorize", h.Admin.CNOAuth.Authorize)
	r.GET("/oauth/callback/:platform/:state", h.Admin.CNOAuth.Callback)
	r.POST("/oauth/callback/:platform/:state", h.Admin.CNOAuth.Callback)
}
