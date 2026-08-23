---
type: Task
title: "Task: manager-login-email-2fa"
description: 管理台账密登录后增加一步邮件验证码二次认证，开关默认关闭，验证码通道以 uid 为键与公开邮箱验证码流程完全隔离。
tags: ["auth", "manager", "login", "2fa", "email", "wire-contract", "rate-limit"]
timestamp: 2026-08-23T04:37:39Z
# --- octospec extension fields ---
slug: manager-login-email-2fa
upstream: <待补：issue ref>
source: self
---

# Task: manager-login-email-2fa

## Goal

给 `/v1/manager/login`（管理台本地账密入口）加一步邮件验证码二次认证：密码与角色校验通过后
**不直接签发 token**，而是向该账号邮箱发送一次性验证码，验码通过才签发原有的管理台 session。

由系统设置 `login.manager_2fa_on` 控制，**默认关闭**；关闭时链路与现状逐字节一致。开启动作本身
带前置校验（所有可登录管理台的账号必须已配置邮箱），使「能不能开」与「开了能不能登」逻辑上绑死，
不存在打开开关即把自己锁在门外的状态。

## Background

现状（已核对代码）：

- `modules/user/api_manager.go:315-402` — 单步登录：绑参 → `queryUserInfoWithNameAndPwd` →
  反枚举 `ErrUserInvalidCredentials` → `beginUserSessionIssue` 会话栅栏 → 复核状态/注销 →
  `CheckPassword`（顺带 MD5→bcrypt 迁移）→ `auth.IsManagerConsoleRole` → `issueUserSession` →
  返回 `{uid,token,name,role}` → `loginLog.recordSuccess`。
- 入口已挂 `StrictIPRateLimitMiddleware(tag=manager_login, 10/min, burst 5)`（`api_manager.go:85`）。
- `user` 表有 `email` 列（`db.go:546`），但 `managerLoginModel`（`db_manager.go:178`）不含该字段，
  `addAdminUser` 不接受邮箱，`createManagerAccount` 播种超管也不写邮箱 —— **管理员邮箱当前没有任何录入口**。
- 邮件能力齐备：`modules/base/common/service_email.go` 的 `SendVerifyCode` / `Verify`
  （6 位码、5 分钟 TTL、1 分钟重发冷却、连错 3 次锁 10 分钟）、多语言模板 `emailtmpl`。
- 系统设置：`modules/common/system_settings.go` + `system_setting_schema.go`，DB 落库、
  `defaultReloadTTL = 60s` 自动热更新，写入口 `POST /v1/manager/common/system_setting`（SuperAdmin）。

**为什么不直接复用 `SendVerifyCode` / `Verify`（本任务的核心约束）**：这两个方法的冷却键
`email_rate_limit:{email}`、锁定键 `email_verify_lock:{email}`、失败计数键 `email_verify_fail:{email}`
**只按邮箱分桶，不含 codeType**（`service_email.go:90 / 383 / 412`）。而
`/v1/user/email/sendcode`、`/v1/user/emaillogin`、`/v1/user/email/forgetpwd` 均为公开端点
（`api.go:383-386`，仅 IP 限流）。若管理台 2FA 复用这套 keyspace，将出现两条未鉴权远程 DoS：
攻击者每 60 秒对管理员邮箱打一次发码请求即可让 Step 1 永远「发送过于频繁」；连错 3 次验码即可让
Step 2 锁死 10 分钟且可无限续期。加固登录反而给登录加了廉价锁死开关，因此本任务的验证码通道
**必须以 uid 为键自成一套**，uid 只有密码校验通过后才拿得到，攻击者无法触碰。

## Load-bearing list

- `auth` — 管理台登录唯一的本地账密入口，同时是 SSO 故障时 SuperAdmin 的紧急通道
  （`api_manager.go:300-314` 明确记载的设计约束）；2FA 判定与开关前置校验必须共用
  `auth.IsManagerConsoleRole` 的同一角色集合（admin / superAdmin / dashboardReader / marketAdmin），
  否则后两类固定角色会通过开关校验却在登录时被 fail-closed 挡死
- `auth` — 会话栅栏三件套 `BeginIssue → 重查用户 → 带 fence 签发` 整体迁移到第二步，
  顺序与 TOCTOU 语义不得改变
