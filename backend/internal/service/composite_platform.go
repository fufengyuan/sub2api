package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// WithResolvedTargetPlatform stores the concrete provider chosen for a request
// made through a composite group.
func WithResolvedTargetPlatform(ctx context.Context, platform string) context.Context {
	platform = strings.TrimSpace(platform)
	if ctx == nil || platform == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.ResolvedTargetPlatform, platform)
}

// ResolvedTargetPlatformFromContext returns the concrete provider chosen for
// the current request, if one was resolved.
func ResolvedTargetPlatformFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	platform, ok := ctx.Value(ctxkey.ResolvedTargetPlatform).(string)
	platform = strings.TrimSpace(platform)
	if !ok || platform == "" {
		return "", false
	}
	return platform, true
}

// WithCompositePoolPlatformFilter 声明 composite 统一账号池在当前入口可转发的平台集合。
//
// composite 分组本身没有平台概念（设计 §2），但网关转发实现按入站协议分族：
// Anthropic 协议入口（/v1/messages 等）没有三渠道的桥接实现，把这类账号交给调度
// 只会得到一次必然失败的转发。因此由 handler 入口声明可服务平台，统一池在候选过滤
// 阶段排除其余平台，让「不支持」明确表现为无可用账号而不是错协议请求。
func WithCompositePoolPlatformFilter(ctx context.Context, platforms ...string) context.Context {
	if ctx == nil || len(platforms) == 0 {
		return ctx
	}
	allowed := make([]string, 0, len(platforms))
	for _, p := range platforms {
		if p = strings.TrimSpace(p); p != "" {
			allowed = append(allowed, p)
		}
	}
	if len(allowed) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.CompositePoolPlatformFilter, allowed)
}

// CompositePoolPlatformFilterFromContext 返回当前入口可转发的平台集合；未设置时 ok=false。
func CompositePoolPlatformFilterFromContext(ctx context.Context) ([]string, bool) {
	if ctx == nil {
		return nil, false
	}
	allowed, ok := ctx.Value(ctxkey.CompositePoolPlatformFilter).([]string)
	if !ok || len(allowed) == 0 {
		return nil, false
	}
	return allowed, true
}

// CompositePoolAccountAllowed 判断账号平台是否可通过当前入口的协议族过滤。
// 未设置过滤（如 OpenAI 协议通用入口）时一律放行。
func CompositePoolAccountAllowed(ctx context.Context, platform string) bool {
	allowed, ok := CompositePoolPlatformFilterFromContext(ctx)
	if !ok {
		return true
	}
	platform = strings.TrimSpace(platform)
	for _, p := range allowed {
		if p == platform {
			return true
		}
	}
	return false
}

// DetectModelPlatform maps common public model IDs to the concrete provider
// platform used by sub2api. It intentionally returns false for ambiguous model
// names so composite groups fail closed instead of guessing.
func DetectModelPlatform(model string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "", false
	}

	normalized = strings.TrimPrefix(normalized, "models/")
	if slash := strings.IndexByte(normalized, '/'); slash > 0 {
		provider := strings.TrimSpace(normalized[:slash])
		rest := strings.TrimSpace(normalized[slash+1:])
		switch provider {
		case "anthropic", "claude":
			return PlatformAnthropic, true
		case "openai", "chatgpt":
			return PlatformOpenAI, true
		case "google", "google-ai-studio", "gemini":
			return PlatformGemini, true
		case "xai", "x-ai", "grok":
			return PlatformGrok, true
		case "kimi", "moonshot":
			return PlatformKimi, true
		case "zhipu", "glm", "bigmodel":
			return PlatformZhipu, true
		case "deepseek":
			return PlatformDeepseek, true
		}
		if rest != "" {
			normalized = strings.TrimPrefix(rest, "models/")
		}
	}

	switch {
	case strings.HasPrefix(normalized, "anthropic.claude-"),
		strings.HasPrefix(normalized, "claude-"):
		return PlatformAnthropic, true
	case strings.HasPrefix(normalized, "gpt-"),
		strings.HasPrefix(normalized, "chatgpt-"),
		strings.HasPrefix(normalized, "codex-"),
		strings.HasPrefix(normalized, "text-embedding-"),
		strings.HasPrefix(normalized, "text-moderation-"),
		strings.HasPrefix(normalized, "omni-moderation-"),
		strings.HasPrefix(normalized, "dall-e-"),
		strings.HasPrefix(normalized, "gpt-image-"),
		strings.HasPrefix(normalized, "tts-"),
		strings.HasPrefix(normalized, "whisper-"),
		hasOpenAISeriesPrefix(normalized):
		return PlatformOpenAI, true
	case strings.HasPrefix(normalized, "gemini-"),
		strings.HasPrefix(normalized, "learnlm-"):
		return PlatformGemini, true
	case normalized == "grok" || strings.HasPrefix(normalized, "grok-"):
		return PlatformGrok, true
	case normalized == "k3",
		normalized == "k3-256k",
		strings.HasPrefix(normalized, "kimi-"),
		strings.HasPrefix(normalized, "moonshot-"):
		return PlatformKimi, true
	case strings.HasPrefix(normalized, "glm-"):
		return PlatformZhipu, true
	case strings.HasPrefix(normalized, "deepseek-"):
		return PlatformDeepseek, true
	default:
		return "", false
	}
}

func hasOpenAISeriesPrefix(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4", "o5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

// CompositeOpenAICompatPlatforms 是「只能经 OpenAI 网关入口服务」的平台集合：
// 这些平台的账号没有 Anthropic 原生转发实现（走 OpenAIGatewayService 的协议桥），
// 因此 composite 统一池的分派依据是「池内是否只有这一类平台」——是则整体按
// OpenAI 入口处理，否则（含 anthropic/gemini/antigravity/三渠道）按 Anthropic
// 入口处理并在选号时按端点协议族过滤。
var CompositeOpenAICompatPlatforms = []string{
	PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek,
}

// IsCompositeOpenAICompatPlatform 报告平台是否属于 CompositeOpenAICompatPlatforms。
func IsCompositeOpenAICompatPlatform(platform string) bool {
	for _, p := range CompositeOpenAICompatPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

// CompositeAnthropicNativePlatforms 是 Anthropic 协议入口（/v1/messages、
// /v1/responses、count_tokens）可直接转发的平台集合。三渠道（workbuddy/traework/
// qoder）没有 Messages/Responses 的 Anthropic 协议桥接实现，OpenAI 兼容平台则由
// OpenAI 网关入口服务，因此都不在此列。
var CompositeAnthropicNativePlatforms = []string{
	PlatformAnthropic, PlatformAntigravity, PlatformGemini,
}

func isConcreteRequestPlatform(platform string) bool {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek,
		PlatformWorkBuddy, PlatformTraeWork, PlatformQoder, PlatformQwenWork:
		return true
	default:
		return false
	}
}
