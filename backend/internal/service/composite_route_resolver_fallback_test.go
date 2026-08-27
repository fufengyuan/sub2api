//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 行为：命中带 fallback 的组合路由时，Resolve 返回有序候选列表
// （主目标在前，fallback 依次在后），单值字段仍指向主目标以兼容旧消费方。
// Source: plan/composite-multi-platform-fallback step 3
func TestCompositeRouteResolverExplicitRouteReturnsCandidates(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             10,
				GroupID:        7,
				PublicModel:    "deepseek-v4-flash",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformTraeWork,
				UpstreamModel:  "DeepSeek-V4-Flash",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
				FallbackTargets: []CompositeRouteTarget{
					{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
				},
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "deepseek-v4-flash", CompositeRouteEndpointAny)
	require.NoError(t, err)
	require.True(t, decision.Matched)

	// 候选列表：主目标 traework 在前，fallback workbuddy 在后
	require.Len(t, decision.Candidates, 2)
	require.Equal(t, PlatformTraeWork, decision.Candidates[0].Platform)
	require.Equal(t, "DeepSeek-V4-Flash", decision.Candidates[0].UpstreamModel)
	require.Equal(t, PlatformWorkBuddy, decision.Candidates[1].Platform)
	require.Equal(t, "deepseek-v4-flash", decision.Candidates[1].UpstreamModel)

	// 单值字段兼容旧消费方，指向主目标
	require.Equal(t, PlatformTraeWork, decision.TargetPlatform)
	require.Equal(t, "DeepSeek-V4-Flash", decision.UpstreamModel)
}

// 行为：无 fallback 的路由，Candidates 长度为 1，仅主目标。
// Source: plan/composite-multi-platform-fallback AC-5
func TestCompositeRouteResolverSingleTargetCandidatesLengthOne(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             11,
				GroupID:        7,
				PublicModel:    "glm-5.3",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformTraeWork,
				UpstreamModel:  "glm-5.3",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "glm-5.3", CompositeRouteEndpointAny)
	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Len(t, decision.Candidates, 1)
	require.Equal(t, PlatformTraeWork, decision.Candidates[0].Platform)
	require.Equal(t, "glm-5.3", decision.Candidates[0].UpstreamModel)
}

// 行为：compositeRouteFromInput 解析 fallback_targets 并校验平台合法。
// Source: plan/composite-multi-platform-fallback step 2
func TestCompositeRouteFromInputParsesFallbackTargets(t *testing.T) {
	route, err := compositeRouteFromInput(7, CompositeRouteInput{
		PublicModel:    "deepseek-v4-flash",
		MatchType:      CompositeRouteMatchExact,
		TargetPlatform: PlatformTraeWork,
		UpstreamModel:  "DeepSeek-V4-Flash",
		Endpoint:       CompositeRouteEndpointAny,
		Enabled:        true,
		FallbackTargets: []CompositeRouteTarget{
			{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, route)
	require.Len(t, route.FallbackTargets, 1)
	require.Equal(t, PlatformWorkBuddy, route.FallbackTargets[0].Platform)
}

// 行为：fallback 平台必须是具体平台（三渠道等），否则报错。
// Source: plan/composite-multi-platform-fallback step 2
func TestCompositeRouteFromInputRejectsInvalidFallbackPlatform(t *testing.T) {
	_, err := compositeRouteFromInput(7, CompositeRouteInput{
		PublicModel:    "deepseek-v4-flash",
		MatchType:      CompositeRouteMatchExact,
		TargetPlatform: PlatformTraeWork,
		Endpoint:       CompositeRouteEndpointAny,
		Enabled:        true,
		FallbackTargets: []CompositeRouteTarget{
			{Platform: PlatformComposite, UpstreamModel: "x"},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "concrete provider")
}
