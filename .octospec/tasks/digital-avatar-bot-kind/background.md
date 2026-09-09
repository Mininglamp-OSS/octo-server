# Historical exploration — superseded by brief.md

---
type: Task
title: "Task: digital-avatar-bot-kind"
description: 为「数字分身」（无主人的独立 bot 实体，可被任何人私聊、拉入群聊、由项目管理员加入 project、加入「我的 AI 团队」，Bot API 权限介于 App Bot 与 User Bot 之间）选定承载实体。两条路线并列：A 在 app_bot 上加 kind，B 在 robot 上加 kind。Route B implementation approved; see Decisions.
tags: ["space", "isolation", "auth", "acl", "bot-api", "thread", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "commit"]
timestamp: 2026-09-08T15:44:18Z
# --- octospec extension fields ---
slug: digital-avatar-bot-kind
upstream: self（需求来自 2026-09-08 口头讨论，尚无 issue）
source: user
---

# Task: digital-avatar-bot-kind

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> 本 brief 的历史部分是 Plan 阶段产物：只记录现状、两条路线的改动清单和待定问题，**不含实现**。
> 文中所有 `path:line` 均在 2026-09-08 对 `main`（`38da069`）核实过；标注「未核实」的
> 是仓库外或未逐行读过的判断。

## Goal

引入一种新的 bot 类型「数字分身」（下文简称 avatar），产品定义四点：

1. **独立实体**：没有个人主人，不跟随任何人的进出级联；由平台或 Space 管理员创建和下架。
2. **可被加入「我的 AI 团队」**：任何 Space 成员都能把它加进自己的 AI 团队并开会话。
3. **可被项目管理员添加为项目成员**。
4. **可被加入群聊**。

同时它的 Bot API 能力要**比现有 App Bot 多、比 User Bot 少**：能在群和子区里收发，不能建群、
改群、加减成员、建删子区、做消息搜索、语音、OBO 代人发言。

本 brief 的目标不是实现，而是：

- 把今天挡住「一个 bot 进群 / 进 project / 进 AI 团队」的每一处代码写清楚；
- 给出两条并列路线（A：`app_bot` 加 kind；B：`robot` 加 kind）各自的改动清单和风险；
- 列出选路前必须先回答的三个问题（见 §Open questions）。

## Background

### 现有的两种 bot 身份

| | User Bot | App Bot |
|---|---|---|
| 权威表 / token 前缀 | `robot` / `bf_` | `app_bot` / `app_` |
| 创建方 | botfather 用户 API，`creator_uid` = 创建者（`modules/botfather/api_user.go:127`） | 平台或 Space 管理员，`created_by` 仅审计（`modules/app_bot/app_bot.go:374`） |
| 可见范围 | `space_member` 行（创建时写入，`modules/botfather/db.go:194`） | `scope=platform` 全平台 / `scope=space` 单空间，**不写** `space_member`（`modules/app_bot/db.go:44`，`modules/bot_api/db.go:276` 注释） |
| 发布态 | `robot.status` 0/1 | draft / published / unpublished |
| Bot API 身份 | `BotKindUser` | `BotKindApp`（`modules/bot_api/auth.go:12`） |
| 私聊 | 需好友；creator 免（`modules/robot/event.go:207`） | 需先调 `/v1/app_bot/apply` 建好友（`modules/app_bot/app_bot.go:1032`） |
| 群 / 子区 | 需 `group_member` 行 | 一律拒绝（DM-only） |
| 收消息 | 服务端事件队列 + WS | **仅 WS 直连**（`modules/bot_api/register.go:479` 注释；队列只承载 `card_action` / `bot_joined_group`） |

### 仓库里已经存在的三个「分身」概念

需求描述的 avatar 和它们都不是同一个东西，命名要先对齐：

- **OBO / Persona Clone**（YUJ-1166）：User Bot 以真人身份代发，grantee 必须是 grantor 自己创建的
  robot（`modules/bot_api/obo_api.go:247`）。这是「个人分身」，avatar **不做** OBO。
