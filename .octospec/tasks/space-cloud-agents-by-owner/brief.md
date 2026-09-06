---
type: Task
title: "Task: space-cloud-agents-by-owner"
description: 通讯录改版：新增 GET /v1/space/directory，返回 Space 内全部活跃真人及各自名下的云端 AI 分身（排除本地 self_hosted），不分页，每人最多返回 50 个分身明细并保留真实计数。
tags: ["space", "isolation", "wire-contract", "error-response", "rate-limit", "testing", "commit"]
timestamp: 2026-09-06T00:00:00+08:00
slug: space-cloud-agents-by-owner
upstream: 通讯录改版原型（口头需求，无 issue）
source: user
---

# Task: space-cloud-agents-by-owner

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

通讯录改版后是两列：左列 Space 内的真人，右列**该真人名下的 AI 分身**，且**只展示云端分身，
排除本地运行的**（本地固定标识 `agent_hosting = 'self_hosted'`）。

今天客户端拼不出这个视图，缺的是**「真人 → 云端分身」的归属关系**：

- `GET /v1/space/:space_id/members`（`modules/space/api.go:694`）的 `memberResp` 只有
  `uid/name/role/robot/created_at`，**没有 owner 字段**；而且它对 bot 做了收窄过滤
  —— `modules/space/db.go:374` 的 `r.robot_id IS NULL OR r.creator_uid = ?`
  只返回**自己创建的** bot，看不到同事的分身。
- `GET /v1/robot/space_bots`（`modules/robot/api.go:1755`）有 `creator_uid` / `creator_name`，
  但**不带 hosting**，分不出本地/云端。
- `GET /v1/user/bots`（`modules/botfather/api_user.go:287`）是唯一下发 `agent_hosting` 的读面，
  但它走 **User API Key（`uk_` 前缀）+ owner-scoped** 认证，通讯录页用 IM session token
  进不去，而且只能看自己的。

新增端点，一次给出通讯录页所需的全部数据：

```
GET /v1/space/directory?space_id=<id>[&only_with_agents=true]
```

```json
{ "data": [
  { "uid": "u_wang", "name": "王宜林", "role": 0,
    "agent_count": 2, "agents_truncated": false,
    "agents": [
      { "uid": "bot_a", "name": "执剑人", "description": "…", "is_friend": true,
        "hosting": "octo_hosted", "hosting_reported_at": "2026-09-03 10:00:00" },
      { "uid": "bot_b", "name": "飞行员E号", "description": "", "is_friend": false,
        "hosting": "", "hosting_reported_at": null }
    ] },
  { "uid": "u_lin", "name": "林晓", "role": 0,
    "agent_count": 0, "agents_truncated": false, "agents": [] }
] }
```

**顶层是 Space 内的全部活跃真人**，无论名下有没有云端分身（无分身者 `agents: []`、
`agent_count: 0`）。`only_with_agents=true` 时只返回有云端分身的人，默认 `false`。
活跃真人要求 `space_member.status=1`、`user.robot=0`、`user.status=1`、
`user.is_destroy<>2`，并排除 `pkg/space.SystemBots` 中的系统账号。

**顶层只有真人，bot 只出现在 `agents` 里。** 这与 `members` 不同 —— 后者会把
「自己创建的 bot」当成独立成员行返回。改版意图正是把 AI 收进右列，所以这个差异是需要的。

**不分页、不做服务端拼音排序、不新增结果缓存或迁移。** 拼音排序与 A-Z 字母索引仍由
前端负责；每人的分身仅按稳定键选取最多 50 个明细，规则见下文。

**默认包含调用者自己，即使其名下没有云端分身。** `only_with_agents=true` 时，调用者
与其他真人使用同一过滤规则。服务端不额外按 viewer 排除自己；前端可按展示需要处理。

## Background

### 为什么不做服务端分页

当前通讯录在客户端拿到全量名册后做拼音排序和 A-Z 字母索引
（`packages/dmworkcontacts/src/Contacts/index.tsx` 的 `fetchAllSpaceMembers`，
10000/页 × 20 页）。引入服务端分页需要同时处理全局排序与字母索引，不能只给本接口
加一个游标就保持现有体验。两者在技术上可以兼容，但本任务不调整这套架构。

