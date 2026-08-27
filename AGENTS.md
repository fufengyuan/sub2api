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
- **国产多渠道上游（workbuddy/traework/qoder）**: 三渠道为普通包，位于 `backend/internal/cnupstream/`（provider/upstream/traework/qoder/pool/scheduler/auth），已替代并删除旧独立子模块 `backend/internal/workbuddy-wild/`。网关入口 `CnUpstreamService`（`internal/service/cnupstream_service.go`）按平台持有 Pool+Upstream；签到/保活由 `CnUpstreamSchedulerService` 按 `config.Checkin` 驱动（qoder 无签到）。阶段3网关分发已接入**最小 Chat 分支**：`ForwardAsChatCompletions` 按 `isCnUpstreamPlatform` 分流到 `forwardCnUpstreamChatCompletions`（`openai_gateway_chat_completions_cnupstream.go`），调 `CnUpstreamService.ChatStream`，私有 SSE 转 OpenAI 兼容流回写、`cnUsageCapturingWriter` 抽 usage 按 tokens 计费；`OpenAIGatewayService` 构造函数已加 `cnUpstream` 依赖。`/v1/models` 复用 `GetAvailableModels` 聚合三渠道模型。真实上游未联调（SSE fixture 验证）。详见 [docs/platform-multi-channel-integration.md](docs/platform-multi-channel-integration.md)。
- **三渠道一键授权**: traework 走浏览器回调流（授权页 `www.trae.cn/authorization`，回调 `/oauth/callback/:platform/:state` 携带 refreshToken+host）；workbuddy 走服务端轮询流（`StartLogin`/`PollLogin`，无浏览器回调）；qoder 无授权流程仅粘贴 auth JSON。凭证键名必须用驼峰（与 `hydrateAuth` 一致）。详见 [docs/oauth-account-add-design.md](docs/oauth-account-add-design.md)。

## Notes

(预留，后续可添加)
