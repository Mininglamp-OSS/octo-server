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

**确认记录（2026-09-07）**：D1–D16 全部由需求方确认，取本文档给出的默认值。D6 / D7 / D8
的"可裁剪"标记随之取消——三者都在本期范围内。仅剩三项无法在本仓库内核实的事项，列在末尾的
「上线前须核对」，它们不阻塞实现。

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
- **分身跟人走**。群侧早有规则「bot 永远跟随其主人」；项目侧要有同一条规则（D13），否则人一离开项目，I4 就被群侧的既有行为打破。
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

**群侧已有"bot 跟随主人"的规则，而项目侧没有。** `RemoveGroupMembers` 在移除一个人时，
会把**这个人名下的 bot**（`robot.creator_uid = 离开者 AND robot.status = 1`，不是按
`invite_uid`）一并移出群（`modules/group/db.go:983 QueryBotsInvitedByUIDTx`，#354：
「bot 永远跟随其主人，无角色例外」），只对 `role = creator` 的离开者不做。P1 的 detach
复用 `RemoveGroupMembers`，所以 owner 之外的任何人离开项目，他的分身会被群侧从全员群带走，
而分身的**项目席位**还在——这就是 D13 存在的原因。

**bot 被删除时不走 Space 移除工单。** `modules/botfather/command.go:679` 用一条裸
`UPDATE space_member SET status=0` 把 bot 从所有 Space 清掉，不写
`space_member_removal_cleanup`，于是 P0 的"Space 席位关闭 → 项目席位关闭"级联不会跑。
今天这是潜在缺陷（没有路径把 bot 加进项目）；本期把分身变成项目成员后它就是现实缺陷：
分身被删，项目席位永远活跃，I1 与 I4 同时违规。见 D14。

**加入方向没有任何同步。** `addOneMemberOnce`（`service.go:771`）提交后只做缓存失效，
不通知群侧。`members/add` 对目标 uid **是不是 bot、bot 归谁**没有任何规则：任何管理员今天
就能把任何有 Space 席位的 bot 加进项目，包括非成员名下的。见 D15。

**没有"全员群"这个概念。** `group` 表没有列能区分"项目的全员群"和"项目里用户自己建的群"；
`GroupResp`（`modules/group/service.go:828`）不透出 `project_id`，这是 P1 明确留给 P2 的
第一项（sidebar / 群详情 / 群列表透出）。项目成员名册 `MemberResp` 只有
`uid / name / role / invite_uid / created_at`，**没有 `robot` 字段**，客户端分不清人和分身。

**建群服务层的两个硬约束。** `Service.CreateGroup`（`modules/group/service.go:1103`）要求
`Members` 非空（handler 的 `groupReq.Check()` 同样要求），创建者由服务层自动加入；只有创建者
一个人的群今天建不出来。每日建群配额 `Group.SameDayCreateMaxCount` 只在 handler 里检查
（`api.go:1101`），服务层调用天然绕过；handler 里针对 `project_id` 的四道门同样**只在
handler**，服务层只做 Space 成员校验和准入闸门。IM 频道在事务提交后创建，失败做补偿删除；
随后发 `SendGroupCreate` 群创建通知。

**AI 分身在代码里是什么。** 通讯录改版（`GET /v1/space/directory`，#842）的口径：
`robot.creator_uid = 本人`、`robot.status = 1`、`robot.agent_hosting <> 'self_hosted'`、
bot 在本 Space 有活跃 `space_member` 行、不在 `pkg/space.SystemBots` 内。botfather 建 bot 时
会写 `space_member`（`modules/botfather/db.go:194`），所以分身能通过 I1，成为项目成员没有
结构性障碍；进项目群时它是普通 bot，**不豁免** I2，需要项目席位——这正好是本期要给它的。
`agent_hosting` 是客户端自报值，只能当展示过滤，不能当授权信号（`api_directory.go:45`）。
`robot` 表和 `agent_hosting` 列分别由 `modules/robot/sql` 与 `modules/botfather/sql` 创建；
生产二进制包含全部模块，`modules/project` 的测试二进制则是靠 `project_external_test.go`
引入 `internal` 才看得到这两个迁移目录，`pkg/project` 的测试二进制看不到。

### 调用方向的硬约束，决定了实现形状

`modules/project` **禁止** import `modules/group`（`pkg/project/import_guard_test.go`，
`go list -deps` 两个方向都是 0，P1 把这条写进了验收）。项目侧要让群侧做事，只能用反向注册：

- 已有先例一：`RegisterProjectMemberRemovalStep` / `RegisterProjectDisbandStep`
  （`modules/project/cascade_registry.go`），群侧在 `1module.go` 构造时挂进来。
- 已有先例二：Space 的预设群自动入群 `RegisterPresetGroupAdmitter`
  （`modules/space/preset_group_admitter.go`），群侧实现 `admitToPresetGroup`
  （`modules/group/preset_group_admission.go`）：**自带事务、自取版本号、走准入口、提交后
  IM 订阅、失败返回错误由调用方记日志**。这就是"加入项目 → 进全员群"要抄的模板。