- **project P2 D5/D6**（`.octospec/tasks/project-p2-subsystem-integration/brief.md:417,445`）：
  个人分身 = `agent_hosting != self_hosted` 的 User Bot，用 `kind=assistant_personal + owner_uid`
  挂在项目成员上并跟随主人级联；「项目分身」被 parked（`:984`），声明了 kind 值但没有写入方。
  这两列都**还没落库**（`modules/project/sql/` 三个迁移里没有 `kind`）。avatar 正好对应
  parked 的「项目分身」：无主人、不级联。
- **Octo Assistant**：`DM_OCTO_ASSISTANT_UIDS` 下发给前端做埋点区分（`modules/common/api.go:496`），
  #784 的说明写明它是一个 **App Bot**。如果 avatar 是 Octo Assistant 往群、project、AI 团队的
  延伸，产品连续性偏向路线 A。

### 今天挡住 avatar 的代码，按子系统

**群聊**

| 位置 | 规则 | 对 App Bot | 对「creator 为空的 robot 行」 |
|---|---|---|---|
| `modules/group/bot_ownership.go:37`，调用点 `api.go:1590`、`api.go:1781`、`invite.go:50` | 只有 `robot.creator_uid == 邀请人` 才能拉 bot 入群；`user.robot=1` 但 robot 行缺失或 creator 为空 → fail-closed 拒绝 | 拒绝 | 拒绝 |
| `modules/group/api.go:1655-1670` | 群属于 Space 时，bot 必须是该 Space 的 `space_member` | platform 拒绝，space 拒绝 | 有行则通过 |
| `modules/group/admission.go:180,198` | 项目群：非 `SystemBots` 白名单的 bot 必须同时是项目成员和 Space 成员；非项目群不查 | 拒绝 | 有行则通过 |
| `modules/group/db.go:1037` | 主人离群时级联移除其 bot，键是 `creator_uid` | 不适用 | creator 为空 → 永不级联 |
| `modules/group/service.go:2337` | `bot_joined_group` 事件按 `user.robot=1` 推 | 会推 | 会推 |

**project**

| 位置 | 规则 |
|---|---|
| `modules/project/api_member.go:44`、`service.go:263`、`db.go:450` | 加成员只校验目标是活跃 `space_member`（I1），**不看** robot 标志；模块内对 bot 零处理 |
| `modules/project/service.go:263` | platform App Bot 无 `space_member` 行 → `errNotSpaceMember` |

**我的 AI 团队**（`DM_AI_TEAM_ON`）

六处 SQL 都硬编码 `JOIN robot r ... AND r.creator_uid = a.user_uid` 加 `JOIN space_member bot_sm`：
`modules/ai_team/service.go:46`（validateAuthority）、`:66`（validateAuthorityTx）、`:166`（getAgent）、
`:195`（ListAgents）、`:443`（GetSession）、`pkg/aiteam/aiteam.go:64`（LookupReadySessionTarget，
消息路由）。brief `.octospec/tasks/my-ai-team-sessions/brief.md:47` 明确把 App Bot 和
shared/third-party bot entitlement 列为 out of scope。容器群准入 `AdmitAITeamContainerMembersTx`
（`modules/group/admission.go:293`）只校验人类是群创建者，对 bot 无额外要求。

**Bot API 里的 App Bot 拒绝门（18 处，9 个文件）**

`send.go` 4（`:471` 规则 1、readReceipt、message/edit 两处）、`groups.go` 4（`:414`、`:527`、`:604`、
`:692`）、`sync.go` 3、`voice_adapter.go` 2、`threads.go:25`、`authtree_guard.go:50`、
`search_route.go:90`、`space_principal.go:35`、`events.go`（`filterAppBotEvents`：丢弃并自动
ACK 所有非私聊事件，**子区事件也在内**）。

**事件投递（服务端队列的五个写入方，`pkg/botevent/chokepoint_guard_test.go:26`）**

