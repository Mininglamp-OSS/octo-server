---
type: Task
title: "Task: profile-visibility-system-bot-whitelist"
description: Narrow the person-profile "system account" fast-path from the writable user.category column to the explicit pkg/space.SystemBots whitelist, so a category=system row that is not a system bot (the admin superuser) no longer bypasses the relationship check.
tags: ["auth", "acl", "isolation", "wire-contract", "test"]
timestamp: 2026-08-12T04:03:02Z
# --- octospec extension fields ---
slug: profile-visibility-system-bot-whitelist
upstream: internal follow-up to channel-get-object-authz (PR #722); reported as "任意用户资料批量泄露" on GET /v1/users/admin
source: self
---

# Task: profile-visibility-system-bot-whitelist

## Goal

`channel-get-object-authz`（PR #722）给 `GET /v1/users/:uid` 与
`GET /v1/channels/:id/:type`(PERSON) 补齐了对象级授权，其中"系统账号恒可见"这条
身份放行用的是**用户表的 `category` 字段**：

```go
SystemAccount: userDetailResp.Category == CategorySystem || userDetailResp.Category == CategoryCustomerService,
```

该字段与"是否公开系统 Bot"并不等价。`pkg/space.SystemBots` 才是仓库里"所有 Space
可见的系统级 Bot"的权威定义（`botfather` / `u_10000` / `fileHelper` /
`notification`），而 `category` 是一个可被建号代码与运维写入的普通列——
`newManagerSeedModel`（`modules/user/api_manager.go:1602`）给**超管 admin 账号**
写的就是 `Category: "system"`，且 `Role: superAdmin`、`robot=0`、不在白名单。

后果：`admin` 是固定可猜 UID，任意登录用户拿它即可命中 `SystemAccount` 短路，
**跳过 Space / 好友 / 共同群三腿关系检查**，拿到完整资料形状而非最小集。

本次把两端的判定输入从 `category` 换成 `spacepkg.IsSystemBot(uid)` 白名单，并把
共享判定结构体的字段 `SystemAccount` 改名为 `SystemBot`，在契约文档里写明「必须用
白名单、禁止用 category 回填」。

## Background

### 实际影响（如实评估，未夸大；**逐端点**分开陈述）

两个端点的暴露面**不同**，早前把它们混为一谈是本 brief 的主要错误来源，故按端点拆开。

**`GET /v1/users/:uid`（admin 实测 200，32 字段）**

- 陌生人视角下超出最小资料集的字段里只有 `category=system` 有值。成因**不是**该行为空
  ——`newManagerSeedModel`（`modules/user/api_manager.go:1602`）写了
  `Name: "超级管理员"` 与 `ShortNo: "30000"`。真正的原因是 `modules/user/api.go:1431-1436`
  在 `Follow != 1 && 非本人` 时把 `ShortNo` 与 `Vercode` 清空。

**`GET /v1/channels/:id/:type`（PERSON，同一预置，本次一并修复的第二个出口）**

- 该端点**没有**上面那道清空。响应经 `newChannelRespWithUserDetailResp`
  （`modules/user/1module.go:189-217`）构造，`extraMap["short_no"] = user.ShortNo`
  无条件写入，`grep short_no modules/channel/api.go` 无任何后置清空。
- 因此修复前，任意登录用户对无关系目标 `GET /v1/channels/admin/1` **确实拿到了**
  `extra.short_no="30000"`，以及 `online` / `last_offline` / `device_flag` / `sex` /
  `source_desc` / `category`。**这是原 brief 漏掉的暴露面。**

**两项经核实**否证**的指控**（review 曾提出，实测不成立，记录在此避免后人沿用）：

- `username="superAdmin"` **未**上 wire：`NewUserDetailResp`（`modules/user/service.go:1554-1556`）
  有门禁 `if m.Robot == 1 { username = m.Username }`，而 admin 是 `robot=0`，故
  `UserDetailResp.Username` 为空；`model.ChannelResp.Username` 又是 `omitempty`。
- `extra.vercode` **未**泄露给陌生人：`service.go:509-524` 仅在
  `friend != nil && friend.IsDeleted == 0` 时才赋 `vercode`，陌生人恒为空；种子 admin
  本身也没有 `Vercode`。故不存在"加好友能力被泄露"这一腿——真要有好友关系，
  `Followed` 腿本来就放行，与本次收紧无关。

**结论（维持"无实质数据泄露"，但理由据上修正）**

- 手机 / 邮箱 / 区号**不在暴露面内**——`NewUserDetailResp` 的 `self := loginUID == m.UID`
  两端一致挡住。
- channel 端点确实多给了超管的 `short_no="30000"` 与在线态，但该短号是**公开仓库里
  写死的默认种子值**（`api_manager.go:1606`），读源码即知，泄露价值近似为零；且不含
  PII、不含能力（vercode）。
- 枚举价值极低：`admin` 可猜，但"存在 admin 超管"本身是公开常识。

因此定位仍是 **P2 口径一致性加固 / 纵深防御**，不触发披露流程；但"陌生人拿不到短号"
这句话**只对 `/v1/users/:uid` 成立**，不能用来描述本 PR 修的第二个端点。

之所以仍要做：

1. `category` 是可写、语义宽泛的展示字段，把它当授权输入违背"公开可见=显式白名单"
   原则。今天 admin 的字段恰好没有敏感值所以无害；将来只要有人建一个 `category=system` 的
   非白名单展示号、或 `customerService` 号被启用并填上资料，这个宽口径就会**无声
   变成**真实泄露面。
2. 白名单让"公开可见"成为必须显式声明的动作，可枚举、可 review。

**一处需要纠正的先前说法**：本人曾在讨论中称"原判定漏掉了 `service` /
`service_notice` 等类别"。核实 `modules/user/const.go` 后确认**该说法不成立**：
category 只有 `system` 与 `customerService`（camelCase）两个取值，原判定覆盖了全部
取值。问题**只是过宽，不存在过窄**。

### 为什么改字段名而不只换判定

`SystemAccount` 这个名字本身就在诱导"用 category 填它"——这正是本 bug 的复发入口。
改名为 `SystemBot` + 在字段注释里写死「必须由调用方用 `spacepkg.IsSystemBot(uid)`
判定」，让下一个改这里的人无法自然地把 category 塞回来。多出的 diff 只是几行，
换来的是防复发。

### 行为不回退的依据

- 白名单内 `robot=0` 的成员（`fileHelper`，见
  `modules/user/sql/20191106000003_user_legacy01.sql:55`）仍由 `SystemBot` 分支放行，
  不依赖 `Robot` 分支——这是本次专门用 `fileHelper` 而非 `u_10000`（`robot=1`）
  写测试的原因：确保命中的是白名单腿。
- 普通 bot（`robot==1`）仍走既有 `Robot` 分支，与"查看资料 ≠ 已可交互"的既定产品
  事实一致。
- `customerService` 账号随之回落到关系检查。这是本次**有意的**收紧：若产品确需客服号
  对所有人公开资料，正道是把该号纳入白名单或设 `robot=1`，而不是再加一条 category
  短路。当前库中未发现启用中的 `customerService` 账号。

## Load-bearing list

- `auth` / `acl` / `isolation` — 单聊资料的对象级可见性判定本身。判定收口在
  `modules/channel/service`（零依赖叶子包）供两端共用，本次两个调用方必须同步改，
  否则一边放宽即静默重开同一越权面。
- `wire-contract` — 对**无关系调用方**而言，非白名单 `category=system` 账号
  （即 admin）的响应从完整形状收窄为最小集。该账号不承载任何客户端业务渲染
  （空号、非会话对端），故判定为无客户端影响；白名单系统 Bot 与普通 bot 的响应形状
  不变。
- `test` — `channel-get-object-authz` 留下的两个用例
  （`TestUserGet_SystemAccount_Viewable` /
  `TestChannelGet_Person_SystemAccount_Viewable`）把**有缺陷的行为**编码进了断言
  （断言 `category=system` 即可见完整资料），修复后必然失败，必须改写为正确契约并
  补反向回归。

## Out of scope

- **不**把 `SystemBots` 改成配置 / DB 驱动。当前 4 个硬编码够用，`pkg/space/query.go`
  注释已留future 口子；现在做属过度设计。
- **不**改动最小资料集的字段策略（`newMinimalUserDetailResp` /
  `NewMinimalChannelResp` 白名单不动），也不改 `real_name` 对外可见性——沿用 #722 的
  既定政策。
- **不**改成 403/404：保留最小集（`uid`/`name`/`follow` + 调用方自身状态）以免客户端
  渲染裂图，与 #722 的分级降级设计一致。
- **不**动其它 `GetUserDetail` 复用点（群邀请人富化 `modules/user/api.go:1384`、
  `modules/user/1module.go:65` 的 datasource）——它们不经过本短路。
- **不**改超管账号的建号逻辑（`newManagerSeedModel` 仍写 `category=system`）：
  category 作为**展示/分类**字段的用途正当，本次只是不再拿它做授权。

## Acceptance

- [x] 两端判定输入改为 `spacepkg.IsSystemBot(uid)`；共享字段更名为 `SystemBot`，
      注释写明禁止用 category 回填。
- [x] 白名单系统 Bot（`fileHelper`，`robot=0`）无任何关系时仍返回完整资料
      （断言含 `short_no`）——两端各一例：
      `TestUserGet_SystemBot_Viewable`、`TestChannelGet_Person_SystemBot_Viewable`。
- [x] **反向回归**：`category=system` 但非白名单账号（admin 同类）无关系时降级为
      最小集（断言**不含** `short_no`）——两端各一例：
      `TestUserGet_SystemCategoryNotWhitelisted_Minimal`、
      `TestChannelGet_Person_SystemCategoryNotWhitelisted_Minimal`。
- [x] 被移除判定的另一半 `customerService` 同样上锁（两端各一例：
      `TestUserGet_CustomerServiceCategoryNotWhitelisted_Minimal`、
      `TestChannelGet_Person_CustomerServiceCategoryNotWhitelisted_Minimal`）。
      仓库无任何代码路径写该 category，故无种子可依赖，这两条断言是防止它被悄悄加回
      放行集的唯一保险。
- [x] 共享判定单测的身份放行子用例由 `system_account` 更名为 `system_bot`
      （`modules/channel/service/profile_visibility_test.go`）。
- [x] 无回归：`go test ./modules/user/ -run TestUserGet` 与
      `go test ./modules/channel/ -run 'TestChannelGet|TestMinimalChannelResp'` 全绿
      （含 NoRelation / CommonGroup / Bot / SameSpace / CrossSpace / Topic 等
      #722 建立的授权矩阵）。
- [x] `go build ./...`、`go vet ./modules/channel/service/ ./modules/channel/ ./modules/user/` 通过。
- [ ] 不涉及新增错误码 / 响应信封改动，故 `make i18n-extract-check` /
      `make i18n-lint` 无新增项（未重跑，本次未触碰 `pkg/errcode` 与
      `httperr` 调用点）。

### 本地验证记录（2026-08-12）

在本地容器环境（MySQL 3306 / Redis 6379 / WuKongIM 5001）实跑，上述集成用例全绿。
过程中两处**环境**前置（非本改动引入）：

- 共享 `test` 库残留其它 workspace 的迁移记录 → `unknown migration` panic，需
  drop & recreate（显式 `COLLATE utf8mb4_general_ci`）；且包间迁移集不同，
  每个测试包前需重建。
- `modules/channel` 测试包启动挂载 `modules/common` 时要求
  `OCTO_MASTER_KEY`（恰好 32 字节），测试时注入。