本期新增**四个**注册点，都在 `modules/project` 定义、由 `modules/group` 实现：

1. **全员群创建器**：`RegisterAllMemberGroupProvisioner(fn)`，输入项目 id / Space / 创建者 /
   初始成员，输出 `group_no`。群侧用 `Service.CreateGroup` 实现，走标准建群（IM 频道、群创建
   通知、准入闸门 A3/A4），不重新实现建群。
2. **全员群准入器**：`RegisterAllMemberGroupAdmitter(fn)`，输入 Space / group_no / uid，
   群侧照 `admitToPresetGroup` 实现。
3. **群主同步器**：`RegisterAllMemberGroupOwnerTransfer(fn)`（D6）。
4. **群名同步器**：`RegisterAllMemberGroupRename(fn)`（D8）。

四个钩子都在项目事务**提交之后**调用，原因和 `runDisbandSteps` 的注释一样：钩子在另一个模块
的表上做事务，锁在项目行上跨模块等待会把建项目串行化到该项目的每一次群写入上。**因此项目侧
不能靠项目行锁给钩子做互斥**，D4 的补建用租约认领。

### 原型里没说、但"全员"二字要求的持续义务

弹窗只覆盖"创建"这一瞬间。后加入的人要进群、群主不能把群解散掉、人走了分身怎么办——弹窗上
没有字，但不做的话产品承诺只成立一秒钟。这些义务在下面全部写成已定决策。

## Decisions

每条记录决定、理由，以及被否决的替代方案。全部于 2026-09-07 由需求方确认。

**D1 — "共同目标"复用 `octo_project.description`，不加新列。** 500 字上限已够；前端文案叫
"共同目标"只是展示层的事。被否决：新增 `goal` 列——两个语义相近的文本字段会让后续每个表单都
要回答"填哪个"。

**D2 — 分身的资格口径与通讯录一致，服务端复核，不信任前端。** 建项目请求新增可选
`agent_uids []string`。服务端逐个校验：`robot.creator_uid = 调用方`、`robot.status = 1`、
`user.robot = 1`、在本 Space 有活跃 `space_member` 行（I1，事务内锁定，与人类成员同一条
路径 `requireSpaceSeatsTx`）、不在系统 bot 白名单、`agent_hosting <> 'self_hosted'`。
**排除本地分身**与通讯录选择器同口径——否则用户在选择器里看不到的分身可以从接口带进来。但
`agent_hosting` 是自报值，排除它是产品一致性，不是安全边界，这句话要写进代码注释。上限沿用
`MemberBatchMax`（默认 200）。资格谓词放在 `modules/project`（它要读 `robot` 表），
不放 `pkg/project`。

**D3 — 任一分身不合格，整单拒绝，不做部分成功。** 建项目是一次性动作，"项目建了但有两个
分身没进来"比"请修正后重试"更难解释，而且部分成功会让 D4 的失败语义再多一种。错误码新增
`err.server.project.agent_not_eligible`，`details.uids` 列出不合格的 uid（调用方自己提交的，
不构成泄露），不区分"不是你的 / 不是 bot / self_hosted / 不在本 Space / 系统 bot / 不存在"，
原因进日志——与 P1 建项目群"三种失败同一个码"的反探测口径一致。

**D4 — 全员群建失败，项目仍算创建成功；响应里 `all_member_group_no` 为空；补建用租约认领。**
项目是主实体，群是它的附属；建群发生在项目事务提交之后、且 IM 频道创建在群事务之外，回滚
项目需要跨模块补偿删除，而 P1 对 disband 步骤失败已经选了"不回滚、对账报出"，这里保持同一个
规则。配套：

- (a) 指标 `project_all_member_group_provision_failures_total`；
- (b) I4 对账扫描 A"活跃项目没有全员群"作为一条独立告警，**只报不修**（与 P0/P1 全部对账
  扫描同一纪律）；
- (c) **幂等补建只发生在写路径上**：建项目本身，以及此后任何一次 `members/add`，发现
  `all_member_group_no = ''` 就尝试补建。互斥不能用项目行锁（钩子在提交后运行，见上），用
  **租约认领**：`octo_project.all_member_group_lease_until DATETIME(3) NULL`，认领是一条
  CAS `UPDATE … WHERE all_member_group_no = '' AND (lease_until IS NULL OR lease_until < now)`，
  认领成功者建群，再用 CAS 写回 `group_no` 并清租约；认领失败者跳过。这与
  `space_member_removal_cleanup` / `octo_project_member_removal_cleanup` 的 lease 是同一个
  形状，过期租约自然可重试。

被否决：回滚项目——见上；同步建在项目事务内——违反锁序，把项目行锁跨到 IM 调用上；
对账驱动补建——对账只报不修是仓库纪律，且补建会写群表，对账 worker 不该持有写路径。

