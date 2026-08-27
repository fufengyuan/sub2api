//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 回归：三渠道（workbuddy/traework/qoder）是具体平台。composite 组合分组
// 必须能够从这些平台的分组复制账号，否则「一 Key 多平台」无法落地。
func TestCanCopyAccountsFromCompositeGroup_CnPlatforms(t *testing.T) {
	for _, src := range []string{PlatformWorkBuddy, PlatformTraeWork, PlatformQoder} {
		require.Truef(t, canCopyAccountsFromGroupPlatform(PlatformComposite, src),
			"copying accounts from %s group into a composite group must be allowed", src)
	}
}

// 回归：组合路由的目标平台必须接受三渠道，否则模型路由无法指向三渠道转发。
func TestCompositeRouteFromInput_AcceptsCnTargetPlatform(t *testing.T) {
	for _, target := range []string{PlatformWorkBuddy, PlatformTraeWork, PlatformQoder} {
		route, err := compositeRouteFromInput(7, CompositeRouteInput{
			PublicModel:    "deepseek-v4-flash",
			MatchType:      CompositeRouteMatchExact,
			TargetPlatform: target,
			UpstreamModel:  "DeepSeek-V4-Flash",
			Endpoint:       CompositeRouteEndpointAny,
			Enabled:        true,
		})
		require.NoErrorf(t, err, "target_platform=%s", target)
		require.NotNil(t, route)
		require.Equal(t, target, route.TargetPlatform)
	}
}
