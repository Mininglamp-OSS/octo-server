---
type: Task
title: "Task: bot-agent-hosting"
description: Bot 的 Agent runtime 自报托管形态（开放取值的小写 slug，自用 self_hosted / octo_hosted）落 robot 表两列，register 上报，owner 面读出。
tags: ["bot-api", "wire-contract", "trust-boundary", "testing", "commit"]
timestamp: 2026-09-03T00:00:00+08:00
slug: bot-agent-hosting
upstream: openclaw runtime 本地/云上区分需求（口头提出，无 issue）
source: user
---

# Task: bot-agent-hosting

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

让服务端**能回答**「驱动这个 Bot 的 OpenClaw runtime 是用户自运维的，还是平台侧托管的」。

今天答不了：`robot` 表只有 `agent_platform` / `agent_version` / `plugin_version`
（`modules/botfather/sql/20260417000001_botfather_legacy01.sql`），表达的是「平台名 + 版本」，
没有托管形态维度；服务端也没有任何可信来源能推导它（见 Background）。

三件事：

1. **上报**：`POST /v1/bot/register` 的 User Bot 分支新增可选 `agent_hosting`，
   落 `robot.agent_hosting`；同时落 `robot.agent_reported_at`（这批自报数据的产生时间）。
2. **读出**：owner 面 `GET /v1/user/bots` 带出这两个值。
   （原计划同时改运维面 `GET /v1/manager/robots{,/:robot_id}`。**实施期发现那两个端点
   不存在**：`modules/robot/api_manager.go` 的 `NewManager` 全仓无调用方、`Route()`
   从未被挂载 —— `modules/robot/1module.go` 只注册了 `New(ctx)`。Plan 阶段只核到
   「resp 结构里没有 agent_* 字段」，没核路由是否挂载，这是调研遗漏。给死代码加字段
   是纯负债，故撤回；详见 Out of scope。）
3. **App Bot 明确不支持**：`registerAppBot` 现在连请求体都不解析，上报被静默丢弃。
   改为解析后打 Warn（不落库），让上报方能发现自己白报了。

取值是**小写 slug，值域开放**：服务端只校验形状（`^[a-z][a-z0-9_]*$` + 长度 ≤ 列宽），
不校验取值。本项目自用 `self_hosted`（用户自运维）/ `octo_hosted`（平台侧托管），
第三方托管方按 `<vendor>_hosted` 自取即可，空串 = 未上报。

## Background

**为什么不能从既有字段推导，必须新增自报字段。** 排查过三个候选，都不成立：

- `user_api_key.client_id`（`botfather` / `octopush`）看似能分，实际不能：
  `modules/integration/api.go:53` 明写「桌面客户端手上只有这种凭据（业务后端自签
  HS256 JWT）」，而 `POST /v1/integrations/oidc/exchange` 给所有调用方签发的 client_id
  是硬编码的唯一值 `octopush`（`modules/integration/model.go:4`）。**本地桌面客户端与
  云端服务共用同一个 client_id**，从它推导会把本地标成云上 —— 输出错误信息比不做更糟。
- `robot.bound_agent_ref`（约定 `octopush:agent_xxx`）是 bind 时由客户端自填的不透明
  标签，`modules/botfather/model.go:168` 明写「Octo 不解析其语义」，全仓零处按前缀分支。
- `robot.agent_platform` 只有平台名，且零校验（自由文本）。

**为什么不用 `bot_setting`。** `modules/robot/bot_setting.go` 是**配置**存储：owner 可编辑、
三层解析链（bot → `system_setting` → 代码默认）、删除即回落上层、`updated_by` 审计、
写入路径是 owner session 端点。本任务存的是**运行时上报事实**：没有「全局默认」可回落、
不该由 owner 编辑、写入凭据是 `bf_`（bot 进程自己）。塞进 KV 表语义全错。
`bot_setting` 的 file-top 注释「别再给 robot 加列」针对的是配置开关
（`auto_approve` / `inline_on` / `placeholder`），运行时上报事实在本仓的既有归属就是
`robot` 列 + register 写入，本任务与 `agent_platform` 三列**同一写入点、同一生命周期、
同一性质**，并列即正确。

**为什么上报点是 register 而不是 bind。** bind 用 `uk_`（用户级 key），调用方与真正跑
OpenClaw 的进程可以不是同一个 —— 云端控制面能代替用户 bind 一个将由本地 runtime 驱动的
bot。bind 声明的是「我占用」，register 用 `bf_`，是进程自己说「我起来了」。