`saveRobotMessage`、`enqueueBotEventGeneric`、两个 `notifyBotJoinedGroup`、bot_mention Lua 写入。
前四者的前置是 `existRobot`（`modules/robot/event.go:100` → `robot/db.go:32` 只查 `robot` 表）或
`user.robot=1`；bot_mention 用 `robot.ExistRobot`（`modules/bot_mention/api.go:136`）。因此群 @、
`mention.ais` 广播、文档 @、AI 会话路由（`robot/event.go:155`）都不会投给 App Bot。

**以 `creator_uid` 为所有者的其他位置**（路线 B 要逐个确认方向）

| 位置 | creator 为空时 | 是否符合 avatar 预期 |
|---|---|---|
| `modules/robot/api.go:1695,1736`、`mention_pref.go`（6 处）、`bot_setting.go`（3 处）、`modules/user/api.go:775` 头像 | 404 / 403，没人能配置 | 否，需要管理员分支 |
| `modules/bot_api/obo_api.go:257`、`obo_db.go:1611` | 不能成为 OBO grantee | 是 |
| `modules/bot_provision/bot_api.go:105` | daemon 取不到 token | 否，见 Q3 |
| `modules/usersecret/db.go:159` | 不能用用户密钥 | 是（待产品确认） |
| `modules/bot_api/voice_adapter.go:38` | 语音直接 not provisioned | 是 |
| `modules/bot_api/db.go:440` | 不能作为 space principal 的调用方或目标 | 调用方是；目标待定（Docs 集成） |
| `modules/space/db_directory.go:49,99` | 通讯录不列出 | 需要单独的「空间分身」区块 |
| `modules/bot_api/register.go:377` | 响应 `owner_uid` 为空 | 插件「通知主人」无主体，未核实插件行为 |
| `modules/internal_resolve/api.go:185` | 已定义为 ownerless，跳过自动挂载 | 是 |

### 已核实与未核实

已核实：以上全部 `path:line`；App Bot 聊天消息走 WS 直连而非队列；`lockSpaceSeatsTx` 不看
robot 标志；语音对空 owner 明确拒绝。

未核实（仓库外或未逐行）：WS 直连下群消息是否送达取决于 WuKongIM 订阅关系；App Bot 插件
（`create-openclaw-octo`）是否也轮询 `/v1/bot/events`（`filterAppBotEvents` 的存在暗示会）；
插件对 `owner_uid` 为空的处理；octo-web 的 bot 选择器、通讯录、群成员选择器的数据源。

## Load-bearing list

- **space / isolation / acl** —— 三个子系统的准入都以 `space_member` 行和 Space 状态为租户
  锚点；avatar 的可见范围必须落到同一锚点，不能靠 `CheckBotsInSpace` 式特判再开一条。
  （rules: space-isolation）
- **auth / bot-api** —— `checkBotOwnership` 的「creator 为空 ⇒ 不可邀请」是写进代码的
  fail-closed 安全属性（`bot_ownership.go:18-21`）；bot_api 18 处 DM-only 判定和 `filterAppBotEvents`
  是 App Bot 的授权边界。放宽任一处都必须以服务端写入的 `kind` 列为唯一键，且 `kind` 不能由
  botfather 用户 API、`/v1/bot/register` 的 agent 自报（`agent_hosting`，P2 D6 明文「只能决定标签
  不能决定权限」）改写。（rules: space-isolation）
- **thread** —— AI 团队会话是子区频道（channel_type 5）；`validateBotGroupAccess` 和
  `filterAppBotEvents` 对子区的处理决定 avatar 在 AI 团队里能不能收到消息。
- **wire-contract** —— `bot_kind` 从两值变三值会出现在 bot_api 上下文、Redis
  `AppBotRegistrySpec`、`botidentity.Identity`、conversation sync 的 `bot_type`（今天只在私聊会话
  打 `app_bot`，`modules/message/api_conversation.go:742`）、`/v1/app_bot/available`、
  `/v1/robot/space_bots` 的响应里。
- **error-response / i18n** —— 新增拒绝码或复用 `ErrBotAPIAppBotUnsupported`，须过
  `make i18n-extract-check` 和 `make i18n-lint`。（rules: error-handling）
- **rate-limit** —— 共享 avatar 被 N 个用户使用时共用一条 per-bot 队列和限流桶；per-bot business
  桶默认关闭（`modules/bot_api/bot_api.go:389` 注释），容量要评估。（rules: rate-limit）