`SpaceService` 注释提到已有 5760 人的 Space，这是既有规模的参考，不是本接口已完成
性能验证的结论。本任务保留全量真人读取，只对新增的分身明细设置每人上限。

### 前端接入说明

本接口按可独立供给通讯录的数据源设计，保留 `name` / `role` 和 `is_friend`。前端接入时
可替换通讯录页的 `members` 请求；是否合并其他 Bot 列表请求，由前端按实际展示范围处理。
本任务只交付服务端契约，不以前端切换为验收前提，也不承诺请求数已经从 4 降到 2。

名字直接复用 `MemberDetailModel.DisplayName()` 的兜底链（`user.name` →
`user_verification.real_name` → 稳定占位符），`user_verification` 的 JOIN 兼容性见
Load-bearing 中的 collation 说明。

`members` 端点本身不动，它还被转发面板、Chat 侧栏、docs 成员选择器、企业模块使用。

### 为什么过滤规则是 `<> 'self_hosted'`

`agent_hosting` 是 `NOT NULL DEFAULT ''`（`modules/botfather/sql/20260903000001_botfather_agent_hosting.sql`），
所以没有 NULL 陷阱。本地运行**固定标识为 `self_hosted`**，因此「排除本地」= `<> 'self_hosted'`。
这条规则会把迁移注释定义的两种空串状态都纳入展示，但不代表已确认其为云端：

- `('', NULL)` = 从未上报 → 包含。存量 bot 不会因为没上报过就整批消失。
- `('', 非 NULL)` = 曾上报后被显式撤回 → 包含。

行为自愈：bot 一旦上报 `self_hosted`，下次请求就退出列表。

**这条过滤是展示过滤，不是安全边界。** `agent_hosting` 由 bot 通过
`POST /v1/bot/register` 自报，持有该 bot `bf_` token 的任何调用方都能随意填 —— 该迁移
注释已明确写「不可用于鉴权」。一个 self_hosted 的 bot 谎报即可进入本列表。服务端目前
**没有**权威的本地/云端判定：两条创建路径都不落 hosting 标记（`MintBotOBO`
在 `modules/botfather/mint_obo.go:33`，`createUserBot` 在 `modules/botfather/api_user.go:127`）。
本任务按需求方决定**不引入**权威列，接受自报值。这一点必须写进代码注释，避免后续有人
拿它当隔离依据。

### `space_id` 必须走 query，不能走 path param

`pkg/space/middleware.go:157` 的 `SpaceMiddleware` 只从 **query `space_id`** 或
**header `X-Space-ID`** 取值，**且取不到时 `c.Next()` 直接放行（fail-open）**。所以
`/v1/space/:space_id/directory` 这种形式中间件完全不生效。端点用 query 传参
（与 `/robot/space_bots` 一致），并且 handler **仍要自己挡 `space_id == ""`**，
否则空值会穿透中间件。

### 每人最多 50 个明细，计数保留真实值

当前**没有任何 per-user bot 数量配额** —— `createUserBot` 与 `mintBot` 都不校验数量，
请求频率限制也不能限制累计创建数量。因此每人返回的 `agents` 最多 **50** 个，
`agent_count` 为该 owner 在当前 Space 内符合全部过滤条件的真实总数，
`agents_truncated = agent_count > 50`。计数和明细必须使用相同的过滤条件。

**上限必须在 SQL 返回明细之前生效，禁止全量 `Load` 到 Go 后再截断。** 按
`robot_id ASC` 选取前 50 个，保证数据未变化时选出的集合稳定；这不是面向用户的拼音排序。
该上限约束 DB 到 Go 的明细传输和应用内存，不消除数据库为真实计数而扫描候选行的成本，
也不把全量真人请求变成固定成本请求。

### 查询分两段，不用一条 LEFT JOIN

两段查询分别读取真人与有界分身明细，避免重复传输真人字段：

1. **全量活跃真人**：`space_member` 驱动（`spacemember_spaceid_status`）JOIN `user`
   （`robot=0` / `status=1` / `is_destroy<>2`），限定 `space_id`、`sm.status=1`，
   排除 `spacepkg.SystemBotList()` 返回的 UID，再 LEFT JOIN `user_verification` 取兜底名。
   **不能仅凭 `robot=0` 判断真人**：`fileHelper` 的迁移预置记录就是 `robot=0`，且现有
   加成员接口允许将其加入 Space。系统 UID 从现有共享清单获取，不在此模块另写名单。