**D5 — 全员群身份记录在项目侧：`octo_project.all_member_group_no VARCHAR(40) NOT NULL
DEFAULT ''`，**不加索引**。**（本段原写"加普通索引"，实现时一度加了两条，最后一条不剩，
两次删除是同一条理由：把每条碰这一列的语句列一遍，没有任何一条会用上它。
第二条 `(all_member_group_no)` 在第四轮 review 删掉——没有语句单独按它过滤。
第一条 `(status, all_member_group_no)` 在第五轮 review 删掉——它的理由是"扫描 A 的谓词是
(status, all_member_group_no)"，而第三轮已经把 `all_member_group_no` 从 WHERE 移进了
violating 标志，此后扫描 A 的谓词只剩 `status` 与 `id > ?`。实测（MySQL 8.0.46，两种排序
规则形态各一次）：扫描 A 走 PRIMARY，扫描 B 根本不从 `octo_project` 驱动，索引都在
possible_keys 里没被选中。代价是一次非 INSTANT 的在线构建，加上每次写入多维护一棵 B+ 树。
结果：整个迁移只剩两条 INSTANT 的 ADD COLUMN，不需要上线窗口。） 一个项目一个值，"有且仅有一个"由列本身保证，不需要在 `group` 表上
做带 NULL 技巧的唯一索引；迁移放 `modules/project/sql`，因为只有 `modules/project` 写它。群侧
要判断"这个群是不是全员群"时（D7），通过 `pkg/project` 新增的谓词
`IsAllMemberGroup(session, projectID, groupNo)` 查项目行（本段原写成带 `spaceID` 的四参数版本；实现时收窄成三参数——`spaceID` 在这个判定里不参与任何谓词，多一个参数只会让调用方以为它被校验过），**只在 `group.project_id != ''`
时才查**，Space 直属群零成本（延续 P1 的 C1 纪律）。谓词必须同时要求
`group.project_id = 该项目`：P1 的 detach 在群主无继任者时会把群回退成 Space 直属而
`all_member_group_no` 还指着它，这时它已经不是全员群，保护和补建都要按"没有全员群"处理。
被否决：在 `group` 表加 `project_role` 列——迁移得放 `modules/space/sql`（P1 踩过的坑），
且"每个项目至多一个"要靠应用层保证。

**D6 — 全员群群主始终是项目 owner。** 建群时群主 = 创建者 = owner，这部分免费。项目 owner
转让（`changeMemberRole` 的 transfer 分支、`leaveProject` 带 `transfer_to`）时**同步转让群主**，
通过注册点 3 实现；群侧要把 `transferGrouper` handler 里的转让逻辑抽成服务层函数供钩子调用，
钩子路径**不经过** D7 的 handler 层保护。理由：P1 只在群主**离开项目**时移交群主；owner 转让后
原 owner 仍在项目里，群主就会停在一个普通项目成员身上，而这个人又不能解散/退出全员群（D7），
形成一个没人能操作的群主。钩子失败时不回滚项目侧转让；原 owner 之后一旦离开项目，P1 的
detach 会按"群主离开"路径把群主移交给资深项目成员，是现成的兜底。

**D7 — 全员群受保护：群主不能解散、任何人不能退群、群内不能踢人、不能拉黑、不能手动转让群主。**
"全员"的含义就是这几件事都由项目侧驱动：退出走项目退出，踢人走项目移除，转让走项目 owner
转让。受影响的接口：`POST /:group_no/exit`、`DELETE /:group_no/disband`、
`DELETE|POST /:group_no/members(_delete)`、`POST /:group_no/transfer/:to_uid`、
`POST /:group_no/blacklist/add`，仅当该群是全员群时拒绝，新错误码 `err.server.group.all_member_group_protected`（一个码，`details.action`
区分动作）。**保护只加在 HTTP handler 层，不加在 `RemoveGroupMembers` 等服务层函数上**：
P1 的 detach、Space 级联、botfather 删 bot、本期的四类钩子都走服务层，加在那里等于把 I2 的
级联一起挡掉。**手动加人不禁止**：加项目成员会被幂等吸收，加非项目成员会被 I2 拒绝，无需
新规则。这是本期最大的一块新增限制，单独一个 PR（PR-C）。

**拉黑是实现时补上的第五条**（本段原文只列了四条）。它不叫"移除"、不走
`RemoveGroupMembers`，只把 `group_member.status` 翻成 Blacklist 并做 IM 退订，但对 I4 的
效果与踢人完全相同：人还是项目成员，却不在全员群的活跃成员集合里。**解除拉黑不挡**——那是
把人放回活跃集合，方向与 I4 一致，挡住反而会让已被拉黑的成员永远出不来。
`bot_api` 的成员移除接口同理补了一份守卫：它直接调服务层原语，Web 侧那道挡不到它。

**`IsAllMemberGroup` 的实际签名是 `(session, projectID, groupNo)`**，比本段上文写的
`(session, spaceID, projectID, groupNo)` 窄一个参数——群行本身已经限定了 Space，多传一个
只会给出两个可能互相矛盾的 Space 来源。