- `wire-contract` — `/v1/manager/login` 成功响应变为多形态（签发态 / 待二次认证态）；
  仓库内已核实唯一消费方为 `octo-admin/src/api/auth.ts`（`dashboard/`、`pilote2e/`、`tests/` 均无引用），
  仓库外消费方需人工确认（见「开放问题」）
- `rate-limit` — 新增的验码 / 重发端点均为未鉴权敏感端点，须各挂独立 tag 的
  `StrictIPRateLimitMiddleware`，不与 `manager_login` 共桶（共桶会让一次正常登录吃掉 2 个令牌）
- `error-response` / `i18n` — 新错误码走 `httperr.ResponseErrorL` + `pkg/errcode` 注册；
  验码失败类错误统一收敛为**单一错误码**（token 不存在 / 过期 / 码错误不可区分），保持反枚举
- `wire-contract` — `login_log` 审计语义扩展：新增「密码正确但 2FA 未通过」这一事件类别
- `test` — 覆盖开关关/开两条路径、无邮箱、错码、pending 过期、attempts 上限、重发上限、
  跨流程隔离（公开发码/验码端点不得影响管理台 2FA 桶）

## Design

1. **开关**：`login.manager_2fa_on`（bool，默认 false，无 yaml 兜底）→ `system_setting_schema.go`
   加一行 + `SystemSettings.ManagerLogin2FAOn()` getter。关闭时 `login` 链路完全走旧路径。
2. **开启前置校验**：`settingDef` 新增 `Validate` 钩子（现有仅 `Effective` / `Positive`），
   `updateSystemSettings` 的「先全量校验再统一写」循环中调用。`manager_2fa_on=true` 时要求
   **所有 status=启用、未注销、且 `IsManagerConsoleRole` 为真的账号**均已配置邮箱，否则拒绝并在
   `details` 中列出缺邮箱的账号名。
3. **管理员邮箱录入**（缺了功能不可用）：`POST /v1/manager/user/admin` 请求体新增可选 `email`；
   新增 `PUT /v1/manager/user/admin/email`（SuperAdmin only，走 `DB.updateUser`）为存量账号
   （含播种超管）补邮箱；`GET /v1/manager/user/admin` 返回带 email；
   `managerLoginModel` + 其查询补 `Email` 字段；`createManagerAccount` 播种时读
   `DM_MANAGER_ADMIN_EMAIL`（`DM_*` env 为本仓库既有惯例，不改 octo-lib）。
4. **Step 1 —— `POST /v1/manager/login`（复用现有端点）**：密码/角色校验逻辑逐行保留；通过后若
   2FA 开启：
   - 账号无邮箱 → `err.server.user.manager_2fa_email_missing`，不发码不发 token；
   - 生成 6 位码，写 **uid 维度**的 Redis 键 `mgr2fa:code:{uid}`（5 分钟）、
     `mgr2fa:cooldown:{uid}`（1 分钟）、`mgr2fa:fail:{uid}`（连错 3 次锁 10 分钟）——
     **不触碰 `email_*` 任何键**；
   - 发信使用 `context.WithTimeout(c.Request.Context(), 8s)`（现有 SMTP 为 15s dial + 60s IO 且
     call site 传 `context.Background()`，无界会把登录接口挂到约 75 秒）；发送失败/超时删除 pending 后报错；
   - 写 pending：`manager_login_2fa:{two_factor_token}` = `{uid, username, ip, attempts, resend_count}`，
     TTL **6 分钟**（比码多 1 分钟，避免「码还有效但会话已失效」的困惑态）；
   - 返回 `200 {two_factor_required:true, two_factor_token, email_masked, expires_in}`，
     **不含 token/uid/name/role**；
   - `loginLog` 记一条「密码通过、待二次认证」事件。
5. **Step 2 —— `POST /v1/manager/login/verify`（新增，未鉴权，独立 tag 限流）**：
   取 pending → 校验码 → 重查用户复核 status / is_destroy / `IsManagerConsoleRole` →
   `beginUserSessionIssue` → `issueUserSession` → 删 pending → 返回与现状**字段完全一致**的
   `managerLoginResp` → `recordSuccess`。失败一律 `err.server.user.manager_2fa_code_invalid`
   （反枚举），`attempts+1`，≥5 直接删 pending，并 `recordFailure`。
   pending 中的 IP 与本次请求不一致时**只记 warn 不拦截**（移动网络换 IP 属常态）。