- **testing** —— `Test<Module>NoLegacyResponseError`、`admission_guard_test` allowlist、
  `TestEveryBotEventQueueWriterRingsTheDoorbell` 都会被触碰。（rules: testing）
- **commit** —— English Conventional Commits。（rules: commit-style）

## Out of scope

- OBO 代人发言：avatar 不成为 grantee，两条路线都保持拒绝。
- 消息搜索、语音上下文与转写：两条路线都保持拒绝。
- P2 D5 的个人分身（`kind=assistant_personal + owner_uid`）：不在本 brief 内落库；avatar 对应的
  是 parked 的「项目分身」。
- 现有 App Bot 和 User Bot 的行为：两条路线都不改动存量 bot 的任何契约。
- 前端（octo-web）和 OpenClaw 插件的改动：列为待确认项，不在本仓库内实现。
- 运行时托管平台本身（fleet / daemon）的改动：本 brief 只定义 octo-server 侧需要暴露什么。

## 两条路线（并列，未选）

### 路线 A —— `app_bot` 加 `kind`

**数据模型**：`app_bot` 加 `kind`（`app` | `avatar`），`scope` 保持 `platform | space` 不变。scope
继续回答「谁能看到、谁能管理」，kind 回答「能力等级」。`AppBotRegistrySpec`、`botidentity.Identity`、
bot_api 上下文 key 都带 kind。

**不用改就有的**：独立实体（`created_by` 无 ownership 语义）；平台和 Space 两套管理员 CRUD、
发布/下架、换 token、查看 token；platform scope 原生；Octo Assistant 的连续性。

**必改清单**

| 模块 | 改动 |
|---|---|
| app_bot | schema `kind`；create/update/detail/list/discover 带 kind；发布时写 `space_member` 行（platform scope 见 Q1）；`applyBot` 对 avatar 免申请或自动通过；`deleteBot`/`unpublishBot` 级联清理 `group_member`、`project_member`、`ai_team_agent` |
| bot_api | `authAppBot` 写 kind；18 处 `== BotKindApp` 改为按能力表查询；`filterAppBotEvents` 按 kind 放行群和子区事件；`isBotSpaceAuthorized` 保持 |
| robot / bot_mention / aiteam | `existRobot`、`ExistRobot`、`LookupReadySessionTarget` 改用 `botidentity.Resolver`（注意 `robot:exist:` 缓存语义） |
| group | `checkBotOwnership` 对 avatar 放行（谁能拉见次级待定）；`api.go:1655` 的 Space 隔离改用 `CheckBotsInSpace` 语义或依赖新写的 `space_member` 行；`bot_owned_by_me` 恒 false；管理员可移除 |
| project | 依赖 `space_member` 行满足 I1；否则改 I1 加 `app_bot` 分支 |
| ai_team | 六处 SQL 加「published avatar App Bot 且 scope 覆盖本 Space」分支 |
| user / message / robot | conversation `bot_type` 加新值；`/v1/app_bot/available` 加 kind 过滤供前端选人 |
| errcode / i18n | 新码或复用；跑 extract-check 与 lint |

**风险**：三个子系统各开一条分支，且都建立在「App Bot 没有 `space_member` 行」这个前提被打破
之后；`existRobot` 一改牵动五个队列写入方；`filterAppBotEvents` 的「防御性过滤」语义要重写。

### 路线 B —— `robot` 加 `kind`

**数据模型**：`robot` 加 `kind`（`user` | `avatar`），avatar 行 `creator_uid` 留空，`auto_approve=1`，
创建时写 `space_member` 行（与 botfather 建 bot 同款）。

**不用改就有的**：bot_api 的群、子区、文件、卡片、命令、免@、设置全部走 `BotKindUser`；五个队列
写入方和 AI 会话路由全部认 robot 表；群 Space 隔离、project I1/I2、AI 容器准入靠 `space_member` 行
直接满足；私聊「人人可聊」= `auto_approve=1` + 共同 Space（`modules/user/api_friend.go:353,558`）；
`/v1/robot/space_bots` 自动列出；`bot_cascade` 因 creator 为空永不级联；OBO、bot_provision、
usersecret、语音、space principal 因 creator 为空已经拒绝。

