//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 行为：composite 多平台候选时，首选平台无健康账号则 fallback 到下一平台。
// Source: plan/composite-multi-platform-fallback step 6 (AC-3)
func TestSelectAccountWithLoadAwareness_CompositeFallbackToSecondPlatform(t *testing.T) {
	ctx := context.Background()
	// traework 无账号，workbuddy 有账号 → 应选 workbuddy
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformWorkBuddy, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Credentials: map[string]any{"creditsRemain": int64(100)}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true

	concurrencyCache := &mockConcurrencyCache{}
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	// 预置候选：主 traework（无账号）、fallback workbuddy（有账号）
	routeCtx := WithCompositeCandidates(ctx, []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	})

	result, err := svc.SelectAccountWithLoadAwareness(routeCtx, nil, "", "deepseek-v4-flash", nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, PlatformWorkBuddy, result.Account.Platform)
	require.Equal(t, int64(1), result.Account.ID)
}

// 行为：composite 多平台候选按余额降序选择（余额高的平台优先）。
// Source: plan/composite-multi-platform-fallback step 6 (AC-2)
func TestSelectAccountWithLoadAwareness_CompositePrefersHigherCredits(t *testing.T) {
	ctx := context.Background()
	// 两个平台都有账号；traework 余额低、workbuddy 余额高 → 应选 workbuddy
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformWorkBuddy, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Credentials: map[string]any{"creditsRemain": int64(900)}},
			{ID: 2, Platform: PlatformTraeWork, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Credentials: map[string]any{"creditsRemain": int64(100)}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true

	concurrencyCache := &mockConcurrencyCache{}
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	// 候选顺序：主 traework（低余额）在前，fallback workbuddy（高余额）在后
	routeCtx := WithCompositeCandidates(ctx, []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	})

	result, err := svc.SelectAccountWithLoadAwareness(routeCtx, nil, "", "deepseek-v4-flash", nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	require.Equal(t, PlatformWorkBuddy, result.Account.Platform, "余额高的平台应优先")
	require.Equal(t, int64(1), result.Account.ID)
}

// 行为：非 load-aware 路径 SelectAccountForModelWithExclusions 在多平台候选时
// 也按余额选平台并 fallback（与 load-aware 一致）。
// Source: plan/composite-multi-platform-fallback step 6 (AC-3)
func TestSelectAccountForModelWithExclusions_CompositeFallback(t *testing.T) {
	ctx := context.Background()
	// traework 无账号，workbuddy 有账号 → 应选 workbuddy
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformWorkBuddy, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Credentials: map[string]any{"creditsRemain": int64(100)}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{1: {ID: 1, Platform: PlatformComposite}},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   groupRepo,
		cache:       &mockGatewayCacheForPlatform{},
		cfg:         cfg,
	}

	groupID := int64(1)
	routeCtx := WithCompositeCandidates(ctx, []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	})

	acc, err := svc.SelectAccountForModelWithExclusions(routeCtx, &groupID, "", "deepseek-v4-flash", nil)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, PlatformWorkBuddy, acc.Platform)
	require.Equal(t, int64(1), acc.ID)
}

// 行为：composite 多平台候选全部无账号时返回 ErrNoAvailableAccounts。
// Source: plan/composite-multi-platform-fallback step 6 (AC-4)
func TestSelectAccountWithLoadAwareness_CompositeAllCandidatesNoAccounts(t *testing.T) {
	ctx := context.Background()
	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{},
		accountsByID: map[int64]*Account{},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
	}

	routeCtx := WithCompositeCandidates(ctx, []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	})

	_, err := svc.SelectAccountWithLoadAwareness(routeCtx, nil, "", "deepseek-v4-flash", nil, "", int64(0))
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
}
