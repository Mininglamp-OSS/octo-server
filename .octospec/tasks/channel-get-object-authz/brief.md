---
type: Task
title: "Task: channel-get-object-authz"
description: Add object-level authorization to the channel-detail and user-profile endpoints so callers can no longer read details for channels/users they have no relationship with.
tags: ["space", "isolation", "auth", "acl", "thread", "wire-contract", "error-response", "i18n", "rate-limit", "test"]
timestamp: 2026-08-09T05:45:39Z
# --- octospec extension fields ---
slug: channel-get-object-authz
upstream: internal security review (object-level read on the channel detail endpoint)
source: self
---

# Task: channel-get-object-authz

## Goal

`GET /v1/channels/:channel_id/:channel_type`（`modules/channel/api.go:158`
`channelGet`）**完全没有对象级授权**：`loginUID` 只被当作渲染参数传给
`BussDataSource.ChannelGet`，从不用于鉴权。任意已登录用户替换 `channel_id`
即可读取自己无权访问的频道详情。

本次给 `channelGet` 补齐对象级授权，按 `channel_type` 分治：

- **GROUP（2）/ COMMUNITY_TOPIC（5）**：校验调用方是（父）群有效成员，非成员
  按与 `GET /v1/groups/:group_no`（`groupGet`）一致的方式拒绝，不再回群名 /
  公告 / 成员数 / `space_id` / 外部群标识。
- **PERSON（1）**：不做二元拒绝，改为**按关系分级返回**。
  - **有关系** —— 本人 / 好友 / 共同 Space / 共同群 / Bot（`robot==1`，恒可查，
    见 Background）/ 系统账号 / `iwh_` webhook —— 返回完整详情，字段与今天完全
    一致（含 `real_name`，保持对外可见，不改动）。
  - **完全无关系** —— 降级为最小集，规则是**下发全部「调用方自己的状态」、只省略
    「对方的身份」**：保留 `channel_id` / `name` / `logo` / `robot` 以供渲染，保留
    `status` / `stick` / `mute` / `show_nick` / `receipt` / `remark` / `flame` /
    `flame_second` / `parent_channel`，以及 `extra` 中调用方自己的键
    （`chat_pwd_on` / `screenshot` / `revoke_remind` / `msg_auto_delete`）；
    省略对方身份与在线态：`username` / `short_no` / `sex` / `category` /
    `source_desc` / `vercode` / `online` / `last_offline` / `device_flag` /
    实名字段 / `bot_*`，以及 `be_deleted` / `be_blacklist`（对方对调用方的动作）
    和 `follow`（关系判决，发送者渲染不需要）。

`GET /v1/users/:uid`（`u.get`）是**同一根因的第二个出口**：同样只有登录鉴权、同样
直调 `GetUserDetail` 不校验关系，任意登录用户拿任意 UID 即可读到完整身份。本次一并
修复——判定收口到 `modules/channel/service`（零依赖叶子包），两端共用同一函数，避免
一边放宽另一边静默重开同一越权面。该端点的最小集**保留 `follow`**：它是资料页渲染
「加好友」入口的依据，省略会让陌生人加好友这个正常入口消失（channelGet 是发送者渲染，
不需要 follow，故刻意省略）。加好友流程不依赖本响应其它字段——`vercode` 由 search /
扫码路径铸造并校验，且该端点对非好友本来就返回空 `vercode`。

最小集的"省略 vs 给值"按**客户端如何解读缺失**逐字段决定，统一到一条规则：

> **下发所有「调用方自己的状态」，只省略「对方的身份」。**

依据：三端客户端**整行写回本地缓存且不做字段存在性检查**——用新分配的对象接收响应，
缺失键取零值，再无条件覆盖 SDK 缓存里正确的值。所以省略"调用方自己的设置"买不到任何
隐私，只会把用户自己开启的功能悄悄关掉。已被追证的后果：

- 缺 `status` → 零值 0 撞上"已禁用/封禁"哨兵（Android `WKChannelStatus.statusDisabled
  = 0`）→ 隐藏输入框并显示封禁视图；
- 缺 `extra.chat_pwd_on` → **聊天密码锁静默失效**，加锁会话直接打开且不提示、会话列表
  预览不再打码（Android 内存态、iOS 落盘）；
- 缺 `extra.msg_auto_delete` → iOS 新发消息不再按期自动删除（该键全端只由本接口注入）；
- 缺 `mute` / `stick` / `remark` → 免打扰、置顶、备注被重置（iOS 历史上已为 `mute`
  单独硬编码过兜底，说明这是已复发过的 bug class）。

