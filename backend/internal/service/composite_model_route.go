package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	CompositeRouteMatchExact  = "exact"
	CompositeRouteMatchPrefix = "prefix"

	CompositeRouteEndpointAny             = "any"
	CompositeRouteEndpointMessages        = "messages"
	CompositeRouteEndpointCountTokens     = "count_tokens"
	CompositeRouteEndpointResponses       = "responses"
	CompositeRouteEndpointChatCompletions = "chat_completions"
	CompositeRouteEndpointEmbeddings      = "embeddings"
	CompositeRouteEndpointImages          = "images"
	CompositeRouteEndpointGemini          = "gemini"

	CompositeRouteSourceExplicit = "route"
	CompositeRouteSourceDetector = "detector"
)

var (
	ErrCompositeRouteNotFound = infraerrors.NotFound("COMPOSITE_ROUTE_NOT_FOUND", "composite route not found")
	ErrCompositeRouteExists   = infraerrors.Conflict("COMPOSITE_ROUTE_EXISTS", "composite route already exists")
)

// CompositeModelRoute maps one public model identifier in a composite group to
// the concrete provider/model that should handle the request.
type CompositeModelRoute struct {
	ID             int64     `json:"id"`
	GroupID        int64     `json:"group_id"`
	PublicModel    string    `json:"public_model"`
	MatchType      string    `json:"match_type"`
	TargetPlatform string    `json:"target_platform"`
	UpstreamModel  string    `json:"upstream_model"`
	Endpoint       string    `json:"endpoint"`
	Priority       int       `json:"priority"`
	Enabled        bool      `json:"enabled"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// FallbackTargets 主目标不可用时的后备平台列表（有序，先到先试）。
	FallbackTargets []CompositeRouteTarget `json:"fallback_targets,omitempty"`
}

// CompositeRouteTarget 一个 fallback 目标平台及其上游真实模型。
type CompositeRouteTarget struct {
	Platform      string `json:"platform"`
	UpstreamModel string `json:"upstream_model,omitempty"`
}

// CompositeRouteCandidate 组合路由解析出的一个候选平台（主目标或 fallback）。
type CompositeRouteCandidate struct {
	Platform      string
	UpstreamModel string
}

type CompositeRoutePreviewRequest struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
}

type CompositeRouteDecision struct {
	Matched        bool                 `json:"matched"`
	Source         string               `json:"source"`
	GroupID        int64                `json:"group_id"`
	PublicModel    string               `json:"public_model"`
	TargetPlatform string               `json:"target_platform"`
	UpstreamModel  string               `json:"upstream_model"`
	Endpoint       string               `json:"endpoint"`
	Route          *CompositeModelRoute `json:"route,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	// Candidates 有序候选平台（主目标在前，fallback 依次在后）；单目标时为长度 1。
	Candidates []CompositeRouteCandidate `json:"-"`
}

type CompositeRouteInput struct {
	PublicModel    string
	MatchType      string
	TargetPlatform string
	UpstreamModel  string
	Endpoint       string
	Priority       int
	Enabled        bool
	Notes          string
	FallbackTargets []CompositeRouteTarget
}

type CompositeModelRouteRepository interface {
	ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error)
	Create(ctx context.Context, route *CompositeModelRoute) error
	Update(ctx context.Context, route *CompositeModelRoute) error
	Delete(ctx context.Context, id int64) error
	DeleteByGroup(ctx context.Context, groupID int64) error
}

func normalizeCompositeRouteEndpoint(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if endpoint == "" {
		return CompositeRouteEndpointAny
	}
	switch endpoint {
	case CompositeRouteEndpointMessages,
		CompositeRouteEndpointCountTokens,
		CompositeRouteEndpointResponses,
		CompositeRouteEndpointChatCompletions,
		CompositeRouteEndpointEmbeddings,
		CompositeRouteEndpointImages,
		CompositeRouteEndpointGemini:
		return endpoint
	default:
		return CompositeRouteEndpointAny
	}
}

func normalizeCompositeRouteMatchType(matchType string) string {
	matchType = strings.ToLower(strings.TrimSpace(matchType))
	switch matchType {
	case CompositeRouteMatchPrefix:
		return CompositeRouteMatchPrefix
	default:
		return CompositeRouteMatchExact
	}
}

func normalizeCompositeRouteInput(input CompositeRouteInput) CompositeRouteInput {
	input.PublicModel = strings.TrimSpace(input.PublicModel)
	input.MatchType = normalizeCompositeRouteMatchType(input.MatchType)
	input.TargetPlatform = strings.TrimSpace(input.TargetPlatform)
	input.UpstreamModel = strings.TrimSpace(input.UpstreamModel)
	input.Endpoint = normalizeCompositeRouteEndpoint(input.Endpoint)
	// 仅对 exact 路由把空 upstream_model 回填成 public_model：exact 命中时请求模型
	// 恒等于 public_model，回填只影响持久化/后台展示，保留原有契约不变。
	// prefix 路由留空则不回填——Resolve 会回退到具体请求模型，从而透传原始模型
	// （否则 public=deepseek-v4 的前缀路由会把 deepseek-v4-flash / deepseek-v4-pro
	// 都塌缩成固定的 deepseek-v4）。显式填写 upstream_model 时任何模式都原样固定转发。
	if input.UpstreamModel == "" && input.MatchType == CompositeRouteMatchExact {
		input.UpstreamModel = input.PublicModel
	}
	// 规范化 fallback 目标平台和上游模型，并过滤空平台条目。
	fallbacks := make([]CompositeRouteTarget, 0, len(input.FallbackTargets))
	for _, t := range input.FallbackTargets {
		t.Platform = strings.TrimSpace(t.Platform)
		t.UpstreamModel = strings.TrimSpace(t.UpstreamModel)
		if t.Platform == "" {
			continue
		}
		fallbacks = append(fallbacks, t)
	}
	input.FallbackTargets = fallbacks
	input.Notes = strings.TrimSpace(input.Notes)
	return input
}