**必改清单**

| 模块 | 改动 |
|---|---|
| robot | schema `kind`；管理员创建入口（`/v1/manager/robots` 今天只有 list/update/delete/revoke，superAdmin，无 create、无 Space 管理员维度）；owner-only 路由（description、auto_approve、mention_pref、bot_setting、头像）加管理员分支 |
| group | `checkBotOwnership` 对 `kind=avatar` 放行 |
| ai_team | 六处 SQL 的 `r.creator_uid = a.user_uid` 加 `OR r.kind='avatar'` |
| bot_api | 在 `BotKindUser` 路径**新增**收窄门：createGroup、群信息改、加减成员、子区建删、搜索（约 6-8 处），同样落能力表 |
| space | 通讯录加「空间分身」区块；platform 级可见需每个 Space 一行并在建 Space 时补齐（见 Q1） |
| bot_provision | avatar 的 token 获取路径（见 Q3） |
| user / message | conversation `bot_type` 是否新增值 |

**风险**：把「creator 为空 ⇒ 不可邀请」从安全属性改成「creator 为空且 kind=avatar ⇒ 可邀请」，
必须严格以 kind 为键并加 source guard；管理面和发布态要新建；`register` 响应 `owner_uid` 为空对
插件的影响未核实；`agent_hosting` 分类分身的谓词（通讯录、P2 D6）不会自动把 avatar 当分身。

### 对比

| | 路线 A：app_bot 加 kind | 路线 B：robot 加 kind |
|---|---|---|
| 独立实体 | 天然满足 | creator 留空，约 10 处 creator 逻辑逐个确认方向 |
| 群、子区、事件投递 | 18 处拒绝门重写 + 三处路由改 botidentity + 补 `space_member` | 改 `checkBotOwnership` 一处 |
| AI 团队 | 六处 SQL + `filterAppBotEvents` 放行子区 | 六处 SQL |
| project | 补 `space_member` 或改 I1 | 写行后直接可用 |
| 收窄权限 | 现有拒绝门保留 | 新增 6-8 处门 |
| 管理面、发布流程 | 现成 | 需新建 |
| 平台级可见 | 原生 scope | 按 Space 铺行 |
| 运行时 token 获取 | 管理员 reveal 后手工配置 | daemon 路径按 creator 校验，avatar 取不到 |
| 与现有产品事实 | Octo Assistant 已是 App Bot | 与 P2「项目分身」kind 声明位置一致 |

### 两条路线共同的前置

1. `kind` 是服务端字段，只能由管理员路径写入。
2. 一张「kind → 能力集」表作为唯一真源，bot_api 判定处改为查表；配 source guard 测试。
3. 拉群、配置、下架的授权来源从 creator 改为 platform/space admin。
4. avatar 必须有 `space_member` 行（Space 级）；platform 级的铺行策略见 Q1。
5. 下架/删除时级联清理 `group_member`、`project_member`、`ai_team_agent`。
6. 与 OBO Persona Clone、P2 个人分身、Octo Assistant 的命名对齐。

## 能力矩阵（建议，最后一列待产品拍板）

| 能力 | User Bot | App Bot 现状 | avatar 建议 |
|---|---|---|---|
| 私聊收发、typing、已读 | 需好友 | 需 apply + Space 成员 | 免申请或自动通过，受 Space 约束 |
| 被拉入群 | 仅 creator | 不可 | 群成员可拉（或仅群管理，待定），受 Space 隔离 |
| 群内发消息、sync、已读 | 需 `group_member` | 拒 | 需 `group_member` |
| 群信息、成员只读 | 允许 | 拒 | 允许 |
| 建群、改群、加减成员 | 允许 | 拒 | 拒 |
| 子区读写 | 允许 | 拒 | 读发允许，建删拒 |
| 加入 project | 主人拉 | 不可 | project 管理员可加，不随人级联 |
| 加入我的 AI 团队 | 仅自己的 bot | 拒 | 任何 Space 成员可加 |
| 消息搜索、语音、OBO、space principals | 允许 | 拒 | 拒 |
| 文件、卡片、命令、免@ 偏好 | 允许 | 允许 | 允许 |