唯一例外是 `follow`：channelGet 省略它（给 0 会被读成"明确非好友"，且发送者渲染不需要
关系判决），users/:uid 保留它（资料页靠 `follow==0` 渲染加好友入口，且走到最小集必然
是 0）。

触发路径不是陌生人，而是"当前正在 1:1 的对端离开可见集"——删好友、同 Space 对端所在
Space 被封禁、或唯一共同群解散/自己退群，下次频道信息刷新即命中。

同时消除两个放大风险：群不存在触发的 nil-panic（500，构成存在性枚举
oracle），以及该路由缺少 per-UID 限流（批量枚举无成本）。

## Background

### 复现（测试环境，2026-08-09，退群闭环，凭据不入库）

建一临时群（我为创建者）→ 退群（群主自动转让给第二老成员，群继续存在）→ 以
**非成员**身份复测同一群号：

- `GET /v1/groups/{gno}` → **403** `err.server.group.view_forbidden`（正确）
- `GET /v1/channels/{gno}/2` → **200 + 完整群详情**，响应 `quit:1` 表明服务端
  明知调用方已退群，仍下发 `name` / `notice` / `member_count` / `space_id` /
  `is_external_group` / `allow_view_history_msg`。

两端点调用的是**同一个** `group.Service.GetGroupDetail`
（`modules/group/service.go:304`），差异只在 `groupGet`（`modules/group/api.go:847`）
调用前有 `ExistMember` 门禁，`channelGet` 没有。

Space 头对该端点完全无效：非成员态下删除 `X-Space-Id` 或伪造成任意值，
`channelGet` 一律 200（`groupGet` 删头仍 403，因它查 `ExistMember` 与头无关）。
`channelGet` 对 `s{spaceID}_{uid}` 前缀只做剥离取 `peerID`
（`modules/channel/api.go:165-170`），解析出的 `spaceID` 直接丢弃，且该路由未挂
`SpaceMiddleware`（`api.go:42-49` 只有 `AuthMiddleware`）。

顺带证实的存在性 oracle：群不存在 → **500 空 body**
（`newChannelRespWithGroupResp`（`modules/group/1module.go:171`）对
`GetGroupDetail` 返回的 `nil` 不判空即解引用，panic 被 gin Recovery 兜成 500）；
用户不存在 → **400**「用户信息不存在！」。两种响应可零成本区分频道/用户是否存在。

### 数据源链现状（四个 `ChannelGet` 实现均不鉴权）

`channelGet` 通过 `register.GetModules` 链式分发到各模块的
`BussDataSource.ChannelGet`。这些是**展示层**数据源，设计上不负责鉴权：

- PERSON：`modules/user/1module.go:56` → `GetUserDetail(uid, loginUID)`
  （`modules/user/service.go:420`）。`phone` / `email` / `zone` 已被
  `NewUserDetailResp`（`service.go:1499`）的 `self := loginUID == m.UID` 挡住，
  未泄露；但 `short_no` / `device_flag` / `last_offline` 无条件下发，`real_name`
  按 `service.go:1460` 注释是**有意对外可见（含非自己）**。
- GROUP：`modules/group/1module.go:97` → `GetGroupDetail`。
- COMMUNITY_TOPIC：`modules/thread/1module.go:200`，**完全不校验父群成员关系**，
  与 CLAUDE.md「thread 必须 verify parent channel access」相违。
- `iwh_` 前缀单聊：`modules/incomingwebhook/display.go:83`，仅合成展示名/头像，
  刻意不下发 `group_no`；本 brief 不改其逻辑。

### 为什么 PERSON 不能简单 403（关键约束）

`modules/user/1module.go:239` 的注释（YUJ-411 根因报告）明确：
`/v1/channels/:id/:type → newChannelRespWithUserDetailResp` 是 Android WKSDK
单用户 Channel 的**唯一数据源**，客户端渲染**任意消息发送者**名字/头像都打这个
端点。若对非好友硬 403，**外部群里来自其它 Space 的非好友成员**（`IsFriend` 与
`GetCommonSpaceID` 都判否）会渲染裂名/裂图。因此 PERSON 分支必须分级降级而非
拒绝。仓库现有关系素材只有 `userService.IsFriend` 和
`space.GetCommonSpaceID`（`modules/space/db.go:438`），**缺"共同群"关系判定
helper**，需新建（外部群跨 Space 成员既非好友也非共同 Space，仅靠共同群可达）。

产品事实（本 task 的关系模型依据，已确认）：

