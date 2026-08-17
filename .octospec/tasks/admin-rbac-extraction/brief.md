---
type: Task
title: "Task: admin-rbac-extraction"
description: Extract admin-console authorization into a capability-based RBAC (roles become data, not code)
tags: [auth, acl, admin-console, rbac, wire-contract, multi-repo]
timestamp: 2026-08-17T04:20:56Z
# --- octospec extension fields ---
slug: admin-rbac-extraction
upstream: mininglamp-oss/octo-server#366
source: self
---

# Task: admin-rbac-extraction

## Goal

管理台的授权目前是 **role → capability 的硬编码派生**：`user.role` 只有四个取值
（`""` / `admin` / `superAdmin` / `dashboardReader`），`managerCapabilities()`
(`modules/user/api_manager.go:188-215`) 把它手工映射成一张 feature 布尔图，各端点
再各自调 `CheckLoginRole()` / `CheckLoginRoleIsSuperAdmin()`。

后果是**任何新的权限组合都必须改代码**：`dashboardReader` 就是为了"只看 Dashboard"
而写死进 `pkg/auth/manager_roles.go` 的固定角色。眼下的直接需求——建一个只有
`mcp.read/write` + `skill.read/write` 的管理员——在这套模型下无解：这四个键全部
`= isSuper`，而 superAdmin 连带 `system_setting` / `backup` / `users.write` /
`users.manage_admin` / `groups.write` / `space.destructive` / `expert.*`。

把授权抽取成 **capability-based RBAC**：capability 成为唯一真源，role 降级为
"capability 的打包别名"，存在数据库里而不是代码里。做完之后，"只有那四项权限的
管理员"= 建一个角色勾四个 capability，零代码改动。

这也是 `modules/user/api_manager.go:186` `TODO(#366 Part 2)` 记录的既定方向：
route→capability 的映射变成一张表，`managerCapabilities` 由它派生，前后端漂移
（前端渲染出后端会 403 的按钮，confused-deputy UI）从结构上消失。

## Background

### 现状链路

- 角色存储：`user.role VARCHAR(40)`（`modules/user/sql/20191106000003_user_legacy01.sql:15`）。
- 角色解析：`RoleService`（`modules/user/role_service.go`）→ Redis `user_role:{uid}`，
  TTL 60s，注入 `pkg/auth.CacheTokenParser` 作为 `RoleResolver`，使 token 里烘焙的
  role 不再活到过期。改 role 必须 `Invalidate(uid)`。
- 管理台闸门：`auth.IsManagerConsoleRole` 控登录（`api_manager.go:359`）与 `/me`
  （`api_manager.go:149`）；各端点 62 处 `CheckLoginRoleIsSuperAdmin()` + 38 处
  `CheckLoginRole()`，散在 24 个文件。`modules/space`、`modules/common`、
  `modules/opanalytics`、`modules/card_template_catalog` 已有本地
  `requireAdmin(c) bool` / `requireSuperAdmin(c) bool` 包装。
- 前端：`octo-admin/src/auth/capabilities.ts` 的 `MANAGER_CAPABILITY_KEYS` 与后端
  键名一一对应；`CapabilityRoute`（`App.tsx:37`）+ `MainLayout` 菜单据此渲染。
- 跨服务：MCP / Skill / Expert 管理页**不走 octo-server**，直连 octo-marketplace
  `/api/v1/admin/*`；marketplace 用同一个 Octo token 调
  `POST /v1/auth/verify?include=context`（`internal/auth/resolver.go`）拿到
  `model.Identity.Role`，然后在 `internal/middleware/admin.go:83` 硬编码
  `identity.Role != "superAdmin"` → 403。

### 三个已勘察确认的事实（决定了方案）

1. **marketplace 是一道门管三类资源。** 同一个 `AdminAuthenticator` 实例挂在
   mcps（`internal/api/router/router.go:243`）、skills（`internal/api/handler/skill/admin.go:16`）、
   skill_categories（`.../category/admin.go:16`）、experts + squads + expert_categories
   + expert_tags（`.../expert/admin.go:35,43,54`）、skill_uploads
   （`.../upload/handler.go:93`）上。因此"往白名单里加一个新固定角色"的做法会
   **连带放开 `expert.*`**，与需求直接冲突。这是不走"再加一个固定角色"而走 RBAC
   抽取的决定性理由。
