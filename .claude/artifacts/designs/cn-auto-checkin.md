# 三渠道自动签到闭环（cn-auto-checkin）

日期：2026-08-27
状态：定稿

## 背景

`checkin.enabled` 开启后调度框架已自启（`ProvideCnUpstreamSchedulerService` → `Start` →
`scheduler.Run` 定时循环），但链路存在断点，自动签到实际不可用/结果不可见：

| # | 断点 | 位置 | 后果 |
|---|------|------|------|
| 1 | token 刷新落库断裂 | `scheduler.refreshForCheckin`/`RunKeepaliveNow` 调 `a.SaveAtomic()` 写本地文件；sub2api `hydrateAuth` 构造的 auth 无 FilePath，必然报错 | token 快过期时签到直接失败；keepalive 刷新的新 token 只在内存，重启即丢 |
| 2 | 签到积分不落库 | `scheduler.checkinOne` 余额只更新内存池（`Pool.ReenableIfCredits`） | 管理页面积分（ent `creditsRemain`）不更新；composite fallback 按余额选平台读不到新值 |
| 3 | 新账号不进池 | `CnUpstreamSchedulerService.Start` 仅启动时 `ReloadAccounts` 一次 | 之后管理端新增账号，定时签到签不到 |
| 4 | `CheckinConfig.BackoffSeconds` 定义未使用 | `parseCheckinMinutes` 只用 times/keepalive | 批量签到无退避 |

## 需求（已确认）

1. 自动签到后：最新余额写入账号 `credentials.creditsRemain`（管理页面积分列自然更新）。
2. 签到状态展示：把上次签到时间/结果写入 credentials，账号列表积分列展示。
3. 每次批量签到/keepalive 前从 ent reload 账号池，新号当天纳入。
4. qoder 无签到活动，保持跳过；workbuddy/traework 参与签到+保活。

## 方案

### 1. scheduler 包钩子化（backend/internal/cnupstream/scheduler/scheduler.go）

`Config` 新增可选钩子（全部 nil 时保持原 workbuddy-wild 文件行为，向后兼容）：

```go
PersistAuth   func(a *auth.Auth) error // token 刷新成功后调用；nil 时退回 a.SaveAtomic()
Reload        func()                   // RunCheckinNow/RunKeepaliveNow 批量前调用
CheckinBackoff time.Duration           // 批量签到账号间退避；0 不退避
```

- `refreshForCheckin` / `RunKeepaliveNow`：`RefreshToken` 成功后优先调 `PersistAuth`，
  无钩子时退回 `SaveAtomic()`（原行为）。
- `RunCheckinNow` / `RunKeepaliveNow`：批量开始前调 `Reload()`（忽略错误，日志）。
- `RunCheckinNow`：每账号签到完 sleep `CheckinBackoff`。

### 2. CnUpstreamService 新增持久化方法（backend/internal/service/cnupstream_service.go）

- `PersistAuth(platform string, a *auth.Auth) error`
  auth → credentials（复用 `credsFromAuth`）经 `acctRefs` 回写 ent；
  `RefreshToken` 改为复用该方法（消重）。
- `PersistCheckinResult(platform string, r CheckinResultLike)`（以 scheduler.CheckinResult 入参）
  写 `credentials.lastCheckinAt`（RFC3339 本地时区）、`lastCheckinResult`（"ok"/错误消息/"已签到"）；
  `HasRemain` 时同步 `creditsRemain` 并 `Pool.SetCredits` + `ReenableIfCredits`。
  uid → `acctRefs` 取 *Account，取不到按 uid 解析 ID 重建（对齐 RefreshToken 兜底）。

### 3. 调度器服务注入钩子（backend/internal/service/cnupstream_scheduler_service.go）

`Start` 构造 scheduler.Config 时：
- `PersistAuth`: 调 `s.cn.PersistAuth(platform, a)`
- `Reload`: 调 `s.cn.ReloadAccounts(ctx)`（忽略错误，日志）
- `CheckinBackoff`: `cfg.Checkin.BackoffSeconds` 秒
- `SetCheckinObserver`: 按平台闭包调 `s.cn.PersistCheckinResult(platform, r)`

### 4. 手动签到对齐（backend/internal/service/cnupstream_service.go CheckinNow）

手动签到成功/失败同样写 `lastCheckinAt`/`lastCheckinResult`，与自动签到口径一致
（前端同一展示位）。

### 5. 前端展示（frontend/src/views/admin/AccountsView.vue）

积分列（`#cell-credits`）三渠道账号：积分数字下方加一行小字
`上次签到 MM-DD HH:mm · 成功/失败`；失败红色。读
`credentials.lastCheckinAt` / `credentials.lastCheckinResult`。
i18n：zh/en/ja `admin.accounts.cnCredits.lastCheckin*`。

## 不做

- 不改签到时间解析语义（`times` HH:MM 本地时区，维持现状）
- 不做签到历史明细表（只保留最近一次状态）
- 不动 qoder（无签到活动）
- 不改 state.json 文件机制（内存池冷却/禁用仍走原路径）

## 验收标准

1. `checkin.enabled=true` + `times` 到点：每个账号签到→积分落库 ent
   `creditsRemain`，账号列表刷新可见。
2. token 刷新（签到前补刷/晚间保活）成功后 ent credentials 更新，重启不丢。
3. 管理端新增账号后无需重启，下个签到周期自动纳入。
4. 手动签到按钮同样更新 `lastCheckinAt/lastCheckinResult`，与自动签到展示一致。
5. 单测覆盖：钩子注入路径（PersistAuth 优先于 SaveAtomic）、Reload 触发、
   Backoff 生效、PersistCheckinResult 落库字段、CheckinNow 写 lastCheckin。
6. `go build ./...` + 相关单测 + 前端 typecheck 全绿。