## Acceptance

**选路前（本 brief 完成的标准）**

- §Open questions 三个问题各有一条书面回答，写回本文件的 §Decisions（待新增）。
- 命名对齐：avatar 与 OBO 分身、P2 个人分身/项目分身、Octo Assistant 的关系在 §Decisions 里写清。
- 选定路线后，对应的「必改清单」逐行确认，未选路线的清单保留在本文件作为对比记录。

**实现后（与路线无关的可检查项）**

- 一个 `kind=avatar` 的 bot：任意 Space 成员可直接私聊；群成员（或管理员，按决策）可拉入本
  Space 的群；project 管理员可加为项目成员；Space 成员可加入自己的 AI 团队并在会话里收到回复。
- 同一 avatar 调 `createGroup`、`groups/:no/info`、`members/add`、`members/remove`、
  `threads`（POST/DELETE）、`search`、`voice/*`、`obo-grant`、`space/principals/:uid` 全部返回
  注册过的拒绝码；`sendMessage`、`messages/sync`、`readReceipt` 在其所在群和子区成功。
- 拉群、配置、下架的授权：非管理员对 avatar 的配置类请求被拒；管理员成功。
- `kind` 不可通过 botfather 用户 API、`/v1/bot/register` 请求体、`/v1/user/bots` 写入；有 source
  guard 或 API 测试钉住。
- 下架/删除 avatar 后，其 `group_member`、`project_member`、`ai_team_agent` 行在同一操作内失效，
  IM token 作废。
- 存量 User Bot 与 App Bot 的既有测试全部通过；bot_api 的 18 处 App Bot 拒绝行为对 `kind=app`
  不变。
- `make i18n-extract-check`、`make i18n-lint`、`go vet`、`git diff --check` 通过。

## Open questions

- **Q1 — avatar 需要平台级（跨所有 Space）可见吗？**
  路线 A 有原生 `scope=platform`，但三个子系统都以 `space_member` 行为租户锚点，platform avatar
  仍要在每个 Space 铺一行并在建 Space 时补齐；路线 B 没有 scope 概念，只能铺行。若 v1 只要
  Space 级，两条路线的差距缩小，路线 B 更省。

- **Q2 — 管理面从哪来？**
  谁创建、谁发布/下架、谁换 token、谁改名改头像：只允许 superAdmin，还是 Space 管理员也可以？
  路线 A 两套管理员 CRUD 现成；路线 B 的 `/v1/manager/robots` 没有创建、没有 Space 维度，也没有
  发布态。答案直接决定路线 B 要补多少后台。

- **Q3 — avatar 的运行时由谁托管，它怎么拿到自己的 token？**
  这是两条路线共同缺的一段。今天 daemon 取 token 要求 api_key 的 uid 等于 `creator_uid`
  （`modules/bot_provision/bot_api.go:105`），无主人的 avatar 在路线 B 下没有任何 daemon 能取到；
  路线 A 下只能管理员 reveal 后手工配置。需要先定：平台托管（fleet）还是 Space 管理员自带运行时；
  取 token 的凭据是管理员身份、Space 级 api_key 还是新的服务身份。没有这个答案，「独立实体」
  只是一条数据库记录。

**次级待定（不阻塞选路，阻塞实现）**

- 谁能把 avatar 拉进群：任意群成员、群主/管理员、还是 Space 管理员。
- 私聊是免申请还是申请自动通过（前者要改 IM 白名单建立时机，后者复用 `auto_approve`）。
- 群消息投给 avatar 是仅 @ 提及还是全量（`mention.ais` 广播已存在）。
- 收窄边界按上表哪一列；群/子区 md 写入是否允许。
- Docs 集成的 `space/principals/:uid` 是否需要把 avatar 当作可解析目标。
- 共享 avatar 的队列容量与限流策略。