2. **但路径按资源干净分段，拆得开。** mcp / skill / expert 三组路径无交叠；expert
   的上传走自己的 `/admin/expert_skill_uploads`，不复用 skill 的 `/admin/skill_uploads`；
   octo-admin 页面也不交叉 import（`ExpertMarket/*` 只用 `api/expert`，
   `SkillMarket`+`SystemSkill` 只用 `api/skill`，`SystemMcp` 只用 `api/mcp`）。
   唯一的 expert→skill 调用在 `internal/service/expert/install.go`，那是终端用户
   安装专家到 fleet 的**公开面**，不经过 admin 门。
3. **吊销延迟是两层缓存之和。** marketplace 自己有按 token 的 identity 缓存
   （`AUTH_CACHE_TTL` 默认 30s，`internal/config/config.go:118`），叠加 octo-server 的
   `user_role:{uid}` 60s → 收回一个管理员的 marketplace 权限最坏 **~90s** 才生效。
   这个窗口**现状即存在**，本任务不使其变差，但必须显式记录并纳入验收。

## 设计要点

### 数据模型

**三张表全部新增，仓内无可复用者。** 现存的 `space_member.role`（smallint 0/1/2）、
`group_member.role` 是**业务**角色（Out of scope，明确不得复用）；`user.role` 是唯一
的管理台角色存储，而它就是本任务要取代的那个单值列。

```sql
admin_role            (role_key PK, name, is_builtin, created_at)   -- 角色定义
admin_role_capability (role_key, capability)                        -- 角色 → 能力（仅自定义角色）
user_admin_role       (uid, role_key, granted_by, granted_at)       -- 用户 → 角色（多对多）
```

- capability 字符串**直接复用前端 `MANAGER_CAPABILITY_KEYS` 的同名同义键**
  （`mcp.read` / `skill.write` / …），前端零改动。

#### 扩展性决定（这几条会长期锁住表结构）

对照 NIST RBAC 分层定义、Kubernetes RBAC、AWS IAM 的已知陷阱后定稿。

- **内建角色存成数据，启动时 reconcile**（k8s 对内建 ClusterRole 的做法）。不采用
  "代码定义能力集、DB 只存存在性"的变体：那会让 resolver 分叉成两条路径（内建走
  代码 map、自定义走 DB），行为迟早漂移，且内建角色无法被同一个 list 端点查出来。
  reconcile 同时解决"用户改过内建角色"的收敛问题。**reconcile 失败必须拒绝启动**，
  与 `createManagerAccount`（`api_manager.go:1576-1582`）宁可不建超管也不建弱口令
  超管的 fail-closed 姿态一致。
- **`superAdmin` 的全集用一行通配 `capability='*'` 表达**，而不是在 resolver 里特判
  `role_key`。"动态全集"因此落在数据模型内，resolver 只需在展开时认识 `*`。
  否则新增 capability 后**超管反而没有它**，是个静默的权限黑洞。
- **只允许全通配 `*`，明确拒绝前缀通配（`mcp.*`）**。前缀通配是 AWS IAM 的已知
  陷阱：会把下个版本新增的同前缀权限静默吸收，授权时无人同意过。自定义角色创建
  校验直接拒 `*`。
- **只有 allow，没有 deny**：多角色取并集，不支持"某角色但排除 X"。NIST RBAC 与
  k8s 均为纯 additive；引入 deny 会让求值变成顺序相关，"这个人能干什么"从一个并集
  变成一段需调试的逻辑。后续此类需求一律以"新建角色"满足。
- **角色扁平，不做继承**。NIST 将 hierarchical RBAC 单列一层自有其代价：菱形继承
  使权限来源无法回溯。多角色并集已覆盖绝大部分场景。
- **`role_key` 不可变**（显示名 `name` 可改），因此可安全地做自然主键。
- **未注册的 capability 必须被忽略，不是报错**：DB 里可能存在代码里已不存在的
  capability（版本回滚、capability 改名）。resolver 遇到未注册键跳过并打 debug 日志；
  若改成报错，一次回滚就会让所有自定义角色全部失效。
