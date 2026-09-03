# 国产渠道积分折算计费 & 缺价日志降级

## 背景

生产日志大量刷 ERROR：

```
ERROR service/gateway_usage_billing.go:1106 Calculate cost failed:
no pricing available for model: qwen3.8-flash: pricing not found
```

`qwen3.8-flash` 是 CN 渠道（qoder/qwenwork）上游返回的动态模型名，在 LiteLLM 价卡
与 `billing_service.go` 的 `fallbackPrices` 硬编码兜底表里都没有条目
→ `ModelPricingResolver.Resolve` 返回 `BasePricing=nil`
→ `billing_service.go:1282` 抛 `ErrModelPricingUnavailable`。

**业务影响**：`calculateTokenCost` 只打日志就 `return &CostBreakdown{ActualCost: 0}`
——记账不中断，但**这条用量按 0 成本入账，用户白嫖了 token**。

## 根因分析

三条关键事实（排查过程中逐一验证）：

1. **缺价不是系统故障**。请求已经成功完成，只是计费侧缺价。刷 ERROR 会淹没真实故障，
   且零成本导致漏扣。正确行为：零成本落账 + WARN + 结构化字段（便于持续补齐价卡）。

2. **上游响应报文里没有「单次消耗积分」**。这一点与最初的猜测相反，必须澄清：
   - traework SSE 的 `token_usage` 事件只有 `prompt_tokens/completion_tokens/
     total_tokens/reasoning_tokens`，`cnUsageCapturingWriter` 也只抽这四个字段；
   - 账号积分余额（`credits_limit`/`credits_amount`、`account-context` 的 quota）是
     **账号级累计值**，不是单次消耗量；
   - 结论：**拿不到单次实耗积分**，因此「按请求前后余额差值计费」不可行（并发下会算错，
     且每次请求多两次上游调用）。

3. **`FetchModelPricing` 提供的是静态倍率 Rate，且生产代码从未调用过**。四渠道均已实现：
   | 渠道 | 接口 | 倍率字段 |
   |------|------|----------|
   | workbuddy | `/console/enterprises/personal/models` | `credits: "x0.79 credits"` → `parseCredits` |
   | traework | `/api/remote/v1/models` | `features.consumption_rate.rate`（折扣价 `discount.data.consumption_rate` 优先） |
   | qoder / qwenwork | 动态模型接口 | `price_factor` |

   **量纲关键**：`Rate` 是**每次请求消耗的积分数**（request-level），而 `fallbackPrices`
   是**每 token 单价**（token-level）。两者量纲不同，**不能直接塞进 `fallbackPrices`**。

## 方案

### 1. 日志降级 + 行为对齐（默认生效，无风险）

把 `isUsagePricingUnavailableError` 抽到 `usage_pricing_shared.go`，两条网关路径共用：

- 旧路径 `GatewayService.calculateTokenCost`（`gateway_usage_billing.go`）
- 新路径 `OpenAIGatewayService`（`openai_gateway_usage.go`，原本已有该逻辑）

缺价时：零成本落账 + WARN + 结构化字段（`billing_model`/`requested_model`/
`upstream_model`/`platform`/`request_id`）。非缺价错误仍走原 ERROR 路径。

### 2. 积分折算（配置开启后才生效）

新文件 `cn_credit_pricing.go`：

```
成本 = Rate × CreditToUSD × multiplier
```

- **配置**（`config.CnCreditPricingConfig`）：
  ```yaml
  cn_credit_pricing:
    enabled: true
    credit_to_usd: 0.002   # 1 积分 = ? USD，按实际采购成本填
  ```
  **默认关闭**。关闭时行为与改动前完全一致（零成本 + WARN），不误扣。

- **未收录模型**：按 `cnCreditPricingDefaultRate = 1.0` 保守折算。
  既不零成本白嫖，也不猜高价误扣，并打 WARN
  `cn_credit_pricing.rate_not_found_using_default` 提示运营者补齐。

- **缓存**：按 `平台|账号ID` 隔离，TTL 5 分钟。倍率接口是账号级鉴权，
  同平台不同账号可能因套餐折扣返回不同倍率，**只按平台缓存会串号**。

- **上游失败**：静默降级为 1x 折算（不返回"不适用"，否则会退回零成本白嫖）。

- **接入点**：仅在内置 token 价卡缺失时才尝试折算（`calculateTokenCost` 的 err 分支），
  已定价流量完全不受影响。

### 3. 依赖注入

`GatewayService`（旧路径）原本没有 `cnUpstream` 依赖，新增构造参数并同步
`wire_gen.go`（`cnUpstreamService` 需提前到 `gatewayService` 之前构造）。
Wire 未安装，手工改的 `wire_gen.go` 与 wire 生成结果一致。

## 验证

- `go build ./...`、`go vet ./...`、`go vet -tags=unit ./...` 全通过
- `go test ./...`、`go test -tags=unit ./...` 全通过（无回归）
- 新增 7 个单测（`cn_credit_pricing_test.go`）：默认关闭生效、命中倍率表、
  未收录模型 1x 兜底、非国产渠道不适用、上游失败降级、倍率系数、缓存键隔离

## 遗留与下一步

1. **`credit_to_usd` 需运营者按实际采购成本填写**，未填则折算保持关闭。
2. 若后续发现上游确实提供单次实耗积分字段（需抓真实报文确认），
   应优先改用实测值替代静态倍率折算。
3. 可考虑把 `cn_credit_pricing.rate_not_found_using_default` 的 WARN 聚合上报，
   作为持续补齐倍率表的输入。
