package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// RegisterCnOAuthPublicRoutes 注册三渠道 OAuth 授权后的公开回调路由
// （用户浏览器授权后跳回，非 admin 鉴权）。
// 仅 traework 使用回调（workbuddy 服务端轮询、qoder 无授权流程）；
// state 放路径而非 query，避免授权页追加 refreshToken/host 参数时破坏 URL。
func RegisterCnOAuthPublicRoutes(r *gin.Engine, h *handler.Handlers) {
	r.GET("/oauth/callback/:platform/:state", h.Admin.CNOAuth.Callback)
}