- **行式存储而非 JSON 数组列**：`(role_key, capability)` 一行一能力，才能反查
  "谁有 `mcp.write`"、加索引、将来挂 per-grant 元数据。
- **`granted_by` / `granted_at` 直接进表**，不依赖事后翻日志。
- **不预留 `scope_type` / `scope_id`（不做作用域化角色）**：管理台角色目前是全局的，
  而"某人只管某个 Space"属于业务权限边界（`space_member.role`），现在没有正确的
  设计依据。k8s 对此的解法是 Role/ClusterRole **两套绑定表**（RoleBinding vs
  ClusterRoleBinding），不是一张表加一个可空 scope 列——将来真要做，正确的迁移本来
  就不是加列，预留亦无用。同理不预留多租户维度。
- **不用外键**，角色删除时在应用层事务里级联清理 `admin_role_capability` +
  `user_admin_role`（仓内 199 个迁移中仅 3 个使用外键，与既有风格一致）。
- `role_key` VARCHAR(64)，约束 `^[a-z][a-z0-9_]{0,63}$`；内建 key 保留，自定义角色
  不得占用。
- **`user.role` 列保留不动**，改由 RBAC **反写的派生投影**维护：持有内建
  `superAdmin` 角色 → `'superAdmin'`；持有任何含 admin 档能力的角色 → `'admin'`；
  否则 `''`。这是能在**不改 octo-lib**（`CheckLoginRole*` 是 octo-lib 的
  `wkhttp.Context` 方法）、不动老 token、不动 marketplace 的前提下平滑迁移的关键，
  同时也是 admin 权限与业务权限之间的隔离带（见 Out of scope）。投影只降不升。
- 内建角色 seed：`superAdmin`（全集，不可编辑/不可删）、`admin`、`dashboardReader`。
  **`dashboardReader` 由代码常量降级为一行数据**——这正是本任务要消除的那类硬编码。

### 新增包 `pkg/authz/`

| 文件 | 内容 |
|---|---|
| `capability.go` | capability 常量注册表（唯一真源）+ `Register()` 防拼写漂移；导出 `All()` 供契约测试与前端键对齐 |
| `policy.go` | route→capability 映射表 + `Guard(c, cap) bool` |
| `resolver.go` | `Resolve(ctx, uid) (Set, error)`，Redis 热缓存，仿 `RoleService` |
| `sql/` | 建表 + seed + 从 `user.role` 回填 `user_admin_role` |

`Guard` 刻意做成 `bool` 返回，与现有 `requireSuperAdmin(c) bool`
（`modules/space/api_manager.go:174`）形状一致，call-site 改动是一行替换：

```go
if !m.requireSuperAdmin(c) { return }          // 旧
if !authz.Guard(c, authz.McpWrite) { return }  // 新
```

`pkg/auth/manager_roles.go` 最终整个删除。

### 缓存与失效

- 新增 `user_caps:{uid}` 热缓存，TTL 与 `RoleCacheTTL` 同量级（60s）——这是管理员
  提权的兜底窗口，不得放长。
- **失效面比 role 大**：改一个角色的能力集会影响所有持有该角色的用户，不能遍历
  用户删 key。做法是缓存值里带一个全局 `role_version`，角色定义变更时 bump，所有
  用户缓存自然失效。

### 失败模式：fail-closed（已确认）

`RoleResolver` 出错是 fail-open（保留 token 快照），保的是**认证**——DB 抖动不该把
所有人踢下线。capability 判的是**授权**，风险方向相反，取 **fail-closed**。

分层定义"失败"，可用性代价才≈0：

- **Redis 不可用不算失败**：穿透到 MySQL，与 `RoleService` 同构（`role_service.go:93`）。
  这是最常见的单点故障，必须保持完全可用。
- **只有 Redis + MySQL 都取不到**才是 resolve error → 拒绝。而此时 handler 自身的
  业务查询同样会失败，所以 fail-closed 的实际代价只存在于"角色查询失败但同一
  handler 的业务查询成功"这个几乎不可达的窗口里；fail-open 的代价则是实打实的
  **被撤销的管理员在可诱发 DB 错误时保住 superAdmin**。

