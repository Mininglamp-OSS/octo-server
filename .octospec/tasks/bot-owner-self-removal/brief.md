---
type: Task
title: "Task: bot-owner-self-removal"
description: 允许普通群成员把自己名下（robot.creator_uid）的 bot 移出群聊，并给成员列表下发 bot_owned_by_me 供前端逐行判权。
tags: ["auth", "acl", "isolation", "wire-contract", "error-response", "thread", "bot-api", "rate-limit", "test"]
timestamp: 2026-08-23T11:10:27Z
# --- octospec extension fields ---
slug: bot-owner-self-removal
upstream: Mininglamp-OSS/octo-web#1511
source: self
---

# Task: bot-owner-self-removal

## Goal

普通群成员（非 Creator/Manager）**无法把自己名下的 bot 移出群聊**。
`memberRemove`（`modules/group/api.go:3060-3063`）对非 Creator/Manager 一律返回
`ErrGroupMemberCannotRemove`；而 bot 归属（`robot.creator_uid`）**只在入群侧校验**
（`checkBotOwnership`，`modules/group/bot_ownership.go:37`；调用点 `api.go:1479`、
`invite.go:50`），移除路径完全不查这个字段。

结果是一条**单向门**：普通成员可以把自己的 bot 拉进群（`memberAdd` 只要求调用方已是
群成员 + 通过 ownership 校验，`api.go:1459-1483`），之后却再也取不出来——除非群主/
管理员出手，或解散整个群。`DELETE /v1/groups/:id/bot_admin/:uid`
（`api.go:4198`）只翻 `bot_admin` 标志位，不动成员关系，不是替代品。

本次给 `memberRemove` 开一条**窄口径自助路径**：当操作者不是 Creator/Manager 时，
不再立即拒绝，而是要求请求目标**全部**是「本群内、`robot.status=1`、
`robot.creator_uid == 操作者`」的 bot；只要有一个目标不满足（人类成员、他人的 bot、
孤儿/禁用 bot），整批维持 `ErrGroupMemberCannotRemove`，不做部分执行。

同时给成员列表响应加 `bot_owned_by_me`，让前端能逐行判断是否渲染移除按钮——当前
`memberDetailResp`（`api.go:4331-4367`）只有 `robot` / `bot_admin`，**没有任何归属
字段**，前端无从判断「这个 bot 是我的」。

## Background

- 上游：`Mininglamp-OSS/octo-web#1511`（Web 端无法移除 bot 成员）。issue 提的
  "Expected Behavior" 就是「至少 bot 的所有者应该能移除自己的 bot」。
- 这是**原始设计刻意留下的空白**，不是回归：`bot_ownership.go:18-21` 的
  "Product rules currently deferred" 明写 ownership 只约束邀请侧，且
  「Whether a bot is auto-kicked when its creator leaves is out of scope」。
- D-2 级联（`bot_cascade.go:27-52`）已覆盖「所有者离群/被踢 → 其 bot 同事务连带移除」，
  但**没有**覆盖「所有者留在群里，只想撤走某个 bot」这一场景。

三个可直接复用的现成件：

1. `DB.QueryBotUIDsOwnedByUIDs(groupNo, ownerUIDs)`（`db.go:901-914`）——
   `INNER JOIN robot r ON r.robot_id = gm.uid AND r.status = 1`
   `WHERE gm.group_no=? AND gm.robot=1 AND gm.is_deleted=0 AND r.creator_uid IN ?`。
   **默认拒绝的白名单语义**，正是本次需要的判据。
2. `RemoveGroupMembersServiceReq.SuppressRemoveNotice`（`service.go:1054`）——
   其 doc 描述的正是本场景形状：「成员自愿离开却要走同一套移除流程」。
3. `sendBotCascadeRemovedTip`（`bot_cascade.go:102-140`）的消息结构（`common.Tip`、
   `NoPersist: 0`），可照搬去写一条新文案。

### 安全要点（本任务最容易写错的一处）

**不得复用 `checkBotOwnership` 做移除侧判权。** 它的 SQL 是
`WHERE u.robot = 1`（`bot_ownership.go:69`），人类 UID 根本查不出行，循环因此不会
拒绝——doc 第 32 行明写 `user.robot=0 (human) → always OK`。把它照搬到移除路径上，
等于让普通成员批量传人类 UID 就能踢人，是**提权漏洞**。判据必须用上面第 1 条的
白名单查询。

另注意 `user.robot` 与 `group_member.robot` 是**两个不同的 flag**
（`checkBotOwnership` 用前者，`botAdminSet` 用后者，`api.go:4177`）；两者一旦漂移，
混用即成绕过点。本次统一走 `group_member.robot`（白名单查询已内建）。

## Load-bearing list

1. **`memberRemove` 的权限模型**（`api.go:3049-3096`）——群成员移除的唯一鉴权闸门，
   同时挂在 `DELETE /:group_no/members`（`api.go:101`）与
   `POST /:group_no/members_delete`（`api.go:104`）两条路由上，改动自动覆盖两者。
   注意 3073-3096 段对自助路径同样生效（白送「目标不在群里 → 404」），而其中的
   管理员守卫只在 `loginMember.Role == Manager` 时触发，普通成员天然跳过。