**D8 — 项目改名同步全员群群名；项目 logo 不同步群头像。** 群名 = 项目名是用户识别全员群的
心智，改了不同步等于让全员群"失联"；群名上限 50 字（`MaxGroupNameLen`）短于项目名 64 字，
截断规则与 `CreateGroup` 一致（取前 50 rune）。头像不同步：群头像有自己的一套自定义规则
（`avatar_text` / `avatar_color`），项目 logo 是一个 URL，两者不是一回事。通过注册点 4 实现，
best-effort，钩子路径绕过群侧改名的角色校验（项目侧已经按 `canUpdateProject` 校验过）。

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
（席位是授权事实，群是它的投影；反过来会让"加人"因为 IM 抖动而失败）。I4 对账扫描 B 负责
发现漏网的人；**不做自动修复**。批量 200 人 = 200 次准入 + 200 次 IM 订阅，和今天
`AddGroupMembers` 一次加 200 人的代价同量级。

**D13 — 分身跟人走：一个人的项目席位关闭时，他名下在该项目内的分身席位在同一事务内一并
关闭，各自入队级联。** 覆盖踢出、退出、Space 级联三条路径，owner 转让不算离开。理由是
Background 里那条代码事实：群侧已经按 `robot.creator_uid` 把离开者的 bot 带出群，项目侧
不跟上，I4 在第一个成员离开时就被打破，而且没有任何路径修复（分身的席位活着，群侧不会再把
它加回来）。"分身"的产品含义也是如此——它以主人的身份运作（`modules/bot_api/obo_fanout.go`），
主人不在了它不该继续读项目群。判定用 `robot.creator_uid`，与群侧同源；分身席位关闭的
`operator_uid` 记为触发者，`reason` 沿用触发者的。被否决：分身独立留在项目里——等于让一个
已离开的人的代理留在项目群里，且立刻违反 I4。

**D14 — botfather 删除 bot 必须走 Space 成员移除的事务性工单，而不是裸 UPDATE。**
`modules/space` 导出一个"关闭 uid 在所有 Space 的席位并入队清理"的入口（内部就是
`enqueueMemberRemovalCleanupTx`，理由 `MemberRemoveReasonForceRemoved` 或新增一个
`bot_deleted`），botfather 改调它。这样 P0 的级联关闭项目席位、P1 的级联从项目群移除，
一条链全部复用。这是本任务的**前置修复**（PR-0），改动在 `modules/botfather` 与
`modules/space`，很小；不做的话 I1 对账会在第一个被删的分身上永久报警。

**D15 — 分身进项目的资格规则，以及谁能操作。** 今天 `members/add` 对 bot 零规则，改为：

- (a) bot 只能由**它的主人**加进项目，且主人必须是该项目的活跃成员；管理员不能替别人带分身，
  也不能带非成员的分身——这是弹窗"仅可带入自己的分身"在创建之后的延伸。
- (b) 因此新增一个**普通成员也有**的窄能力 `can_manage_own_agents`：任何活跃项目成员可以把
  自己的分身加进项目、把自己的分身移出项目，不需要 `can_manage_member`；管理员对分身的移除
  权与对人一致。
- (c) 入口不新增路由：`members/add` / `members/remove` 对 bot 目标改按这套规则判定。

被否决：沿用今天"管理员想加谁加谁"——和创建弹窗的承诺矛盾，也让 D13 的"分身跟人走"没有对应的
"分身跟人来"。

**D16 — 名册透出 `robot` 与 `owner_uid`；`member_count` 只数人。** `MemberResp` 增加
`robot int` 和 `owner_uid string`（bot 才有），客户端才能像通讯录那样把分身挂到人下面；
项目 `Resp.member_count` 改为只数 `user.robot = 0` 的活跃成员，另加 `agent_count`。
配额 `max_members` 仍按全部席位计（分身占席位是有意的：它是一个会读消息的成员）。
**这是既有字段的语义变更**，PR 描述里要单独说明，并按「上线前须核对」第 1 条确认客户端影响面。
被否决：`member_count` 混数——弹窗和列表页显示"3 人"里有两个是分身，用户会问。

**D17 — 产品文案不得承诺外部成员。** P0 / P1 两次记录的未决项：原型的「客户联合交付」叙事与
v1 的两条约束冲突（没有外部成员；Project 不是读边界）。本期把"项目就是一个群"的心智推到用户
面前，这个期待会更早出现。技术上不做任何外部成员支持（见 Out of scope），文案侧由产品保证不
出现相关承诺。记在这里是为了让它有归属，而不是继续挂在"未决"里。

## Load-bearing list