2. **分身计数与最多 50 个明细**：候选集由当前 Space 的活跃 `space_member` JOIN
   `robot`（`status=1` 且 `agent_hosting <> 'self_hosted'`）和 `user`（`robot=1`）得到，
   同时 JOIN owner 在**同一 Space** 的活跃成员及 user 行。owner 使用第一段相同的
   真人过滤条件，系统账号不作为 owner 或分身返回。在这个候选集上使用 MySQL 8 窗口函数：
   `COUNT(*) OVER (PARTITION BY creator_uid)` 得到真实计数，
   `ROW_NUMBER() OVER (PARTITION BY creator_uid ORDER BY robot_id ASC)` 得到序号 `rn`，
   外层查询限定 `rn <= 50` 后再返回明细。

窗口计算尽量只携带 owner UID、Bot UID 等必要列；分身名称、简介等明细在选出前 50 个后
读取。好友关系以 `friend.uid=loginUID AND friend.to_uid=botUID AND is_deleted=0`
LEFT JOIN 到选出的明细，得到 `is_friend`，不按人或 Bot 发起 N+1 查询。

Go 侧按 `creator_uid` 挂到第一段的人身上。无分身的人使用 `agents: []`、`agent_count: 0`；
`only_with_agents=true` 最后按计数过滤。owner 不在真人名单中的记录丢弃，不生成无主行。
两段读取不承诺跨查询快照；同一 owner 的计数和分身选取在第二条 SQL 内完成。

沿用现有 handler 的 context 超时模式（参见 `modules/space/api_welcome.go`）：从
`c.Request.Context()` 派生 **3 秒**预算，两段查询共用该 context，通过 `LoadContext`
等方法传入数据库调用。3 秒是本任务的实现默认值，不是压测结论。任一查询失败或超时，
记录 `zap.Error` 并返回 `ErrSpaceQueryFailed`；不返回半份目录，也不把好友查询失败当作
`is_friend=false`。

## Load-bearing list

- **`space` / `isolation`** — 新端点读 Space 内**全部**成员及其 bot 归属关系，是新增的
  跨成员读取面。鉴权链：`AuthMiddleware` → `SharedUIDRateLimiter` → `SpaceMiddleware`
  →（handler 自查 `space_id` 非空）。
- **可见性口径变更（需求已确认）** — 本端点返回**所有人**的云端分身，与
  `modules/space/db.go:374` 的 `r.creator_uid = ?` 收窄口径**不一致**。等于确认
  「谁有几个云端 AI 分身」在 Space 内可枚举。与 `/robot/space_bots`（全 Space bot 可见，
  已挂 `SpaceMiddleware` + UID 限流）的口径一致，不是新开的枚举面。
- **`wire-contract`** — 新端点是**新增**契约，不改任何既有响应形状。`agent_hosting` /
  `agent_reported_hosting_at` 从此有了第二个读面（第一个是 `GET /v1/user/bots`），
  两处对空串三态的解释必须一致。
- **名字兜底链（issue #344）** — 顶层 `name` 必须与 `members` 的
  `MemberDetailModel.DisplayName()` 给出同样的结果（`u.name` → `user_verification.real_name`
  → 稳定占位符），否则同一个人在通讯录和其它成员列表里显示不同的名字。
  **禁止**用 `short_no` / `username` 兜底（privacy-gated）。
- **真人判定** — `robot=0` 之外还须排除 `spacepkg.SystemBotList()`；系统账号即使有
  活跃的 `space_member` 行，也不出现在顶层，不作为分身 owner。
- **collation** — 名字兜底需要 LEFT JOIN `user_verification`（强制 `utf8mb4_general_ci`），
  而 `space_member` 建表未显式 COLLATE，8.0 库上有 `1267` 隐患。等同于既有
  `queryMembers` 的风险面，非新增，但跑测试时要留意。
- **`robot.agent_hosting` 的语义依赖** — 本端点的过滤直接建立在这个**自报**字段上。
  若将来引入权威的 provision 来源列，过滤条件要一起改。
