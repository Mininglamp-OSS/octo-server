---
type: Task
title: "Task: project-p2-all-member-group"
description: 新建项目弹窗落地：建项目时可带入自己的 AI 分身、创建后自动生成一个"全员群"，并让全员群的成员集合始终等于项目的活跃成员集合（加入项目自动进群；离开项目自动退群已由 P1 覆盖）。P1 把"群属于项目"做实了，本期把"项目自带一个群"做实。
tags: ["space", "isolation", "acl", "error-response", "i18n", "rate-limit", "wire-contract", "migration", "testing", "commit"]
timestamp: 2026-09-07T00:00:00+08:00
# --- octospec extension fields ---
slug: project-p2-all-member-group
upstream: 新建项目弹窗原型截图（口头需求，无 issue）
source: user
---

# Task: project-p2-all-member-group

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> 本文是**产品视角的第一版**：每条决策都给了建议默认值，但都标着"待确认"。
> 确认前不进入 Implement。

## Goal

新建项目弹窗（原型）有四个要素，逐项对应到要交付的行为：

| 弹窗要素 | 要交付的行为 |
|---|---|
| 项目名称 | 已有，不变 |
| 共同目标 | 建项目时可填一段文字，随项目保存并回显 |
| 我的 AI 分身（多选，"仅可带入自己的分身，确认后直接加入"） | 勾选的分身在建项目时**直接成为项目成员**，并进入全员群 |
| "创建后自动生成全员群" | 项目创建成功后，**立即**存在一个属于该项目的群：群名 = 项目名，群主 = 项目创建者，初始成员 = 创建者 + 勾选的分身 |

"全员群"的定义，也是本期要守住的不变量：

> **I4** — 每个活跃项目有且仅有一个全员群；全员群的活跃成员集合 **等于** 该项目的活跃成员集合（`status = 1 AND removing = 0`），系统 bot 按 P1 白名单豁免。

I2（P1）保证的是 ⊆：项目群里的人一定是项目成员。I4 补上 ⊇ 这一半，只对全员群成立，普通项目群不受影响。

于是全员群要跟着项目成员变：

- **加入项目 → 自动进全员群**。今天加项目成员只写 `octo_project_member`，不进任何群，这是本期最主要的新逻辑。
- **离开项目 → 自动退全员群**。P1 的级联（踢出 / 退出 / Space 移除）已经从该项目**所有**群里移人，全员群自然在内，**不需要新代码**。
- **解散项目 → 全员群回退为普通群、成员保留**。沿用 P1 已确认的产品规则，不做例外（见 D10）。

客户端需要能识别"这个群是项目 X 的全员群"，所以项目响应要带 `all_member_group_no`，群响应要开始透出 `project_id`。

## Background

### 现状（按 HEAD 代码核对，P0 #841 / P1 #846 均已合并）

**建项目今天只做两件事。** `POST /v1/space/:space_id/projects`
（`modules/project/service.go:333 createProjectOnce`）在一个事务里写项目行和创建者的
owner 席位，锁序 `space_member(S) → space(X) → octo_project`，三项配额在锁内计数，
`member_epoch` 初始为 0。不建群，不接收任何 bot 参数，响应里没有群号。

**群挂到项目下的能力已经齐了，但只能由用户手动建。** `group.project_id`
（`modules/space/sql/20260906000001_group_project_binding.sql`，`NOT NULL DEFAULT ''`，
`''` 是哨兵）；`POST /v1/group/create` 接受 `project_id`（`modules/group/api.go:1025`），
四道门：必须带 `space_id`、`project.enabled` 开关打开、调用方在该 Space、项目存在且活跃且同
Space（三种失败同一个错误码，防探测）。所有入群路径收口到唯一准入口
`admitOrRestoreMembersTx`（`modules/group/admission.go`），项目群的准入谓词是
「活跃 Space 成员 **且** 活跃项目成员（`removing = 0`）」，`project_id = ''` 时零查询短路。

**离开方向的级联已经做完。** 项目席位关闭 → `octo_project_member_removal_cleanup`
outbox → 群侧 `detachMemberFromProjectGroups` 逐群调 `RemoveGroupMembers`
（`modules/group/project_cascade.go`）；群主离开时先做群主移交，无继任者则把群回退为
Space 直属；项目解散 → `revertProjectGroupsToSpace`。这些对全员群同样生效。