2. **`Service.RemoveGroupMembers` 的全部副作用**（`service.go:1732-1974`）：
   事务内按 UID 排序取 `FOR UPDATE` 行锁（1809-1832）、bot 级联（1860）、
   外部群标识重置（1885-1906）、IM 退订（1912-1918）、被踢系统消息（1921-1931）、
   `CMDGroupMemberUpdate` 刷新（1934-1941）、bot 级联 Tip（1949-1959）、
   子区/置顶/会话扩展清理（1962-1967）。
   ⚠️ `service.go:1792-1806` 记录了与 `handOverGroupCreator` 之间**尚未解决的 ABBA
   死锁**；本次不修，但会给这条路径增加并发调用方，放大暴露面。
3. **`memberDetailResp` 的 wire contract**（`api.go:4331-4367`）——三个 handler 共用
   （`membersGet` 409、`memberGet` 463、`syncMembers` 785）。注释 4357-4360 明确
   Android/iOS 走 WKSDK `ChannelMember.extraMap` 缓存路径，
   `/members` 与 `/membersync` **必须同名同型**。
4. **bot 归属判定口径**：`robot.status=1` 的 fail-closed 语义
   （`bot_ownership.go:61-65`、`db.go:874-876`）——孤儿/禁用 bot 不视为任何人的 bot。
   另：`QueryBotUIDsOwnedByUIDs` **故意不过滤 `group_member.status`**（`db.go:900`
   注释），黑名单态的 bot 也会命中；对移除场景无害，但它不等价于「活跃成员」。
5. **`/v1/groups` 路由组当前无任何限流**（`api.go:98`，仅 `AuthMiddleware`）——
   对比 `api.go:139` welcome 组、`api.go:152` invite 组均挂了 `SharedUIDRateLimiter`。
   本次把一个写操作开放给普通成员，等于扩大未限流的自助写入面。
6. **群系统消息的「透明度」产品约定**（`service.go:1060-1064`、`1943-1948`）：
   「群成员看见 bot 凭空消失，有权知道原因，这与『谁移出了谁』是两件事」。
   本任务据此**不做静默移除**。
7. **`modules/bot_api/groups.go:769`** 是 `RemoveGroupMembers` 的另一个调用方
   （bot 凭 `bot_admin` 移除他人），它自带角色守卫（`groups.go:751-767`）。本次不改它，
   但两者共用同一 service，回归面需覆盖。

## Out of scope

- **不改** `botAdminSet` / `botAdminRemove`（`api.go:4141` / `4198`）的语义。
- **不改** 入群侧 `checkBotOwnership` 的判定口径与其调用点。
- **不改** D-2 级联（`bot_cascade.go`）与拉黑级联的既有行为。
- **不改** `modules/bot_api` 的 `POST /v1/bot/groups/:group_no/members/remove`
  （`bot_api/groups.go:676`），包括它「不能移除自己」的现状。
- **不做** BotManage 控制台里「所在群列表 → 移出该群」的第二入口（数据链路已具备：
  `GET /v1/robot/:robot_id/groups` + `assertRobotOwner`，留作 v2）。
- **不新增 errcode**：拒绝路径复用 `ErrGroupMemberCannotRemove`
  （`pkg/errcode/group.go:91-95`），符合反枚举约定（授权失败收敛到一个码）。
  预期 `make i18n-extract-check` / `make i18n-lint` 无新增产出。
- **不修** `service.go:1792-1806` 记录的 ABBA 死锁。
- **不加**群级能力位（如 `can_remove_own_bots`）：与 `can_manage_bot_admin`
  （`service.go:342` 白嫖已查到的 role）不同，归属判断需要额外一次 robot 表 JOIN，
  而该函数在每次群信息拉取时都走（结果经 `1module.go:252` 灌进 `extraMap`），
  是热路径。改由前端按行级 `bot_owned_by_me` 判断。
- **不改** 前端 `showRemove()`（`Subscribers/vm.ts:98-108`）的群主/管理员入口逻辑。

## Acceptance

### 后端（新增用例落在 `modules/group/api_bot_ownership_test.go`）

1. 普通成员移除**自己名下**的 bot → 200，`group_member` 对应行 `is_deleted=1`。
2. 普通成员移除**他人名下**的 bot → `ErrGroupMemberCannotRemove`，成员关系不变。
3. 普通成员移除**人类成员** → `ErrGroupMemberCannotRemove`。
   （**核心回归防线**：防止误用 `checkBotOwnership` 式「对非 bot 放行」的判据。）
4. 普通成员提交**混合批次**（自己的 bot + 人类 / + 他人的 bot）→ 整批拒绝，
   断言无任何目标被移除（不得部分执行）。
5. 普通成员移除**孤儿/禁用 bot**（`robot` 行缺失或 `status != 1`）→ 拒绝（fail-closed）。
6. 群主 / 管理员 / 后台管理（`c.CheckLoginRole() == nil`）三条既有路径行为不变：
   `api_i18n_regression_test.go`、`bot_cascade_test.go`、`validation_test.go` 全绿。
