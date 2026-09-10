---
type: Journal
title: "Journal: bot-owner-self-removal"
description: 普通群成员现在可以把自己名下的 bot 移出群聊。移除侧的判据必须用默认拒绝的白名单（QueryBotUIDsOwnedByUIDs），复用入群侧的 checkBotOwnership 会是提权漏洞——它对非 bot UID 返回 nil。三条只有 code review 才抓到的问题：授权谓词漏了活跃口径让被拉黑成员拿到写操作、成员查询漏选 robot 列让新字段静默恒 false、以及功能在 19 人以下的群完全没有入口。
tags: ["auth", "acl", "isolation", "wire-contract", "error-response", "thread", "bot-api", "rate-limit", "testing"]
timestamp: 2026-08-23T13:13:43Z
# --- octospec extension fields ---
task: bot-owner-self-removal
upstream: Mininglamp-OSS/octo-web#1511
source: self
---

# Journal: bot-owner-self-removal

## 做了什么

`memberRemove`（`modules/group/api.go`）原先对每一个非 Creator/Manager 的调用方
一律回 `ErrGroupMemberCannotRemove`，而 bot 归属（`robot.creator_uid`）**只在入群侧**
校验（`checkBotOwnership`，调用点 `api.go` memberAdd 与 `invite.go`）。于是形成一道
单向门：普通成员能把自己的 bot 拉进群（memberAdd 只要求已是群成员 + 通过 ownership），
之后再也取不出来——除非群主出手或解散整个群。`DELETE .../bot_admin/:uid` 只翻标志位，
不是替代品。

现在多了一条窄口径自助路径：调用方非 Creator/Manager 时，要求目标**全部**是
「本群内、`robot.status=1`、`robot.creator_uid == 调用方`」的 bot，否则整批拒绝，
不做部分执行。配套：

- 成员列表新增 per-viewer 的 `bot_owned_by_me`，前端据此逐行决定是否渲染移除按钮；
- 抑制「你被 X 移除群聊」，改发 owner 视角的 Tip（`sendBotOwnerRemovedTip`）；
- 两条移除路由挂上 `SharedUIDRateLimiter`——它们现在对普通成员开放了。

前端（octo-web）：`canRemoveChannelSettingSubscriber` 增加自助分支、`showRemove()`
认「我在本群有 bot」、bot 专用确认文案，并把 `removeAction` 透传给「查看全部」路径。

## 结构性教训

### 1. 入群侧的 ownership 判据搬到移除侧就是提权

`checkBotOwnership` 的 SQL 是 `WHERE u.robot = 1`，人类 UID 查不出行、循环因此不
拒绝——它的 doc 明写 `user.robot=0 (human) → always OK`。这在**邀请**语境下是对的
（「非 bot 不归我管，交给别的守卫」），搬到**移除**语境就变成「普通成员传一批人类
UID 即可踢人」。

移除侧要的是**默认拒绝的白名单**：`QueryBotUIDsOwnedByUIDs` 只返回符合全部条件的
bot UID，任何不在集合里的目标一律拒绝。两者名字相近、语义相反，是极易踩的一脚。
已由 `TestBotOwnerSelfRemoval_RejectsHumanTarget` 钉死。

### 2. 新增授权谓词必须用活跃口径，不能只看 is_deleted

自助分支最初复用了闸门上方的 `QueryMemberWithUID`（只过滤 `is_deleted`）。而
`QueryBotUIDsOwnedByUIDs` **故意**不过滤 `group_member.status`（拉黑级联要靠它恢复
黑名单态的 bot）。两者叠加的结果：被拉黑的成员（`status=Blacklist`、`is_deleted=0`）
凭空获得一个能改群成员表、并往拉黑他的群里写一条持久化 Tip 的写操作——而在本改动
之前他会被直接拒绝。

`db.go` 的 `QueryActiveMemberGroupNosWithUID` 早就写了这条约定：「用作授权谓词的
调用方必须用本方法……后者只看 is_deleted，会把被拉黑成员当作仍然在群」。教训是
这条约定同样适用于**新开的**判据，不只是替换既有调用点。已改用 `ExistMemberActive`。

