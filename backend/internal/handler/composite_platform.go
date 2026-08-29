package handler

import (
	"strings"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func ensureCompositeTargetPlatform(c *gin.Context, apiKey *service.APIKey, model string) {
	if c == nil || c.Request == nil || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformComposite {
		return
	}
	if _, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context()); ok {
		return
	}
	if platform, ok := service.DetectModelPlatform(model); ok {
		c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), platform))
	}
}

func compositeTargetPlatformAllowed(c *gin.Context, apiKey *service.APIKey, model string, allowed ...string) bool {
	if c == nil || c.Request == nil || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformComposite {
		return true
	}
	ensureCompositeTargetPlatform(c, apiKey, model)
	platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	if !ok {
		// 统一账号池不再解析模型路由：别名等探测不出平台的模型名放行，交给调度层的
		// 账号级模型支持判定决定有没有账号能服务——把「猜不出平台」当成「不支持该
		// 端点」会让 composite 组的自定义模型名全部 403。
		return true
	}
	for _, allowedPlatform := range allowed {
		if platform == allowedPlatform {
			return true
		}
	}
	return false
}

// bindCompositePoolAnthropicProtocolFilter 在 Anthropic 协议入口（/v1/messages、
// /v1/messages/count_tokens、/v1/responses）为 composite 统一池声明可转发平台。
//
// 统一池本身不分平台，但转发实现分：三渠道（workbuddy/traework/qoder）与 OpenAI
// 兼容平台在这个入口没有可用的 Anthropic 转发实现（后者由 OpenAI 网关入口服务）。
// 不过滤就会把请求交给一个必然失败的账号，表现为难以定位的协议错误而不是清晰的
// 「无可用账号」。
func bindCompositePoolAnthropicProtocolFilter(c *gin.Context, apiKey *service.APIKey) {
	if c == nil || c.Request == nil || apiKey == nil || apiKey.Group == nil {
		return
	}
	if apiKey.Group.Platform != service.PlatformComposite {
		return
	}
	c.Request = c.Request.WithContext(service.WithCompositePoolPlatformFilter(
		c.Request.Context(), service.CompositeAnthropicNativePlatforms...))
}

func effectiveAPIKeyPlatform(c *gin.Context, apiKey *service.APIKey) string {
	if c != nil && c.Request != nil {
		if platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context()); ok {
			return platform
		}
	}
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	return apiKey.Group.Platform
}

func openAIReasoningEffortPolicyForRequest(c *gin.Context, apiKey *service.APIKey) (string, []service.ReasoningEffortMapping, bool) {
	if apiKey == nil || apiKey.Group == nil {
		return "", nil, false
	}
	if apiKey.Group.Platform != service.PlatformOpenAI && apiKey.Group.Platform != service.PlatformComposite {
		return "", nil, false
	}
	if effectiveAPIKeyPlatform(c, apiKey) != service.PlatformOpenAI {
		return "", nil, false
	}
	return apiKey.Group.MaxReasoningEffort, apiKey.Group.ReasoningEffortMappings, true
}

func bindRequestedReasoningEffort(c *gin.Context, body []byte, model string) {
	if c == nil || c.Request == nil {
		return
	}
	effort := service.CanonicalRequestedReasoningEffort(body, model)
	if effort == nil {
		return
	}
	c.Request = c.Request.WithContext(service.WithRequestedReasoningEffort(c.Request.Context(), *effort))
}

func stampOpenAIRequestedReasoningEffort(result *service.OpenAIForwardResult, c *gin.Context) {
	if result == nil || result.RequestedReasoningEffort != nil {
		return
	}
	if c == nil || c.Request == nil {
		return
	}
	result.RequestedReasoningEffort = service.RequestedReasoningEffortFromContext(c.Request.Context())
}

func stampForwardRequestedReasoningEffort(result *service.ForwardResult, requested *string) {
	if result == nil || result.RequestedReasoningEffort != nil {
		return
	}
	result.RequestedReasoningEffort = requested
}

func applyOpenAIReasoningEffortPolicyForRequest(c *gin.Context, apiKey *service.APIKey, body []byte) ([]byte, bool) {
	bindRequestedReasoningEffort(c, body, strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	maxEffort, mappings, ok := openAIReasoningEffortPolicyForRequest(c, apiKey)
	if !ok {
		return body, false
	}
	return service.ApplyOpenAIReasoningEffortPolicy(body, maxEffort, mappings)
}

// CompositePoolDispatchOnlyPlatforms 报告 composite 统一池是否应整体按某类入口服务。
//
// 取消 composite 模型路由后，「这一组走哪个 handler」不再由路由表解析出的目标平台决定，
// 而是由池组成决定：组内可调度的账号平台非空、且全部落在 allowed 集合内时返回 true。
// 调用方按端点能力传入 allowed——例如 OpenAI 兼容平台全集（OpenAI 网关入口）、单独的
// openai（Codex/Images 类端点）、单独的 grok（视频类端点）。混合池（如 anthropic 与
// openai 账号同组）对任何 allowed 集合都返回 false，由通用入口按选中账号平台转发。
func (h *GatewayHandler) CompositePoolDispatchOnlyPlatforms(c *gin.Context, allowed ...string) bool {
	if h == nil || h.gatewayService == nil || c == nil || c.Request == nil || len(allowed) == 0 {
		return false
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != service.PlatformComposite {
		return false
	}
	groupID := apiKey.Group.ID
	platforms := h.gatewayService.GetSchedulablePlatforms(c.Request.Context(), &groupID)
	if len(platforms) == 0 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, p := range allowed {
		allowedSet[p] = struct{}{}
	}
	for platform := range platforms {
		if _, ok := allowedSet[platform]; !ok {
			return false
		}
	}
	return true
}

func bindOpenAIReasoningEffortPolicyForMessagesRequest(c *gin.Context, apiKey *service.APIKey, body []byte) {
	if c == nil || c.Request == nil {
		return
	}
	bindRequestedReasoningEffort(c, body, strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	// The Messages bridge synthesizes a default OpenAI effort when
	// output_config.effort is omitted. Bind the group policy only for an
	// explicit client value so the ceiling does not alter that default.
	effort := gjson.GetBytes(body, "output_config.effort")
	if !effort.Exists() || effort.Type != gjson.String || strings.TrimSpace(effort.String()) == "" {
		return
	}
	maxEffort, mappings, ok := openAIReasoningEffortPolicyForRequest(c, apiKey)
	if !ok {
		return
	}
	c.Request = c.Request.WithContext(service.WithOpenAIReasoningEffortPolicy(c.Request.Context(), maxEffort, mappings))
}
