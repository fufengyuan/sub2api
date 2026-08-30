# Bug: 国产渠道 API 接口经常报 504 Gateway Time-out + 4028 频控

> Status: FIXED
> Mode: default (multi-root-cause)
> Severity: blocker
> Author: Wei-Shaw
> Last updated: 2026-08-30

## Symptom

用户反馈 API 接口经常报：

```
504 Gateway Time-out (nginx/1.18.0) (HTTP Status: 504) (4028)
```

- 使用 qwenwork（千问办公）特有模型 `qwen3.8-flash` 时触发。
- 仅国产渠道（workbuddy/traework/qoder/qwenwork）报，标准 OpenAI/Claude 渠道正常。
- `(4028)` 是上游频控/配额业务错误码（`ErrSoftRate`）。

用户诉求（明确）：
1. 消除 504 网关超时。
2. 把上游 4028 这类频控包装成**通用 429**，市面上 agent 客户端遇到 429 会自动重试（502 多数不重试）。

## Expected

- 长推理/长输出的国产渠道流式请求不应被中间 nginx 空闲超时判 504。
- 上游 SSE 流只要持续有数据（或心跳），就不应被 `http.Client.Timeout` 总超时掐断。
- 4028 频控最终返回给客户端的是 `429` + `Retry-After`，而非 502。

## Reproduction

### 缺陷 B：流式被 http.Client.Timeout 总超时掐断
- 测试：`backend/internal/cnupstream/qwenwork/stream_timeout_repro_test.go`
  `TestChatStreamNotCutByClientTotalTimeout`
- 复现稳定性：red→green→red 均稳定。

### 缺陷 C：4028 未映射为 429
- 测试：`backend/internal/service/openai_gateway_chat_completions_cnupstream_test.go`
  `TestCnUpstreamFailoverErrorSoftRateFromBody` / `TestCnUpstreamFailoverErrorSSEStreamSoftRate`
- 修复前断言 `status=502, want 429`，`Retry-After: ""`。

### 缺陷 A：国产渠道流式缺 keepalive
- 测试：`backend/internal/service/openai_gateway_chat_completions_cnupstream_test.go`
  `TestForwardCnUpstreamStreamWritesKeepaliveBeforeFirstOutput`
- 修复前响应体无 `: keepalive`。

## Hypotheses & diagnosis

| # | Hypothesis | Verdict | Evidence |
|---|---|---|---|
| H1 | qwenwork/qoder `ChatStream` 用带 `http.Client.Timeout=180s` 的 `HTTP` 做流式，SSE 流 >180s 被掐断 | confirmed (缺陷B) | [qwenwork/client.go:289](client.go) `c.HTTP.Do(req)`；`New()` → `NewWithTimeout(180s)` 无独立流式客户端；复现测试 red（`context deadline exceeded while reading body`）。traework 已有无超时 `StreamHTTP`（[traework/client.go:174](client.go)），qwenwork/qoder 缺失 |
| H2 | 国产渠道流式转发缺 keepalive，nginx proxy_read_timeout 空闲超时回 504 | confirmed (缺陷A) | `forwardCnUpstreamSinglePlatform` 流式分支用 `cnUsageCapturingWriter` 纯透传，无 `startOpenAISSEKeepalive`（standard passthrough 有）；keepalive 测试 red（无心跳字节） |
| H3 | `cnUpstreamFailoverError` 对 SSE 流内 4028（ErrSoftRate）返回默认 502 而非 429 | confirmed (缺陷C) | [cnUpstreamFailoverError](cnupstream.go) 只在 ErrSoftRate 分支设 `backoff`，未改 `status`；复现测试 red（`status=502, want 429`） |

### 反向追溯（H1 缺陷 B）
`qwenwork.ChatStream` 返回 `resp.Body` 后由 `forwardCnUpstreamSinglePlatform` 流式分支读取。掐断点是 `http.Client{Timeout:180s}`——Go 的 `http.Client.Timeout` 包含**整个请求+读 body** 时长，流式一旦超过 180s 必然 `context deadline exceeded`。修复在**首次引入该超时约束的 client 层**（`NewWithTimeout`），加独立无超时的 `StreamHTTP` 供流式专用——与 traework 既有范式一致。

## Root cause

三个独立缺陷叠加导致国产渠道 504 + 4028 透传异常：

1. **缺陷 A（504 主因）**：国产渠道路径（`forwardCnUpstreamChatCompletions` 流式分支）完全没有 keepalive 心跳。standard `/v1/responses` passthrough 已有 `startOpenAISSEKeepalive`，但国产渠道漏了。长推理（思考数分钟无输出）期间下游零字节，中间 nginx `proxy_read_timeout`(600s) 按空闲判死回 504。
2. **缺陷 B（504 助因）**：qwenwork/qoder 的 `ChatStream` 用带 `http.Client.Timeout=180s` 的客户端做流式。`http.Client.Timeout` 含读 body 总时长，SSE 流 >180s 被强制掐断（长输出/长思考必炸）。traework 已有独立无超时 `StreamHTTP`，qwenwork/qoder 缺失。
3. **缺陷 C（4028 处理）**：`cnUpstreamFailoverError` 对 SSE 流内 4028（`ErrSoftRate`）只设 `backoff`，`status` 保持默认 502。用户期望映射为通用 429（agent 自动重试），OpenAI 网关路径 `mapUpstreamError(429)` 恰好会返回 `429 rate_limit_error`。

## Fix

