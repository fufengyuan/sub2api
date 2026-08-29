package handler

import (
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompositeTargetPlatformAllowedResolvesKnownAllowedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/embeddings", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

	require.True(t, compositeTargetPlatformAllowed(c, apiKey, "text-embedding-3-large", service.PlatformOpenAI))
	platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, service.PlatformOpenAI, platform)
}

func TestOpenAICompatibleTextTargetAllowsCompositeProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	providers := []struct {
		model    string
		platform string
	}{
		{model: "grok-4.3", platform: service.PlatformGrok},
		{model: "kimi-k2-thinking", platform: service.PlatformKimi},
		{model: "glm-5.2", platform: service.PlatformZhipu},
		{model: "deepseek-v3.2", platform: service.PlatformDeepseek},
	}
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1/responses/input_tokens", "/v1/messages/count_tokens"} {
		for _, provider := range providers {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", path, nil)
			apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

			require.True(t, openAICompatibleTextTargetAllowed(c, apiKey, provider.model), "path=%s model=%s", path, provider.model)
			platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
			require.True(t, ok, "path=%s model=%s", path, provider.model)
			require.Equal(t, provider.platform, platform, "path=%s model=%s", path, provider.model)
		}
	}
}

// WS ingress 对 CN 账号既过不了 transport 过滤、HTTP 桥也没有 Responses 转换，
// 放行只会把明确的策略拒绝换成 "no available account"，因此 WS 白名单保持 openai+grok。
func TestResponsesWebSocketCompositePlatformGuardKeepsOpenAIAndGrokOnly(t *testing.T) {
	require.True(t, isResponsesWebSocketCompositePlatform(service.PlatformOpenAI))
	require.True(t, isResponsesWebSocketCompositePlatform(service.PlatformGrok))
	for _, platform := range []string{
		service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek,
		service.PlatformAnthropic, service.PlatformGemini,
	} {
		require.False(t, isResponsesWebSocketCompositePlatform(platform), "platform=%s", platform)
	}
}

func TestCompositeTargetPlatformAllowsUnknownModelAndRejectsWrongProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("探测不出平台的别名放行，交给统一池判定", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/embeddings", nil)
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

		require.True(t, compositeTargetPlatformAllowed(c, apiKey, "llama-4-maverick", service.PlatformOpenAI))
		_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		require.False(t, ok, "放行为主时不应臆造目标平台")
	})

	t.Run("探测到明确不匹配的平台仍拒绝", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/embeddings", nil)
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

		require.False(t, compositeTargetPlatformAllowed(c, apiKey, "claude-sonnet-4-5", service.PlatformOpenAI))
	})
}

// composite 统一池在 Anthropic 协议入口按端点协议族过滤账号；非 composite 组不受影响。
func TestBindCompositePoolAnthropicProtocolFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	compositeCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	compositeCtx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	bindCompositePoolAnthropicProtocolFilter(compositeCtx, &service.APIKey{
		Group: &service.Group{Platform: service.PlatformComposite},
	})
	allowed, ok := service.CompositePoolPlatformFilterFromContext(compositeCtx.Request.Context())
	require.True(t, ok, "composite 组应写入端点协议族过滤")
	require.ElementsMatch(t, service.CompositeAnthropicNativePlatforms, allowed)
	require.True(t, service.CompositePoolAccountAllowed(compositeCtx.Request.Context(), service.PlatformAnthropic))
	require.False(t, service.CompositePoolAccountAllowed(compositeCtx.Request.Context(), service.PlatformWorkBuddy),
		"三渠道无 Anthropic 协议桥接实现，不应被选中")

	plainCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	plainCtx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	bindCompositePoolAnthropicProtocolFilter(plainCtx, &service.APIKey{
		Group: &service.Group{Platform: service.PlatformOpenAI},
	})
	_, ok = service.CompositePoolPlatformFilterFromContext(plainCtx.Request.Context())
	require.False(t, ok, "非 composite 分组不写过滤")
}

func TestOpenAIReasoningEffortPolicyForCompositeTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{
		Platform:           service.PlatformComposite,
		MaxReasoningEffort: "medium",
		ReasoningEffortMappings: []service.ReasoningEffortMapping{
			{From: "max", To: "xhigh"},
		},
	}
	apiKey := &service.APIKey{Group: group}
	body := []byte(`{"reasoning":{"effort":"max"}}`)

	openAICtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	openAICtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	openAICtx.Request = openAICtx.Request.WithContext(service.WithResolvedTargetPlatform(openAICtx.Request.Context(), service.PlatformOpenAI))
	got, changed := applyOpenAIReasoningEffortPolicyForRequest(openAICtx, apiKey, body)
	require.True(t, changed)
	require.JSONEq(t, `{"reasoning":{"effort":"medium"}}`, string(got))

	bindOpenAIReasoningEffortPolicyForMessagesRequest(openAICtx, apiKey, []byte(`{"output_config":{"effort":"max"}}`))
	bound, changed := service.ApplyOpenAIReasoningEffortPolicyFromContext(openAICtx.Request.Context(), body)
	require.True(t, changed)
	require.JSONEq(t, `{"reasoning":{"effort":"medium"}}`, string(bound))

	omittedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	omittedCtx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	omittedCtx.Request = omittedCtx.Request.WithContext(service.WithResolvedTargetPlatform(omittedCtx.Request.Context(), service.PlatformOpenAI))
	bindOpenAIReasoningEffortPolicyForMessagesRequest(omittedCtx, apiKey, []byte(`{"model":"gpt-5"}`))
	omitted, changed := service.ApplyOpenAIReasoningEffortPolicyFromContext(omittedCtx.Request.Context(), body)
	require.False(t, changed)
	require.Equal(t, body, omitted)

	grokCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	grokCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	grokCtx.Request = grokCtx.Request.WithContext(service.WithResolvedTargetPlatform(grokCtx.Request.Context(), service.PlatformGrok))
	got, changed = applyOpenAIReasoningEffortPolicyForRequest(grokCtx, apiKey, body)
	require.False(t, changed)
	require.Equal(t, body, got)
}

// 取消路由解析后不再存在「公开别名 → 上游模型名」的 ctx 改写：内容审计入参与用量
// 字段都直接使用请求里的模型名，Provider 由端点/模型探测出的平台决定。
func TestClientRequestedModelFallsBackToRequestedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenAI))

	input := buildContentModerationInput(c, nil, middleware2.AuthSubject{UserID: 42}, service.ContentModerationProtocolOpenAIChat, "gpt-5", nil)
	require.Equal(t, "gpt-5", input.Model)
	require.Equal(t, service.PlatformOpenAI, input.Provider)

	fields := clientRequestedUsageFields(c, service.ChannelMappingResult{MappedModel: "gpt-5"}, "gpt-5", "gpt-5")
	require.Equal(t, "gpt-5", fields.OriginalModel)
}
