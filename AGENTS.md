# Sub2API — AI API Gateway Platform

AI API 网关平台，用于订阅配额分发。Go 后端 + Vue 3 前端，PostgreSQL + Redis 存储。

## Project

- **Module**: `github.com/Wei-Shaw/sub2api`
- **Stack**: Go 1.26.3 (Gin + Ent ORM + Redis + PostgreSQL) · Vue 3.4+ (Vite + Pinia + Vue Router + i18n)
- **Entry points**:
  - Backend: `backend/cmd/server/main.go` (Wire DI)
  - Frontend: `frontend/src/main.ts`
- **Config**: `backend/internal/config/config.go` — 单一配置结构体，所有配置字段集中定义

## Commands

| 命令 | 说明 |
|------|------|
| `make build` | 编译前后端 |
| `make build-backend` | 编译后端 → `backend/bin/server` |
| `make build-frontend` | 编译前端 (pnpm) |
| `cd backend && make build-linux` | **Linux amd64 部署包**，固定输出 → `backend/dist/server-linux-amd64`（脚本 `backend/scripts/build-linux-amd64.sh`，支持 `SUB2API_OUT` 覆盖） |
| `make test` | 运行所有测试 |
| `make test-backend` | `go test ./...` + `golangci-lint run ./...` |
| `make test-frontend` | lint:check + typecheck + 关键 vitest 测试 |
| `make test-unit` | 仅后端单元测试 (`-tags=unit`) |
| `make test-integration` | 仅后端集成测试 (`-tags=integration`) |
| `make test-e2e` | E2E 测试脚本 |
| `make generate` | 生成 Ent 代码 + Wire 注入 |
| `cd backend && go generate ./...` | 触发 Ent/Wire 代码生成 |
| `pnpm --dir frontend run dev` | 前端开发服务器 (Vite) |
| `pnpm --dir frontend run lint` | 前端 ESLint 修复 |
| `pnpm --dir frontend run typecheck` | 前端 TypeScript 类型检查 |

> **打包注意**：`build-linux`（脚本 `backend/scripts/build-linux-amd64.sh`）**必须带 `-tags embed`**。前端页面经 `backend/internal/web/embed_on.go`（`//go:build embed`）的 `//go:embed dist` 在编译期打进二进制；漏掉该 tag 则 `frontendFS` 为空、页面 404。构建前先跑 `make build-frontend`（`pnpm --dir frontend run build`）产出最新 `internal/web/dist`。产物固定输出 `backend/dist/server-linux-amd64`。

> **⚠️ Agent 终端环境 PATH 不完整**：TRAE IDE 启动的终端会话 PATH 不含 `/usr/local/go/bin`、`/usr/bin` 之外的常用路径，直接敲 `go` / `git` / `head` / `tail` / `grep` 会报 `command not found`（**不是没装**）。已确认可用全路径：`/usr/local/go/bin/go`（go1.27.0）、`/usr/bin/git`（2.50.1）。在 agent 终端里执行构建/测试一律用全路径，或先 `export PATH=$PATH:/usr/local/go/bin:/usr/bin`。

## Architecture

```
backend/
├── cmd/server/          # 入口 + Wire 依赖注入
├── internal/
│   ├── config/          # 配置定义与加载
│   ├── handler/         # HTTP handler (Gin) — 网关、认证、管理、支付
│   ├── service/         # 业务逻辑层 — 网关转发、计费、账户、认证、配额…
│   ├── repository/      # 数据访问层 (Ent + Redis 缓存)
│   ├── middleware/       # HTTP 中间件 (限流)
│   ├── domain/          # 领域类型与常量
│   ├── payment/         # 支付系统 (Alipay/WeChat Pay/Stripe/EasyPay)
│   ├── model/           # 数据模型
│   ├── server/          # HTTP 服务器配置
│   └── setup/           # 应用启动引导
├── ent/                 # Ent ORM 自动生成代码
└── migrations/          # 数据库迁移
frontend/
└── src/
    ├── views/           # 页面组件
    ├── components/      # 可复用组件
    ├── api/             # API 客户端
    ├── stores/          # Pinia 状态管理
    ├── router/          # 路由配置
    └── i18n/            # 国际化
```