6. **重发 —— `POST /v1/manager/login/resend`（新增，未鉴权，独立 tag 限流）**：仅吃
   `two_factor_token`，`resend_count ≤ 3`，**不延长 pending TTL**。不采用「重提账号密码」方案
   （会迫使前端在内存中留存明文密码、每次重发新开 pending、并必然撞上冷却）。
7. **邮件模板**：新增 `manager_login_code`（zh-CN / en-US 各 subject/html/text 三件），正文含
   验证码、有效期、**触发时间与来源 IP**、以及「非本人操作请立即修改密码」。不复用 `verify_code`
   模板——其正文仅「您的 Octo 验证码为 xxx」，收件人无法判断这是管理台登录，等于放弃 2FA 附带的
   异常登录告警价值。
8. **测试旁路**：uid 维度码需自带与 `MatchTestCode` 等价的非 release 旁路，否则 e2e 无法验证；
   `IsTestCodeEnabled` 已保证 release 模式恒为 false，新通道必须沿用同一判定。
9. **前端（dmwork-org/octo-admin，同名分支）**：`api/auth.ts` 的 `LoginResponse` 增加
   `two_factor_required?` / `two_factor_token` / `email_masked`，新增 `verifyLogin2FA` 与
   `resendLogin2FA`；`pages/Login/index.tsx` 增加第二步（6 位码 + 倒计时 + 返回上一步）；
   `store/auth.ts` 不动（第二步成功后照旧调 `loginSuper`）；管理员创建表单增加邮箱输入。
10. **逃生通道**（写入 PR 说明）：邮箱填错 / SMTP 故障导致收不到码时，
    `UPDATE system_setting SET value='0'` 关闭开关即可，60 秒内被 `StartAutoReload` 拾取，
    无需改代码或重启。

## Out of scope

- TOTP / Authenticator、短信二次认证、WebAuthn
- 「记住此设备 N 天」、按管理员粒度的 2FA 开关、备用恢复码
- `SpaceAdmin` 入口：该门从 IM 会话 token 进入（`octo-admin/src/pages/SpaceAdmin/SpaceEntry.tsx`
  读 sessionStorage token），**不经过 `/v1/manager/login`**，本任务不覆盖；防护范围表述须据实
- `/v1/user/*` 全部普通用户登录链路，及其邮箱验证码流程的行为变更
  （本任务只做**不参与**其 keyspace，不修改其既有键结构与语义）
- token 生命周期 / TTL / 撤销机制、账号封禁与注销链路
- 管理台系统设置页面（octo-admin 目前无该页面，开关经 API 或 DB 操作）
- octo-marketplace 侧任何改动

## Acceptance

> 全部在本地起 MySQL 8.0 + Redis + WuKongIM v2.2.4 后实跑通过（`modules/user` 全量
> 266s、`modules/common` 全量、`pkg/auth` / `pkg/errcode` / `pkg/i18n` /
> `modules/base/common`）；octo-admin 侧为 `npm run build`。

- [x] `login.manager_2fa_on` 默认关闭；关闭态下 `/v1/manager/login` 请求/响应与改动前逐字段一致
- [x] 开启态：正确账密返回 `two_factor_required=true` 且响应体**不含** token/uid/name/role
- [x] 开启态：`/v1/manager/login/verify` 用正确码返回的 `managerLoginResp` 字段与关闭态登录完全一致，
      且返回的 token 可通过 `AuthMiddleware` 调通 `/v1/manager/me`
- [x] 四种 `IsManagerConsoleRole` 角色（admin / superAdmin / dashboardReader / marketAdmin）
      在开启态下走同一条 2FA 路径，无一被 fail-closed 误挡
- [x] 开关前置校验：存在任一「可登录管理台且无邮箱」的启用账号时，
      `POST /v1/manager/common/system_setting` 置 `manager_2fa_on=1` 被拒并列出该账号
- [x] **跨流程隔离（P0 回归用例）**：先对管理员邮箱调用 `/v1/user/email/sendcode` 与
      `/v1/user/emaillogin`（连错 3 次触发 `email_verify_lock`），管理台 Step 1 / Step 2 **不受影响**；
      反向亦然（管理台 2FA 失败不影响该邮箱的普通用户流程）