- **项目侧锁序与写路径守卫。** 声明的锁序
  `space_member → space → project → group → group_member → octo_project_member`
  （`modules/project/db.go:354`）：本期所有群侧钩子在项目事务**提交后**运行，项目事务内不得
  触碰 `group` / `group_member`。新增/改动的写路径（带分身的创建、D13 的分身席位关闭、owner
  转让触发的群主同步、解散时清空 `all_member_group_no`、D4 的租约 CAS）必须登记进
  `TestWritePathsRevalidateTheActorSpaceSeatInTx` 和 `TestEveryWritePathEmitsAnAuditEntry`，
  否则两个守卫静默缩水。分身席位的写入按 `project.member_add` 逐条审计，不并入
  `project.create`。touches: `space`, `isolation`, `acl`
- **`robot` 表是本模块的新依赖。** D2 / D13 / D15 都要读 `robot.creator_uid` / `status` /
  `agent_hosting`；表由 `modules/robot/sql` 与 `modules/botfather/sql` 创建。生产二进制齐全；
  `modules/project` 的测试二进制经 `project_external_test.go` 引入 `internal` 才有它，这个
  依赖要在测试文件里写明而不是靠巧合；谓词不放 `pkg/project`。JOIN `robot` 时带显式
  `COLLATE`（`robot` 同属未声明 COLLATE 的老表）。touches: `migration`, `testing`
- **唯一准入口与源码守卫。** 任何"把人放进全员群"的动作必须经过 `admitOrRestoreMembersTx`；
  `TestNoGroupMemberWritesOutsideTheAdmissionFunnel` 和
  `TestNoGroupMemberRowsBuiltOutsideModulesGroup` 会拦住任何绕路。准入器需要一个新的
  entry 标签（`AdmissionEntryAllMemberGroup`），并加入"每个标签至少被测试发出一次"的守卫。
  touches: `isolation`, `acl`
- **群侧 bot 连带移除规则（#354）。** `RemoveGroupMembers` 按 `robot.creator_uid` 带走离开者
  的 bot，P1 detach 复用它。D13 让项目侧与之同源；判定字段必须一致，否则两边对"谁的分身"
  会有不同答案。touches: `acl`, `isolation`
- **I1 与 Space 移除工单。** D14 把 botfather 的 bot 删除接到
  `space_member_removal_cleanup` 上；P0 的 `cleanupSpaceMemberProjects` 步骤随之对 bot 生效。
  `modules/space` 要新导出一个入口；步骤契约（幂等、自决、失败重跑整单）不变。
  touches: `space`, `isolation`
- **I3 不变量。** `group.project_id` 只在建群路径写入、只在 detach 步骤清空
  （`TestNoProjectIDRewritesOutsideTheDetachStep`）。全员群通过 `CreateGroup` 带 `project_id`
  建出，不新增 `project_id` 写点。
- **`CreateGroup` 服务层契约。** `Members` 非空的检查要放宽为"允许只有创建者"，这是对一个
  被 `bot_api`、`integration` 两处调用的公共服务的行为变更；handler 层 `groupReq.Check()`
  **保持不变**（用户手动建群仍要求至少一个成员）。IM 频道创建失败的补偿删除、`SendGroupCreate`
  通知、`bot_admin` 设置逻辑照旧。provisioner 直接调服务层，handler 的四道门**不会重跑**；
  这是可以接受的，因为开关和 Space 成员已由建项目路径校验，I2 由准入闸门在事务内校验。
  touches: `wire-contract`
- **`member_epoch` 规则。** 只允许 +1、每次成员/角色写入同事务内、创建为 0；分身席位在创建事务
  内写入，不 bump；D13 关闭分身席位与关闭本人席位在同一事务，**只 bump 一次**。
- **功能开关单一真源。** `project.enabled`（system_setting → env → false，客户端字段
  `project_on`）已经同时约束建项目和建项目群；本期不新增开关，全员群跟随它。关掉只停止新建，
  不放松已有全员群的约束（与 P1 的回滚口径一致）。touches: `wire-contract`
- **对账纪律。** 新增的 I4 两个扫描（A "活跃项目无全员群或指向非本项目的群"、B "活跃项目
  成员不在全员群"）必须游标分页、按**检查行数**有界，纳入 `TestReconcileQueriesAreBounded` /
  `TestReconcilePageQueriesExamineBoundedRows`；跨进 `group` / `group_member` / `robot` 的
  JOIN 带显式 `COLLATE`（P1 的 `TestP1ScansSurviveCollationDrift` 是范本）；只在完整轮转后
  发布 gauge；只报不修。
- **群侧五个接口的行为变更（D7）。** `exit` / `disband` / `members` 删除 / `blacklist add` / `transfer` 对全员群
  拒绝，对其他群（含普通项目群）响应字节不变；判定只在 `project_id != ''` 时发起**两次**
  单表索引点查（第五轮 review：原来是一次跨 schema 的 JOIN，在生产的排序规则形态下
  实测退化成 `group` 全表扫），Space 直属群零查询；保护只在 handler 层。touches: `error-response`, `i18n`, `acl`
