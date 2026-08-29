package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// composite 路由解析机制已取消（见 .claude/artifacts/designs/composite-unified-pool.md）：
// 网关链路不再读 composite_model_routes、不再改写请求体模型名、不再写候选平台列表。
// 这里保留三条契约测试：① composite 组的 body/ctx 不被端点解析污染；
// ② Gemini 协议入口仍按端点语义为 composite 组写入目标平台（信息来自路径而非路由表）；
// ③ 池组成判定的空指针安全。

func compositeRoutesTestRouter(middlewares []gin.HandlerFunc, path string, final gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.HandlerFunc(servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		groupID := int64(1)
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
			GroupID: &groupID,
			Group:   &service.Group{ID: groupID, Platform: service.PlatformComposite},
		})
		c.Next()
	})))
	for _, mw := range middlewares {
		router.Use(mw)
	}
	router.POST(path, final)
	return router
}

func compositeAPIKeyRequest(t *testing.T, router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// 取消路由解析后：不写目标平台、不改写模型名，账号级 model_mapping 是唯一映射层。
func TestCompositeEndpointsDoNotRewriteRequestBody(t *testing.T) {
	router := compositeRoutesTestRouter(nil, "/v1/chat/completions", func(c *gin.Context) {
		_, resolved := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		require.False(t, resolved, "composite 请求不应再被解析出目标平台")

		reqBody, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"deepseek-v4-flash","messages":[]}`, string(reqBody), "模型名不得被改写")
		c.Status(http.StatusNoContent)
	})

	w := compositeAPIKeyRequest(t, router, "/v1/chat/completions", `{"model":"deepseek-v4-flash","messages":[]}`)
	require.Equal(t, http.StatusNoContent, w.Code)
}

// Gemini 入口的实现只服务 gemini 协议账号，这一信息来自端点本身而非路由表。
func TestCompositeGeminiEndpointWritesEndpointPlatform(t *testing.T) {
	router := compositeRoutesTestRouter(
		[]gin.HandlerFunc{compositeGeminiEndpointPlatform()},
		"/v1beta/models/x:generateContent",
		func(c *gin.Context) {
			platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
			require.True(t, ok, "Gemini 入口应为 composite 组写入端点语义平台")
			require.Equal(t, service.PlatformGemini, platform)
			c.Status(http.StatusNoContent)
		})

	w := compositeAPIKeyRequest(t, router, "/v1beta/models/x:generateContent", `{"contents":[]}`)
	require.Equal(t, http.StatusNoContent, w.Code)
}

// 已有平台提示（如 handler 侧按模型名探测）优先，端点默认不得覆盖。
func TestCompositeGeminiEndpointKeepsExistingPlatformHint(t *testing.T) {
	preflight := func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))
		c.Next()
	}
	router := compositeRoutesTestRouter(
		[]gin.HandlerFunc{preflight, compositeGeminiEndpointPlatform()},
		"/v1beta/models/x:generateContent",
		func(c *gin.Context) {
			platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, service.PlatformOpenAI, platform, "端点默认不应覆盖已解析出的平台")
			c.Status(http.StatusNoContent)
		})

	w := compositeAPIKeyRequest(t, router, "/v1beta/models/x:generateContent", `{"contents":[]}`)
	require.Equal(t, http.StatusNoContent, w.Code)
}

// 池组成判定的空指针安全：未接调度器的 handler、以及 nil receiver 都只返回 false，
// 让混合/未知池落到通用入口。
func TestCompositePoolDispatchIsNilSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: 1, Platform: service.PlatformComposite},
	})

	require.False(t, (&handler.GatewayHandler{}).CompositePoolDispatchOnlyPlatforms(c, service.PlatformOpenAI))
	var nilHandler *handler.GatewayHandler
	require.False(t, nilHandler.CompositePoolDispatchOnlyPlatforms(c, service.PlatformOpenAI))
	// 非 composite 分组不参与该判定。
	c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: 2, Platform: service.PlatformOpenAI},
	})
	require.False(t, (&handler.GatewayHandler{}).CompositePoolDispatchOnlyPlatforms(c, service.PlatformOpenAI))
}

// compositePoolAccountRepoStub 只提供「组内可调度账号」查询的账号仓储桩。
type compositePoolAccountRepoStub struct {
	service.AccountRepository
	platforms []string
}

func (s *compositePoolAccountRepoStub) ListSchedulableByGroupID(_ context.Context, _ int64) ([]service.Account, error) {
	accounts := make([]service.Account, 0, len(s.platforms))
	for i, platform := range s.platforms {
		accounts = append(accounts, service.Account{
			ID: int64(i + 1), Platform: platform, Status: service.StatusActive, Schedulable: true,
		})
	}
	return accounts, nil
}

// newCompositePoolRouter 构造接了真实 GatewayService（账号池平台可配置）的 composite 路由。
func newCompositePoolRouter(t *testing.T, poolPlatforms ...string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}}
	groupID := int64(1)
	group := &service.Group{ID: groupID, Platform: service.PlatformComposite, Status: service.StatusActive}
	apiKey := &service.APIKey{ID: 11, GroupID: &groupID, Group: group, Status: service.StatusActive}
	gatewayService := service.NewGatewayService(
		&compositePoolAccountRepoStub{platforms: poolPlatforms},
		nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gatewayHandler := handler.NewGatewayHandler(
		gatewayService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil)

	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{Gateway: gatewayHandler, OpenAIGateway: &handler.OpenAIGatewayHandler{}},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			c.Set(string(servermiddleware.ContextKeyAPIKey), apiKey)
			c.Next()
		}),
		nil, nil, nil, nil, cfg,
	)
	return router
}

// OpenAI-only 端点的能力不再由路由表解析出的目标平台决定，而由池组成决定：
// 纯 openai 池可进 embeddings，gemini 池则按通用入口拒绝。
func TestCompositePoolCompositionDrivesEndpointDispatch(t *testing.T) {
	openAIPool := newCompositePoolRouter(t, service.PlatformOpenAI)
	w := compositeAPIKeyRequest(t, openAIPool, "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hello"}`)
	require.NotEqual(t, http.StatusNotFound, w.Code, "纯 openai 池应可进 embeddings 入口")

	geminiPool := newCompositePoolRouter(t, service.PlatformGemini)
	w = compositeAPIKeyRequest(t, geminiPool, "/v1/embeddings", `{"model":"gemini-2.5-pro","input":"hello"}`)
	require.Equal(t, http.StatusNotFound, w.Code, "非 openai 池不得进 embeddings 入口")
	require.Contains(t, w.Body.String(), "Embeddings API is not supported")

	mixedPool := newCompositePoolRouter(t, service.PlatformOpenAI, service.PlatformAnthropic)
	w = compositeAPIKeyRequest(t, mixedPool, "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hello"}`)
	require.Equal(t, http.StatusNotFound, w.Code, "混合池按通用入口处理")
}