**加入方向没有任何同步。** `addOneMemberOnce`（`service.go:771`）提交后只做缓存失效，
不通知群侧。

**没有"全员群"这个概念。** `group` 表没有列能区分"项目的全员群"和"项目里用户自己建的群"；
`GroupResp`（`modules/group/service.go:828`）不透出 `project_id`，这是 P1 明确留给 P2 的
第一项（sidebar / 群详情 / 群列表透出）。

**建群服务层的两个硬约束。** `Service.CreateGroup`（`modules/group/service.go:1103`）要求
`Members` 非空（handler 的 `groupReq.Check()` 同样要求），创建者由服务层自动加入；只有创建者
一个人的群今天建不出来。每日建群配额 `Group.SameDayCreateMaxCount` 只在 handler 里检查
（`api.go:1101`），服务层调用天然绕过。IM 频道在事务提交后创建，失败做补偿删除；随后发
`SendGroupCreate` 群创建通知。

**AI 分身在代码里是什么。** 通讯录改版（`GET /v1/space/directory`，#842）的口径：
`robot.creator_uid = 本人`、`robot.status = 1`、`robot.agent_hosting <> 'self_hosted'`、
bot 在本 Space 有活跃 `space_member` 行、不在 `pkg/space.SystemBots` 内。botfather 建 bot 时
会写 `space_member`（`modules/botfather/db.go:194`），所以分身能通过 I1，成为项目成员没有
结构性障碍；进项目群时它是普通 bot，**不豁免** I2，需要项目席位——这正好是本期要给它的。
`agent_hosting` 是客户端自报值，只能当展示过滤，不能当授权信号（`api_directory.go:45`）。

### 调用方向的硬约束，决定了实现形状

`modules/project` **禁止** import `modules/group`（`pkg/project/import_guard_test.go`，
`go list -deps` 两个方向都是 0，P1 把这条写进了验收）。项目侧要让群侧做事，只能用反向注册：

- 已有先例一：`RegisterProjectMemberRemovalStep` / `RegisterProjectDisbandStep`
  （`modules/project/cascade_registry.go`），群侧在 `1module.go` 构造时挂进来。
- 已有先例二：Space 的预设群自动入群 `RegisterPresetGroupAdmitter`
  （`modules/space/preset_group_admitter.go`），群侧实现 `admitToPresetGroup`
  （`modules/group/preset_group_admission.go`）：**自带事务、自取版本号、走准入口、提交后
  IM 订阅、失败返回错误由调用方记日志**。这就是"加入项目 → 进全员群"要抄的模板。

所以本期新增两个注册点，都在 `modules/project` 定义、由 `modules/group` 实现：

1. **全员群创建器**：`RegisterAllMemberGroupProvisioner(fn)`，输入项目 id / Space / 创建者 /
   初始成员，输出 `group_no`。群侧用 `Service.CreateGroup` 实现，走标准建群（IM 频道、群创建
   通知、准入闸门 A3/A4），不重新实现建群。
2. **全员群准入器**：`RegisterAllMemberGroupAdmitter(fn)`，输入 Space / group_no / uid，
   群侧照 `admitToPresetGroup` 实现。

两者都在项目事务**提交之后**调用，原因和 `runDisbandSteps` 的注释一样：钩子在另一个模块的表上
做事务，锁在项目行上跨模块等待会把建项目串行化到该项目的每一次群写入上。

### 原型里还没说清、代码也回答不了的事

弹窗只覆盖"创建"这一瞬间。"全员"这个词隐含的持续义务（后加入的人要进、群主不能把群解散掉）
弹窗上没有字，但不做的话产品承诺就只成立一秒钟。本 brief 把这些义务显式列成决策，交产品拍板。

## 产品决策（建议默认值，全部待确认）

每条给出建议、理由、被拒绝的替代方案。确认后改成 P1 brief 那种 "D-n — 已定" 的口吻。

**D1 — "共同目标"复用 `octo_project.description`，不加新列。** 500 字上限已够；前端文案叫
"共同目标"只是展示层的事。替代方案是新增 `goal` 列，被拒绝：两个语义相近的文本字段会让
后续每个表单都要回答"填哪个"。

