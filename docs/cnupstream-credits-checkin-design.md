# 三渠道积分余额/签到/模型映射设计

> 范围：workbuddy / traework / qoder 三渠道的账号级积分余额查询、积分明细、手动签到、自动签到开启、账号级模型映射配置。
> 上游客户端能力（`UserResource` / `UserResourceDetail` / `DailyCheckin`）在 `backend/internal/cnupstream/` 已完整移植，本次只做 service/handler/前端接线。

## 1. 需求结论（已与用户确认）

| 需求 | 结论 |
|------|------|
| 积分余额 | 账号列表三渠道行内显示积分余额（刷新时写入账号 credentials），列表可点「刷新积分」 |
| 积分明细 | 弹窗展示各套餐条目（套餐名/额度/剩余），调上游 `UserResourceDetail` |
| 手动签到 | 账号列表加「立即签到」按钮（单账号），签到成功后自动刷新余额；qoder 无签到活动（按钮置灰/提示） |
| 自动签到 | config.yaml 开启 `checkin.enabled`（times 09:00/21:00、keepalive_hours 22），复用现有 `CnUpstreamSchedulerService` |
| 模型映射 | **账号级配置**：账号编辑弹窗加「模型映射」区（请求模型→上游真实模型），提供「拉取上游模型列表」辅助（新端点 `GET /admin/accounts/:id/upstream-models`，因现有 `/models` 返回账号已配置模型而非上游真实列表） |

## 2. 后端设计

### 2.1 CnUpstreamService 新增方法（`cnupstream_service.go`）

```go
// RefreshCredits 刷新单账号积分余额：调 provider.UserResource，写入账号
// credentials["creditsRemain"]（int64）并同步 pool.SetCredits，返回余额。
func (s *CnUpstreamService) RefreshCredits(ctx context.Context, accountID int64) (int64, error)

// ResourceDetail 查询积分明细（不落库）：调 provider.UserResourceDetail，
// 返回 CnCreditsDetail{credits_remain, items: [{name, total, used, remain, expiry}]}。
func (s *CnUpstreamService) ResourceDetail(ctx context.Context, accountID int64) (*CnCreditsDetail, error)

// CheckinNow 单账号立即签到：调 provider.DailyCheckin（已签到按业务错误返回），
// 成功后刷新余额（ReenableIfCredits + RecordCheckin 语义对齐原项目 checkinOne）。
func (s *CnUpstreamService) CheckinNow(ctx context.Context, accountID int64) (*CheckinResult, error)
```

- 账号定位：按 accountID 从 ent 账号库取（复用 `ReloadAccounts`/现有账号仓储），校验 platform ∈ {workbuddy, traework, qoder}，`hydrateAuth` 构造 `auth.Auth`。
- 三渠道统一走 `provider.Upstream` 接口（`UserResource`/`UserResourceDetail`/`DailyCheckin`），qoder 的 `DailyCheckin` 返回「无签到活动」业务错误，handler 透传给前端提示。
- 余额落库：写入 credentials 驼峰键 `creditsRemain`（与现有凭证键风格一致），账号列表响应原样带出。

### 2.2 Admin API（新增路由，挂 `internal/server/routes/admin.go` accounts 组）

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/admin/accounts/:id/credits/refresh` | 刷新积分余额，返回 `{credits_remain}` |
| GET  | `/admin/accounts/:id/credits/detail` | 积分明细，返回 `{credits_remain, items: [{name, limit, used, remain}]}` |
| POST | `/admin/accounts/:id/checkin` | 立即签到，返回签到结果 + 最新余额 |
| GET  | `/admin/accounts/:id/upstream-models` | 拉取上游真实模型列表（模型映射配置辅助） |

- 仅三渠道平台账号可用，其他平台返回 400（`CN_ACCOUNT_UNSUPPORTED_PLATFORM`）。
- 响应结构对齐项目 `response` 包规范。

### 2.3 自动签到开启（配置，不改代码）

`config.yaml` 追加（`CnUpstreamSchedulerService.Start` 已实现）：

```yaml
checkin:
  enabled: true
  times: ["09:00", "21:00"]
  keepalive_hours: [22]
  backoff_seconds: 30
```

### 2.4 模型映射（后端零改动）

账号级映射读的是 `credentials["model_mapping"]`（`Account.GetModelMapping`，转发时 `GetMappedModel` 已生效）。现有账号 Create/Update API 已支持写 credentials 任意字段，后端无需改动，只做前端 UI。

## 3. 前端设计

### 3.1 账号列表（AccountsView）

- 三渠道平台行新增「积分」列：显示 `credentials.creditsRemain`（无值显示 `-`），标题提示「点击刷新」。
- 行操作新增（仅三渠道平台显示）：
  - **刷新积分**：调 refresh API，成功后更新行内余额。
  - **积分明细**：弹窗表格（套餐名/额度/已用/剩余），打开时调 detail API。
  - **立即签到**：调 checkin API；qoder 显示按钮但点击提示「无签到活动」；成功后刷新余额列。

### 3.2 账号编辑弹窗（EditAccountModal / CreateAccountModal）

- 三渠道平台时显示「模型映射」编辑区：动态 key-value 行（左侧=对外请求模型名，右侧=上游真实模型名）。
- 「拉取上游模型」按钮：调 `GET /admin/accounts/:id/upstream-models`（编辑态）填充上游模型名候选（点击模型 chip 快速填入映射目标）。
- 保存时写入 credentials.model_mapping（对象 src→dst）。

### 3.3 i18n

zh/en 两语言补：积分、积分明细、刷新积分、立即签到、签到成功/已签到/无签到活动、模型映射、拉取上游模型等 key。

## 4. 验证

- 单测：CnUpstreamService 三方法（stub provider，覆盖余额写入/签到后刷新/qoder 无活动分支）、handler 平台校验与响应结构。
- `go build` / `go vet` / 相关包 `-tags=unit` 测试全绿。
- 前端 `vue-tsc --noEmit` + `vite build`。
- 本地重启后端实测：刷新积分/明细弹窗/手动签到/模型映射保存后请求命中映射。

## 5. 不做的事

- 不做批量签到（单账号逐个点即可；后续需要再加）。
- 不做签到历史记录表（原项目仅内存 state，签到结果即时反馈）。
- 不动渠道级 model_mapping 机制。