### 缺陷 B：qwenwork/qoder 增加独立无超时流式客户端
- `qwenwork/client.go`、`qoder/client.go`：
  - `Client` 增加 `StreamHTTP *http.Client` 字段。
  - `NewWithTimeout` 初始化 `StreamHTTP: &http.Client{Transport: tr}`（无 `Timeout` 总超时）。
  - `ChatStream` 优先用 `StreamHTTP`（`hc := c.HTTP; if c.StreamHTTP != nil { hc = c.StreamHTTP }`），为 nil 时回退 `HTTP`。
- 范式对齐 traework 的 `StreamHTTP`。

### 缺陷 A：国产渠道流式路径启动 keepalive
- `openai_gateway_chat_completions_cnupstream.go` `forwardCnUpstreamSinglePlatform` 流式分支：
  - `cfg.Gateway.StreamKeepaliveInterval > 0` 时调用 `startOpenAISSEKeepalive(c, interval)`，`defer` 停止。
  - **关键细节**：`cnUsageCapturingWriter` 必须包裹 keepalive 启动**之前**的原始 writer（`originalWriter := c.Writer`），而非被替换后的 `openAICompactKeepaliveWriter`——否则 `Stream` 内首次 `w.Header()` 触发 `Header()` 的 `suspend()`，在首拍前把心跳停掉（这正是实现时踩到的真实问题，见 V-3）。

### 缺陷 C：cnUpstreamFailoverError 4028→429 + Retry-After
- `openai_gateway_chat_completions_cnupstream.go` `cnUpstreamFailoverError`：
  - SSE 流内业务错误分支（`soloErr.Kind() == ErrSoftRate`）：`status = 429`，`headers = Retry-After = ceil(backoff秒)`。
  - HTTP 4xx + 业务码 4028 分支：同样 `status = 429` + `Retry-After`。

## Verification

- V-1: `TestChatStreamNotCutByClientTotalTimeout` → GREEN ✓（qwenwork StreamHTTP 读完 16 chunk 长流）
- V-2: `TestCnUpstreamFailoverErrorSoftRateFromBody` / `TestCnUpstreamFailoverErrorSSEStreamSoftRate` → GREEN ✓（4028→429 + Retry-After）
- V-3: `TestForwardCnUpstreamStreamWritesKeepaliveBeforeFirstOutput` → GREEN ✓（首个输出前写出 `: keepalive` 心跳；期间发现并修复 originalWriter 包裹问题）
- V-4: 反向验证：stash 各修复后，对应复现测试重新 RED ✓（证明测试真抓 bug）
- V-5: `go test ./internal/service/ ./internal/cnupstream/... ./internal/handler/ -tags embed` → 全部 GREEN ✓
- V-6: `go build ./...`、`go vet` → 通过 ✓

## Regression test

- `backend/internal/cnupstream/qwenwork/stream_timeout_repro_test.go`
  `TestChatStreamNotCutByClientTotalTimeout`
- `backend/internal/service/openai_gateway_chat_completions_cnupstream_test.go`
  `TestCnUpstreamFailoverErrorSoftRateFromBody` / `TestCnUpstreamFailoverErrorSSEStreamSoftRate` / `TestForwardCnUpstreamStreamWritesKeepaliveBeforeFirstOutput`

## Defense-in-depth

- **根因 C** 增加 `429 + Retry-After` 断言（软限流错误必须映射为通用 429，防回归为 502）。
- **根因 A** 增加 keepalive 心跳写出断言（防国产渠道长推理被 nginx 判死）。
- **根因 B** 增加流式长流完整读取断言（防 `http.Client.Timeout` 总超时掐流）。

## Pattern analysis

| 搜索方式 | 命中 | 是否本次同类隐患 |
|---|---|---|
| `http.Client{Timeout:` in `cnupstream/` | qwenwork/client.go:49、qoder/client.go:50（已修）、upstream/client.go:113（HTTP 120s + BillingHTTP 30s） | qwenwork/qoder 本次已修；upstream `HTTP` 仍 120s 但 `StreamHTTP` 已存在（traework 路径走 StreamHTTP） |
| `newCnUsageCapturingWriter` 流式路径 | 仅国产渠道 | 已加 keepalive |

## Open questions / Follow-ups

- `upstream.Client.HTTP`（[client.go:113](client.go)）仍 `Timeout:120s`。workbuddy 平台 `ChatStream` 走该 `HTTP` 做流式——若 workbuddy 也出现长流 504，需同样加 `StreamHTTP`。本次 qwenwork（用户报错平台）+ qoder（同构）已修，workbuddy 因未复现且改动面扩大，留作 follow-up。
- 国产渠道 keepalive 依赖 `gateway.stream_keepalive_interval` 配置（默认 10s），确认生产配置已启用。

## RCA

- **何时引入**：国产多渠道接入阶段，`forwardCnUpstreamChatCompletions` 直接沿用纯透传写法，未对照 standard passthrough 补齐 keepalive；qwenwork/qoder client 从 workbuddy-wild 移植时未带上 traework 的 `StreamHTTP` 拆分。
- **为什么没被发现**：真实上游未联调（此前用 SSE fixture 验证），fixture 即时读完，无法暴露 180s 掐流与 nginx 空闲超时；4028 分支缺少 `StatusCode` 断言。
- **预防措施**：新增三个回归测试（流式长流、keepalive、4028→429）；后续新增国产渠道需对照 traework `StreamHTTP` 与 passthrough keepalive 范式。