**D2 — 分身的资格口径与通讯录一致，服务端复核，不信任前端。** 建项目请求新增可选
`agent_uids []string`。服务端逐个校验：`robot.creator_uid = 调用方`、`robot.status = 1`、
`user.robot = 1`、在本 Space 有活跃 `space_member` 行（I1，事务内锁定，与人类成员同一条
路径 `requireSpaceSeatsTx`）、不在系统 bot 白名单。**是否排除 `self_hosted`：建议排除**，与
通讯录选择器同口径——否则用户在选择器里看不到的分身可以从接口带进来。但 `agent_hosting`
是自报值，排除它是产品一致性，不是安全边界，写进注释。上限沿用 `MemberBatchMax`（默认 200）。

**D3 — 任一分身不合格，整单拒绝，不做部分成功。** 建项目是一次性动作，"项目建了但有两个
分身没进来"比"请修正后重试"更难解释，而且部分成功会让 D4 的失败语义再多一种。错误码新增
`err.server.project.agent_not_eligible`，`details.uids` 列出不合格的 uid（调用方自己提交的，
不构成泄露），不区分"不是你的 / 不在本 Space / 不存在"，原因进日志——与 P1 建项目群"三种失败
同一个码"的反探测口径一致。

**D4 — 全员群建失败，项目仍算创建成功；响应里 `all_member_group_no` 为空。** 项目是主实体，
群是它的附属；建群发生在项目事务提交之后、且 IM 频道创建在群事务之外，回滚项目需要跨模块
补偿删除，而 P1 对 disband 步骤失败已经选了"不回滚、对账报出"，这里保持同一个规则。
配套：(a) 指标 `project_all_member_group_provision_failures_total`；(b) I4 对账扫描
"活跃项目没有全员群"作为一条独立告警；(c) **幂等补建**：`addOneMemberOnce` 和 I4 扫描都在
发现 `all_member_group_no = ''` 时尝试补建（补建走同一个 provisioner，用项目行锁保证只建一个）。
被拒绝的替代方案：回滚项目——见上；同步建在项目事务内——违反锁序，把项目行锁跨到 IM 调用上。

**D5 — 全员群身份记录在项目侧：`octo_project.all_member_group_no VARCHAR(40) NOT NULL
DEFAULT ''`，加普通索引。** 一个项目一个值，"有且仅有一个"由列本身保证，不需要在 `group` 表上
做带 NULL 技巧的唯一索引；迁移放 `modules/project/sql`，因为只有 `modules/project` 写它。群侧
要判断"这个群是不是全员群"时（D7），通过 `pkg/project` 新增的谓词
`IsAllMemberGroup(session, spaceID, projectID, groupNo)` 查项目行，**只在 `group.project_id != ''`
时才查**，Space 直属群零成本（延续 P1 的 C1 纪律）。被拒绝的替代方案：在 `group` 表加
`project_role` 列——迁移得放 `modules/space/sql`（P1 踩过的坑），且"每个项目至多一个"要靠
应用层保证。

**D6 — 全员群群主始终是项目 owner。** 建群时群主 = 创建者 = owner，这部分免费。项目 owner
转让（`changeMemberRole` 的 transfer 分支、`leaveProject` 带 `transfer_to`）时**同步转让群主**，
通过群侧注册的第三个钩子 `RegisterAllMemberGroupOwnerTransfer` 走既有的
`transferGrouper` 逻辑。理由：P1 只在群主**离开项目**时移交群主；owner 转让后原 owner 仍在
项目里，群主就会停在一个普通项目成员身上，而这个人又不能解散/退出全员群（D7），形成一个
没人能操作的群主。**可裁剪**：不做的话把 D7 里对群主的限制也一并放宽，两者要一起决定。

**D7 — 全员群受保护：群主不能解散、任何人不能退群、群内不能踢人、不能手动转让群主。**
"全员"的含义就是这四件事都由项目侧驱动：退出走项目退出，踢人走项目移除，转让走项目 owner
转让。受影响的接口：`POST /:group_no/exit`、`DELETE /:group_no/disband`、
`DELETE|POST /:group_no/members(_delete)`、`POST /:group_no/transfer/:to_uid`，仅当该群是
全员群时拒绝，新错误码 `err.server.group.all_member_group_protected`（一个码，`details.action`
区分动作）。**手动加人不禁止**：加项目成员会被幂等吸收，加非项目成员会被 I2 拒绝，无需新规则。
这是本期最大的一块新增限制，建议**单独一个 PR**，产品可以决定先不做——但先不做意味着
"全员群"在第一个群主点了解散之后就不存在了，I4 对账会立刻报告。

