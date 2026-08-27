# 三渠道账号「一键跳转授权 + 粘贴凭证」需求设计

## 1. 背景与目标

workbuddy / traework / qoder 三渠道走私有 OAuth 协议，后端真正认证需要的是 `accessToken / refreshToken / machineId / deviceId / domain / apiHost / machineToken` 这套 OAuth 凭证（见 `service/cnupstream_service.go` 的 `hydrateAuth`），**不是 URL + APIKey**。

当前问题：
- 前端 `CreateAccountModal.vue` / `EditAccountModal.vue` 选中 workbuddy/traework 时**硬性强制为 `apikey` 表单**（填 URL + APIKey），对三渠道无效。
- qoder 前端**连「创建账号」入口都没有**。
- 原项目（workbuddy-wild）的「一键跳转授权」在 sub2api 未落地（登录编排被列为独立后续项，未搬）。

目标：
1. 为三渠道提供**能正确录入 OAuth 凭证**的方式（粘贴 auth JSON / 逐字段）。
2. 提供**一键跳转授权**（全自动回调）：面板点「授权添加」→ 跳平台授权页 → 平台回调 → 后端换 token 自动建号。
3. qoder 补建号入口。

## 2. 决策结论（已与用户对齐）

| 决策点 | 结论 |
|--------|------|
| TraeWork 一键授权 | **对齐原项目 login_trae 真实流程（含 PKCE 新流程）**：`www.trae.cn/authorization?auth_type=local&code_challenge=…&code_challenge_method=S256&…`，授权页对 `auth_callback_url` 做正则硬校验 `^http://127\.0\.0\.1:(\d+)/authorize$`（本机 IDE 协议，域名/localhost/带 query 一律拒绝），callback 固定为 `http://127.0.0.1:{port}/authorize`。回调携带凭证有三种形态（解析优先级：`refreshToken` > `userJwt.RefreshToken` > `userJwt.Token` > `authCodeInfo.AuthCode`），AuthCode 走 `ExchangeAuthCode`（PKCE），refreshToken 走 `RefreshToken` 交换。仅当浏览器与 sub2api 同机（本机部署或 SSH 端口转发）时可完成回调，远程域名访问提示改用粘贴 auth JSON 建号。首版误用虚构的 `/oauth/authorize` 授权码流程（路径不存在导致跳主页），已废弃 |
| WorkBuddy 一键授权 | **对齐原项目 login 的服务端轮询模式**：后端 POST `copilot.tencent.com/v2/plugin/auth/state` 拿 state+授权 URL → 浏览器登录 → 后端轮询 `/v2/plugin/auth/token?state=`，成功即自动建号（无浏览器回调） |
| Qoder 一键授权 | **不做**：原项目（workbuddy-wild）没有 Qoder 登录实现（凭证来自 qoderwork2api 抓包），保持「粘贴 auth JSON」 |
| 回调基地址 | **不新增配置项**，前端发起时带当前 `window.location.origin`；traework 仅取其端口构造 `127.0.0.1:{port}/authorize` 回环 callback |
| 兜底方式 | workbuddy / traework / qoder **全部提供「粘贴 auth 凭证」入口**（JSON 或逐字段） |
| 验证标准 | 编译 + 单测 + fixture（真实上游未联调；授权回调以 fixture 验证链，后续真实联调收敛） |

## 3. 现状盘点

- **traework**：`backend/internal/cnupstream/traework/authcode.go` 已有 `GenPKCE`、`ExchangeAuthCode`（AuthCode+CodeVerifier+设备公钥 → AccessToken/RefreshToken）。缺：授权 URL 生成、回调路由、建号落库。
- **workbuddy / qoder**：登录编排未搬全；qoder 有 `cosy.go`（机器指纹/签名）但不是 OAuth 授权流程。
- **后端**：`handler/admin/account_handler.go` 的 `CreateAccount` 已支持 `type=oauth` + 任意 `credentials map`；`service/cnupstream_service.go` 的 `hydrateAuth` 已能读这套字段。故**粘贴凭证**后端基本无需改动。
- **前端**：`CreateAccountModal.vue` 强制三渠道 apikey；qoder 无选择/建号分支。

## 4. 总体设计

两平台流程不同，state 状态机（一次性 + 防重放 + TTL）共用：

