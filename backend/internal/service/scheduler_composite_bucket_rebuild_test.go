//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuildByAccountRebuildsCompositeBucket 回归测试：composite 统一账号池的
// 桶（SchedulerModeComposite）必须随分组内账号变更一起重建，否则新加入平台的
// 账号（如 qwenwork）不会被识别，导致「No available accounts」503。
//
// 根因：composite 桶不在 schedulerCanonicalBuckets / bucketsForPlatform /
// rebuildByAccount 的任何重建集合里。一旦 composite 桶被首次请求写入缓存，
// 之后账号增删（触发 SchedulerOutboxEventAccountChanged → rebuildByAccount）
// 不会刷新它，缓存永远停留在不含新账号的旧快照。
func TestRebuildByAccountRebuildsCompositeBucket(t *testing.T) {
	cache := newBatchSnapshotCache()
	repo := &mockAccountRepoForPlatform{}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)

	const groupID = int64(5)
	acct := &Account{ID: 37, Platform: PlatformQwenWork, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	if err := svc.rebuildByAccount(context.Background(), acct, []int64{groupID}, "account_change", nil); err != nil {
		t.Fatalf("rebuildByAccount failed: %v", err)
	}

	compositeBucket := SchedulerBucket{GroupID: groupID, Platform: PlatformComposite, Mode: SchedulerModeComposite}
	require.Contains(t, cache.captures, compositeBucket,
		"账号变更必须重建 composite 统一账号池桶，否则新平台账号不会被调度识别")
}