**D8 — 项目改名同步全员群群名；项目 logo 不同步群头像。** 群名 = 项目名是用户识别全员群的
心智，改了不同步等于让全员群"失联"；群名上限 50 字（`MaxGroupNameLen`）短于项目名 64 字，
截断规则与 `CreateGroup` 一致（取前 50 rune）。头像不同步：群头像有自己的一套自定义规则
（`avatar_text` / `avatar_color`），项目 logo 是一个 URL，两者不是一回事。通过第四个钩子
`RegisterAllMemberGroupRename` 实现，best-effort。

**D9 — 自动建群不占用创建者的个人每日建群配额，但受项目每日创建配额约束。** 服务层调用天然
绕过 handler 的 `SameDayCreateMaxCount`，这里把它写成有意为之：用户点一次"创建项目"消耗一次
项目配额（`OCTO_PROJECT_MAX_DAILY_CREATE`，默认 20），不应再被建群配额二次拒绝。

**D10 — 解散项目：全员群按 P1 规则回退为 Space 直属群、成员保留，同时清空
`all_member_group_no`。** 不做例外。P1 已经记录了两条产品建议（解散确认文案说明"N 个群会以
普通群保留"；如果要"连群一起收走"应是显式的 archive 而不是 disband 的默认行为），本期照旧
不动。清空列由项目侧在解散事务内完成，与群侧 detach 是否成功无关（I3 对账兜底）。

**D11 — 建项目带分身时 `member_epoch` 仍为 0。** P0 的规则是"创建即初始名册，第一次真正的
成员变化把它变成 1"；分身是初始名册的一部分，不是变化。

**D12 — 加入项目 → 进全员群，在加成员事务提交后同步调用准入器，逐人 best-effort。**
`addMembers` 本来就是一人一事务，所以每个人提交后紧接着调一次准入器（自带事务 + IM 订阅），
失败记日志 + 指标 `project_all_member_group_admit_failures_total{reason}`，不回滚项目席位
（席位是授权事实，群是它的投影；反过来会让"加人"因为 IM 抖动而失败）。I4 对账扫描负责发现
漏网的人；**不做自动修复**，与 P0/P1 所有对账扫描"只报不修"的纪律一致。批量 200 人 = 200 次
准入 + 200 次 IM 订阅，和今天 `AddGroupMembers` 一次加 200 人的代价同量级。

## Load-bearing list

- **项目侧锁序与写路径守卫。** 声明的锁序
  `space_member → space → project → group → group_member → octo_project_member`
  （`modules/project/db.go:354`）：本期所有群侧钩子在项目事务**提交后**运行，项目事务内不得
  触碰 `group` / `group_member`。新增/改动的写路径（带分身的创建、owner 转让触发的群主同步、
  解散时清空 `all_member_group_no`）必须登记进 `TestWritePathsRevalidateTheActorSpaceSeatInTx`
  和 `TestEveryWritePathEmitsAnAuditEntry`，否则两个守卫静默缩水。touches: `space`, `isolation`, `acl`
- **唯一准入口与源码守卫。** 任何"把人放进全员群"的动作必须经过 `admitOrRestoreMembersTx`；
  `TestNoGroupMemberWritesOutsideTheAdmissionFunnel` 和
  `TestNoGroupMemberRowsBuiltOutsideModulesGroup` 会拦住任何绕路。准入器需要一个新的
  entry 标签（`AdmissionEntryAllMemberGroup`），并加入"每个标签至少被测试发出一次"的守卫。
  touches: `isolation`, `acl`
- **I3 不变量。** `group.project_id` 只在建群路径写入、只在 detach 步骤清空
  （`TestNoProjectIDRewritesOutsideTheDetachStep`）。全员群通过 `CreateGroup` 带 `project_id`
  建出，不新增 `project_id` 写点。
- **`CreateGroup` 服务层契约。** `Members` 非空的检查要放宽为"允许只有创建者"，这是对一个
  被 `bot_api`、`integration` 两处调用的公共服务的行为变更；handler 层 `groupReq.Check()`
  **保持不变**（用户手动建群仍要求至少一个成员）。IM 频道创建失败的补偿删除、`SendGroupCreate`
  通知、`bot_admin` 设置逻辑照旧。touches: `wire-contract`