### 3. 「加字段 + 回填」的模式会因漏选列而静默失效

`bot_owned_by_me` 由 `fillBotOwnedByMe` 回填，回填前有个 `resps[i].Robot == 1` 的
前置判据（避免无 bot 的群白打一次查询）。而 `queryMemberWithGroupNoAndUID`
（memberGet 用）的选择列里**没有** `group_member.robot`——于是 `Robot` 恒 0、
前置判据直接短路，该端点上新字段恒为 false。**不报错、不告警**，只是永远返回 false。

这类「派生字段依赖另一个也得由 SQL 选出来的字段」的耦合，只能靠端点级测试兜住。
更结构性的解法是在原查询里直接 `LEFT JOIN robot ... AND r.creator_uid=?` 推导，
连前置判据都不需要——本次没做（要给 `SyncMembers` 等加 loginUID 参数，改动面更大），
记在这里备查。

### 4. 「把入口透传下去」不等于「入口存在」

前端最初只做了「让『查看全部』这条路径带上 `removeAction`」，但没验证这条路径本身
在小群里是否渲染：它的条件是 `subscribers.length > shouldShowMemberNum()`，
普通成员算下来是 19。**19 人以下的群完全没有入口**——后端放行、`bot_owned_by_me`
也为 true，用户却点不到任何东西，issue 等于没修。

教训：给一个既有入口加能力时，要先确认该入口对**目标用户角色**可见。这里目标角色
恰恰是此前从未见过这个入口的普通成员。

## 踩过的坑

### 测试脚手架：模块实例是进程级单例

`register.GetModules`（octo-lib `pkg/register/register.go`）用 `once.Do` 构造模块
实例。所以一个测试二进制里，**所有 handler 永远持有第一个 `NewTestServer` 的 ctx**；
之后每次 `NewTestServer` 只是把同一批 handler 重新挂到新路由上。

后果：`newGroupIMStub(t, ctx)` 改的是「传给它的那个 ctx」的 `WuKongIM.APIURL`，
除进程内**第一个**测试外，任何测试装的桩都拦不到经 HTTP 路由发出的消息——消息会
静默发到先前解析出的地址，不报错、不被捕获。表现为「单独跑绿、一起跑红」。

既有的 `space_member_removal_test.go` 之所以一直好好的，是因为它们**直接调 service**
（`cascadeSetup` 返回本地 `New(ctx)`），用的就是自己那个 ctx。

结论：**在 group 模块里对系统消息做断言，要走 service 层，不要走 HTTP 路由。**
HTTP 层的断言限于状态码与 DB 状态（这两者共用同一个 MySQL，不受影响）。

副作用是「handler 是否正确置位 `BotOwnerSelfRemoval`」这一段目前没有直接断言，
只能由行为反证（普通成员能删自己的 bot、删不动人类和他人的 bot，只有走自助分支才
可能）。要补需要引入 spy service + 自建路由，记在 pending learnings。

### 给既有路由加限流会波及既有测试

两条移除路由挂 `SharedUIDRateLimiter` 后，既有的
`TestManagerMemberRemove_NotInGroupIsNotFound` 就成了「打 UID 限流路由却不重置桶」
的用例。桶是进程级共享、存活在 Redis、且不被 `CleanAllTables` 清理，当前顺序下侥幸
通过，`-shuffle` 下会拿到 429。给路由加限流时要顺带扫一遍打这条路由的既有测试。

## 已知取舍

- **Tip 硬编码中文**（`bot_cascade.go`）。与既有 `sendBotCascadeRemovedTip` 一致，
  brief 明确定了不走 i18n。但本次给确认框做了 i18n，于是 en-US 用户会看到英文弹窗 +
  永久留在群历史里的中文系统消息。建议单独立项把所有系统 Tip 一起 i18n 化。
- **热路径多一次查询**：成员列表每页多一次 `group_member INNER JOIN robot`
  （仅当该页含 bot）。见上面「结构性教训 3」的替代方案。
- **bot 被授予 bot_admin 时所有者仍可撤走**（维护者拍板：所有权优先）。移除成员行
  本身即让 `bot_admin` 失效。