- **同一 Space 内的用户可直接聊天，无需好友关系**——这是最主要的可达关系，对齐现有
  `GetUserDetail`（`service.go:518`）对共同 Space 自动补 `follow=1` 的写法。跨 Space
  才依赖好友或共同群可达。因此对绝大多数目标"有关系"成立；"完全无关系"（不同
  Space、非好友、无共同群）只出现在历史消息残留的发送者渲染或枚举探测。
- **Bot 需先加好友才能交互**，但其**资料必须可查看**（用户要先看到 bot 才能决定
  是否添加）。故 Bot（`robot==1`）在本分支**恒归入"可查"**，与"未加好友不能和
  bot 聊天"不冲突（查看资料 ≠ 已可交互）。这也对齐现有 `GetUserDetail`
  （`service.go:518`）里 `follow==0 && robot==0` 才用共同 Space 补 `follow` 的
  写法——Bot 被单独排除在该补位逻辑之外。
- **`real_name` 保持对外可见**（`service.go:1460` 现状不变）；它只在"有关系"路径
  进入响应，无关系走最小集时随整个 `extra` 不下发。

### 合法非成员场景不受影响（已核对）

- 加群预览走**独立公开端点** `/v1/group/invite/detail`（`groupInviteDetail`，
  `modules/group/api.go:4613`，`QueryWithGroupNo` + `QueryMemberCount`，需持有效
  邀请码），扫码入群走 `scanjoin`，均不经 `channelGet`。
- 退群后 `conversation/sync` 已把该群移出会话列表（实测），正常客户端不会再对
  退群的群调 `channelGet`。

## Load-bearing list

- `space` / `isolation` / `auth` / `acl` — 对象级读边界；本改动即该边界本身。
  GROUP/TOPIC 的成员校验、PERSON 的关系分级、以及 Space 前缀解析后 `spaceID`
  被丢弃的现状都在此范围。
- `thread` — COMMUNITY_TOPIC 频道详情继承父群 ACL；新增父群成员校验。
- `wire-contract` — `channelGet` 响应形状收窄：PERSON 无关系时字段裁剪，
  GROUP/TOPIC 非成员从 200+详情 变为拒绝。需核对客户端字段依赖不回归。**已知
  影响**：历史消息里的群引用（群名片转发、"你已被移出群"通知、搜索历史点入）若
  客户端**实时**调 `channelGet /2` 渲染群名，非成员将拿到拒绝而非群名——需与
  客户端确认这些位置是否已用本地缓存的群名而非实时拉取。
- `error-response` / `i18n` — `modules/channel/` 尚未做 i18n 迁移（现全是 legacy
  `c.ResponseError`）。新增拒绝分支必须走 `httperr.ResponseErrorL` + 注册
  `pkg/errcode` 码；`incomingwebhook`（`webhook_render_test.go`）与 `user` 侧有
  跨模块测试断言 `channelGet` 响应文本/行为，改响应体要同步。
- `rate-limit` — 该路由组（`api.go:42`）仅 `AuthMiddleware`，需在 `AuthMiddleware`
  之后挂 `SharedUIDRateLimiter`（默认 2rps/burst60），与其它认证路由一致。
- `test` — 跨用户 / 跨 Space / 退群后 / 非父群成员子区 / `iwh_` webhook /
  不存在目标 的回归测试，两个端点各一套；限流测试 setup 需重置本用户的
  `ratelimit:uid:{uid}`（只删自己那把 key，KEYS 全量扫会删掉并发包的桶）。

## Out of scope

- 用户资料的**字段策略本身**（哪些字段属于"完整"）不变：手机号 / 邮箱 / 区号仍只
  对本人下发，`real_name` 在有关系视角下的可见性保持现状。本 task 只加"无关系时
  降级"这一层。
- `iwh_` webhook 展示分支（`incomingwebhook/display.go`）逻辑不变。
- 加群预览 / 扫码入群端点（`groupInviteDetail` / `scanjoin`）不改。
- todo 中其它条目（扫码登录 4.1、预签名下载、HTML 内联、Token 生命周期、公共配置
  收敛）不在本 task。

## Acceptance

### A. 对象级授权（核心）

- [ ] GROUP：非成员调用 `GET /v1/channels/{gno}/2` 不再返回群详情，其拒绝行为与
      `GET /v1/groups/{gno}` 对齐（复用 `errcode.ErrGroupViewForbidden` 或等价
      语义码）。退群闭环复测：退群后 `channels/{gno}/2` 与 `groups/{gno}` 响应
      一致，均不下发 `name`/`notice`/`member_count`/`space_id`。
- [ ] COMMUNITY_TOPIC：非父群成员调用 `GET /v1/channels/{topicID}/5` 被拒，
      不下发子区名 / `group_no` / `creator_uid` / `message_count`。