- **`member_epoch` 规则。** 只允许 +1、每次成员/角色写入同事务内、创建为 0；分身席位在创建事务
  内写入，不 bump。
- **功能开关单一真源。** `project.enabled`（system_setting → env → false，客户端字段
  `project_on`）已经同时约束建项目和建项目群；本期不新增开关，全员群跟随它。关掉只停止新建，
  不放松已有全员群的约束（与 P1 的回滚口径一致）。touches: `wire-contract`
- **对账纪律。** 新增的 I4 两个扫描（"活跃项目无全员群"、"活跃项目成员不在全员群"）必须
  游标分页、按**检查行数**有界，纳入 `TestReconcileQueriesAreBounded` /
  `TestReconcilePageQueriesExamineBoundedRows`；跨进 `group` / `group_member` 的 JOIN 带显式
  `COLLATE`（P1 的 `TestP1ScansSurviveCollationDrift` 是范本）；只在完整轮转后发布 gauge。
- **群侧四个接口的行为变更（D7）。** `exit` / `disband` / `members` 删除 / `transfer` 对全员群
  拒绝，对其他群（含普通项目群）字节不变；判定只在 `project_id != ''` 时发起查询。
  touches: `error-response`, `i18n`, `acl`
- **群主移交的既有逻辑。** `groupExit` 群主退群时选第二老成员（排除其名下 bot）；P1 的
  `querySuccessorForProjectGroupTx` 把继任者收窄到项目成员。D6 的 owner 同步转让复用
  `transferGrouper`，不新写一套。
- **i18n。** 新错误码进 `pkg/errcode/project.go` / `group.go`，全部走 `httperr.ResponseErrorL`，
  5xx ⟺ `Internal=true`；`make i18n-extract-check` + `make i18n-lint` 通过；zh-CN 翻译进
  `active.zh-CN.toml`；新的 group 文件加进 `TestGroupNoLegacyResponseError` 的固定文件清单，
  新的 project 文件加进 `TestProjectNoLegacyResponseError`。touches: `error-response`, `i18n`
- **限流。** 不新增路由；`agent_uids` 搭乘已挂 `SharedUIDRateLimiter` 的建项目路由；
  钩子和对账不是 HTTP 路径，不得长出 Redis 计数器。touches: `rate-limit`
- **迁移。** `octo_project` 加 `all_member_group_no` 列 + 索引，放 `modules/project/sql`；
  `ADD COLUMN … NOT NULL DEFAULT ''` 在 MySQL 8.0 为 INSTANT；无 `group` 表变更。
  迁移文件注释不得出现撇号（P1 迁移文件记录的解析缺陷）。touches: `migration`
- **反探测。** 分身校验失败一个码；`IsAllMemberGroup` 对非项目成员不暴露项目是否存在
  （群侧四个接口在拒绝之前已经要求调用方是群成员，所以不新增探测面）。touches: `isolation`
- **响应契约。** 项目 `Resp` 新增 `all_member_group_no`（空串表示尚未建成）；`GroupResp` 及
  群详情新增 `project_id`（空串 = Space 直属），这是 P1 留给 P2 的透出项的一部分。
  `POST /v1/auth/verify?include=context` 的 `projects[]` **不变**。touches: `wire-contract`
- **测试纪律。** 不改既有测试文件的断言；命中 UID 限流路由的测试在 setup 里重置
  `ratelimit:uid:*`。touches: `testing`

## Out of scope

- **sidebar / 会话同步透出 `project_id` 与"项目树"展示。** 只做群详情和群列表的透出；
  `/v1/sidebar/sync` 的形状变更单独开任务。
- **项目级自助加入 / 邀请码 / `join_mode` 消费方**（P0 留下的 P2 项）。本期加入项目的入口
  仍只有 `members/add`。
- **用户手动创建的普通项目群**的行为：不受 D7 保护，不跟项目改名，不由项目加人同步。
- **`agent_hosting` 的真实性**：自报值，只做展示口径过滤。
- **IM 订阅 / 退订泄漏**（#797、`im-pending-outbox`）：准入器和级联继承同一个泄漏，本期不解。
- **全员群欢迎语**：走既有 `group-welcome-message` 的群级配置，不做项目级默认。
- **项目 logo 同步群头像**（D8 已说明）。
- **外部成员、跨 Space 项目、项目作为读边界**：与 P0/P1 同样不做。
- **对账自动修复**：I4 只报不修（D4 的补建是写路径上的幂等重试，不是对账驱动）。
- **P1 遗留的未决项**（`(space_id, project_id)` 索引构建时长、`queryI2Page` 游标计划、
  org-directory 监听器是否存活、A1/A2/A4-A8 的路径级覆盖）：不在本期顺手解决。