- **`error-response` / i18n** — 复用现成码 `ErrSpaceRequestInvalid` / `ErrSpaceNotMember` /
  `ErrSpaceQueryFailed`（`pkg/errcode/space.go`），走 `httperr.ResponseErrorL`（固定 400，
  与 space 模块其余 handler 一致）。**不新增错误码**。
  已知不一致（既有，本任务不修）：`SpaceMiddleware` 的 401/403 走
  `c.AbortWithStatusJSON`，不是 i18n 信封。
- **`rate-limit`** — 挂 `appwkhttp.SharedUIDRateLimiter`，必须在 `AuthMiddleware`
  **之后**，否则读不到 uid 会静默 fail-open。
- **孤儿分身会消失** — 分身要求 owner 是本 Space 的活跃真人。owner 已退出/已注销/
  `creator_uid` 为空的 bot 不出现。App Bot 天然不出现（`modules/app_bot/app_bot.go:1124`
  注释：App Bot 不进 `space_member`，也不在 `robot` 表）。既有 Bot 广场接口不改。
- **响应体量** — 顶层仍随 Space 真人数增长，分身明细每人最多 50 个；不能以“替代
  members”为由宣称响应体积或数据库开销不增加。不得全量加载分身后在 Go 截断。
- **索引** — 复用 `spacemember_spaceid_status(space_id, status)`、
  `spacemember_spaceid_uid(space_id, uid)`、`robot_id_robot_index`、`user.uid` 的唯一索引
  和 `friend(uid, to_uid)` 的唯一索引。窗口计算可能需要排序或临时表，实现时用 `EXPLAIN`
  核对实际 SQL，不能仅按 JOIN 的书写顺序断言执行计划。**本任务不新增索引或迁移。**
- **异常与观测** — 复用现有 HTTP 指标和常规错误日志；查询携带超时和取消信号。
  不为此接口单独建设指标、响应大小采集或截断次数计数器。
- **`testing`** — 新增 handler 文件要加进 `modules/space/api_i18n_test.go:27`
  的 `TestSpaceNoLegacyResponseError` 文件清单。

## Out of scope

- **不做服务端分页**、不做 Redis 物化有序名册、不做游标分页、不改前端拼音排序逻辑。
  理由见 Background。
- **不改 `GET /v1/space/:space_id/members`** —— 包括不放宽 `modules/space/db.go:374`
  的 bot 收窄过滤。两套口径并存的问题记录在 Load-bearing，留给后续任务。
- **不引入权威的本地/云端判定列**（如 `robot.provision_source`）。本任务接受自报的
  `agent_hosting`。
- **不加 per-user bot 数量配额**。虽然缺配额是真实问题（一个泄露的 `uk_` key 就能刷
  `robot` 表和 `space_member`），但与本需求无关，另排。
- **不下发 `online`**。前端可沿用现有的可见 AI per-uid
  `fetchCurrentImChannelInfo` 预取和在线态监听；显示在线绿点不要求本接口查询 WuKongIM。
- **不下发 avatar**。现有 `memberResp` / `space_bots` 里的 `avatar` 本来就是死字段，
  前端实际用 `WKAvatar channel={new Channel(uid, ChannelTypePerson)}`；原型里 bot 头像
  右下角的 owner 小角标，前端拿顶层 `uid` 再挂一个即可。
- **绝不下发**：`bound_agent_ref`（不透明内部标签）、`agent_platform` / `agent_version` /
  `plugin_version`（运维信息）、任何 token。
- **不改前端**（`octo-web` 是独立仓库）。本任务只交付服务端契约。
- **不新增专属灰度开关、切换回退机制或监控体系**。前端数据源接入另行处理。

## Acceptance

**行为（集成测试，`modules/space`）**

- `GET /v1/space/directory?space_id=X` 顶层返回该 Space 内**全部**活跃真人，
  无分身者 `agents: []` 且 `agent_count: 0`。
- 顶层**不含** bot（含调用者自己创建的 bot），bot 只出现在 `agents` 里。
- 给 `fileHelper` 建立活跃 Space 成员行，且其 `user.robot=0` / `status=1` /
  `is_destroy=0` → 仍不出现在顶层；共享系统清单中的账号也不能作为 owner 或分身返回。
