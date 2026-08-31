//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cnFilterRepoStub 实现 ListAllWithFilters 与 GetByID/Update，供按筛选条件的
// 汇总/一键刷新测试使用。
type cnFilterRepoStub struct {
	*fakeCnRepo
}

func (f *cnFilterRepoStub) ListAllWithFilters(_ context.Context, platform, accountType, status, search string, _ int64, _ string) ([]Account, error) {
	var out []Account
	for _, a := range f.accounts {
		if platform != "" && a.Platform != platform {
			continue
		}
		if accountType != "" && a.Type != accountType {
			continue
		}
		if search != "" && !strings.Contains(a.Name, search) {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

// 后端汇总接口：按筛选条件对满足条件的国产渠道账号积分求和（全量口径，跨页）。
func TestCnUpstreamSummarizeCreditsByFilter(t *testing.T) {
	repo := &cnFilterRepoStub{fakeCnRepo: &fakeCnRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformWorkBuddy, Name: "wb-a", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(100)}},
		2: {ID: 2, Platform: PlatformWorkBuddy, Name: "wb-b", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(250)}},
		3: {ID: 3, Platform: PlatformTraeWork, Name: "tw-a", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(50)}},
		4: {ID: 4, Platform: "openai", Name: "oai", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(999)}},
		5: {ID: 5, Platform: PlatformQoder, Name: "q-a", Type: AccountTypeOAuth, Credentials: map[string]any{}}, // 无积分
	}}}
	svc := NewCnUpstreamService(repo, nil)

	// 全部国产渠道：100+250+50 = 400，counted=3（qoder 无积分不计入）。
	total, counted, err := svc.SummarizeCreditsByFilter(context.Background(), CnCreditsFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(400), total)
	require.Equal(t, int64(3), counted)

	// 限定 workbuddy：100+250 = 350。
	total, counted, err = svc.SummarizeCreditsByFilter(context.Background(), CnCreditsFilter{Platform: PlatformWorkBuddy})
	require.NoError(t, err)
	require.Equal(t, int64(350), total)
	require.Equal(t, int64(2), counted)

	// 搜索 name=wb-b：仅账号2，250。
	total, counted, err = svc.SummarizeCreditsByFilter(context.Background(), CnCreditsFilter{Search: "wb-b"})
	require.NoError(t, err)
	require.Equal(t, int64(250), total)
	require.Equal(t, int64(1), counted)
}

// 一键刷新：按筛选条件逐个刷新，成功/失败计数，单账号失败不中断。
func TestCnUpstreamRefreshCreditsByFilter(t *testing.T) {
	up := &fakeCnUpstream{
		remain:    1000,
		remainErr: nil,
	}
	repo := &cnFilterRepoStub{fakeCnRepo: &fakeCnRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformWorkBuddy, Name: "wb-a", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(10)}},
		2: {ID: 2, Platform: PlatformWorkBuddy, Name: "wb-b", Type: AccountTypeOAuth, Credentials: map[string]any{"creditsRemain": float64(20)}},
	}}}
	svc := NewCnUpstreamService(repo, nil)
	for platform := range svc.platforms {
		svc.platforms[platform].upstream = up
	}

	success, failed, err := svc.RefreshCreditsByFilter(context.Background(), CnCreditsFilter{Platform: PlatformWorkBuddy})
	require.NoError(t, err)
	require.Equal(t, 2, success)
	require.Equal(t, 0, failed)
}