### TraeWork（浏览器回调流，含 PKCE 新流程）

```
面板「一键授权」──▶ POST /admin/cn-oauth/start {platform=traework, redirect_base}
                     │ 生成 state + machineId(64位hex)/deviceId(15位数字) + PKCE verifier/challenge，暂存
                     └─▶ 返回 {authorize_url, state}
前端跳转 authorize_url = www.trae.cn/authorization
    ?login_version=1&auth_from=solo&login_channel=native_ide&plugin_version=…
    &auth_type=local&client_id=…&redirect=0&login_trace_id=<uuid>
    &auth_callback_url=http://127.0.0.1:{port}/authorize
    &machine_id=…&device_id=…&x_device_id=…&x_machine_id=…
    &x_device_brand=…&x_device_type=windows&x_os_version=…&x_env=&x_app_version=…&x_app_type=stable
    &code_challenge=…&code_challenge_method=S256
    &hide_saas_login=true&channel_name=common&click_id=TRAE SOLOSetup-stable-…
                          ▼ 用户在平台登录授权
平台回调 http://127.0.0.1:{port}/authorize（GET query 或新版授权页 POST JSON body）
    携带 refreshToken / userJwt / authCodeInfo / host 之一组合
后端取「最近一条 pending 的 traework state」（callback 硬校验不带 state，无法显式关联）
    凭证解析优先级：refreshToken > userJwt.RefreshToken > userJwt.Token > authCodeInfo.AuthCode
    AuthCode 路径：ExchangeAuthCode(authCode, state配对verifier, 设备公钥) 换 token（PKCE 新流程）
    refreshToken 路径：traework.Client.RefreshToken 换 accessToken
    → GetUserInfo 拿 uid/nickname/enterpriseId → 写 Account(credentials, type=oauth)
前端轮询 /admin/cn-oauth/status?state=… → used 即刷新账号列表
```

- **callback 硬校验**：授权页对 `auth_callback_url` 正则校验 `^http://127\.0\.0\.1:(\d+)/authorize$`（域名/localhost/带 query 一律拒绝），端口取自前端访问地址（`redirect_base`），state 无法随 callback 传递，回调侧用 `LatestPending(traework)` 关联最近一条 pending 流。
- **设备指纹格式**：machine_id 为 64 位 hex（32 随机字节）、device_id 为 15 位纯数字（对齐真实客户端格式，AuthCode 交换的 DeviceInfo 也用同一对 ID）。
- **host 缺省**回落 `traework.OAuthHost`（原项目行为）；AuthCode 交换成功的 origin 会覆盖回调 host。

### WorkBuddy（服务端轮询流，无浏览器回调）

```
面板「一键授权」──▶ POST /admin/cn-oauth/start {platform=workbuddy}
                     │ 后端 POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI
                     │   → {state, authUrl}（上游签发，无 PKCE）
                     │ 本地 state ↔ 上游 state 映射暂存
                     └─▶ 返回 {authorize_url: authUrl, state: 本地state}
前端跳转 authUrl（codebuddy.cn 登录页，可能经 301 跳转链）
                          ▼ 用户在平台登录
前端轮询 /admin/cn-oauth/status?state=…
    后端每轮 GET /v2/plugin/auth/token?state={上游state}
      pending（业务 code 非 0，"login ing"）→ 返回 pending
      成功 → accessToken/refreshToken/expiresIn/domain
           → GET /v2/plugin/login/account?state=（Bearer）→ uid/nickname
           → 消费 state + 写 Account(credentials, type=oauth) → 返回 used
```

- **请求头**：CodeBuddy CLI UA + `Origin/Referer: https://www.codebuddy.cn`（与原 login.go 逐字一致）。
- **建号失败的容错**：上游轮询成功但建号失败时记日志并保持 pending（TTL 10 分钟内可重试）；上游硬错误同样保持 pending，由 TTL 收敛。

### 粘贴凭证（三渠道兜底）

OAuth 表单支持「粘贴 auth JSON 一键填充」+ 逐字段（accessToken/refreshToken/machineId/deviceId/domain/apiHost/machineToken/uid/nickname/enterpriseId）。Qoder 仅此方式。

## 5. 前端改动

