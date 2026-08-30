# Bug: composite 统一账号池新加平台账号 503「No available accounts」

> Status: FIXED
> Mode: default
> Severity: blocker（生产网关 503）
> Author: sub2api
> Last updated: 2026-08-30

## Symptom
composite 分组（国内模型）新增 qwenwork 账号后，用 qwenwork 特有模型 `qwen3.8-flash` 调
`/v1/chat/completions` 报 503「No available accounts」。同组 workbuddy 的 `glm-5.3-flash` 正常。

## Expected
composite 统一账号池应公平轮询各平台账号；请求 qwenwork 特有模型时，应命中支持该模型的 qwenwork 账号。

## Reproduction
- 生产库(203.56.185.41)里 qwenwork 账号(id=33,34,36,37) `active + schedulable=true`，`model_mapping` 含 `qwen3.8-flash`，关联 composite 组 5「国内模型」，调度状态干净。
- 复现测试：`scheduler_composite_bucket_rebuild_test.go:TestRebuildByAccountRebuildsCompositeBucket`
  - 断言 `rebuildByAccount` 重建的桶集合**应含** `SchedulerBucket{5, composite, composite}`。
  - 修复前该测试 FAIL（只重建 `5:qwenwork:single`/`5:qwenwork:forced`，无 composite 桶）→ 3/3 reliably fails。

## Hypotheses & diagnosis
| # | Hypothesis | Verdict | Evidence |
|---|---|---|---|
| H1 | qwenwork 账号没落库/没关联组 | eliminated | 生产库查证：5 个 qwenwork 账号 active+sched=true+model_mapping 含 qwen3.8-flash+关联组5 |
| H2 | qwenwork 被模型支持/利润门/调度状态过滤 | eliminated | `IsModelSupported` 空映射放行所有；`profit_control_enabled=false`；无 temp_unsched/overload/rate_limit/expires |
| H3 | **composite 桶（SchedulerModeComposite）不在账号变更重建路径，旧缓存缺新账号** | **confirmed (root cause)** | `rebuildByAccount`/`rebuildByGroupIDs`/`schedulerCanonicalBuckets` 均不含 composite 桶；composite 桶首次 miss 时写入，之后命中缓存永不刷新；复现测试证实 |

## Root cause
composite 统一账号池桶（`SchedulerBucket{GroupID, PlatformComposite, SchedulerModeComposite}`）**不在任何调度快照重建集合里**：
- `schedulerCanonicalBuckets` / `bucketsForPlatform` / `rebuildByAccount` / `rebuildByGroupIDs` 都只遍历 8 个常规平台桶。
- composite 桶内容只在 `ListSchedulableAccounts` 首次 miss 时经 `loadAccountsFromDB`（DB 直查全平台账号）+ `SetSnapshot` 写入。
- 一旦写入，`GetSnapshot` 命中缓存即返回，**无自动过期、不被账号增删刷新**。

因此：composite 组先有 workbuddy（首次请求写入 composite 桶），后加 qwenwork（触发
`SchedulerOutboxEventAccountChanged` → `rebuildByAccount` 只重建常规平台桶，**composite 桶不刷新**），
composite 桶仍停留在不含 qwenwork 的旧快照 → 候选无 qwenwork → `selectCompositePoolAccount` 候选为空 → 503。

workbuddy 能通，正因为它在旧快照里。

## Fix
- 改动文件：`backend/internal/service/scheduler_snapshot_service.go`
  - `rebuildByAccount`：账号变更时追加 `compositeBucketsForGroups(groupIDs)`。
  - `rebuildByGroupIDs`：追加 `compositeBucketsForGroups(groupIDs)`。
  - 新增 `compositeBucketsForGroups`：为每个分组构造 composite 桶。
- 修复原理：账号归属的 composite 组在账号增删时一并重建 composite 桶，使其缓存随真实账号集合刷新。
- 配套：`scheduler_snapshot_bulk_event_test.go` 走 `rebuildByGroupIDs` 的两个测试断言补 composite 桶，
  给 `bulkEventAccountRepo` 补 `ListSchedulableByGroupID`/`ListSchedulable`（composite 桶重建需要）。

## Verification
- V-1: `go test -tags=unit ./internal/service/ -run TestRebuildByAccountRebuildsCompositeBucket` → GREEN ✓
- V-2: `git stash` 修复后同测试 → RED ✓（证明测试真捕获 bug）；`git stash pop` 后 → GREEN ✓
- V-3: `go test -tags=unit -count=1 ./internal/service/` → 全部 GREEN ✓
- V-4: `go build ./...` + `go vet ./internal/service/` → OK ✓

## Regression test
- 路径：`backend/internal/service/scheduler_composite_bucket_rebuild_test.go:15`
- 名称：`TestRebuildByAccountRebuildsCompositeBucket`
- 覆盖：账号变更（rebuildByAccount）必须重建 composite 统一账号池桶。

## Pattern analysis
同类隐患：composite 桶不在 `schedulerCanonicalBuckets`（full rebuild / group lifecycle）里。本次
聚焦账号增删（用户场景）；full rebuild 与 group lifecycle 对 composite 桶的覆盖可作后续独立跟进。

## RCA
- 引入：composite 统一账号池落地（`e061b9225`），`SchedulerModeComposite` 仅做懒加载，漏了重建路径。
- 为什么没被发现：composite 桶懒加载 + 无 TTL，单测只覆盖轻量函数，未覆盖「写入后账号变更」的缓存刷新。
- 预防措施：回归测试 `TestRebuildByAccountRebuildsCompositeBucket` 锁定「账号变更必须重建 composite 桶」。
