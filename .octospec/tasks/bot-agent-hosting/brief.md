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
   落 `robot.agent_hosting`；同时落 `robot.agent_reported_hosting_at`（该次 hosting 上报的时间）。
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
  走形状校验，但**形状非法一律忽略该字段（已存值保持不变）+ Warn，照常返回成功**。#696 的二次事故就是
  register 被连带拒绝导致 bot 永远起不来，`modules/bot_api/ratelimit_integration_test.go:272-274`
  有回归断言钉着。为一个纯观测字段的取值校验去阻断自愈通道是错误的取舍。
- **四个 `agent_*` 字段一律稀疏写入：缺席的字段必须在 SQL 层面缺席。**（PR #837
  review 后修正，两位 reviewer 独立判为阻塞）初版只对 `agent_hosting` 做了稀疏写入，
  三个既有版本字段仍然「把刚读到的值 merge 回去」—— 正是同一段注释明确否决的写法，
  而且这个 diff **扩大**了窗口：改前只在值不同时 UPDATE，改后任何 `agent_*` 字段存在
  就 UPDATE，于是「只报 hosting」（本功能新引入的请求形态）会把三个版本列写成陈旧读值。
  丢更新路径：A 读 → B 写新 agent_version → A 把旧值写回，而两个 runtime 同时 register
  同一 Bot 今天没有任何机制阻止。现在四个字段全是 `*string`。
- **「已上报」的判定在两组字段上不同，这个不对称是刻意的**（review round 2 修正）：
  - **三个 legacy 版本字段：非空才算上报。** 它们在 merge base 上的契约是「空值即不变」
    （省略与传 `""` 都是 no-op），所以 `updateRobotAgentInfo` 对它们**空值也跳过**。
    第一轮修复只跳过 nil、把空串照写，于是「报空」从保留变成清空 —— 而 register 是
    重连路径，任何序列化器对未填字段输出 `""` 的客户端都会**每次重连擦一次**，
    HTTP 200、无日志、事后与「从未上报」不可区分。两位 reviewer 独立端到端复现。
    **稀疏写入与「空值即不变」本来兼容**：丢更新的根源是「替换成刚读到的值」，
    不是「跳过该列」。
  - **`agent_hosting`：非空值才算上报，`""` 同样是「不变」。** 与三个 legacy 字段
    **完全同口径** —— 撤回走保留 slug `none`（`bot_api.AgentHostingNone`），
    由 `normalizeAgentHosting` 归一成空串入库。
    这一条是 round 4 定稿，取代了 round 2 那句「`""` 是撤回的唯一方式」：让同一个
    JSON 里 `""` 对三个字段是「保持」、对第四个是「清空」，既是客户端作者的陷阱，
    也会让「从不填该字段但总是发这个 key」的客户端每次重连落进 `('', 非NULL)` ——
    而那个状态被三处文档定义为「曾上报后显式清空」，等于把序列化器默认值读成刻意撤回。
    保留 slug 让撤回显式、可 grep，且它本身满足 `agentHostingPattern`，不需要额外字段。
  推论：`isEmpty()` 只是廉价前检，真正的「有无内容可写」是 `len(set)==0`。