- **群主移交的既有逻辑。** `groupExit` 群主退群时选第二老成员（排除其名下 bot）；P1 的
  `querySuccessorForProjectGroupTx` 把继任者收窄到项目成员。D6 的 owner 同步转让要把
  `transferGrouper` 的转让逻辑抽成服务层函数复用，不新写一套。
- **i18n。** 新错误码进 `pkg/errcode/project.go` / `group.go`，全部走 `httperr.ResponseErrorL`，
  5xx ⟺ `Internal=true`；`make i18n-extract-check` + `make i18n-lint` 通过；zh-CN 翻译进
  `active.zh-CN.toml`；新的 group 文件加进 `TestGroupNoLegacyResponseError` 的固定文件清单，
  新的 project 文件加进 `TestProjectNoLegacyResponseError`。touches: `error-response`, `i18n`
- **限流。** 不新增路由；`agent_uids` 搭乘已挂 `SharedUIDRateLimiter` 的建项目路由；
  钩子和对账不是 HTTP 路径，不得长出 Redis 计数器。touches: `rate-limit`
- **迁移。** `octo_project` 加 `all_member_group_no` 与 `all_member_group_lease_until` 两列，
  不加索引（理由见 D5），放 `modules/project/sql`；两条 `ADD COLUMN` 都显式写
  `ALGORITHM=INSTANT`，让"这是 INSTANT"成为被强制执行的断言而不是期望；
  无 `group` 表变更。迁移文件注释不得出现撇号（P1 迁移文件记录的解析缺陷）。touches: `migration`
- **反探测。** 分身校验失败一个码；`IsAllMemberGroup` 对非项目成员不暴露项目是否存在
  （群侧五个接口里，转让/解散/拉黑在拒绝之前已经要求调用方是群主或管理员；**踢人和退群
  这两条不是**——它们的守卫排在"读调用方的群成员身份"之前，退群那一处还是被
  IMRemoveSubscriber 的位置逼出来的。所以泄露面不是零而是**有界**：一个非成员能读出
  "这个群属于某个项目"，而"这个群存不存在"上游的 getGroupInfo 已经用 404 回答过了。
  实现侧的注释按这个口径写，本行原来的"所以不新增探测面"是错的）。touches: `isolation`
- **响应契约。** 项目 `Resp` 新增 `all_member_group_no`（空串表示尚未建成）、`agent_count`，
  `member_count` 语义改为只数人（D16，**既有字段语义变更**）；`MemberResp` 新增 `robot`、
  `owner_uid`；`Capabilities` 新增 `can_manage_own_agents`；`GroupResp` 及群详情新增
  `project_id`（空串 = Space 直属）。`POST /v1/auth/verify?include=context` 的 `projects[]`
  **不变**。touches: `wire-contract`
- **测试纪律。** 不改既有测试文件的断言；命中 UID 限流路由的测试在 setup 里重置
  `ratelimit:uid:*`。touches: `testing`

## Out of scope

- **sidebar / 会话同步透出 `project_id` 与"项目树"展示。** 只做群详情和群列表的透出；
  `/v1/sidebar/sync` 的形状变更单独开任务。
- **项目级自助加入 / 邀请码 / `join_mode` 消费方**（P0 留下的 P2 项）。本期加入项目的入口
  仍只有 `members/add`。
- **用户手动创建的普通项目群**的行为：不受 D7 保护，不跟项目改名，不由项目加人同步；
  D13 关闭分身席位后，P1 的级联会把分身从这些群里也移掉，这是 I2 的既有行为，不是本期新增。
- **`agent_hosting` 的真实性**：自报值，只做展示口径过滤。
- **IM 订阅 / 退订泄漏**（#797、`im-pending-outbox`）：准入器和级联继承同一个泄漏，本期不解。
- **全员群欢迎语**：走既有 `group-welcome-message` 的群级配置，不做项目级默认。
- **项目 logo 同步群头像**（D8 已说明）。
- **外部成员、跨 Space 项目、项目作为读边界**：与 P0/P1 同样不做（D17 是文案侧的对应约束）。
- **对账自动修复**：I4 只报不修；D4 的补建只在写路径上，不由对账触发。
- **P1 遗留的未决项**（`(space_id, project_id)` 索引构建时长、`queryI2Page` 游标计划、
  org-directory 监听器是否存活、A1/A2/A4-A8 的路径级覆盖）：不在本期顺手解决。
- **bot 删除路径的其他清理**（好友、会话）：D14 只接 Space 席位这一段。

## Acceptance

**建项目带分身**

- [ ] `POST /v1/space/:space_id/projects` 带 `agent_uids` 时，每个分身在**同一事务**内成为
      `role = 0` 的活跃项目成员，`invite_uid = 创建者`，`member_epoch` 仍为 0，每个分身一条
      `project.member_add` 审计。
- [ ] 任一 `agent_uids` 元素不满足 D2（非本人创建 / 非 bot / `self_hosted` / 不在本 Space /
      系统 bot / 不存在），整单以 `err.server.project.agent_not_eligible` 拒绝，
      `details.uids` 列出不合格 uid，项目行**未**写入；六种原因在响应上字节相同，仅日志区分。