**分层**: Handler → Service → Repository。Handler 处理 HTTP 请求/响应，Service 实现业务逻辑，Repository 封装数据访问。依赖注入通过 Google Wire 管理。

**关键服务**: `gateway_service.go` (核心网关转发)、`setting_service.go` (系统设置)、`admin_service.go` (管理后台)、`billing_service.go` (计费)、`account_service.go` (上游账户管理)。

## Conventions

- **Go 代码风格**: 标准 Go 布局，`internal/` 包隔离。使用 `golangci-lint` 检查。
- **依赖注入**: Google Wire — 修改依赖后运行 `make generate` 重新生成 `wire_gen.go`。
- **数据库**: Ent ORM — schema 定义在 `ent/schema/`，修改后运行 `go generate ./ent`。
- **测试**: 大量测试覆盖，单元测试用 `-tags=unit`，集成测试用 `-tags=integration`。测试文件与被测文件同目录。
- **前端**: Vue 3 Composition API + TypeScript，ESLint + vue-tsc 类型检查。
- **国际化**: 通过 `vue-i18n` 实现，支持 EN/CN/JA。
- **构建**: Makefile 作为统一构建入口。GoReleaser 用于发布。Docker 多阶段构建。
- **配置**: 所有配置集中在 `config.go`，通过环境变量加载。
- **提交**: 改完代码立即 commit 并推送。
- **国产多渠道上游（workbuddy/traework/qoder/qwenwork）**: 四渠道为普通包，位于 `backend/internal/cnupstream/`（provider/upstream/traework/qoder/qwenwork/pool/scheduler/auth），已替代并删除旧独立子模块 `backend/internal/workbuddy-wild/`（qwenwork 移植自其 internal/qwenwork，2026-08-29 同步）。网关入口 `CnUpstreamService`（`internal/service/cnupstream_service.go`）按平台持有 Pool+Upstream；签到/保活由 `CnUpstreamSchedulerService` 按 `config.Checkin` 驱动（qoder/qwenwork 无签到）。阶段3网关分发已接入**最小 Chat 分支**：`ForwardAsChatCompletions` 按 `isCnUpstreamPlatform` 分流到 `forwardCnUpstreamChatCompletions`（`openai_gateway_chat_completions_cnupstream.go`），调 `CnUpstreamService.ChatStream(platform, accountID, body) -> *CnChatTarget`（**以统一池选中的账号 ID 优先取号**，`auth.Auth.AccountID` 承载 ent 主键），私有 SSE 转 OpenAI 兼容流回写、`cnUsageCapturingWriter` 抽 usage 按 tokens 计费；**转发结束必须 `target.Report(err)`** 把 SSE 流内业务错误（4008/4028/1005）回灌池冷却，否则被限流账号会被反复选中。`OpenAIGatewayService` 构造函数已加 `cnUpstream` 依赖。`/v1/models` 复用 `GetAvailableModels` 聚合国产渠道模型。真实上游未联调（SSE fixture 验证）。详见 [docs/platform-multi-channel-integration.md](docs/platform-multi-channel-integration.md)。
- **composite 统一账号池（已取消 composite 路由）**: composite 分组是跨平台统一账号池，**没有平台概念**——网关链路不再读 `composite_model_routes`、不再改写 `body.model`（别名映射只剩账号级 `model_mapping`）。选号见 `selectCompositePoolAccount`（`gateway_scheduling.go`）：粘性会话 → 候选过滤（含端点协议族）→ 优先级分层 → 同层 `creditsRemain` 降序 → **进程内分层轮转游标**（`rotateCompositePoolTiers`，键 `groupID|priority`）→ 抢不到槽换同层下一个 → 全忙兜底排队；单请求最多尝试 8 个账号（`handler.capAccountAttemptsForGroup`，不改全局 `gateway.max_account_switches`）。端点能力由**池组成**决定：`handler.CompositePoolDispatchOnlyPlatforms` 读 `GetSchedulablePlatforms`（带 15s 缓存）决定走 OpenAI 网关还是通用入口；Anthropic 协议入口用 `WithCompositePoolPlatformFilter(ctx, CompositeAnthropicNativePlatforms...)` 排除无桥接实现的三渠道账号；`/v1/chat/completions` 是混合池通用入口（按选中账号平台委派转发）。`composite_model_routes` 表、Ent schema、管理端接口与前端路由编辑器**已彻底下线**（迁移 `230_drop_composite_model_routes.sql` DROP TABLE，ent 重生成，admin `/:id/composite-routes*` 接口与 GroupsView 路由弹窗删除）；要给渠道配对外模型名只能用账号级 `model_mapping`。**composite 桶（`SchedulerModeComposite`，`{GroupID, PlatformComposite, Mode}`）必须随账号变更重建**：`rebuildByAccount`/`rebuildByGroupIDs` 已通过 `compositeBucketsForGroups` 补建；`schedulerCanonicalBuckets`/`bucketsForPlatform`/`schedulerSnapshotPlatforms` 只遍历 8 个常规平台桶、不含 composite 桶，新增/删除 composite 组账号若只走常规桶重建会让 composite 桶停留在旧快照 → 新平台账号因缓存缺号而 503。详见 [.claude/artifacts/designs/composite-unified-pool.md](.claude/artifacts/designs/composite-unified-pool.md)、[.claude/artifacts/fixes/composite-bucket-stale-on-account-change.md](.claude/artifacts/fixes/composite-bucket-stale-on-account-change.md)。
- **qwenwork（千问办公）渠道要点**: 协议与 Qoder 同源（COSY 签名/QoderEncoding/嵌套 SSE），差异：单一域名 `gateway.qwenwork.cn`、积分走 `/api/v1/adapter/user/account-context`（quota total/used/remaining）、`Encode=0` 直连 + `x-qw-request-id` 头、无签到、支持图片多模态（body 图片数组化）。Classify 已接业务码表（provider.errcode.go）。一键授权为 PKCE 设备流（`cnupstream/qwenwork/login.go` 的 StartLogin/PollLogin，经 `cn_oauth_service` 的 `cnQwenWorkLoginClient` 接入）：PKCE 状态存 oauthStateEntry（QwVerifier/QwNonce），machine_id 由 traework.GenMachineID 生成并随建号写 credentials，机器指纹用 `qwenwork.EnsureFingerprint` 补齐 machineToken/machineType。上游 workbuddy-wild 仓库是其协议参照（internal/qwenwork）。**动态模型映射（客户端名→上游 key）按账号隔离在 `auth.Auth` 上**（`SetModelMap`/`ModelKey`/`ModelMapsStale`/`MarkModelMapAttempt`），不要挂回 `Client`——Client 是平台级单例，同平台不同套餐账号会互相覆盖 key；`FetchUpstreamModels` 必须写进 `pool.AuthByAccountID` 拿到的池内实例，`ChatStream` 在两级映射都未命中且该账号表过期时按账号懒刷新（失败只 MarkModelMapAttempt 防每请求重打上游）。**流式健壮性约定**：① 四渠道 `Client` 的 `ChatStream` 必须走独立无超时的 `StreamHTTP`（`http.Client.Timeout` 含读 body 总时长，SSE 流超过它会被强制掐断——traework/qwenwork/qoder/upstream(workbuddy) 均已如此拆分）；② 国产渠道流式转发（`forwardCnUpstreamSinglePlatform`）必须**在 `cnInvokeChatStream`（上游调用，可阻塞）之前**启动 `startOpenAISSEKeepalive` 心跳——否则上游建连/首包等待期客户端零字节会被 nginx 判 504（`StreamHTTP` 无 ResponseHeaderTimeout 可长时间阻塞）；且 `cnUsageCapturingWriter` 必须包裹保活启动前的 `originalWriter`（而非被替换的包装器，否则 `Stream` 内首次 `w.Header()` 触发 `suspend()` 在首拍前停掉心跳）；**非流式（stream:false）长聚合同样会 504**——同样需在 `cnInvokeChatStream` 之前启动 `startCnNonStreamJSONPadding`（首个 interval 超时后提交 200+application/json 头并周期写空格填充，JSON 解析器容忍前导空白；写最终 JSON 前先 stop 防 字节交错）——否则长推理/建连等待被 nginx proxy_read_timeout 判 504；③ 上游频控（4028/4008 ErrSoftRate）由 `cnUpstreamFailoverError` 映射为通用 **429 + Retry-After**（agent 客户端遇 429 自动重试），SSE 流内错误分支与 HTTP 4xx 业务码分支都要设。详见 [.claude/artifacts/fixes/cn-channel-504-keepalive-and-4028.md](.claude/artifacts/fixes/cn-channel-504-keepalive-and-4028.md)、[.claude/artifacts/fixes/cn-channel-504-connect-wait-keepalive.md](.claude/artifacts/fixes/cn-channel-504-connect-wait-keepalive.md)。
- **三渠道一键授权**: traework 走浏览器回调流（授权页 `www.trae.cn/authorization`，回调 `/oauth/callback/:platform/:state` 携带 refreshToken+host）；workbuddy 走服务端轮询流（`StartLogin`/`PollLogin`，无浏览器回调）；qoder 无授权流程仅粘贴 auth JSON。凭证键名必须用驼峰（与 `hydrateAuth` 一致）。详见 [docs/oauth-account-add-design.md](docs/oauth-account-add-design.md)。
- **三渠道积分/签到/模型映射**: 管理端账号级 API `POST /:id/credits/refresh`、`GET /:id/credits/detail`、`POST /:id/checkin`、`GET /:id/upstream-models`（`CnUpstreamHandler`，签到业务失败返回 success=false 而非 error）；余额落 credentials `creditsRemain`；账号级模型映射存 credentials `model_mapping`（转发经 `GetMappedModel` 生效）；自动签到由 config.yaml `checkin.enabled` 开启（调度器随 Wire Provider 自启）。**自动签到闭环已实现**：scheduler 钩子（`PersistAuth` 凭证回写 / `Reload` 批量前刷池 / `CheckinBackoff` 退避）+ 签到结果统一经 `PersistCheckinResult` 落库（`creditsRemain`/`lastCheckinAt`/`lastCheckinResult`），前端积分列展示上次签到状态；token 刷新回写必须 **COW 合并** credentials（仓储 Update 整行覆盖，整包替换会抹掉 `model_mapping` 等业务键）。详见 [docs/cnupstream-credits-checkin-design.md](docs/cnupstream-credits-checkin-design.md)。
- **推理字段透出（reasoning_content / thinking tokens）**: 网关**所有链路响应侧原样透传，不剥 reasoning_content、不清 usage thinking 字段**——客户端收到 reasoning_content 恒空或 thinking tokens=0，根因在上游（上游没返回思考内容或字段名非 `reasoning_content`），不是网关剥字段。请求侧：qwenwork/qoder 的 `buildAgentBody` 重构精简 body 时会**丢弃客户端 `reasoning_effort` 强度值**（已修复为写入 `model_config.reasoning_effort`，2026-09-02，提交 `dfc0fff63`）且**不把 `thinking` 字段本身传给上游**（只转 `model_config.is_reasoning` 布尔）；traework/workbuddy 为浅改写透传，thinking/reasoning_effort 原样转发。详见 [docs/cn-upstream-reasoning-content-transparency.md](docs/cn-upstream-reasoning-content-transparency.md)。
- **`/v1/models` 必须输出 OpenAI 规范条目** `{id, object:"model", created:<Unix秒整数>, owned_by}`，否则 agent 工具的「一键拉取模型」会失败（缺 `object`/`owned_by`、或 `created` 是 Anthropic 风格字符串 `created_at` 时，客户端按 int 解析即失败）。`writeModelsList`（`handler/gateway_handler.go`）已统一规范化，`owned_by` 由 `ownedByForPlatform` 按平台映射（openai/anthropic/google/xai + 四国产渠道/复合池用 sub2api）。**注意 `/v1/models` 与根 `/models` 共用 handler**，而根 `/models` 是 Anthropic 客户端入口，故保留 `display_name` / `created_at` 兼容字段，不要删。Grok 走 `writeGrokModelsList` 独立分支（带 reasoning 能力字段），Anthropic 原生 `/antigravity/models` 与 Gemini `/v1beta/models` 是各自原生协议，**不要**改成 OpenAI 格式。唯一走 OpenAI 专用输出的是 `writeOpenAIModelsList`（OpenAI 平台 + 自定义列表场景）。详见 [.claude/artifacts/fixes/cn-traework-stream-openai-spec.md](.claude/artifacts/fixes/cn-traework-stream-openai-spec.md)。
- **CN 渠道流式 SSE 必须符合 OpenAI chunk 规范**（否则 zcode 等客户端把 reasoning_content 切成"一段一段思考"）。规范基线取自项目内 `internal/pkg/apicompat` 的 `resToChatHandleCompleted` / `makeChatFinishChunk`，四条硬约束：① `finish_reason` 字段**恒定存在**——末 chunk 为具体原因（`stop`/`length`/`tool_calls`/…），中间 chunk 为 `null`，**绝不能缺字段或给空串**（缺字段时客户端退化为按 chunk 到达时延/字段切换切分 UI 块）；② **首 chunk 必须带 `delta.role="assistant"`**，且 role 只出现一次（中间/末 chunk 不带）；③ **`reasoning_content` 与 `content` 不得同处一个 delta**（上游混发时必须拆成两个 chunk，先思考后正文）；④ **`usage` 是 finish chunk 之后的独立 chunk 且 `choices` 为 `[]`**（挂在 finish chunk 上会让客户端收到 finish_reason 就停止解析而丢 usage，表现为 token 恒为 0）。四渠道均已对齐：traework 由 `streamOpts.writeChunk`（`cnupstream/traework/solosse.go`）实现，用独立状态 `wroteAnyChunk` 判断首 chunk——**不可复用 `sawOutput`**（后者语义是"上游产出过内容"，被 `case "error"` 用于决定是否返回 `StreamBeforeOutputError` 让上层换号重试）；上游无 output 直接 done 时末 chunk 同时是首 chunk，仍须带 role。qoder/qwenwork 由重写后的 `streamAsOpenAI` 实现（注意 qoder 的 `parseNestedSSE` **只认嵌套格式** `data:{"body":"<json>"}`，qwenwork 兼容嵌套+标准两种）。workbuddy（upstream）由 `emitNormalized` 逐行解析规范化，**非 JSON 行（注释行/event: 行/[DONE]）仍原样透传**。非流式由 `normalizeCnAggregateResponse` 补齐 `model`（traework 聚合恒为 `""`）、零值 `usage`、`choices[0].index/finish_reason/message.role`。详见 [.claude/artifacts/fixes/cn-traework-stream-openai-spec.md](.claude/artifacts/fixes/cn-traework-stream-openai-spec.md)。

## Notes

- **国产渠道上游监控（workbuddy-wild / wild-work）**: sub2api 的国产渠道（workbuddy/traework/qoder/qwenwork）功能是从 **wild-work 原仓库 `github.com/rockswang/wild-work`** 合并来的，逆向调用接口容易因上游协议/接口变动失效，**必须定期检查其更新并评估同步到 `backend/internal/cnupstream`**。本地镜像在 `/Users/duormi/workspace/workbuddy-wild`（当前分支 master，领先上游若干 commit，含 qwenwork 图片多模态等本地增强）。**⚠️ 拉取上游用直连 `upstream` remote（`https://github.com/rockswang/wild-work.git`），不要用 `origin`（`ghproxy.net` 代理，其 SSL 证书已过期导致 fetch 失败）**。已配置每日 09:00 定时任务「每日检查上游更新」执行 `git fetch upstream` + 只读对比报告（只报告不自动合并）。