- **`agent_reported_hosting_at` 只在 hosting 被上报时前进，且由 SQL `NOW()` 写。**
  两点都是 PR #837 review 后修正的：
  - **只在 hosting 上报时前进**（列名也从 `agent_reported_at` 改成现名）。初版对任何
    `agent_*` 上报都刷新它，于是「只报版本」会替一份该次上报从未提及的数据背书新鲜度 ——
    而这个分歧场景正是用来论证指针语义的那个「新 runtime 漏报 hosting」。语义与列名
    现在一致：它回答「同一行的 `agent_hosting` 有多新」，不是「这个 bot 最近有没有
    上报过什么」。
  - **SQL `NOW()` 而非 Go 侧 `time.Now()`**。它与 `bound_at`（`botfather/db.go` 用
    `NOW()` 写）在同一个响应里并列，而 Go 侧写入要经驱动 `Config.Loc`（默认 UTC，
    DSN 未设 `loc`）转换，应用镜像又固定 `TZ=Asia/Shanghai` —— MySQL session 时区非
    UTC 时两个时间戳相差 8 小时且无标记解释（两位 reviewer 各自实测复现）。
    生产 MySQL 目前是 UTC，所以那是**潜伏而非已发生**；改用 `NOW()` 后彻底不再依赖
    这个前提。初版的测试结构上抓不到它（两个 Go 写的值互比，或用 SQL `NOW()` 播种），
    所以补了一条把两种来源放进同一断言、并显式把 session 时区设成 `+08:00` 的测试。
  即使值与库中一致也刷新（「收到过上报」而非「值何时改变」），这去掉了「值未变则跳过
  UPDATE」的优化。成本可接受：`modules/common/system_settings.go:1346` 记录 register
  「只在重连时调用」。**没有这一列，`agent_hosting` 是个无法判断可信度的裸值** ——
  `robot.updated_at` 连 `ON UPDATE` 都没有（`modules/robot/sql/20210926000001_robot_legacy01.sql:14`），
  现有 `agent_platform` 就是这种无从判断新鲜度的裸值，不要复制这个缺陷。
- **自报数据不可信，不得用于鉴权或配额。** 列 COMMENT 与 Go 字段/常量注释都要写明。
  服务端拿不到可信的托管形态来源（见 Background 的 client_id 论证），这个字段填的是
  **观测**缺口，不是信任缺口。形状校验是**数据质量**约束，不是授权约束 ——
  它挡 caller-controlled 字节，不建立「调用方有资格声称该值」。任何后续想用它做
  授权判定的改动，得先建可信来源。
- **值域开放，只校验形状；校验顺序在两个方向上都是载荷性的。**
  `TrimSpace`（返回子切片、零分配）→ 长度上界 → **ASCII 检查** → `ToLower` + regexp。
  - **长度在折叠之前**：register 的 JSON body 有 4 KiB 上限（PR #837 review 后新增，
    见下），但 `ToLower` 仍会分配与输入等大的新串，没理由为否决一个不可能匹配的值付这笔钱。
  - **ASCII 在折叠之前**（review 后修正）：Go 的简单小写映射**不限于 ASCII** ——
    `U+212A KELVIN SIGN` 折成 `k`、`U+0130` 折成 `i`，所以折叠排在 ASCII-only 正则
    之前时它们能通过（两位 reviewer 各自实测）。这不是注入（落库的仍是干净 ASCII slug，
    且每个这类输入都有一个调用方可以直接上报的 ASCII 孪生值），但它把两个不同输入
    静默折叠成同一个存储值，并让本函数注释里「confusables all fail it」这句话变成假的
    ——而测试里的 Unicode 用例恰好只挑了会失败的 `U+200B`，使 brief 的验收标准看起来
    已验证。**ASCII 性质同时是列宽不变量的前提**：`len()` 数字节而 `VARCHAR(64)` 数字符，
    两者相等只因为拒了非 ASCII。
  大小写折叠而非拒绝（`Self_Hosted` 与 `self_hosted` 意图相同，折叠避免一类无声失败）。
- **形状非法 = 本次忽略该字段，已存值保持不变；撤回要显式报 `none`。**（round 4 定稿）
  初版把 `hosting = &normalized` 写在合法性分支外面，于是被拒的值以空串进 `SetMap`
  覆盖已存值 —— 而 PR 描述说的是「degrades to 'not reported'」（读作保持不动），
  字段注释的规则是「present overwrites」（实际清空），两种读法互相矛盾。定为保持不变的
  理由是触发场景不需要恶意：`self-hosted`（带连字符，正是本命名引用的 GitHub Actions
  写法）会被拒，一次客户端拼错就把全量 bot 的这一列刷空，只留一行日志。
- **register 的 body 有 4 KiB 上限**（review 后新增）。body 是纯 telemetry，但
  `binding.JSON` 是无上限的 `json.NewDecoder`，而本路由此前没有任何 body 界
  （四个 sibling bot_api 路由都有）。App Bot 分支上这一点最刺眼：那次解码只换来一行日志。
  超限按「未上报」处理，绝不让 register 失败。
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
  `modules/botfather/db.go:30-55` 各有一份。`agent_reported_hosting_at` 是 `TIMESTAMP NULL`，
  必须用 `dbr.NullTime` 承接 —— `modules/botfather/db.go:50-52` 的 `BoundAt` 注释已记下
  同一个坑：「用 NullTime 承接 NULL，否则 `Select("*")` 把 NULL bound_at 扫进 string
  会报错，殃及所有 robot 查询」。