- [ ] `agent_uids` 超过 `MemberBatchMax` 以 `err.server.project.batch_too_large` 拒绝。
- [ ] 分身席位计入 `max_members` 配额，超限以 `err.server.project.quota_members` 拒绝整单。
- [ ] 新路径出现在 `TestWritePathsRevalidateTheActorSpaceSeatInTx` 与
      `TestEveryWritePathEmitsAnAuditEntry` 的枚举里。

**全员群创建**

- [ ] 建项目成功后，`octo_project.all_member_group_no` 非空，且该群
      `project_id = 项目 id`、`space_id = 项目 Space`、`creator = 项目创建者`、
      `name = 项目名（≤50 rune）`，活跃成员集合 = {创建者} ∪ 分身集合。
- [ ] 只有创建者、没有分身时也能建出全员群（`Service.CreateGroup` 允许空 `Members`），
      而 `POST /v1/group/create` 空 `members` 仍被拒绝（handler 不变）。
- [ ] 群侧 provisioner 未注册（只含 `modules/project` 的二进制）时，建项目成功、
      `all_member_group_no = ''`、记一条 Error，不 panic，不回退项目。
- [ ] 让 provisioner 故意失败（如 IM 频道创建失败）：项目已创建，响应
      `all_member_group_no = ''`，`project_all_member_group_provision_failures_total` +1，
      租约按过期时间释放；随后一次 `members/add` 触发补建。
- [ ] 并发：两个 `members/add` 同时对一个无群项目触发补建，只产生一个群，另一个认领失败
      跳过（CAS 断言）；租约过期后再次触发能补建。
- [ ] 全员群的 IM 频道订阅者 = 群成员；`SendGroupCreate` 通知发出一次。
- [ ] 自动建群不消耗创建者当天的 `SameDayCreateMaxCount`：把该配额设为 0 仍能建出全员群。
- [ ] 开关关闭时建项目本身被拒（既有行为），因此不会产生"无群的项目"；用一条测试钉住
      provisioner 只在项目提交后被调用（钩子内断言没有打开的项目事务）。

**成员同步（I4）**

- [ ] `members/add` 成功后目标 uid 出现在全员群活跃成员里，携带 `version` / `vercode` /
      `role = 0` / `invite_uid = 操作者`，并完成 IM 订阅；再次添加是空操作，不重复通知。
- [ ] 准入器失败（IM 订阅失败）时：项目席位已提交、`member_epoch` 已 +1、
      `project_all_member_group_admit_failures_total` +1、请求仍返回该 uid `admitted = true`。
- [ ] 曾退出全员群（旧 `group_member` 行 `is_deleted = 1`）的项目成员被重新加入项目时，
      走 restore 分支恢复，不撞唯一索引。
- [ ] 踢出 / 退出项目 / Space 移除 → 该 uid 从全员群移除，**由 P1 既有级联完成**，本期不新增
      移除代码；用一条测试钉住"全员群走的是同一条 detach 路径"。
- [ ] **D13**：一个带了分身的成员被踢出 / 退出 / 被 Space 移除后，其分身的项目席位在同一事务
      内进入 `removing = 1`，各有一条级联工单，`member_epoch` 只 +1；owner 转让不触发。
      级联跑完后，人和分身都不在全员群里，I4 扫描 B 报 0。
- [ ] **D14**：通过 botfather 删除一个已是项目成员的分身，`space_member_removal_cleanup`
      有工单，P0 步骤关闭其项目席位，P1 步骤把它从全员群移除；I1 与 I4 扫描都报 0。
- [ ] **D15**：普通成员可加/移自己的分身（`can_manage_own_agents = true`）；管理员加别人的
      分身、加主人不在项目里的分身，均以 `err.server.project.agent_not_eligible` 拒绝；
      管理员可移除任何分身。
- [ ] I4 扫描 A（活跃项目 `all_member_group_no = ''`、或指向已解散 / `project_id` 不等于本项目
      的群）和扫描 B（活跃项目成员不在全员群活跃成员里）各自游标分页、按检查行数有界，
      被 `TestReconcileQueriesAreBounded` 覆盖；在故意漂移的库上不报 1267；正常建项目 / 加人 /
      移人期间报 0，直接 SQL 造出违规时报 1。扫描 B 的豁免：`octo_project_member.updated_at`
      在可配置的宽限期内的行（准入尚未完成的窗口）、系统 bot、被封禁 Space 的项目。

**全员群保护（D7，PR-C）**

- [ ] 对全员群：群主 `disband` 被拒、任何成员 `exit` 被拒、`members` 删除被拒、
      `transfer` 被拒，码为 `err.server.group.all_member_group_protected`，
      `details.action ∈ {disband, exit, remove, transfer}`。
