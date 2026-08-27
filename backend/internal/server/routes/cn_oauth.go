package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// RegisterCnOAuthPublicRoutes 注册三渠道 OAuth 授权后的公开回调路由
// （用户浏览器授权后跳回，非 admin 鉴权）。
func RegisterCnOAuthPublicRoutes(r *gin.Engine, h *handler.Handlers) {
	r.GET("/oauth/callback/:platform", h.Admin.CNOAuth.Callback)
}
