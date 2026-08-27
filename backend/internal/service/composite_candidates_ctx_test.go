//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 行为：CompositeCandidates 可写入并从 ctx 读回（有序）。
// Source: plan/composite-multi-platform-fallback step 4
func TestCompositeCandidatesContextRoundtrip(t *testing.T) {
	cands := []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	}
	ctx := WithCompositeCandidates(context.Background(), cands)

	got := CompositeCandidatesFromContext(ctx)
	require.Len(t, got, 2)
	require.Equal(t, cands, got)
}

// 行为：空候选列表不写入 ctx（读回 nil）。
func TestCompositeCandidatesContextEmpty(t *testing.T) {
	ctx := WithCompositeCandidates(context.Background(), nil)
	require.Nil(t, CompositeCandidatesFromContext(ctx))
}

// 行为：WithCompositeRouteDecision 用主目标写入单值字段，
// 候选列表也随 decision.Candidates 透传。
// Source: plan/composite-multi-platform-fallback step 7
func TestWithCompositeRouteDecisionSetsCandidates(t *testing.T) {
	decision := CompositeRouteDecision{
		Matched:        true,
		GroupID:        7,
		PublicModel:    "deepseek-v4-flash",
		TargetPlatform: PlatformTraeWork,
		UpstreamModel:  "DeepSeek-V4-Flash",
		Endpoint:       CompositeRouteEndpointAny,
		Candidates: []CompositeRouteCandidate{
			{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
			{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
		},
	}
	ctx := WithCompositeRouteDecision(context.Background(), decision)

	platform, ok := ResolvedTargetPlatformFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, PlatformTraeWork, platform)

	model, ok := ResolvedUpstreamModelFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "DeepSeek-V4-Flash", model)

	cands := CompositeCandidatesFromContext(ctx)
	require.Nil(t, cands, "WithCompositeRouteDecision 不写候选列表，候选由 middleware/调度显式写入")
}