- **`UserBotResp` 是 wire contract，只能 additive。**
  （`modules/botfather/model.go:149-165`）只增字段，现有字段不改名/不删/不改类型/不改语义。
  `agent_hosting` 跟同组的 `agent_platform` 一样带 `omitempty`（同一组字段两种缺省行为
  会让客户端写两套解析）；`agent_reported_hosting_at` 跟 `bound_at` 一样用 `*string` 显式下发
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
- **迁移写朴素 DDL，且必须是单条原子 ALTER。** 前半句是本仓既定原则：sql-migrate 已用
  `gorp_migrations` 追踪每个文件的版本，不要在每条迁移里堆幂等代码；存在性守卫
  （如 `modules/botfather/sql/20260603000002_botfather_legacy01.sql`）是"同一份迁移
  必须跨多个状态不同的环境运行"时的应急路径，不是默认写法 —— 它可读性差、reviewer
  看不出真实意图。实测口径（只数 `Up` 段，排除 `Down` 里的 `ADD COLUMN`）：全仓 83 个含 `ADD COLUMN` 的迁移里仅 14 个带守卫，
  且集中在该原则确立之前（2026-06 前后），此后的 20260728 / 20260810 / 20260830
  全是裸 DDL。
  后半句（单条原子 ALTER）解决的是**另一件事**：不留「一列已加、一列没加」的列级
  中间态。#239 那次半应用出在**多语句**迁移上（ADD COLUMN + DROP INDEX + ADD UNIQUE，
  DDL 隐式提交、中途失败即残留），而 20260417000001 的 agent_* 三列是三条独立 ALTER、
  同样可半应用。本任务两列写在一条 `ALTER TABLE ... ADD COLUMN, ADD COLUMN` 里。
  **单条原子 ALTER 不解决可重入性** —— review round 4 指出这一点，是对的：两个 pod
  竞争同一迁移、或进程在 DDL 隐式提交与 `gorp_migrations` 记账之间死掉，原子性都帮不上。
  那两个风险的解法在**部署层**（迁移加锁或单 pod 执行），不在每条迁移里。
  另外显式 pin `ALGORITHM=INSTANT`（同 `modules/opanalytics/sql/20260830000001`）：
  不 pin 的话，目标 MySQL 无法满足时会静默退化为 COPY 锁表。
- **迁移文件放 `modules/botfather/sql/`，与 `agent_platform` 三列同源。** 执行顺序按
  文件名时间戳**全局**排序、跨模块（`internal/modules.go:3-8`：sql-migrate 把所有模块的
  SQL 汇成一个 slice 按 VersionInt 排序，「botfather ALTERs robot」是被认可的形状）。
- **自报值的长度上界必须在折叠大小写之前。** 顺序固定为 `TrimSpace`
  （返回子切片、零分配）→ 长度上界（`maxAgentHostingLen`）→ ASCII 检查 → `ToLower`。
  原始理由（「10MB 的值会花 10MB 分配去否决」）在 4 KiB body 上限落地后**已不适用**
  （review round 2 指出）；保留该顺序的现有理由是它零成本，且不让这个界依赖两层调用
  之外的另一个限制。
  （**已删除**：此处原有一条「三个既有版本字符串保持原有的 merge-then-write 契约，
  只有新列享受更严处理」——它已被上面「四个字段一律稀疏写入」那条取代，两条并存正是
  round 2 的 P2-1：同一份文档里两条互相矛盾的载荷性规则，而**过时的那条描述的行为
  恰好能防住 P1-1**。这个 PR 自己新增的 learning
  `a-rule-in-a-comment-is-not-applied.md` 在 brief 里重演了一次。）