7. 自助路径**不产生**「你被 X 移除群聊」系统消息，**产生**一条说明 bot 去向的 Tip。
   机制已具备，同包直接复用：`newGroupIMStub(t, ctx)`
   （`space_member_removal_test.go:39`）装桩，`sentPayloads()`（`:80`）取回，
   `payloadsContain(payloads, fragment)`（`:361`）匹配文案片段。
   断言两侧：`payloadsContain(..., "移除群聊") == false`（负向）
   且新 Tip 文案片段存在（正向）。
8. 两条路由（`DELETE /members`、`POST /members_delete`）行为一致。
9. 移除后该 bot 在本群所有子区的成员身份被清理（复用既有 thread cleanup 断言口径，
   `thread_cleanup.go:50-89`）。
10. `bot_owned_by_me` 在 `membersGet` / `memberGet` / `syncMembers` 三处**同名同型**
    下发；非 bot 成员恒为 `false`；他人的 bot 恒为 `false`。
11. **跨群作用域**：我在 A 群拥有的 bot，不能通过对 B 群发起请求而被移除
    （白名单查询按 `groupNo` 作用域，天然安全；此用例是钉子，防止后续重构把
    `groupNo` 从判据里漏掉）。对应 space-isolation 规则（P92）。
12. **不得静默成功**：自助路径的每个用例都断言 `RemoveGroupMembersServiceResp.Removed`
    与预期实际移除数一致。`RemoveGroupMembers` 会静默跳过 Creator 角色
    （`service.go:1764-1770`），若某 bot 恰为群 Creator，当前会返回 200 但零移除，
    前端将误报成功——本条把该行为钉住（拒绝 or 明确非静默，见决策点 4）。
13. **重复移除幂等**：对已不在群内的 bot 再次发起自助移除 →
    `ErrGroupMemberNotInGroup`（`api.go:3080-3083` 既有分支），不得 5xx。
14. `api_realname_test.go` 两个契约守卫（`:44` struct 契约、`:74` handler 调用契约）
    仍绿。（已核实：该守卫只断言三条 realname 正则**能匹配**，新增字段不会使其失败。）

### 前端（octo-web，同名分支）

15. `canRemoveChannelSettingSubscriber`（`channelSettingMemberSection.tsx:19-33`）
    的表驱动用例（`channelSettingSections.test.ts:211-258`）扩展：自己的 bot →
    可移除；他人的 bot / 人类 → 沿用原角色判定。
16. 两条进入 `SubscriberList` 的路径都携带 `removeAction`
    （现状：`Subscribers/index.tsx:159-173` 的「查看全部」未传）。
17. **字段缺失时 fail-closed**：`bot_owned_by_me` 缺失/非 `true` 一律按**不可移除**
    处理。`membersync` 是按 version 的增量同步，新字段上线前已缓存的成员行在其
    version 变动前不会带该字段；降级方向必须是「退回现状」，绝不能误开权限。
    对应用例：`orgData` 无该键 → `canRemove` 返回 false。

### 命令

```bash
go test ./modules/group/...
golangci-lint run ./modules/group/...
make i18n-extract-check && make i18n-lint   # 预期无变化（不新增 errcode）
```

## 待人类确认的决策点

1. **bot 已被授予 `bot_admin` 时，所有者能否单方面撤走？**
   现有守卫**不拦**——`api.go:3084-3095` 只看 `role`，而 `bot_admin` 是独立列
   （`MemberDetailModel.BotAdmin`，`db.go:770`）。默认行为是放行。
   倾向：**允许**（所有权优先，且移除成员行本身即让 `bot_admin` 失效）。
   若需拦，在自助分支加一条 `bot_admin == 1 → 拒绝`。
2. **是否给这两条路由挂 `SharedUIDRateLimiter`？**
   倾向：**挂**，但只挂这两条，不要挂整个 `groups` 组（会一次性影响 ~28 个端点，
   且相关测试须在 setup 里清 `ratelimit:uid:*`，见 `api_welcome_test.go:35`）。
3. **Tip 文案。** 现有 `sendBotCascadeRemovedTip` 模板是
   「{leaver}{action}群聊，其机器人 X 已一并移除」，描述的是「人走 bot 跟着走」，
   不适用于本场景。需新写一条（如「{owner} 移出了机器人 {bot}」）。
   该文案为**硬编码中文**（`bot_cascade.go:126`），不走 i18n 体系。
   验收 #7 会按文案片段匹配，定稿后需同步该片段。
4. **bot 恰为群 Creator 时的行为**（验收 #12）。`RemoveGroupMembers` 对 Creator
   角色是**静默跳过**（`service.go:1764-1770`），handler 不检查 `Removed` 计数即
   返回 200，前端会误报「已移除」。
   倾向：自助分支在调用 service 前显式拒绝 Creator 角色目标，返回
   `ErrGroupMemberCannotRemove`，避免「200 但没动」。
   （现实中 bot 极少成为群主，但这是廉价的一致性钉子。）