**取名的两个撞词。** `managed` 在本仓已被 OBO 代理人格占用且语义完全不同
（`modules/bot_api/obo_friend_gate.go`、`send.go:189/590/599`、`obo_fanout.go:71/79`、
`bot_api.go:161` 的 "managed persona"；`modules/botfather/mint_obo.go:21` 还有第三种用法），
用它会让读代码的人误连到 OBO。`platform` 与 `app_bot.scope='platform'`（可见范围）轻度撞词。
`local` / `cloud` 在私有化部署下会说错话 —— README §Principles 明写
"Local-first. ... cloud is a choice, not a requirement"，客户整套在自己机房时，平台侧托管的
runtime 叫 `cloud` 是错的，而这个错会长期留在数据里。`self_hosted` / `octo_hosted` 与业界
最知名先例（GitHub Actions runner `self-hosted` vs `github-hosted`）同构，且表达的是
**责任归属**（自运维的 runtime 掉线是用户的事，托管的是平台的事），正是这个字段的决策价值。

**为什么这两个词是约定而不是枚举**（第二轮定稿）：初版把它们做成服务端白名单，
被一个具体问题推翻 —— 「客户端能不能传自己的 `<vendor>_hosted`」。白名单下不能，得改服务端
代码 + 发版；而托管方增加是业务常态。更关键的是白名单提供的保证是**虚假的**：
它校验「值在集合内」，不校验「你有资格声称这个值」—— 任何持 `bf_` token 的进程照样能
报 `octo_hosted` 冒充官方托管。真正需要挡住的是引号 / 尖括号 / 空格 / 控制字符 /
Unicode 混淆字符，而 `^[a-z][a-z0-9_]*$` 把这些全挡了且不需要预知有哪些 vendor。
附带收益：开放取值下服务端源码里不出现任何 vendor 名 —— 托管方名字只存在于它自己的
配置和它写的数据行里，永不进这个开源仓；白名单则必须把每个名字硬编码进开源代码。

## Load-bearing list

- **register 是 Bot 掉线自愈的唯一通道，任何新校验都不得阻断它。** `agent_hosting`
  走形状校验，但**形状非法一律落空串 + Warn，照常返回成功**。#696 的二次事故就是
  register 被连带拒绝导致 bot 永远起不来，`modules/bot_api/ratelimit_integration_test.go:272-274`
  有回归断言钉着。为一个纯观测字段的取值校验去阻断自愈通道是错误的取舍。
- **`agent_hosting` 的合并语义与既有三字段刻意不同。** `modules/bot_api/register.go:80-101`
  现有语义是「字段级 merge，空值保留旧值」。对版本号合理，对托管形态有害：本地→云上切换时
  新端漏报会保留陈旧的 `self_hosted`。所以请求体用 `*string` 区分「没传」与「传了空」，
  **传了就覆盖（含清空）**。对这个字段陈旧值比空值更有害。
- **`agent_reported_at` 的语义是「最近一次收到上报」，不是「值变更时间」。** 因此即使
  上报值与库中一致也要刷新它 —— 这会把现有的「值未变则跳过 UPDATE」优化
  （`register.go:95-101` 的 if 条件）改成「请求带了 agent 字段就 UPDATE」。成本可接受：
  `modules/common/system_settings.go:1346` 记录 register「只在重连时调用」，与 heartbeat
  的 0.1 rps 量级不同数量级。**没有这一列，`agent_hosting` 是个无法判断可信度的裸值** ——
  `robot.updated_at` 连 `ON UPDATE` 都没有（`modules/robot/sql/20210926000001_robot_legacy01.sql:14`），
  现有 `agent_platform` 就是这种无从判断新鲜度的裸值，不要复制这个缺陷。
- **自报数据不可信，不得用于鉴权或配额。** 列 COMMENT 与 Go 字段/常量注释都要写明。
  服务端拿不到可信的托管形态来源（见 Background 的 client_id 论证），这个字段填的是
  **观测**缺口，不是信任缺口。形状校验是**数据质量**约束，不是授权约束 ——
  它挡 caller-controlled 字节，不建立「调用方有资格声称该值」。任何后续想用它做
  授权判定的改动，得先建可信来源。