- [ ] 对同一项目下用户手动建的项目群、以及任意 Space 直属群，这五个接口的响应与改动前
      **字节一致**（golden 断言）；Space 直属群路径上不多出任何查询（计数断言，C1 纪律），
      普通项目群最多多两次单表索引点查（第五轮 review 把一次 JOIN 拆成两条单表读；
      `TestTheD7PredicateReachesItsRowByAnIndexUnderCollationDrift` 在漂移库上对计划做断言）。
- [ ] 服务层不受保护：P1 detach 把一个人从全员群移除、botfather 删 bot 把它从全员群移除，
      都仍然成功——用测试钉住，否则 D7 会挡掉 I2 的级联。
- [ ] 手动向全员群加项目成员：幂等成功；加非项目成员：被 I2 以
      `err.server.group.project_member_required` 拒绝（既有行为，不新增码）。
- [ ] 一个已被 P1 detach 成 Space 直属、但 `all_member_group_no` 仍指向它的群，五个接口
      **不再**被拒（谓词要求 `group.project_id = 项目 id`）。

**owner 与名称同步（D6 / D8）**

- [ ] 项目 owner 转让（角色变更或退出带 `transfer_to`）后，全员群 `creator` = 新 owner，
      原 owner 降为普通群成员；钩子失败不回滚项目侧转让，记日志 + 指标；钩子路径不被 D7 拒绝。
- [ ] 项目改名后全员群 `name` 同步（>50 rune 截断），群 `version` 前进，客户端通过
      既有的群信息更新命令收到变化。

**解散与响应契约**

- [ ] 解散项目：`all_member_group_no` 在解散事务内清空；群按 P1 规则回退为 Space 直属、
      成员不动；从此对该群 `exit` / `disband` 恢复正常。
- [ ] 项目 `Resp` 含 `all_member_group_no`、`agent_count`，`member_count` 只数人；
      `MemberResp` 含 `robot`、`owner_uid`；`Capabilities` 含 `can_manage_own_agents`；
      `GroupResp` 与群详情含 `project_id`；`POST /v1/auth/verify?include=context` 的响应
      golden 断言不变。

**约定**

- [ ] 新错误码全部通过 `httperr.ResponseErrorL`；`make i18n-extract-check`、`make i18n-lint`
      通过；zh-CN 翻译齐全；新文件登记进两个 `*NoLegacyResponseError` 守卫。
- [ ] `go list -deps ./modules/project | grep modules/group` 为 0；
      `pkg/project` 不 import `modules/project`，也不读 `robot` 表。
- [ ] 不改任何既有测试文件的断言；`git diff --stat` 不触碰 `modules/message/`、
      `pkg/space/channel.go`、既有迁移文件。
- [ ] 锁序守卫 `TestNoExclusiveProjectMemberLockUnderAGroupMemberLock` 仍通过。

## 实现切分

顺序是载重的：**没有任何一个 PR 会留下"全员群存在但没人维护它"的状态**，也没有一个 PR 会让
分身成为项目成员而没有 D13 / D14 兜底。

0. **PR-0 前置修复（D14）**：botfather 删 bot 走 Space 移除工单。独立可合，今天就是缺陷。
1. **PR-A 创建**：`agent_uids` + D13 分身跟人走 + D15 资格规则 + D16 名册字段 + 全员群
   provisioner + `all_member_group_no` / 租约列 + 幂等补建 + 响应字段 + I4 扫描 A。
   合并后：全员群存在，退出方向由 P1 + D13 维护，加入方向还没有。
2. **PR-B 加入同步**：准入器 + `members/add` 后同步 + I4 扫描 B。合并后：I4 在没有人为破坏时
   成立。
3. **PR-C 保护与同步**：D7 五个接口的拒绝 + D6 群主同步 + D8 改名同步。合并后：I4 在有人为
   操作时也成立。
4. **PR-D 透出**：群详情 / 群列表 `project_id`。与前三者无依赖，可并行。

## 上线前须核对

无法在本仓库内核实，不阻塞实现，合并前要有答案。

1. **`member_count` 语义变更的客户端影响面（D16）。** 服务端可以改，但哪些端已经在用这个
   字段、是否有把它当"含 bot 的席位数"使用的地方，只能问客户端。若影响面大，退路是保留
   `member_count` 原语义并新增 `human_member_count`，那是一次纯加字段的变更。
2. **前端分身选择器是否已排除 `self_hosted`（D2）。** 服务端按通讯录口径排除；如果选择器
   没排除，用户会看到一个选了就被拒的分身。两侧口径必须一致，以服务端为准。
3. **botfather 删 bot 改走工单后，是否有调用方依赖旧裸 UPDATE 的同步性（D14）。**
   工单是异步的，Space 席位关闭与后续清理之间会出现一个窗口。删除接口本身的响应语义
   （"已删除"）不变，但若有测试或调用方在删除返回后立即断言 `space_member.status = 0`，
   需要同步调整——席位关闭本身仍是同步的，异步的只有级联清理。