## Decisions — implementation approved 2026-09-09

The user explicitly selected route B and requested a new worktree implementation
after requiring ALL ordinary-group, thread, project and AI Team capabilities.
Implementation base: `5053a00bcf95bfdd3e0919bb9fb2200ada781102`.
This section supersedes the historical draft's no-implementation language and
outdated baseline observations. The A/B analysis above is retained as history.

- **Entity:** `robot.kind=user|avatar`, default user for existing rows. Avatar
  has no personal owner; `created_by` is audit only. Type, management scope and
  publication state are server-owned and immutable through user/Bot API input.
- **Q1:** support platform and Space scope. Published avatars have real active
  `space_member` rows in eligible active Spaces; platform publication and new
  Space creation reconcile those seats. Visibility never grants group/project
  membership. Operations use the resource's Space, never an arbitrary first seat.
- **Q2:** superadmins manage platform avatars; active Space owners/admins manage
  that Space's avatars. Existing user-bot ownership contracts remain intact.
  Add draft/create, edit, publish, unpublish, delete, token reveal/rotation and
  scoped discovery. Name/avatar/description/preferences use the same authority.
- **Q3:** server-side bootstrap uses authenticated, scoped administrator
  credentials to retrieve/rotate the bot token; that token registers either a
  platform-operated or administrator-operated runtime. No new service identity
  and no fleet/daemon implementation are introduced. Existing personal creator
  provisioning must not leak avatar tokens.
- **Invite:** any actor otherwise permitted by the group's existing invitation
  policy may invite a published avatar in the same Space. Project admission
  requires project member-management authority. Avatar never follows an
  inviter/creator's departure.
- **DM:** same-active-Space auto-approval, with friend/IM whitelist handling;
  removal from the Space revokes access even if friendship persists.
- **Delivery:** ordinary groups keep existing mention/mention.ais/no-mention
  behavior; private AI containers remain no-mention. Ready session routing
  preserves per-user/per-Space channel keys. Multiple users add/remove
  independently; history is retained when their Agent is removed.
- **Capabilities:** default-deny avatar allowlist. Permit DM/group/thread
  message send, typing, sync, receipts; group/member/thread reads; files, cards,
  commands and mention preferences as appropriate. Deny group creation,
  group metadata/member mutations, thread creation/deletion, group/thread MD
  mutation, search, voice, OBO (including being a grantee), user secrets and
  Space-principal calls/target resolution. App Bot contracts remain unchanged.
- **Lifecycle:** authoritative disable and membership/Agent revocation are
  durable; external IM/token/cache cleanup is idempotent and retryable, never
  treated as a cross-system DB transaction. Republish does not silently restore
  group/project or per-user AI Team membership.
- **Capacity:** retain existing shared per-bot queue/limits and enforce bounded
  configuration through existing infrastructure; no per-consumer duplicate
  queues or new hand-rolled HTTP counters. Document shared runtime context
  isolation and queue/limit operation.
- **Naming:** avatar is an organization-managed digital employee, independent
  of OBO/persona and personal hosted bots. Existing Octo Assistant App Bots are
  not automatically migrated or upgraded.

## Current-branch corrections and acceptance additions

- Project D15 now checks owned-agent eligibility: explicit avatar admission
  AND removal for project administrators must be implemented, with no
  owner-cascade behavior. A Space seat alone is insufficient.
- Existing robot deletion already has group/Space cleanup; App Bot unpublish
  only invalidates registry state. Reuse common cleanup where appropriate.
- Test every allow/deny route family, platform new-Space enrollment, same-Space
  DM and cross-Space denial, independent AI membership and ordinary groups,
  project admin admission/removal, no owner cascade, token lifecycle and retry.
- Add regression/source guards against self-reported kind, unknown avatar
  endpoints defaulting to user permissions, and unlocalized errors.
- Run focused unit/integration checks using isolated test state, broader build,
  vet and i18n gates; record commands/results and any environment limitations.

## Implementation out of scope

Migrating existing App Bot storage/contracts, changing front-end/plugin/fleet
code, deploying, committing, pushing or opening a PR are not part of this task.