## Acceptance

**建项目带分身**

- [ ] `POST /v1/space/:space_id/projects` 带 `agent_uids` 时，每个分身在**同一事务**内成为
      `role = 0` 的活跃项目成员，`invite_uid = 创建者`，`member_epoch` 仍为 0。
- [ ] 任一 `agent_uids` 元素不满足 D2（非本人创建 / 非 bot / `self_hosted` / 不在本 Space /
      系统 bot / 不存在），整单以 `err.server.project.agent_not_eligible` 拒绝，
      `details.uids` 列出不合格 uid，项目行**未**写入；四种原因在响应上字节相同，仅日志区分。
- [ ] `agent_uids` 超过 `MemberBatchMax` 以 `err.server.project.batch_too_large` 拒绝。
- [ ] 分身席位计入 `max_members` 配额，超限以 `err.server.project.quota_members` 拒绝整单。
- [ ] 新路径出现在 `TestWritePathsRevalidateTheActorSpaceSeatInTx` 与
      `TestEveryWritePathEmitsAnAuditEntry` 的枚举里；分身写入有 `project.create` 审计条目。

**全员群创建**

- [ ] 建项目成功后，`octo_project.all_member_group_no` 非空，且该群
      `project_id = 项目 id`、`space_id = 项目 Space`、`creator = 项目创建者`、
      `name = 项目名（≤50 rune）`，活跃成员集合 = {创建者} ∪ 分身集合。
- [ ] 只有创建者、没有分身时也能建出全员群（`Service.CreateGroup` 允许空 `Members`），
      而 `POST /v1/group/create` 空 `members` 仍被拒绝（handler 不变）。
- [ ] 群侧 provisioner 未注册（只含 `modules/project` 的二进制）时，建项目成功、
      `all_member_group_no = ''`、记一条 Error，不 panic，不回退项目。
- [ ] 让 provisioner 故意失败（如 IM 频道创建失败）：项目已创建，响应
      `all_member_group_no = ''`，`project_all_member_group_provision_failures_total` +1；
      随后一次 `members/add` 触发补建，且并发两次补建只产生一个群（项目行锁）。
- [ ] 全员群的 IM 频道订阅者 = 群成员；`SendGroupCreate` 通知发出一次。
- [ ] 自动建群不消耗创建者当天的 `SameDayCreateMaxCount`：把该配额设为 0 仍能建出全员群。
- [ ] 建项目走 `project_id` 的四道门（开关、Space、项目活跃）在 provisioner 内**再次**成立：
      开关关闭时建项目本身已被拒，不会产生无群的项目。

**成员同步（I4）**

- [ ] `members/add` 成功后目标 uid 出现在全员群活跃成员里，携带 `version` / `vercode` /
      `role = 0` / `invite_uid = 操作者`，并完成 IM 订阅；再次添加是空操作，不重复通知。
- [ ] 准入器失败（IM 订阅失败）时：项目席位已提交、`member_epoch` 已 +1、
      `project_all_member_group_admit_failures_total` +1、请求仍返回该 uid `admitted = true`。
- [ ] 曾退出全员群（旧 `group_member` 行 `is_deleted = 1`）的项目成员被重新加入项目时，
      走 restore 分支恢复，不撞唯一索引。
- [ ] 踢出 / 退出项目 / Space 移除 → 该 uid 从全员群移除，**由 P1 既有级联完成**，本期不新增
      移除代码；用一条测试钉住"全员群走的是同一条 detach 路径"。
- [ ] I4 扫描 A（活跃项目 `all_member_group_no = ''` 或指向已解散/非本项目的群）和
      扫描 B（活跃项目成员不在全员群活跃成员里）各自游标分页、按检查行数有界，
      被 `TestReconcileQueriesAreBounded` 覆盖；在故意漂移的库上不报 1267；正常加人/移人期间
      报 0，直接 SQL 造出违规时报 1；`removing = 1` 与待处理级联工单豁免（与 I2 同一套豁免）。