1. `CreateAccountModal.vue` / `EditAccountModal.vue`：
   - 选中 workbuddy/traework/qoder 时改为 **oauth 类型表单**，不再强制 apikey。
   - 提供「粘贴 auth JSON」按钮（解析 JSON 填充字段）+ 逐字段输入。
   - 「一键授权」按钮：**仅 traework / workbuddy 显示**（qoder 隐藏，原项目无登录实现）。调 `/admin/cn-oauth/start`，新标签页打开授权 URL，弹状态轮询（pending → used 即成功）。
   - **traework 本机守卫**：授权页只回调 `127.0.0.1` 回环地址，非本机访问（hostname 非 127.0.0.1/localhost）时点一键授权给出明确提示（SSH 端口转发或改用粘贴 auth JSON）。
2. 新增 qoder 创建入口（平台选择区补 qoder）。
3. 账号新建成功后正常刷新列表。

## 6. 后端改动

1. `cnupstream/traework/login.go`：`BuildAuthorizeURL(callbackURL, machineID, deviceID, codeChallenge)` 构造 `www.trae.cn/authorization` 授权页 URL（参数表逐字对齐原 login_trae：含 `code_challenge/code_challenge_method/hide_saas_login/channel_name/click_id/x_env`，`x_device_type=windows`，`login_trace_id` 为 UUID）；`ParseCallbackValues`/`ParseCallback` 回调凭证解析（优先级 refreshToken > userJwt.RefreshToken > userJwt.Token > authCodeInfo.AuthCode）；`GenMachineID`（64 位 hex）/`GenDeviceID`（15 位数字）。
2. `cnupstream/upstream/login.go`（新增）：`StartLogin()`（POST auth/state → state+authUrl）、`PollLogin(state)`（GET auth/token，pending 返回 `ErrLoginPending`；成功再 GET login/account 拿 uid/nickname）。
3. `service/cn_oauth_service.go`（重写）：
   - TraeWork：Start 生成 state+machineId/deviceId+PKCE verifier/challenge 并构造授权 URL；`ExchangeLatestTraeWork(info, host)`（真实回调，LatestPending 关联）与 `ExchangeAndCreate(state, info, host)`（显式 state 兼容路径）消费 state 后按凭证类型分流：AuthCode → `ExchangeAuthCode`（PKCE），refreshToken → `RefreshToken`，userJwt.Token → 直接建号（兜底）。
   - WorkBuddy：Start 调 `StartLogin` 存「本地 state ↔ 上游 state」映射；`Status` 轮询中调 `PollLogin`，成功即建号并置 used。
4. `handler/admin/cn_oauth_handler.go` + `server/routes/cn_oauth.go`：真实回调路由 `GET/POST /authorize`（新版授权页 redirect=1 时 POST，JSON body 字段合并进 query 后统一解析）；`GET/POST /oauth/callback/:platform/:state` 为保留的显式 state 兼容路径。`web/embed_on.go` 的 bypass 白名单放行 `/authorize`。
5. 复用现有 `CreateAccount` 落账逻辑建号（type=oauth、credentials=驼峰键 token 字段）。
6. state 存储：进程内存（明确重启失效，多实例部署后续换 Redis）。

## 7. 验收标准

1. `go build ./...`、`go vet`、相关单测通过。
2. 单测 fixture 覆盖：traework Start URL 参数表 + 回调换 token 建号 + 重放/过期拒绝；workbuddy Start→pending→成功建号；upstream 登录三端点（httptest）；qoder 不支持。
3. 前端：三渠道创建表单出现 oauth「粘贴凭证」；一键授权按钮仅 traework/workbuddy 显示。
4. 现有 workbuddy/traework/qoder 已有账号不受影响（网关/签到照常）。

## 8. 待办 / 风险

- **真实上游未联调**：授权回调 / 轮询换 token 以 fixture 验证；traework 已实测确认授权页硬校验 callback 为 `127.0.0.1:{port}/authorize`，非本机部署无法完成回调（前端已有守卫提示），须 SSH 端口转发或粘贴 auth JSON。
- **Qoder 一键授权**：原项目无登录实现，如需支持须自行逆向 qoderwork2api 的 OAuth 协议（独立后续项）。
- **state 存储**：进程内存，多实例部署下回调/轮询命中非持有实例会失败；正式多实例需 Redis。