- **值域开放，只校验形状；校验顺序是载荷性的。** `TrimSpace`（返回子切片、零分配）
  → 长度上界 → `ToLower` + regexp。register 的 JSON body 无大小上限，而 `ToLower`
  会分配一个与输入等大的新串，所以 10MB 的值必须在 `ToLower` 之前就被否决。
  大小写折叠而非拒绝（`Self_Hosted` 与 `self_hosted` 意图相同，折叠避免一类无声失败）。
- **`maxAgentHostingLen` 必须与列宽严格相等（64），由测试断言 `Equal` 而非 `<=`。**
  大了：过校验的值写库撞 `1406`，而 agent_* 共用一条 UPDATE，会连带挡掉同一请求里的
  agent_platform/version/plugin_version（见下方已知限制）。小了：白占列宽还拒掉本可
  存下的 vendor slug。第一轮曾写 `64` 配 `VARCHAR(20)`「给未来形态留余量」——
  **列宽之上的余量不是余量，是延迟的失败**；第二轮开放取值后 vendor 名长度不可控，
  列宽与上界一起提到 64。
- **已知代价：命名约定不再由服务端强制。** `cloud` / `local` 符合形状，从此会合法通过 ——
  当初否掉它们的理由（私有化部署下 `cloud` 说错话）仍然成立，但降级为客户端命名约定。
  **刻意不做黑名单**：永远不完整，且既然放弃了值域枚举，再留个半吊子黑名单两头不靠。
  另一个代价是值域可能碎片化（`selfhosted` 与 `self_hosted` 并存），缓解靠文档约定 +
  前端原样展示 slug 而不做映射表（顺带好处：新托管方前端也不用发版）。
  权衡的另一半：白名单下拼错的后果是**落空串**（数据静默丢失），开放下是**存了个怪值**
  （可见、可发现、可修）—— 后者更容易运维。
- **`robot` 表有三个独立的 struct 走 `Select("*")`，NULL 列会炸掉不相关查询。**
  `modules/robot/db.go:171-186`（管理端）、`modules/bot_api/db.go:30-45`、
  `modules/botfather/db.go:30-55` 各有一份。`agent_reported_at` 是 `TIMESTAMP NULL`，
  必须用 `dbr.NullTime` 承接 —— `modules/botfather/db.go:50-52` 的 `BoundAt` 注释已记下
  同一个坑：「用 NullTime 承接 NULL，否则 `Select("*")` 把 NULL bound_at 扫进 string
  会报错，殃及所有 robot 查询」。
- **`UserBotResp` 是 wire contract，只能 additive。**
  （`modules/botfather/model.go:149-165`）只增字段，现有字段不改名/不删/不改类型/不改语义。
  `agent_hosting` 跟同组的 `agent_platform` 一样带 `omitempty`（同一组字段两种缺省行为
  会让客户端写两套解析）；`agent_reported_at` 跟 `bound_at` 一样用 `*string` 显式下发
  `null`（它是判断 `agent_hosting` 新鲜度的唯一依据，省略字段等于让调用方猜）。
- **IM 全员面刻意不下发。** `GET /v1/users/:uid` 的 extraMap、`POST /v1/users/batch`、
  `GET /v1/channels/:id/:type` 这条路今天已经在下发 `bot_agent_platform` /
  `bot_agent_version` / `bot_plugin_version`（`modules/user/service.go:583,846` →
  `modules/user/1module.go:233-238`），受众是**任何能看到该 bot 的用户**（含外部空间成员）。
  「跑在本地还是云上」是部署拓扑信息，对普通用户零价值。本任务**不**接入这条路，并用
  负向断言钉住，防止后来人顺手加。
- **App Bot 的不支持是显式的，不是遗漏。** `app_bot` 表无 agent_* 列
  （`modules/app_bot/sql/` 建表只有 id/uid/display_name/description/avatar/scope/space_id/
  status/token/welcome_msg/created_by/时间戳），`registerAppBot`（`register.go:124-174`）
  连 `ShouldBindJSON` 都不调。但 App Bot **确实由 OpenClaw 驱动** ——
  `connectInfo()`（`modules/app_bot/app_bot.go:1003-1013`）就是给 agent 下发
  `plugin_package` + `api_url` 的连接引导。所以要解析请求体**只为打 Warn**，
  并加守卫测试钉住「app_bot 表无 agent 列」这个前提，避免后来人误以为对称支持。
