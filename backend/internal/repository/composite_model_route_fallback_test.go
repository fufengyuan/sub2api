//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 行为：fallback 目标在 service 层与 ent jsonb 存储形态间往返不丢失。
// Source: plan/composite-multi-platform-fallback step 8
func TestCompositeRouteTargetsRoundtrip(t *testing.T) {
	targets := []service.CompositeRouteTarget{
		{Platform: service.PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: service.PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	}

	raw := compositeRouteTargetsToAny(targets)
	require.Len(t, raw, 2)
	require.Equal(t, "traework", raw[0]["platform"])
	require.Equal(t, "DeepSeek-V4-Flash", raw[0]["upstream_model"])

	back := compositeRouteTargetsFromAny(raw)
	require.Len(t, back, 2)
	require.Equal(t, targets, back)
}

// 行为：空 fallback 目标存储为 nil，读取回空切片（与旧行为一致）。
func TestCompositeRouteTargetsEmptyRoundtrip(t *testing.T) {
	raw := compositeRouteTargetsToAny(nil)
	require.Nil(t, raw)
	back := compositeRouteTargetsFromAny(raw)
	require.Nil(t, back)
}

// 行为：存储形态中 platform 为空的条目被过滤。
func TestCompositeRouteTargetsFromAnySkipsEmptyPlatform(t *testing.T) {
	back := compositeRouteTargetsFromAny([]map[string]any{
		{"platform": "", "upstream_model": "x"},
		{"platform": service.PlatformQoder, "upstream_model": "qoder-max"},
	})
	require.Len(t, back, 1)
	require.Equal(t, service.PlatformQoder, back[0].Platform)
}