- [ ] PERSON 无关系目标：按「调用方自身状态全留、对方身份全剥」降级——断言响应**保留**
      `status` / `stick` / `mute` / `remark` / `extra.chat_pwd_on` /
      `extra.msg_auto_delete` 等调用方自身设置（缺失会让用户自己开启的功能被清零），
      且**不下发** `short_no` / `sex` / `device_flag` / `last_offline` /
      `source_desc` / `vercode` / 实名字段 / `follow`。
- [ ] PERSON 有关系目标（本人 / 好友 / 共同 Space / 共同群 / Bot / 系统账号）：
      现有字段齐全无回归，`real_name` / `realname_verified` 等在已实名好友视角
      下仍正常下发（保持现状）。
- [ ] Bot 目标（`robot==1`）即使调用方未加其好友也可查看资料（`name` / `logo` /
      `bot_description` / `bot_commands` 等），不被降级为最小集。
- [ ] 外部群跨 Space 非好友成员：`GET /v1/channels/{uid}/1` 仍成功返回 `name` /
      `logo`（不 403、不裂名），由新建的"共同群"关系判定覆盖。
- [ ] Space 头不再是授权依据也不再被静默忽略造成越权：删除/伪造 `X-Space-Id`
      不能读取调用方无权访问的频道（与新增的成员/关系校验一致即可）。

### A2. `GET /v1/users/:uid`（同根因，一并修复）

- [ ] 无关系目标：降级为最小集，同样遵循「调用方自身状态全留、对方身份全剥」——保留
      `uid`/`name`/`robot`/`follow`（恒 0）以及 `status`/`mute`/`top`/`chat_pwd_on`/
      `screenshot`/`revoke_remind`/`receipt`/`flame`/`flame_second`/`remark`；
      不下发 `short_no` / `sex` / `online` / `last_offline` / `device_flag` /
      `source_desc` / `vercode` / 实名字段 / `be_deleted` / `be_blacklist`。
- [ ] 最小集**保留 `follow`**：陌生人资料页仍能渲染「加好友」入口（与 channelGet
      最小集的刻意差异，理由见 Goal）。
- [ ] 有关系目标（本人 / 好友 / 同 Space / 共同群 / bot / 系统账号）：字段无回归。
- [ ] 不存在的 UID 返回 `err.server.user.not_found`，不再返回"查询失败"（后者会误导
      客户端重试，也混淆了故障与不存在）。
- [ ] 可见性判定与 channelGet **共用** `modules/channel/service` 的同一函数；判定
      依赖未注入时 fail closed（降级而非放开）。

### B. 健壮性与响应一致性

- [ ] 群不存在不再返回 500：`channelGet` 对 `GetGroupDetail` 返回 `nil` 的分支
      判空处理（同时修 `newChannelRespWithGroupResp` 的 nil 解引用）。
- [ ] 存在性枚举收敛（分类型，避免自相矛盾）：
      - GROUP/TOPIC：不存在的（父）群与非成员返回**同一** forbidden 响应（码+状态
        一致），对齐 `groupGet` 先 `ExistMember` 的行为，天然不泄露群是否存在。
      - PERSON：不存在目标不再 500，返回稳定的 not_found；"无关系但存在"仍需返回
        最小集（`name`/`logo`，供历史发送者渲染），故与 not_found **有意可区分**。
        这是"发送者渲染必须成功"与"防 uid 枚举"的固有张力；因 uid 为 32-hex 高熵、
        非现实枚举面（真正的枚举面 `short_no` 不经此端点），此处取"消除 5xx +
        渲染优先"，不强行统一。
- [ ] 新增拒绝/降级分支全部走 `httperr.ResponseErrorL` + 已注册 `pkg/errcode`
      码；`make i18n-extract-check` 与 `make i18n-lint` 通过；对应 zh-CN 翻译
      入 `active.zh-CN.toml`。
- [ ] 该路由组在 `AuthMiddleware` 之后挂 `SharedUIDRateLimiter`。
- [ ] `go test ./modules/channel/... ./modules/user/... ./modules/group/... ./modules/thread/... ./modules/incomingwebhook/...` 通过；
      新增 `channelGet` 授权矩阵单测（当前 `channelGet` 零测试覆盖）；限流相关
      测试在 setup reset `ratelimit:uid:*`。
- [ ] `Test<Module>NoLegacyResponseError` 源码守卫与 D23 lint baseline 不因新增
      handler 文件而破。

### 交付方式

- 单个 PR 交付以上全部 Acceptance（授权分级 + 健壮性 + 限流 + 测试）。`real_name`
  可见性已定（保持现状），无外部阻塞项。