- **迁移写朴素 DDL，且必须是单条原子 ALTER。** 本仓已确立的原则是「sql-migrate 用
  `gorp_migrations` 追踪版本，不要在每条 migration 里堆幂等魔法」——
  `INFORMATION_SCHEMA` 探测 + 存储过程（如
  `modules/botfather/sql/20260603000002_botfather_legacy01.sql`）是应急路径，不是默认写法：
  它可读性差、reviewer 看不出真实意图。#239 的半应用态之所以发生，是因为那条迁移是
  **多语句**的（ADD COLUMN + DROP INDEX + ADD UNIQUE，DDL 隐式提交，中途失败即残留），
  而 20260417000001 的 agent_* 三列是**三条独立 ALTER**、同样可半应用。本任务两列写在
  **一条** `ALTER TABLE ... ADD COLUMN, ADD COLUMN` 里，MySQL 层面原子，不存在需要守卫的
  中间态。Down 同理单条。
- **迁移文件放 `modules/botfather/sql/`，与 `agent_platform` 三列同源。** 执行顺序按
  文件名时间戳**全局**排序、跨模块（`internal/modules.go:3-8`：sql-migrate 把所有模块的
  SQL 汇成一个 slice 按 VersionInt 排序，「botfather ALTERs robot」是被认可的形状）。
- **字段缺席必须在 SQL 层面缺席，不能靠回写读到的旧值**（Verify 期自修）。
  `agent_hosting` 的指针一路下推到 `SetMap`：`nil` 时该列不进 SetMap。把 `nil` 解析成
  「刚读到的值」再写回去看起来等价，实际会在并发 register 下丢更新
  （A 读旧值 → B 写新值 → A 把旧值写回）。两个 runtime 同时 register 同一个 Bot
  今天没有任何机制阻止（见 Out of scope 的占用锁缺口）。三个既有版本字符串保持
  原有的 merge-then-write 契约，只有新列享受这个更严的处理。
- **自报值的长度上界必须在折叠大小写之前**（Verify 期自修）。`register` 的 JSON body
  无大小上限，而 `strings.ToLower` 会分配一个与输入等大的新串 —— 一个 10MB 的
  `agent_hosting` 会花 10MB 分配去否决一个不可能匹配 11 字节字面量的值。
  顺序固定为 `TrimSpace`（返回子切片、零分配）→ 长度上界（`maxAgentHostingLen=64`）
  → `ToLower`。
- **已知限制：agent_* 全组共用一条 UPDATE，一个超长字段会挡住整组**（Verify 期发现，
  本任务不修）。三个既有字符串字段无长度校验、列宽 `VARCHAR(50)`，而生产与测试库都开着
  `STRICT_TRANS_TABLES`（实测 200 字节写 `VARCHAR(50)` → `1406 Data too long`）。
  所以一个报超长 `agent_platform` 的客户端，会让同一请求里合法的 `agent_hosting`
  也写不进去，且 `register` 仍返回 200（失败只进日志，带 1406）。
  改动前后该 UPDATE 都会失败（旧代码里超长值同样存不进，且值永不相等所以每次重试、
  每次失败），**不是本任务引入的回归** —— 但新字段的可写性现在依赖旧字段的卫生。
  修它要么给三个既有字段加「超长则忽略该字段」的降级（改既有行为），要么把 UPDATE
  拆成两条（放弃原子性换解耦），都该单独决策。
  当前行为由 `TestRegisterOverlongPlatformBlocksTheWholeAgentUpdate` 钉住，
  作为可执行文档：谁修了它，那条测试会红并提醒同步更新本段。
  **新列自己不受这个限制**：`maxAgentHostingLen` 与列宽严格相等（64），
  所以任何能过校验的值都必然能写进库（`TestAgentHostingBoundMatchesColumnWidth` 断言
  两者 `Equal`）。详见 Load-bearing 的同名条目。
- **`modules/botfather/db.go:335` 的 `updateRobotAgentInfo` 是死代码，不要动它。**
  全仓唯一调用方是 `modules/bot_api/register.go:98` 调的 bot_api 自己那份
  （`modules/bot_api/db.go:71`）；botfather 那份与 `modules/botfather/model.go:14-19` 的
  `BotRegisterReq` 都无调用方。改它等于给死代码加功能，还会让人误以为有两条上报路径。
  同理 botfather 的 `robotModel` 只需加**读**字段（`listUserBots` 经
  `api_user.go:344` 用它）。

## Out of scope