**Carve-out**：`/v1/manager/me` 在 resolve error 时必须**返回错误**，不得返回全
false 的 capability 图——全 false 会被 `firstManagerPath` 判成 `/no-access`
（`octo-admin/src/auth/capabilities.ts:73`），把一次故障呈现成"你的权限被收了"。
前端已有正确的失败态（`MainLayout.tsx:360` `managerProfileFailed` → "加载失败"），
`/me` 老实报错即可落进去。

### 交付阶段

本 brief 是整个抽取的**程序级 spec**；实施拆成多个 PR / 后续 task：

| Phase | 内容 | 行为变更 |
|---|---|---|
| **0**（本 task 首个 PR） | 建表 + seed + 回填；`pkg/authz` 骨架；`managerCapabilities()` 改为 authz 派生，同时用旧逻辑算一遍做 **shadow 对比**，不一致打 warn | **零** |
| **1**（逐模块，10+ PR） | 把 `CheckLoginRole*` / `require*Admin` 换成 `authz.Guard`；每个端点登记进 `policy.go`；每模块配源码守卫测试 | 无（等价替换） |
| **2** | 角色管理 API（建角色 / 勾 capability / 授予撤销）；`dashboardReader` 端点降级为兼容 shim。**只做端点，不做 UI**（已确认） | 新增面 |
| **3**（跨仓） | `/v1/auth/verify` 加 `capabilities []string`；marketplace 按资源组挂 capability 门 + 契约钉死 | 跨仓 |

Phase 2 不做 UI 的后果已评估：**目标账号本身零前端改动即可用**——`mcp.read` /
`skill.read` 的菜单项（`MainLayout.tsx:126-131`）、路由（`App.tsx:212-227`）、落地页
（`capabilities.ts:70` → `/system-mcp`）都已接好。缺 UI 的只是**授予流程**，需调
API 完成，与今天的 `dashboardReader` 现状一致。

Phase 1 先啃已有本地 helper 的模块（space / common / opanalytics /
card_template_catalog，改 helper 内部即可，call site 不动），再啃直接调
`c.CheckLoginRole()` 的（user / group / message / robot / backup / report /
workplace / statistics / integration / app_bot）。

## Load-bearing list

- `auth` / `acl` — 管理台授权判定本身，安全边界。任何 capability 解析失败必须
  fail-closed；Phase 1 的等价替换不得放宽任何一个端点的现有档位。
- `auth` — `pkg/auth.CacheTokenParser` / `RoleResolver` 注入链路，以及
  `IsManagerConsoleRole` 控制的**登录准入**（`api_manager.go:359`）。放宽它等于
  放开管理台登录面，必须与 capability 判定分开考虑。
- `wire-contract` — `/v1/manager/me` 的 `capabilities` 布尔图是与 octo-admin 的
  稳定约定（`octo-admin/src/auth/capabilities.ts`）。Phase 0 必须逐角色 byte-identical。
- `wire-contract` — `/v1/auth/verify` 的 `authVerifyTokenResp`
  （`modules/user/api.go:4707`）：该 struct 注释明确要求默认响应形状对老 IM 客户端
  字节兼容，Phase 3 只能加 `omitempty` 字段，`role` 一个字不能动。
- `user.role` 列的现有读者：octo-lib `CheckLoginRole*`、marketplace
  `AdminAuthenticator`、`queryUserWithNameAndRole`、`addAdminUser`
  （`api_manager.go:796` 硬编码写 `admin`）、`dashboardReaderRoleTransition` 的 CAS
  （`db_manager.go:166`，防并发覆盖 superAdmin）。派生投影不得破坏其中任何一个。
- 角色缓存链路 `RoleService` + Redis `user_role:{uid}` TTL 60s，以及"改 role 必须
  Invalidate"这条不变量（`role_service.go:29-34`）。
