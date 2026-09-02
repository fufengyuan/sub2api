# 国产渠道推理字段透出（reasoning_content / thinking tokens）调查与修复

> 日期：2026-09-02
> 触发：OpenAI 兼容接口规范性反馈清单 P2-8 —— `reasoning_content` 恒为空字符串且 usage 的 thinking tokens 为 0（模型 `deepseek-v4-flash`）。

## 结论（稳定，跨 agent 需遵守）

**sub2api 网关所有链路（国产渠道流式/非流式、标准 CC→Responses→CC、Anthropic 桥接）的响应侧都是「原样透传」，不会剥离 `reasoning_content`，也不会清空 usage 里的 thinking 相关字段。**

因此：客户端收到 `reasoning_content` 恒空或 `completion_thinking_tokens`/`reasoning_tokens` 为 0/缺失时，**根因在上游**（上游在该模型/配置下没返回思考内容，或返回的字段不在 `reasoning_content` 名下）。网关侧没有可修的「剥字段」问题，只有请求侧意图传达的完整性问题。

## 请求侧缺陷（已修复）

qwenwork / qoder 两渠道的 `buildAgentBody` 重构精简请求体时：
- 客户端 `reasoning_effort` 强度值（low/medium/high）被读取后**从未写入上游 body**（函数签名收了 `reasoningEffort` 但函数体未用）——半成品实现。
- `thinking` 字段（`thinking.type=enabled`，Anthropic 风格）本身不传给上游，只转成 `model_config.is_reasoning` 布尔开关。

这解释了「`thinking:{"type":"enabled"}` 参数无效」——qwenwork/qoder 重构 body 时丢掉该字段，上游只能收到 `is_reasoning:true`。

### 修复

在 qwenwork / qoder 的 `buildAgentBody` 中，当 `reasoningEffort != ""` 时将其写入 `model_config.reasoning_effort`（`model_config` 与 `chat_context.extra.modelConfig` 共用同一 map）。空串时省略该字段，不污染未启用推理的请求。

- 文件：`backend/internal/cnupstream/{qwenwork,qoder}/body.go`
- 测试：`backend/internal/cnupstream/{qwenwork,qoder}/qwenwork_test.go`、`qoder_test.go` 新增 `TestBuildAgentBodyReasoningEffortPassthrough`
- 提交：`dfc0fff63` fix(国产渠道): qwenwork/qoder请求体透传reasoning_effort强度值

### 上游字段名说明

上游 `model_config` 目前仅确认支持 `key` + `is_reasoning`（`DynamicModel.IsReasoning` 标记模型推理能力）。`reasoning_effort` 是否为上游识别字段**未联调确认**；多数 JSON 服务对未知字段静默忽略，故该改动风险低。若真实上游不识别，仅强度不精确、不影响是否返回推理内容。

## 各渠道响应侧透传行为速查

| 渠道 | 流式 | 非流式聚合 | usage |
|---|---|---|---|
| qwenwork | `streamAsOpenAI` 原样透传 chunk（只改 model/finish_reason） | `aggregate` 读 `delta.reasoning_content` | 上游原始 usage 原样透传 |
| qoder | 同上 | 同上 | 同上 |
| traework | `Stream` 把上游 `reasoning` 显式转写为 `delta.reasoning_content` | 保留 | 同上 |
| workbuddy(upstream) | `Stream` 纯字节透传 | 保留 | 同上 |
| 标准 CC→Responses→CC | `response.reasoning_text.delta` → `ChatDelta.ReasoningContent` | `BufferedResponseAccumulator` 累积 reasoning | `completionDetailsFromResponses` 构造 `reasoning_tokens` |

## 排查建议（若再遇「reasoning_content 恒空」）

1. 确认该账号走哪个渠道/平台（qwenwork/qoder vs traework vs 标准池）。
2. 若 qwenwork/qoder：确认客户端是否带 `reasoning_effort` 或 `thinking`；本修复后应已透传，需真实环境验证上游是否识别。
3. 抓上游原始 SSE，确认上游 chunk 的 delta 里思维字段名（`reasoning_content` vs `reasoning` vs `thinking`）。若为后两者，需在各渠道聚合/流式补齐（参照 `normalizeOllamaCloudChatCompletionsResponseJSON`）。
4. 确认该模型 `is_reasoning` 标记是否支持思考。