- **实例标识（`agent_instance_ref`）与多实例可见性。** 与 `bound_agent_ref` 语义重叠
  且分属两个凭据（`uk_` bind vs `bf_` register），必然漂移且无权威方；而它想解决的
  「同时有几个实例在跑 / 双跑检测」，**单值覆盖列在结构上解决不了**。将来要做，正确落点是
  Redis 活跃集合（`bot:heartbeat:{robotID}` 的 value 位是空闲的 —— 全部 7 处引用
  `modules/bot_api/typing.go:227`、`modules/botfather/command.go:622/759/790`、
  `modules/botfather/api_user.go:472`、`modules/robot/api_manager.go:371` 全是 `Del`，
  无人 `Get`；或另开 `bot:instances:{robotID}` HASH + TTL），不需要本任务的列作前置。
  本仓已有「建了列没人写」的死列先例（`user_api_key.last_used_at` / `last_used_ip` /
  `last_used_user_agent`，全仓零写入点），不重复它。
- **heartbeat 承载托管形态**（把 value 从 `"1"` 换成 hosting 字面量以获得 60s TTL 的
  即时新鲜度）。可行且零读取方破坏，但属独立增强。
- **`POST /v1/user/bots/:bot_id/bind` 放开该字段**（理由见 Background）。
- **App Bot 对称支持**（`app_bot` 加列 + 响应带出）。
- **integration 暴露**：`POST /v1/integrations/oidc/exchange` 的 `include_bots`、
  `GET /v1/integrations/oidc/spaces` 的 `has_available_bot` 都不带该字段。顺带记下一个
  既有不一致（本任务不修）：`queryBots`（`modules/integration/db.go:283-303`）**不过滤**
  `bound_agent_ref`，而同模块 `queryAvailableBotSpaces`（`db.go:150-170`）过滤。
- **占用锁强制化**。`GET /v1/user/bots/:bot_id/token`（`modules/botfather/api_user.go:540-578`）
  不校验 `bound_agent_ref`，取 token 与占用解耦，所以 CAS 锁是协作性的、两端可同时持同一
  `bf_` 并发 long-poll `/v1/bot/events`（单队列、读不删、ack 才 ZREM）。这是一个独立缺口，
  与托管形态正交（本地×本地双跑同样触发），本任务不碰。
- **用该字段做鉴权、配额或能力差异化。**
- `user_api_key.last_used_*` 三列的写入补齐（独立的凭据审计议题）。
- **运维面 `GET /v1/manager/robots{,/:robot_id}`（实施期发现：端点根本没挂载）。**
  `modules/robot/api_manager.go` 的整个 `Manager` 是死代码 —— `NewManager` 全仓无调用方
  （`grep robot.NewManager` 为空），`Route()` 从未执行，`modules/robot/1module.go` 只
  注册 `New(ctx)`。所以 `/v1/manager/robots`、`/v1/manager/robots/:robot_id`、
  `/v1/manager/robot/menus` 等在生产里都不存在。本任务一度给 `robotListResp` /
  `robotDetailResp` 加了字段并写了测试，测试以 `404 page not found` 暴露了这一点，
  随后**整体撤回**（`git checkout modules/robot/`）：给死代码加字段不会被任何调用方看到，
  却会让下一个人误以为运维面已有这个能力。
  要真正补上运维可见性，得先挂载 `Manager` 路由 —— 那是新增一整个管理端路由面
  （鉴权 + 按 rate-limit 规则补 `SharedUIDRateLimiter`，该 group 目前只有
  `AuthMiddleware`），是独立任务，不该塞进本任务。

## Acceptance

**上报（User Bot）**

- `POST /v1/bot/register` 带 `agent_hosting:"self_hosted"` → 200，`robot.agent_hosting`
  为 `self_hosted`，`agent_reported_at` 非 NULL。
- 带 `agent_hosting:"octo_hosted"` → 落库为 `octo_hosted`。
- 带形状非法的值（`<script>alert(1)</script>`、内嵌空格、连字符、数字/下划线开头、
  控制字符、Unicode 混淆字符、中文、超长串）→ **仍 200**，`agent_hosting` 落空串，
  日志有 Warn（且**不记原始值**，只记 `rejectedLen`）。
- 第三方 `<vendor>_hosted` 无需改服务端即可原样落库，并原样出现在 `GET /v1/user/bots`。
- `cloud` / `local` 合法通过（已知代价，见 Load-bearing）。
- 不传该字段（旧客户端、空 body）→ 旧值保留，且现有三字段行为逐字节不变。
- 显式传 `""` → 清空为空串（指针语义与「没传」可区分）。
- 上报值与库中一致时 `agent_reported_at` 仍被刷新（语义是「最近一次收到上报」）。