- marketplace `AdminAuthenticator` 与 octo-server 之间**靠约定而非 import 的耦合**
  （`internal/middleware/admin.go:11-17` 自述：octo-server 改名即静默 403）。Phase 3
  会把待对齐的字符串从 1 个（`superAdmin`）扩大到 10+ 个 capability 键，每个对应
  一组路由，写错即该组静默 403。因此**契约钉死是 Phase 3 的前置条件，不是可选项**：
  octo-server 暴露 `authz.All()` 的只读契约，marketplace 启动时断言自己用到的每个
  capability 都在其中，缺失则拒绝启动——把运行时静默 403 变成启动期/CI 期的响亮失败。
- marketplace `AdminAuthenticator` 的 dev bypass（`admin.go:68-72`，`AUTH_ENABLED=false`）
  今天完全跳过角色检查；换成 capability 门后 dev identity 必须被 stamp 上全量
  capability，否则本地开发全线 403。
- `error-response` / `i18n` — 新增端点与新错误码走 `httperr.ResponseErrorL` +
  `pkg/errcode`；403 沿用通用 `ErrSharedForbidden`，**不得**按"缺哪个 capability"
  分化错误码（反枚举）。
- `rate-limit` — Phase 2 的角色管理端点须在 `AuthMiddleware` 之后挂
  `SharedUIDRateLimiter`。
- 提权面（Phase 2 新增，本身即安全边界）：授予/建角色的自我提权、最后一个
  superAdmin 的撤销。

## Out of scope

**业务权限一律不动。** 判据：*"这个人在管理台里能点什么" = admin RBAC（in scope）；
"这个人在自己的空间 / 群 / 机器人里能做什么" = 业务权限（out of scope）*。

- 空间成员角色与空间隔离：`pkg/space/middleware.go`、`membership.go`、`member_role` 列。
- octo-admin 中 `scope === 'space'` 的整个半边（`SpaceAdmin/*`、`SpaceEntry`、
  `/space/my`）——那是空间管理员，身份来自空间成员关系，不是管理台角色。
- 群主 / 群管理员、bot ownership（`modules/bot_api`）、thread 父频道访问、matter access。
- `pkg/authtree` 的两棵凭证树（User API Key `uk_*` / Bot token `bf_*`、`app_*`）——
  凭证边界，不是人的角色。
- `/v1/auth/verify` 响应中 `owned_bots` / `spaces` / `owned_bots_by_space` 的语义。
- **不改 octo-lib**：`CheckLoginRole` / `CheckLoginRoleIsSuperAdmin` 保持原样，靠
  `user.role` 派生投影兼容。
- **不删 `user.role` 列**，不改其取值集合。
- App Bots 菜单的可见性（`MainLayout.tsx:119` 走 `GET /v1/app_bot/available` 数据探测，
  `modules/app_bot/app_bot.go:147` 只校验登录不校验角色）——现状保留，另行决定。
- 现有 ~90s 吊销窗口的**改造**（本任务只记录并断言其不变差）。
- **角色管理 UI**（建角色 / 勾 capability / 授予撤销的前端界面）——Phase 2 只做端点，
  UI 另起 task。目标账号的**使用**面不受影响（见"交付阶段"）。
- **作用域化角色**（管理台角色绑定到某个 Space / 租户）与多租户维度——见"扩展性决定"。

### 单模块内的文件级边界（Phase 1 必须遵守）

`modules/space`、`modules/group`、`modules/message` 在同一模块内混有两种权限：
`api_manager*.go` 是跨空间的**管理台**权限（in scope），`api.go` 里的成员校验是
**业务**权限（out of scope）。Phase 1 只能按文件切，不能按模块切。

## Acceptance

### Phase 0（零行为变更，必须能证明）

- `managerCapabilities()` 对 `""` / `admin` / `superAdmin` / `dashboardReader`
  四种角色的输出与旧实现**逐键相等**（表驱动测试，扩展
  `modules/user/api_manager_me_test.go`）。
- shadow 对比：新旧结果不一致时打 warn 日志并**以旧结果为准**返回；灰度期
  mismatch 计数为 0 方可进入 Phase 1。
- 回填正确性：对每个现存 `user.role != ''` 的 uid，`user_admin_role` 有对应行，且
  派生投影回算出的 `user.role` 与原值相等（迁移测试）。