- `only_with_agents=true` → 只返回有云端分身的人；缺省或 `false` → 返回全部真人。
- 顶层 `name` 与同一 Space 下 `GET /v1/space/:id/members` 对同一个 uid 给出的
  `name` 一致（含 `u.name` 为空时的 `real_name` / 占位符兜底）。
- `agent_hosting = 'self_hosted'` 的 bot **不出现**。
- `agent_hosting = ''` 且 `agent_reported_hosting_at IS NULL`（从未上报）**出现**。
- `agent_hosting = ''` 且 `agent_reported_hosting_at` 非 NULL（已撤回）**出现**。
- `agent_hosting = 'octo_hosted'` 与任意第三方 `<vendor>_hosted` slug **出现**，
  `hosting` 原样返回。
- `robot.status <> 1` 的 bot 不出现。
- bot 不在本 Space（`space_member` 无行或 `status<>1`）→ 不出现。
- owner 不在本 Space / `user.status<>1` / `is_destroy=2` / `creator_uid=''` → 该 bot 不出现，
  且不产生任何「无主」顶层行。
- App Bot 不出现。
- 调用者自己名下的云端分身**出现**（不做 viewer 过滤），调用者自己也作为顶层行出现。
- 调用者没有云端分身 → 默认仍出现且 `agents: []`；`only_with_agents=true` 时不出现。
- `is_friend` 反映调用者与该 bot 的好友关系（`friend` 表 `uid=loginUID AND to_uid=bot AND is_deleted=0`）。
- 单个 owner 分别有 0、50、51 个符合条件的分身 → 明细分别为 0、50、50 个，
  `agent_count` 分别为 0、50、51，`agents_truncated` 分别为 false、false、true。
  计数不含本地、非活跃或不在当前 Space 的分身。
- 两个 owner 同时超过 50 个 → 各自返回 50 个，不得使用全 Space 的 `LIMIT 50`。
  数据不变时重复查询，选中的 Bot UID 集合相同。
- DB 层直接执行实现使用的窗口查询，断言每个 owner 返回的明细不超过 50 行且总数准确；
  验收不能仅检查 handler 截断后的 JSON。名字、好友关系等 JOIN 不得放大行数。
- `hosting_reported_at` 未上报时为 `null`，已上报时格式与 `GET /v1/user/bots` 的
  `bound_at` 一致（`2006-01-02 15:04:05`）。

**鉴权 / 限流**

- 缺 `space_id` → `ErrSpaceRequestInvalid`（**必须由 handler 挡住**，因为 `SpaceMiddleware`
  在 `space_id` 为空时 fail-open 放行）。
- 非该 Space 成员 → 403（由 `SpaceMiddleware` 拦下）。
- 未登录 → 401。
- 路由上挂了 `SharedUIDRateLimiter` 且位置在 `AuthMiddleware` 之后（源码断言或路由测试）。
- 测试 setup 必须 reset `ratelimit:uid:*`（该桶在 Redis 持久，`CleanAllTables` 不清）。
- 断言 `error.code` 的测试须注入 `ErrorRenderer`（`testutil.NewTestServer` 不装）。

**门禁**

- 以下为后续接口实现的验收要求，修订本 brief 不代表已经执行或通过。
- `go test ./modules/space/...` 通过（本地测试库需显式 `COLLATE utf8mb4_general_ci`）。
- 查询出错或 context 超时/取消的用例通过：不返回成功的部分数据，不继续下一段查询；
  请求仍可响应时走 `ErrSpaceQueryFailed`。确认两段查询共用超时 context。
- 对实际两段 SQL 执行 `EXPLAIN`，记录索引使用与窗口计算情况；本任务不要求单独建设
  压测或监控系统。
- 新 handler 文件已加入 `TestSpaceNoLegacyResponseError` 清单，该测试通过。
- `make i18n-lint` 通过（无新增 raw error response；本任务不新增错误码，
  `make i18n-extract-check` 应无变化）。
- `golangci-lint run ./modules/space/...` 通过。
- 无新增 SQL 迁移文件（可机检：`git diff --name-only` 不含 `modules/*/sql/`）。
