//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// composite 统一账号池（取消 composite 路由）调度测试。
//
// 覆盖设计 §验证：跨平台拉取、优先级分层、同层余额降序轮转、失败账号排除后重试、
// 以及「整池不可用 → ErrNoAvailableAccounts」。旧的「模型→主平台→fallback 候选」
// 路由用例（composite-multi-platform-fallback AC-2/AC-3）已随路由机制一并废弃。

func compositePoolAccount(id int64, platform string, priority int, credits int64) Account {
	creds := map[string]any{}
	if credits >= 0 {
		creds["creditsRemain"] = credits
	}
	return Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Priority:    priority,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 5,
		Credentials: creds,
	}
}

func indexAccounts(accounts []Account) map[int64]*Account {
	byID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		byID[accounts[i].ID] = &accounts[i]
	}
	return byID
}

// newCompositePoolSvc 构造 composite 分组 + 跨平台账号池的调度服务。
func newCompositePoolSvc(t *testing.T, groupID int64, accounts []Account) *GatewayService {
	t.Helper()
	repo := &mockAccountRepoForPlatform{
		accounts:     accounts,
		accountsByID: indexAccounts(accounts),
		byGroup:      accounts,
	}
	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformComposite, Status: StatusActive, Hydrated: true}},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	return &GatewayService{
		accountRepo:        repo,
		groupRepo:          groupRepo,
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
	}
}

// 统一池跨平台拉取：组内三个不同渠道的账号都在候选集里，平台不参与选号。
func TestSelectAccountWithLoadAwareness_CompositeUnifiedPoolCrossPlatform(t *testing.T) {
	groupID := int64(31)
	accounts := []Account{
		compositePoolAccount(1, PlatformWorkBuddy, 1, 100),
		compositePoolAccount(2, PlatformTraeWork, 1, 100),
		compositePoolAccount(3, PlatformQoder, 1, 100),
	}
	svc := newCompositePoolSvc(t, groupID, accounts)

	seen := map[int64]bool{}
	for i := 0; i < 3; i++ {
		result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "any-model", nil, "", int64(0))
		require.NoError(t, err, "统一池应能选中账号")
		require.NotNil(t, result.Account)
		seen[result.Account.ID] = true
		if result.ReleaseFunc != nil {
			result.ReleaseFunc()
		}
	}
	// 同层轮转：三次选号应覆盖池内三个不同账号（跨平台、不固定首个）。
	require.Len(t, seen, 3, "同层轮转应把流量摊到三个账号，got %v", seen)
}

// 优先级分层：高优先层（数值小）优先，即使其余额更低。
func TestSelectAccountWithLoadAwareness_CompositePrefersPriorityTier(t *testing.T) {
	groupID := int64(32)
	accounts := []Account{
		compositePoolAccount(1, PlatformWorkBuddy, 1, 10),
		compositePoolAccount(2, PlatformTraeWork, 5, 100000),
	}
	svc := newCompositePoolSvc(t, groupID, accounts)

	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "any-model", nil, "", int64(0))
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Account.ID, "优先级层先于余额")
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
}

// 同层余额降序：余额未知的账号排最后。
func TestSortCompositePoolCandidatesOrdersByCreditsDesc(t *testing.T) {
	low := compositePoolAccount(1, PlatformWorkBuddy, 1, 100)
	high := compositePoolAccount(2, PlatformTraeWork, 1, 900)
	unknown := compositePoolAccount(3, PlatformQoder, 1, -1)
	unknown.Credentials = nil

	ordered := sortCompositePoolCandidates([]*Account{&low, &unknown, &high})
	require.Equal(t, []int64{2, 1, 3}, []int64{ordered[0].ID, ordered[1].ID, ordered[2].ID})

	// 分层不被轮转打乱：轮转只发生在同层内部。
	mixed := []*Account{
		{ID: 9, Priority: 5},
		{ID: 7, Priority: 1},
		{ID: 8, Priority: 1},
	}
	rotated := rotateCompositePoolTiers(sortCompositePoolCandidates(mixed), 777)
	require.Equal(t, int64(9), rotated[2].ID, "低优先层账号不得被轮转到前面")
	require.ElementsMatch(t, []int64{7, 8}, []int64{rotated[0].ID, rotated[1].ID}, "高优先层内部才允许轮转")
}

// 失败账号由 handler 加入排除集后，重选同层下一个账号（失败重试本层下一个）。
func TestSelectAccountWithLoadAwareness_CompositeExcludesFailedAccounts(t *testing.T) {
	groupID := int64(33)
	accounts := []Account{
		compositePoolAccount(1, PlatformWorkBuddy, 1, 900),
		compositePoolAccount(2, PlatformTraeWork, 1, 800),
	}
	svc := newCompositePoolSvc(t, groupID, accounts)

	// 排除两次里余额较高的一个，剩下的必须被选中。
	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "any-model",
		map[int64]struct{}{1: {}}, "", int64(0))
	require.NoError(t, err)
	require.Equal(t, int64(2), result.Account.ID)
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}

	_, err = svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "any-model",
		map[int64]struct{}{1: {}, 2: {}}, "", int64(0))
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "全部候选被排除时报无可用账号")
}

// 组内无账号时直接 ErrNoAvailableAccounts（不再按候选平台逐个尝试）。
func TestSelectAccountWithLoadAwareness_CompositeNoAccounts(t *testing.T) {
	svc := newCompositePoolSvc(t, 34, []Account{})
	groupID := int64(34)
	_, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "any-model", nil, "", int64(0))
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
}

// 非 load-aware 的传统路径同样走统一池语义（legacy 路径）。
func TestSelectAccountForModelWithExclusions_CompositeUnifiedPool(t *testing.T) {
	groupID := int64(35)
	accounts := []Account{
		compositePoolAccount(1, PlatformWorkBuddy, 1, 100),
		compositePoolAccount(2, PlatformTraeWork, 1, 100),
	}
	repo := &mockAccountRepoForPlatform{
		accounts:     accounts,
		accountsByID: indexAccounts(accounts),
		byGroup:      accounts,
	}
	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformComposite, Status: StatusActive, Hydrated: true}},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   groupRepo,
		cache:       &mockGatewayCacheForPlatform{},
		cfg:         cfg,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "any-model", nil)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.True(t, acc.Platform == PlatformWorkBuddy || acc.Platform == PlatformTraeWork,
		"跨平台池内任一账号都可被选中，got %s", acc.Platform)
}

// 统一池不再读取 composite 路由：即使 ctx 里带着旧候选平台列表，也只按选中账号
// 的平台语义处理（此处表现为不再改写请求模型名，账号候选来自组内全平台）。
func TestSelectAccountWithLoadAwareness_CompositeIgnoresRouteCandidates(t *testing.T) {
	groupID := int64(36)
	accounts := []Account{
		compositePoolAccount(1, PlatformWorkBuddy, 1, 100),
	}
	svc := newCompositePoolSvc(t, groupID, accounts)
	ctx := WithCompositeCandidates(context.Background(), []CompositeRouteCandidate{
		{Platform: PlatformTraeWork, UpstreamModel: "DeepSeek-V4-Flash"},
		{Platform: PlatformWorkBuddy, UpstreamModel: "deepseek-v4-flash"},
	})

	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "any-model", nil, "", int64(0))
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Account.ID)
	require.Equal(t, PlatformWorkBuddy, result.Account.Platform)
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
}