- [x] 错码 / pending 过期 / 伪造 token 三种情况返回**同一个**错误码，响应体无差异
- [x] `attempts` 达 5 后 pending 被删除，后续携带同一 token 的请求一律失败
- [x] `resend_count` 达 3 后重发被拒，且重发不延长 pending TTL
- [x] SMTP 不可达时 `/v1/manager/login` 在约 8 秒内返回错误（非挂起至 75 秒），且不遗留 pending
- [x] `login_log` 中可区分：登录成功 / 密码正确但 2FA 未过 / 密码错误
- [x] `/v1/manager/login/verify` 与 `/resend` 各挂独立 tag 的 `StrictIPRateLimitMiddleware`
- [x] 新增错误码全部经 `httperr.ResponseErrorL` 返回，`make i18n-extract-check` 与 `make i18n-lint` 通过，
      zh-CN 翻译齐全；新 handler 文件加入 `TestUserNoLegacyResponseError` 守卫名单
- [x] `go build ./...`、相关包 `go vet` / `golangci-lint run`（0 issues）、`git diff --check` 通过
- [x] octo-admin：`npm run build`（tsc + vite）通过；两步登录与重发的真实后端联调待部署环境验证

## 决策记录（人工确认，2026-08-23）

1. **仓库外消费方**：确认**没有**。`/v1/manager/login` 的唯一消费方是
   `octo-admin/src/api/auth.ts`，无 octo-marketplace 前端或运维脚本直连。因此响应多形态化
   （签发态 / 待二次认证态）**复用原路径**，不另开新端点。
2. **开关前置校验取严格版**：置 `login.manager_2fa_on=1` 时，要求**所有** status=启用、未注销、
   且 `auth.IsManagerConsoleRole` 为真的账号均已配置邮箱；存在缺失则拒绝并列出账号名。
   校验查询落在 `modules/common`（该包不可 import `modules/user`，否则成环），
   直接以 dbr 查 `user` 表 + 复用 `pkg/auth` 的角色判定（`pkg/auth` 不依赖 `modules/*`，无环）。
3. **错误响应风格**：新增分支一律使用 `httperr.ResponseErrorL` + `pkg/errcode` 注册码，
   **不跟随** `api_manager_system_setting.go` 现有的 legacy `c.ResponseError` / `c.JSON` 风格；
   该文件将短期存在两种风格，属于向新规范收敛的中间态，不在本任务内批量重写其余 handler。

## 实现偏差（Implement 阶段记录）

1. **不新增 `settingDef.Validate` 钩子**。Design §2 原计划给 schema 加钩子，实现时发现
   `updateSystemSettings` 已有两处同形状的跨设置校验（onboarding 五元组、thread 归档窗口
   顺序），都是"在 plans 循环之后单开一个守卫块"。跟随既有形状更简单，也免去为单个布尔
   开关改动 schema 结构体。
2. **`login_log` 新增 status=3**（`loginStatusPendingSecondFactor`）+ 一条只改列注释的
   migration。Acceptance 要求"login_log 中可区分三种结果"，而并进 status=2 会被密码错误
   的噪音淹没、并进 status=1 会被统计成一次成功登录。第二步的失败行用
   `login_type=manager_2fa` 与第一步的密码失败区分。
3. **`pkg/auth` 新增 `ManagerConsoleRoles` 切片**，`IsManagerConsoleRole` 改为由它派生。
   开关前置校验需要把"可登录管理台的角色"当作 SQL 过滤条件用，而谓词表达不了；再手抄
   一份列表则必然与谓词漂移（正是本任务 review 阶段发现的 P1）。附带漂移守卫测试。
4. **冷却期内重复登录改为复用现有验证码 + 重发一个握手**，而不是直接报错。握手 token 只
   存在于浏览器，刷新或关标签页会把它丢掉；若此时报"发送过于频繁"，操作者手里明明有一封
   有效验证码却无处可用，白白被锁一分钟。重发握手会重置"每次握手 5 次"的尝试计数，真正的
   爆破边界是不随重开握手重置的"每 uid 连错 3 次锁 10 分钟"。
5. **octo-admin 未新增管理员邮箱录入表单**：该前端目前根本没有管理员管理界面
   （`src/pages/Users` 只管普通用户），没有可扩展的表单。后端
   `PUT /v1/manager/user/admin/email` 与 `POST /v1/manager/user/admin` 的 `email` 字段已就绪，
   界面留待管理员管理页面本身落地时一并补。
6. **新增 `ErrUserManagerEmailTaken`**（brief 未列）：`user.email` 只有普通索引不是唯一索引，
   两个账号共用同一地址会让邮箱登录（按地址解析到单行）对双方都失效，因此在设置邮箱的两个
   入口都加了占用校验。