- **已知限制：agent_* 全组共用一条 UPDATE，一个超长字段会挡住整组**（Verify 期发现，
  本任务不修）。三个既有字符串字段无长度校验、列宽 `VARCHAR(50)`，而生产与测试库都开着
  `STRICT_TRANS_TABLES`（实测 200 字节写 `VARCHAR(50)` → `1406 Data too long`）。
  所以一个报超长 `agent_platform` 的客户端，会让同一请求里合法的 `agent_hosting`
  也写不进去，且 `register` 仍返回 200（失败只进日志，带 1406）。
  改动前后该 UPDATE 都会失败（旧代码里超长值同样存不进，且值永不相等所以每次重试、
  每次失败），**不是本任务引入的回归** —— 但新字段的可写性现在依赖旧字段的卫生。
  修它要么给三个既有字段加「超长则忽略该字段」的降级（改既有行为），要么把 UPDATE
  拆成两条（放弃原子性换解耦），都该单独决策。
  **review round 2 的补充判断（认同，留 follow-up）**：稀疏写入落地后，语句层面
  各列已经解耦，所以「校验三个 legacy 字段的长度」或「拆分语句」比本 brief 原先
  「deserves its own decision」的措辞暗示的**要便宜得多**。应开 follow-up issue
  而不是无声带过。仍不在本任务范围：它改的是三个既有字段的行为。
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

## 测试的区分力（round 4 定稿的规则）

**每个行为断言都要过变异检查，且测试注释必须写明它杀掉哪个变异。** 这条是被
reviewer 的过程观察逼出来的：连续三轮，每轮都产出一条"为回答『你的测试区分不了两种
实现』而写"的测试，而每轮那条测试都有同一个缺陷 —— 注释声称的区分力没有被验证过。
一条注释声称有区分力却没有的测试，比没有测试更糟：它会被相信。

已验证能杀掉对应变异的测试（每条都实际跑过变异版）：

| 变异 | 会红的测试 |
|---|---|
| `dbr.Expr("NOW()")` → `time.Now()` | `TestUpdateRobotAgentInfoStampsHostingTimeWithSQLNow`（断言语句里是字面 `NOW()`）|
| 在调用点把 robot 行的值赋回 `req.Agent*` | `TestRegisterUserBotPassesStoredOnlyAsASkipSnapshot` |
| 去掉 legacy 的空值跳过 | `TestRegisterEmptyLegacyVersionPreservesStoredValue` + SQL 形状表 |
| `zap.Int("rejectedLen")` → `zap.String("value", …)` | `TestApplyAgentReportWarnsOnMalformedHostingWithoutLeakingTheValue` |
| 删掉 `registerAppBot` 的 `ba.Warn` | `TestRegisterAppBotWarnIsTheDeliverable` |
| 改回 `_ = c.ShouldBindJSON` | `TestRegisterPartiallyMalformedBodyAdoptsNothing` |
| db 层回写 stored（`else if report.X == nil && stored.X != nil`） | `TestUpdateRobotAgentInfoOmitsUnreportedColumns` 的两行 `stored` 有值用例 |
| 快照里加 `Hosting:` + `applyAgentReport` 里补 `req.AgentHosting = stored.Hosting` | 两条源码守卫（快照形状 + 禁止赋回 req） |
| 删掉 `api_user.go` 的时间戳映射 | `TestListUserBotsExposesAgentHosting`（按 robot_id 解码逐个断言） |
| `MaxRegisterBodyBytes` 改成 4096 之外的值 | `TestRegisterOversizedBodyDegradesToNoTelemetry`（字面量 4096/4097） |
| hosting 的 `""` 回到清空语义 | `TestRegisterHostingNoneClearsTheShape` |

**round 5 补的两处，各自的教训**：

1. **一张断言"某列缺席"的表，必须有一行让那列在库里有值。** SQL 形状表原先所有
   「legacy 列缺席」用例都跑在 `stored` 为空上，于是「stored 有值 + 字段缺席」——
   唯一能暴露 read-merge-write 的组合 —— 从未被构造，reviewer 实测那个变异能穿过
   整个套件。表越大越容易漏掉这种"缺一行"，因为看起来已经很全了。
