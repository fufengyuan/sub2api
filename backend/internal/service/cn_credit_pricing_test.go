//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/provider"
	"github.com/stretchr/testify/require"
)

var errCnPricingDown = errors.New("pricing api 503")

func cnTestAccountForPricing(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Name:        "t-" + platform,
		Credentials: map[string]any{"accessToken": "at"},
	}
}

// TestCnCreditPricingDisabled 折算未开启时（默认）不得介入计费：
// 必须返回 false 让调用方继续走零成本 + WARN 的既有路径，避免误扣。
func TestCnCreditPricingDisabled(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{1: cnTestAccountForPricing(1, PlatformQoder)}}
	up := &fakeCnUpstream{pricing: []provider.ModelPricing{{Model: "qwen3.8-max", Rate: 1.5}}}
	cnSvc := newCnUpstreamTestSvc(repo, up)
	svc := &GatewayService{cnUpstream: cnSvc, cfg: &config.Config{}}

	_, ok := svc.tryCnCreditCost(context.Background(),
		cnTestAccountForPricing(1, PlatformQoder), "qwen3.8-max", 1.0)
	require.False(t, ok, "默认关闭时不得启用积分折算")
}

// TestCnCreditPricingEnabledUsesUpstreamRate 命中上游倍率表时按 Rate × CreditToUSD 计费。
func TestCnCreditPricingEnabledUsesUpstreamRate(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{1: cnTestAccountForPricing(1, PlatformQoder)}}
	up := &fakeCnUpstream{pricing: []provider.ModelPricing{
		{Model: "qwen3.8-max", Rate: 1.5},
		{Model: "glm-5.3", Rate: 0.3},
	}}
	cnSvc := newCnUpstreamTestSvc(repo, up)
	svc := &GatewayService{
		cnUpstream: cnSvc,
		cfg:        &config.Config{CnCreditPricing: config.CnCreditPricingConfig{Enabled: true, CreditToUSD: 0.002}},
	}

	got, ok := svc.tryCnCreditCost(context.Background(),
		cnTestAccountForPricing(1, PlatformQoder), "qwen3.8-max", 1.0)
	require.True(t, ok)
	require.InDelta(t, 1.5*0.002, got.ActualCost, 1e-9, "成本 = Rate × CreditToUSD")
}

// TestCnCreditPricingUnlistedModelUsesDefaultRate 未收录模型按 1x 保守折算：
// 既不让用户白嫖（非零成本），也不猜测高价（不误扣）。qwen3.8-flash 即此类。
func TestCnCreditPricingUnlistedModelUsesDefaultRate(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{1: cnTestAccountForPricing(1, PlatformQwenWork)}}
	up := &fakeCnUpstream{pricing: []provider.ModelPricing{{Model: "qwen3.8-max", Rate: 1.5}}}
	cnSvc := newCnUpstreamTestSvc(repo, up)
	svc := &GatewayService{
		cnUpstream: cnSvc,
		cfg:        &config.Config{CnCreditPricing: config.CnCreditPricingConfig{Enabled: true, CreditToUSD: 0.002}},
	}

	got, ok := svc.tryCnCreditCost(context.Background(),
		cnTestAccountForPricing(1, PlatformQwenWork), "qwen3.8-flash", 1.0)
	require.True(t, ok, "未收录模型仍应计费（1x 保守折算）")
	require.InDelta(t, cnCreditPricingDefaultRate*0.002, got.ActualCost, 1e-9)
}

// TestCnCreditPricingNonCnPlatformInapplicable 非国产渠道一律不折算，
// 保证既有平台计费口径不受本次改动影响。
func TestCnCreditPricingNonCnPlatformInapplicable(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{1: cnTestAccountForPricing(1, PlatformAnthropic)}}
	cnSvc := newCnUpstreamTestSvc(repo, &fakeCnUpstream{pricing: []provider.ModelPricing{{Model: "claude-sonnet-4", Rate: 9}}})
	svc := &GatewayService{
		cnUpstream: cnSvc,
		cfg:        &config.Config{CnCreditPricing: config.CnCreditPricingConfig{Enabled: true, CreditToUSD: 0.002}},
	}

	_, ok := svc.tryCnCreditCost(context.Background(),
		cnTestAccountForPricing(1, PlatformAnthropic), "claude-sonnet-4", 1.0)
	require.False(t, ok, "非国产渠道不得走积分折算")
}

// TestCnCreditPricingUpstreamFailureFallsBack 上游倍率接口失败时按 1x 折算，
// 不阻断计费链路（不返回 false，否则会退回零成本白嫖）。
func TestCnCreditPricingUpstreamFailureFallsBack(t *testing.T) {
	repo := &fakeCnRepo{accounts: map[int64]*Account{1: cnTestAccountForPricing(1, PlatformTraeWork)}}
	up := &fakeCnUpstream{pricingErr: errCnPricingDown}
	cnSvc := newCnUpstreamTestSvc(repo, up)
	svc := &GatewayService{
		cnUpstream: cnSvc,
		cfg:        &config.Config{CnCreditPricing: config.CnCreditPricingConfig{Enabled: true, CreditToUSD: 0.002}},
	}

	got, ok := svc.tryCnCreditCost(context.Background(),
		cnTestAccountForPricing(1, PlatformTraeWork), "some-model", 1.0)
	require.True(t, ok, "上游失败应降级为 1x 折算而非零成本")
	require.InDelta(t, 0.002, got.ActualCost, 1e-9)
}

// TestCnCreditCostMultiplier 倍率系数（分组/账号 RateMultiplier）参与计算；
// 非正值时按 1x 兜底，避免把成本乘成 0 而漏扣。
func TestCnCreditCostMultiplier(t *testing.T) {
	require.InDelta(t, 1.5*0.002*2, cnCreditCost(1.5, 0.002, 2), 1e-9)
	require.InDelta(t, 1.5*0.002, cnCreditCost(1.5, 0.002, 0), 1e-9, "倍率 0 应兜底为 1x")
	require.InDelta(t, 0.0, cnCreditCost(0, 0.002, 1), 1e-9, "Rate<=0 不计费")
	require.InDelta(t, 0.0, cnCreditCost(1.5, 0, 1), 1e-9, "换算系数为 0 不计费")
}

// TestCnCreditRateCacheKey 缓存键按平台+账号隔离：同平台不同账号可能因套餐
// 折扣返回不同倍率，只按平台缓存会串号。
func TestCnCreditRateCacheKey(t *testing.T) {
	require.Equal(t, "qoder|1", cnCreditRateCacheKey(PlatformQoder, 1))
	require.NotEqual(t, cnCreditRateCacheKey(PlatformQoder, 1), cnCreditRateCacheKey(PlatformQoder, 2))
	require.NotEqual(t, cnCreditRateCacheKey(PlatformQoder, 1), cnCreditRateCacheKey(PlatformQwenWork, 1))
}