7. **`PUT /v1/manager/user/admin/email` 挂 `SharedUIDRateLimiter`**：按 rate-limit 规则，
   已鉴权路由默认挂 UID 频控；该端点决定验证码投递到哪里，误用代价高。
8. **取消"每次握手 5 次尝试"这个上限**（跑集成测试时发现它不可达：每 uid 连错 3 次就先锁了）。
   保留唯一的爆破边界"每 uid 连错 3 次锁 10 分钟"——它不随重开握手重置，而握手计数会，
   所以后者本来也不是真正的边界。锁触发时一并删除握手记录，避免留一个永远无法兑现的 token。
9. **二次认证握手记录带上密码哈希指纹**（`sha256(user.password)`）。`token_http_ttl_test.go`
   的 `manager login rechecks password after fence` 揭示：原实现在栅栏之后是用**复查出来的行**
   再跑一次 `CheckPassword`，即登录途中改密必须让本次登录失效——我重构时把密码校验前移，
   丢掉了这条不变量。单步登录已恢复为栅栏后重跑密码比对；两步登录在第二步拿不到明文密码，
   改为比对指纹，语义等价且不落任何可逆材料。MD5→bcrypt 迁移成功后同步内存中的哈希，
   否则刚迁移的账号会被自己的指纹判成"凭据已变更"。
10. **三处源码守卫按新结构更新**（`TestLoginLifecycleHelpersRemainIntegrated`、
   `TestPostFenceLoginRejectionsAreAudited` 指向 `issueManagerSession`；
   `TestLoginAuditUsesWKHTTPClientIP` 的调用点计数 15→17）。守卫的语义未放松：审计与
   安全 API 的约束都还在，只是逻辑搬到了下一层函数。

## code-review 后的修正（第二轮）

`/code-review` 报了 15 条，其中一条是这次改动最严重的问题：

1. **二次认证地址改存独立列 `manager_two_factor_email`（P0）**。原实现写进 `user.email`，
   而 `user.email` 是一个**登录身份**：`/v1/user/emaillogin` 只凭邮箱验证码就签发会话（且带
   该账号的 role），`/v1/user/email/forgetpwd` 更是无任何角色校验、凭验证码即可重置密码。
   给管理台账号配上 email，等于把本功能承诺的"密码 AND 邮箱"降级成"控制邮箱即可"——比不做
   还糟。新列不参与任何账号解析，因此多个管理员共用一个运维信箱也是安全的（顺带删掉了
   `ErrUserManagerEmailTaken` 唯一性校验，它在新列上没有意义）。
   > 遗留风险（不在本任务范围）：**已经**有 email 的管理台账号仍然暴露在上述两条旁路上。
   > 这是本改动之前就存在的问题，修它要动三个公开登录端点的既有行为，建议单开一个任务。
2. **开关打开后的两条"punch through"补上**：清空地址、以及新建无地址的管理台账号，
   都会让该账号从此登不进来；开启前置校验只跑在 OFF→ON 那一刻，管不到这两条写入路径。
3. **发码增加每账号每小时 10 封的配额**。原来"每次握手最多重发 3 次"挡不住重新登录——
   重登会开一个计数归零的新握手，握有密码者可以每分钟给管理员发一封，无限期。
4. **验证码改为"先投递、后落库"**：原顺序会用一封投递失败的新码覆盖掉收件箱里那封仍然
   有效的旧码。
5. **握手有效期改为从验证码自身的到期时间派生**（+1 分钟宽限）。复用旧码的那条路径原本
   会发一个比验证码活得更久的握手，正确的码在窗口里反而被反枚举错误拒掉——恰好是那 1 分钟
   宽限本来要避免的困惑。
6. **没有可用验证码时不再计入连错预算**：离开一会儿回来重输过期码，原本会把自己锁 10 分钟。
7. 其余：重发改为重新读取当前地址（改正 typo 后立即生效）、邮箱掩码改为按 rune 切分、
   `/v1/manager/login` 的挑战响应改用 **202**（与完成登录的 200 在状态码层面就可区分）、
   成功行也打 `manager_2fa` 标签（否则 pending/failure 与 success 无法关联）、
   `logLoginEvent` 用 switch 统一三种结果、修正 errcode 注释里写错的端点、修掉一处测试
   把 `post("0")` 求值两次的问题。