- `user.role` 的取值集合不变；octo-lib `CheckLoginRole*` 行为不变。

### 贯穿全程

- capability 解析失败 → **拒绝**请求（fail-closed），有测试覆盖；与 `RoleResolver`
  的 fail-open 行为区分开，两者各有断言。
- **Redis 单独不可用不得触发 fail-closed**：穿透 MySQL 后请求正常完成（测试覆盖）。
- `/v1/manager/me` 在 resolve error 时返回**错误**而非全 false 的 capability 图
  （测试断言响应不是 `{}`/全 false），使前端落进 `managerProfileFailed` 而非 `/no-access`。
- `superAdmin` 的能力集是**动态全集**（经 `capability='*'` 通配行）：新注册一个
  capability 后，超管**无需任何 data migration** 即拥有它（测试：注册一个测试用
  capability，断言超管为 true）。
- 自定义角色**不得**持有 `*`，且前缀通配（`mcp.*`）在创建/更新时被拒（测试覆盖）。
- 内建角色被人为改动后，重启 reconcile 将其恢复到定义态（测试）；reconcile 失败时
  进程**拒绝启动**而非降级。
- 多角色取并集，无 deny 语义（测试：两个角色的能力集合并后逐键断言）。
- DB 中存在**未注册**的 capability 时，resolver 跳过该键且其余能力正常解析
  （回滚安全性测试），不得整体报错。
- Phase 1 每替换一个端点，其**现有档位不变**：原 `CheckLoginRoleIsSuperAdmin` 的
  端点替换后仍只有 superAdmin 能过（逐端点回归测试）。
- Phase 1 每个模块配一条"本模块无裸 `CheckLoginRole*`"的源码守卫测试
  （照抄 `Test<Module>NoLegacyResponseError` 的形状）。
- 403 响应保持单一通用码（`ErrSharedForbidden`），不因缺失的 capability 不同而分化。
- `make i18n-extract-check` + `make i18n-lint` 通过；新错误码补 `active.zh-CN.toml`。
- `go build ./...`、相关模块 `go test`、`golangci-lint run` 通过。

### Phase 2（提权防护，安全验收）

- 非 superAdmin **不能**创建或授予一个自己不持有的 capability 集合（测试覆盖自我
  提权尝试）。
- 内建 `superAdmin` 角色不可编辑、不可删除。
- 撤销操作不得使系统中 superAdmin 数量归零（fail-closed，有测试）。
- 角色定义 / 授权变更写审计日志，含 `actor_uid` + `target_uid`（沿用
  `finishDashboardReaderRoleRequest`，`api_manager.go:529-549` 的形状）。
- 角色能力集变更后，`role_version` bump 使受影响用户的 `user_caps:{uid}` 在一个
  请求往返内失效（不依赖 TTL 自然过期）。
- `PUT/DELETE /v1/manager/user/:uid/dashboard-read` 作为兼容 shim 行为不变
  （内部改为授予内建 dashboardReader 角色），现有测试全绿。

### Phase 3（跨仓，端到端）

- `/v1/auth/verify` 默认响应（不带 `?include=context`）形状**逐字节不变**；
  `capabilities` 仅以 `omitempty` 追加。
- 端到端断言：一个只持 `mcp.*` + `skill.*` 的账号——
  - 可以读写 marketplace `/admin/mcps/*`、`/admin/skills/*`、`/admin/skill_categories/*`、
    `/admin/skill_uploads*`；
  - 对 `/admin/experts/*`、`/admin/squads/*`、`/admin/expert_categories/*`、
    `/admin/expert_tags`、`/admin/expert_skill_uploads` 一律 403；
  - octo-admin 侧不渲染 Expert Market 菜单与路由（`expert.read` 为 false）。
- 老 octo-server（不返回 `capabilities`）+ 新 marketplace 的组合下，回退到
  `role == superAdmin` 判定，行为与今天一致。
- 吊销窗口：显式记录 octo-server 60s + marketplace `AUTH_CACHE_TTL` 30s = 最坏
  ~90s，并断言本任务**不使其变大**。