**全员群保护（D7，可独立 PR）**

- [ ] 对全员群：群主 `disband` 被拒、任何成员 `exit` 被拒、`members` 删除被拒、
      `transfer` 被拒，码为 `err.server.group.all_member_group_protected`，
      `details.action ∈ {disband, exit, remove, transfer}`。
- [ ] 对同一项目下用户手动建的项目群、以及任意 Space 直属群，这四个接口的响应与改动前
      **字节一致**（golden 断言），且 Space 直属群路径上不多出任何查询（计数断言，C1 纪律）。
- [ ] 手动向全员群加项目成员：幂等成功；加非项目成员：被 I2 以
      `err.server.group.project_member_required` 拒绝（既有行为，不新增码）。

**owner 与名称同步（D6 / D8，可裁剪）**

- [ ] 项目 owner 转让（角色变更或退出带 `transfer_to`）后，全员群 `creator` = 新 owner，
      原 owner 降为普通群成员；钩子失败不回滚项目侧转让，记日志 + 指标。
- [ ] 项目改名后全员群 `name` 同步（>50 rune 截断），群 `version` 前进，客户端通过
      既有的群信息更新命令收到变化。

**解散与响应契约**

- [ ] 解散项目：`all_member_group_no` 在解散事务内清空；群按 P1 规则回退为 Space 直属、
      成员不动；从此对该群 `exit` / `disband` 恢复正常。
- [ ] 项目 `Resp` 含 `all_member_group_no`；`GroupResp` 与群详情含 `project_id`；
      `POST /v1/auth/verify?include=context` 的响应 golden 断言不变。

**约定**

- [ ] 新错误码全部通过 `httperr.ResponseErrorL`；`make i18n-extract-check`、`make i18n-lint`
      通过；zh-CN 翻译齐全；新文件登记进两个 `*NoLegacyResponseError` 守卫。
- [ ] `go list -deps ./modules/project | grep modules/group` 为 0；
      `pkg/project` 不 import `modules/project`。
- [ ] 不改任何既有测试文件的断言；`git diff --stat` 不触碰 `modules/message/`、
      `pkg/space/channel.go`、既有迁移文件。
- [ ] 锁序守卫 `TestNoExclusiveProjectMemberLockUnderAGroupMemberLock` 仍通过；
      新钩子都在项目事务提交后调用，用一条测试钉住（钩子内断言当前没有打开的项目事务）。

## 建议切分

不是硬性要求，但顺序是有意的：**没有任何一个 PR 会留下"全员群存在但没人维护它"的状态**。

1. **PR-A 创建**：`agent_uids` + 全员群 provisioner + `all_member_group_no` 列 + 幂等补建 +
   响应字段 + I4 扫描 A。合并后：全员群存在，退出方向由 P1 维护，加入方向还没有。
2. **PR-B 加入同步**：准入器 + `members/add` 后同步 + I4 扫描 B。合并后：I4 在没有人为破坏时
   成立。
3. **PR-C 保护与同步**：D7 四个接口的拒绝 + D6 群主同步 + D8 改名同步。合并后：I4 在有人为
   操作时也成立。产品可以决定 PR-C 延后，代价见 D7 末尾。
4. **PR-D 透出**：群详情 / 群列表 `project_id`。与前三者无依赖，可并行。

## Open questions（需要产品拍板）

1. **共同目标**：接受 D1（复用 `description`）？
2. **分身范围**：是否排除 `self_hosted`（D2）？截图文案只说"自己的分身"。
3. **分身校验失败**：整单拒绝（D3）还是跳过不合格的？
4. **全员群建失败**：项目照常创建 + 补建（D4），还是整单失败？
5. **全员群保护**（D7）：本期做、延后、还是不做？不做则"全员群"只是一个初始状态。
6. **owner 转让与改名是否同步群主 / 群名**（D6 / D8）？两者可以只做其一，但 D6 与 D7 要
   一起决定。
7. **解散项目时全员群的去向**：接受 D10（沿用 P1 规则，回退为普通群）？
8. **原型的"客户联合交付"叙事**：P0/P1 两次记录的未决项，本期全员群把"项目就是一个群"的
   心智推到用户面前，外部成员的期待会更早出现；仍需产品在文案上先说清 v1 没有外部成员。