2. **用常量表达期望值，等于没有期望值。** 第一次修 body 上限边界时我写的是
   `exactBody(bot_api.MaxRegisterBodyBytes)`，改常量测试跟着变，`8<<10` 和 `4095`
   两个变异都照样绿 —— 与 reviewer 指出的「区间没钉住」是同一个病，只是换了个写法。
   现在断言 `require.Equal(t, 4096, bot_api.MaxRegisterBodyBytes)` 再用字面量构造。

**一条端到端时区测试被删除而非修好。** 它把会话时区调到 `+08:00` 再比两个时间戳，
确实能杀掉 `NOW()` → `time.Now()` 的变异（实测偏差 `7h59m59s`），但要让 register 的写入
落在被改过时区的会话上，必须把 `ctx.DB()` 的连接池压到单连接 —— 那是**进程级共享状态**：
压池期间其它测试的查询排队甚至超时，整包成片失败；用 `SetConnMaxLifetime` 强制作废连接
收尾同样不稳定（失败集每次不同）。而它守的不变量有确定性的替代守法 ——
「语句里用的是 SQL `NOW()` 而非绑定参数」是充分条件，由
`TestUpdateRobotAgentInfoStampsHostingTimeWithSQLNow` 直接断言、同样能杀同一变异、且不碰
共享状态。**取舍原则：一条需要污染进程级状态才能运行的测试，在整包里就是不可靠的。**
删除理由写在原代码位置，防止下一个人以为「时区没人管」而重新加回同一形状。

**两条端到端测试已如实降级**：`TestRegisterVersionOnlyReportDoesNotTouchHosting` 与
`TestRegisterHostingOnlyReportDoesNotClobberVersions` 只证明「缺席字段的值不被本次上报
改动」这个可观察结果。它们**不能**区分稀疏写入与 read-merge-write —— 带外 UPDATE 落在
下一次 register **之前**，合并实现读到的已是新值、再写回去结果相同；真正的交错窗口在
`queryRobotByBotToken` 与 UPDATE 之间，行为测试无法确定性插入。那个不变量改由
`modules/bot_api` 的两条源码守卫承担（`TestApplyAgentReportCannotReadTheRobotRow` /
`TestRegisterUserBotPassesStoredOnlyAsASkipSnapshot`），它们区分的是**赋值目标**：
把 robot 行的值作为独立 `stored` 快照传参用于 skip 比较是合法的（不匹配时写入的是
调用方的值），赋回 `req.Agent*` 任一字段则被禁止。

## 测试环境的一个坑（排查耗时，值得记）

`modules/botfather` 整包出现「失败集每次不同、且失败测试耗时恰好 ~5s / 30s」时，
**先重启 WuKongIM 容器再怀疑代码**。本任务在此误判过：连续 8 天运行的 本地 WuKongIM 容器状态劣化，`UpdateIMToken` 开始 5s 超时，表现为
`err.server.bot_api.im_token_failed`（HTTP 400）。它看起来像测试污染 —— 单跑绿、整包红 ——
但串行跑（`-p 1 -parallel 1`）同样失败、且失败集仍在变，就说明不是顺序依赖。
重启 IM 后整包 15.7s 全绿，含此前 30s 超时的 `TestBotGroupCreate_*`（那两条在改动前的
head 上也失败，本就与本任务无关）。

排查过程中确实找出并修掉了两个**真实的**测试污染，保留：
- `POST /v1/bot/register` 挂的是 `StrictIPRateLimitMiddleware("bot_register")`，桶按 **IP**
  计而整包共享同一 httptest 客户端 IP；本文件是全仓 register 调用最密集的一批
  （26 次），所以 `botRegister` helper 每次调用前清桶，而不是在 setup 清一次
  （一条测试内连发多次就会自己打满）。
- 上一版时区测试的 `SET time_zone` 打在连接池上。

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
  为 `self_hosted`，`agent_reported_hosting_at` 非 NULL。
- 带 `agent_hosting:"octo_hosted"` → 落库为 `octo_hosted`。
- 带形状非法的值（`<script>alert(1)</script>`、内嵌空格、连字符、数字/下划线开头、
  控制字符、零宽空格、`U+212A`/`U+0130` 折叠型混淆字符、带重音拉丁字母、全角字符、
  中文、超长串）→ **仍 200**，且**已存的合法值保持不变**（不是清成空串），
  日志有 Warn（且**不记原始值**，只记 `rejectedLen`）。