**App Bot**

- `app_` token 调 register 带 `agent_hosting` → 200，响应形状与今天逐字节相同，
  **不落任何库**，日志有 Warn。
- 守卫测试断言 `app_bot` 表不存在 `agent_hosting` 列（钉住「A 方案」前提，防止半支持）。

**读出**

- `GET /v1/user/bots` 每项含 `agent_hosting` 与 `agent_reported_at`；未上报时分别为
  空串与 `null`。
- **负向断言**：`GET /v1/users/:uid` 的 extraMap 与 `POST /v1/users/batch` 的响应
  **不含** `agent_hosting`（防止后来人顺手接入 IM 全员面）。
- `UserBotResp` 的现有字段值与形状不因本任务改变。
- 未上报的 Bot：`agent_hosting` 被 `omitempty` 省略（不下发空串），
  `agent_reported_at` 显式为 `null`。

**边界与已知限制**

- 空 JSON `{}` 与完全无 body 的 register：不写任何 agent_* 列，`agent_reported_at`
  保持 `NULL`（否则「收到过上报」的语义失真，且是纯写放大）。
- 只报版本、不带 `agent_hosting`：该列不被写（值保留），而 `agent_reported_at`
  仍前进 —— 两个断言合起来才能区分「没碰这列」与「回写了旧值」。
- 超长 `agent_hosting`（>64 字节 = >列宽）在折叠大小写之前就被否决，落空串；
  恰好 64 字节的合法 slug 放行（边界是 `<=`）。
- `maxAgentHostingLen == 列宽`（从迁移文件正则提取比对），且两个自用取值都能过校验。
- 超长 `agent_platform` 连带挡住合法 `agent_hosting`：这是**已知限制**，由测试钉住
  当前行为，见 Load-bearing 同名条目。

**存储与迁移**

- 迁移是单条原子 `ALTER`：要么两列都在、要么都不在，不存在「一列已加一列没加」的
  中间态。重跑保护由 `gorp_migrations` 记账承担（朴素 DDL 原则），**不**在迁移里做幂等；
  dev/staging 若已跑歪，按既定路径 drop & recreate test DB，不反过来给迁移加守卫。
- `agent_reported_at` 为 NULL 的行不破坏任何 `Select("*")` 查询：三个 struct
  （`modules/robot/db.go`、`modules/bot_api/db.go`、`modules/botfather/db.go`）
  各自的既有查询测试全绿。
- 列 COMMENT 含「不可用于鉴权」字样。

**工程门**

- `register.go`（`modules/bot_api/api_i18n_test.go:38-39`）与 `api_manager.go`
  （`modules/robot/api_i18n_test.go:33`）已在 `NoLegacyResponseError` 守卫列表内，
  本任务不新增 handler 文件，无需改列表。
- 不新增 errcode、不改任何错误响应路径（非法值只 Warn）。
- `go build ./...`、`go vet`（本仓 lint 只启用 govet）、
  `go test ./modules/bot_api/ ./modules/botfather/`、
  `make i18n-extract-check`、`make i18n-lint` 通过。
- **契约文档：本仓无处可补，需要配套的跨仓 PR**（实施期核实后修正）。
  `modules/botfather/swagger/api.yaml` 是 30 行的**占位式端点清单** —— 零
  `requestBody`、零 `description`、零 `components/schemas`，且 `/v1/user/bots` 压根
  不在文件里；为两个字段引入完整 schema 会在一个没人维护 schema 的文件里开新形态。
  而 Bot API 的**权威**字段契约根本不在本仓：`modules/botfather/skill.go` 生成的
  `/v1/bot/skill.md` 自称 deprecated 且「no longer receives Bot API updates」，
  正文指向 `Mininglamp-OSS/openclaw-channel-octo` 的 `skills/octo-bot-api/SKILL.md`。
  所以本任务的契约载体是**代码注释**（`BotRegisterReq.AgentHosting` 的指针语义、
  `normalizeAgentHosting` 的边界理由、`UserBotResp` 两个字段的缺省行为都写在字段上），
  客户端可发现性要靠 openclaw-channel-octo 侧的 SKILL.md 补一条 —— 那是本仓之外的
  改动，**本任务未完成，需要单独跟进**。