- 显式报 `""` → **保持不变**（"" 对四个字段一律是「未上报」）。
- 显式报 `none` → 撤回（入库为空串，时间戳前进）；与「被拒」区分：撤回是格式良好的上报。
- 第三方 `<vendor>_hosted` 无需改服务端即可原样落库，并原样出现在 `GET /v1/user/bots`。
- `cloud` / `local` 合法通过（已知代价，见 Load-bearing）。
- 不传该字段（旧客户端、空 body）→ 旧值保留，且现有三字段行为逐字节不变。
- 显式传 `""` → 保持不变；传 `none` → 撤回为空串（指针语义让「没传」与「传了」可区分，
  而「传了什么」再决定是保持还是撤回）。
- 上报值与库中一致时 `agent_reported_hosting_at` 仍被刷新（语义是「最近一次收到上报」）。

**App Bot**

- `app_` token 调 register 带 `agent_hosting` → 200，响应形状与今天逐字节相同，
  **不落任何库**，日志有 Warn。
- 守卫测试断言 `app_bot` 表不存在 `agent_hosting` 列（钉住「A 方案」前提，防止半支持）。

**读出**

- `GET /v1/user/bots` 每项含 `agent_hosting` 与 `agent_reported_hosting_at`；未上报时
  `agent_hosting` 被 `omitempty` **省略**（不下发空串），`agent_reported_hosting_at`
  显式 `null`。
- **负向断言**：`GET /v1/users/:uid` 的 extraMap 与 `POST /v1/users/batch` 的响应
  **不含** `agent_hosting`（防止后来人顺手接入 IM 全员面）。
- `UserBotResp` 的现有字段值与形状不因本任务改变。
- 未上报的 Bot：`agent_hosting` 被 `omitempty` 省略（不下发空串），
  `agent_reported_hosting_at` 显式为 `null`。

**legacy 字段的空值语义（review round 2 新增）**

- 三个 legacy 字段报 `""` → **保留已存值**（merge base 的契约），只有非空值才写。
  端到端：播种三值 → 报 `{platform:"", version:"1.2.4", plugin:""}` →
  version 更新、另两个保持不变。
- 三个 legacy 字段**全**报 `""` → 一行都不写，且不盖 hosting 时间戳
  （「被送来」≠「可写」）。
- `agent_hosting` 报 `""` → 保持不变（与 legacy 的空值语义**刻意相同**）；
  报 `none` → 撤回（列写空串 + 时间戳前进，因为撤回是一次真实上报）。
- SQL 层逐形态断言：legacy 空串不进语句、空串与非空混报只写非空的那个。
- **变异验证**：去掉 legacy 的空值跳过后，端到端与 SQL 层两条都必须红。

**body 与解码（review round 2 新增）**

- body 超 `MaxRegisterBodyBytes`（4 KiB）→ register 仍 200，已存列**不受影响**。
- 类型错误的 body（`{"agent_platform":"OpenClaw","agent_version":12345}`）→
  **一个字段都不采纳**。`json.Decoder` 会先填好已解析字段再报错，所以「忽略 bind 错误」
  等于采纳一个前缀 —— 半更新的列看起来像一次成功上报，比不更新更糟。
- nil `Request.Body` → 当作「什么都没上报」，**不得 panic**。线上走不到（`net/http`
  保证非 nil），但 handler 也会被进程内驱动，`http.NewRequest(..., nil)` 留下的就是
  nil；`MaxBytesReader` 会包住 nil `ReadCloser` 并在首次 `Read` 解引用空指针。
- 尾随垃圾（`{...} trailing`）→ **前面那个完整对象照常采纳**，尾巴被忽略。
  这是 round 7 的修正，早先一轮写的是「整体不采纳」。改回去要判定输入结束，而判定
  输入结束要在未终止的 body 上多读一个字节 → 端点可被挂死（见下）。
- **停滞的 body 不得挂住 handler**：客户端发完一个完整对象后不终止 body（chunked
  无结束块），`readAgentReport` 必须仍在有限时间内返回。`MaxBytesReader` 限的是字节
  数不是时间，而该路由由零值 `http.Server` 承载（gin 的 `r.Run()`，`ReadTimeout=0`），
  没有任何东西兜底。register 是 bot 掉线后唯一的自愈通道（#696）。
- **变异验证**：① 改回 `_ = c.ShouldBindJSON(&staged)` → 类型错误那条红；
  ② 加回 `dec.Token()` 的 EOF 检查 → 停滞 body 那条超时（实测正好 3.00s 后 Fatal）
  且尾随垃圾两条（单元 + 端到端）红；③ 删掉 nil body 早返回 → nil 那条 panic。

**并发与时钟（PR #837 review 后新增）**

- 只报 `agent_hosting` 时，三个版本列**不在 UPDATE 语句里**（sqlmock 直接断言 SQL 文本）。
  端到端补一条：带外改掉 `agent_version` 后只报 hosting，带外值必须存活。
- 只报版本时，`agent_hosting` 与其时间戳都不在语句里；端到端同样用带外写入验证
  （初版那条「值保留 + 时间戳前进」的断言在写回实现下同样会过 —— 论证不成立）。
- 什么都没报 → 不发任何语句（sqlmock 无 Expect，一旦发出即失败）。
- `agent_reported_hosting_at` 由 SQL `NOW()` 写：语句里出现字面 `NOW()`（SQL 层断言）。
  端到端的时区比对测试已删除 —— 理由见「测试的区分力」。
- 形状非法时发一条 Warn，且日志**不含原始值**（只有 `rejectedLen`）；合法上报无 Warn。
- App Bot 那条 Warn 的守卫走 `go/ast` 而非子串：要断言的「带 uid、不带客户端上报的
  原始值」是**调用实参**的性质，`assert.Contains(body, "ba.Warn(")` 表达不了 ——
  round 7 量到把它换成 `ba.Warn("ignored", zap.String("agent_hosting", *req.AgentHosting))`
  （丢 uid 又把调用方可控值写进日志）照样能过。AST 版同时对接收者改名免疫（纯重构
  不该让守卫变红，子串版会）。
- App Bot 上报时的 Warn 由 `TestRegisterAppBotWarnIsTheDeliverable`（源码守卫）钉住 ——
  它是这个分支解析 body 的唯一产出，删掉那行 Warn 会让该测试红。

**边界与已知限制**

- 空 JSON `{}` 与完全无 body 的 register：不写任何 agent_* 列，`agent_reported_hosting_at`
  保持 `NULL`（否则「收到过上报」的语义失真，且是纯写放大）。
- 只报版本、不带 `agent_hosting`：该列不被写（值保留），且 `agent_reported_hosting_at`
  **不**前进（它只在 hosting 被上报时推进）。
  注意这条只证明「可观察结果」，**不**区分稀疏写入与 read-merge-write —— 后者由
  `modules/bot_api` 的源码守卫钉住，理由见下方「测试的区分力」。
- 超长 `agent_hosting`（>64 字节 = >列宽）在折叠大小写之前就被否决；
  恰好 64 字节的合法 slug 放行（边界是 `<=`）。
- register 的 body 超 4 KiB → 按「未上报」处理，register 仍 200。
- `TestRegisterOverlongPlatformBlocksTheWholeAgentUpdate` 显式检查
  `@@SESSION.sql_mode`，非严格模式下 skip 而不是假红（它断言的行为只在严格模式成立）。
- `maxAgentHostingLen == 列宽`（从迁移文件正则提取比对），且两个自用取值都能过校验。
- 超长 `agent_platform` 连带挡住合法 `agent_hosting`：这是**已知限制**，由测试钉住
  当前行为，见 Load-bearing 同名条目。

**存储与迁移**

- 迁移是单条原子 `ALTER`：要么两列都在、要么都不在，不存在「一列已加一列没加」的
  中间态。重跑保护由 `gorp_migrations` 记账承担（朴素 DDL 原则），**不**在迁移里做幂等；
  dev/staging 若已跑歪，按既定路径 drop & recreate test DB，不反过来给迁移加守卫。
- `agent_reported_hosting_at` 为 NULL 的行不破坏任何 `Select("*")` 查询：三个 struct
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
