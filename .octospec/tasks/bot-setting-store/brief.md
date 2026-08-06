---
type: Task
title: "Task: bot-setting-store"
description: Generic per-bot config store (bot_setting) with a bot → global → code-default resolution chain; first consumer is the reasoning-progress card policy.
tags: ["bot-api", "auth", "error-response", "wire-contract", "rate-limit", "testing"]
timestamp: 2026-08-06T00:00:00Z
slug: bot-setting-store
upstream: openclaw-channel-octo 推理进度卡策略请求
source: user
---

# Task: bot-setting-store

## Goal

给 Bot 一个**通用**的服务端配置存储，取代「一个开关加一列 `robot` 表」的现状。

新增 `bot_setting` 稀疏覆盖表，解析链固定为：

```
bot_setting（该 Bot 的覆盖）→ system_setting（服务端全局默认）→ 代码默认
```

三个面向：

1. **owner 配置面**（`/v1/robot/:robot_id/settings`，user session + `assertRobotOwner`）——
   「Bot 管理」页要能**查询本部署有哪些可配置项**并**回显当前值**，而不是由客户端
   硬编码一张列表。读接口返回全部已注册键 + 该 Bot 的覆盖值 + 三层解析后的有效值 +
   值来源；写/删接口改这一份覆盖。
2. **adapter 消费面**（`GET /v1/bot/card/profile`，botToken）——下发**已解析好**的
   卡片有效配置（`reasoning_progress.mode` / `template_ref`），插件不再自行推断。
3. **发送端**（`POST /v1/bot/sendMessage` 模板分支）——按同一份有效配置二次校验。

首个消费者是推理进度卡策略：Bot 级的 `mode`(off|on) 与 `template_ref`。

## Background

**一维 per-bot 配置在本仓的既有做法是「给 `robot` 扩一列 + 一个 owner 自助端点」**：
`auto_approve`（`modules/robot/api.go:1516-1554`）、`inline_on`、`placeholder`、
`bot_commands`、`app_bot.welcome_msg` 都是这个形状。`robot` 表初始 7 列，之后 7 个
迁移累计追加十余列，混装了身份、凭据、上报元数据与产品配置；滚动发布下 ALTER 还需
手写并发守卫（`modules/botfather/sql/20260603000001_botfather_legacy01.sql:7-12`）。
**本任务取代的正是「继续扩列」这条路**——因为 Bot 级配置项还会继续增加。

`bot_mention_pref` 不是本任务的先例，也不迁入 `bot_setting`：它的维度是
`(robot_id, group_no)` **二维**，`robot` 扩列在结构上做不到，开表是唯一选择。但它的
**周边形状**是本任务要照搬的样板（owner 守卫、删除即回落、写后推事件失效缓存、
owner 读与 adapter 读分属两个模块）。

`system_setting`（`modules/common/system_settings.go`）已经把「schema 白名单 + 类型
校验 + DB→fallback 解析链 + 管理端 `effective_value` 回显」做完并在生产验证过。本任务
按同一形状做 per-bot 版本，并把它作为解析链的第二层。

上游诉求（推理卡）要求服务端是配置权威、用户不改插件配置文件；`mode=off` 只阻止
**新建**进度卡，已发出的卡仍可编辑到终态。

## Load-bearing list

- **`bot_setting` 稀疏语义**：只存偏离全局默认的覆盖项；无行 / 空串 value = 未配置，
  回退下一层。删除覆盖 == 回落全局，不是「设为 off」。
- **解析优先级**：bot 覆盖恒优先于全局默认；全局默认恒优先于代码默认。代码默认为
  `mode=off`（未升级/未配置的部署行为不变，零回归）。
- **键白名单是可查询的**：同一份注册表既是写入校验的白名单，也是 owner 读接口
  返回的「可配置项目录」——客户端据此渲染「Bot 管理」列表，新增一个键不需要客户端
  发版。未注册的 key 写入必须 400，不得长出野键（同 `system_setting` 的 `settingDef`）。
- **回显三态必须可区分**：读接口对每个键同时返回 `value`（该 Bot 的显式覆盖，未设
  为空）、`effective_value`（三层解析结果）与 `source`（`bot`/`global`/`default`）。
  只返回一个合并值，UI 就无法区分「我显式设成了 off」与「我没设、跟随全局默认 off」，
  也就无法正确渲染「恢复默认」。
- **`enabled` 从属关系**：`cardmsg.BotEnabled()`（`OCTO_CARD_MESSAGE_ENABLED` AND
  `OCTO_BOT_CARD_ENABLED`）为假时，profile 下发的有效 `mode` 必须是 `off` ——
  清单绝不能报 `on` 而发卡被 `card_disabled` 拒（`pkg/cardmsg/cardmsg.go:177-200`
  已确立的「清单与发卡门禁同源」不变量）。
- **`reasoning_progress` 与 `templating` 自洽**：`mode=="on"` ⟹ 下发的 `template_ref`
  必在同一响应的 `templating.templates` 内（即部署广告集
  `defaultBotTemplatePolicy().AdvertisedSend`）。
- **`/v1/bot/card/profile` wire 契约 additive-only**（`modules/bot_api/card_profile.go:23-24`）：
  只增字段，不改名/不删/不改类型。
- **发送端二次校验**：profile 是能力清单，`sendMessage` 模板分支必须独立按有效配置
  校验（`mode==off` 拒绝；`template_ref` 必须等于该 Bot 的有效 ref），拒绝走单一
  泛化码 `err.server.bot_api.card_invalid`，具体原因只进日志（防枚举）。
- **`mode=off` 只拦新发**：`send.go` 模板分支加门，`botMessageEdit` 的模板分支不加 ——
  已发出的卡必须能编辑到终态，否则线上残留「永久处理中」的卡。
- **写入口鉴权 = bot owner 自助**：对齐 `setAutoApprove` / `setMentionPref` 的
  `assertRobotOwner`（`creator_uid == loginUID`，robot 不存在 → 404、非 owner → 403、
  DB 故障 → 500 且不得伪装成 404）。挂在 `/v1/robot/:robot_id/*`（user session）。
  `/v1/manager/robots/*` 是平台运维面（状态/删除/重置 token），不是产品配置面。
  `updated_by` 记录操作者 UID（即审计，不另开审计表）。
- **删除覆盖 = 回落上一层，幂等**：删不存在的覆盖也返回 200（对齐
  `deleteMentionPref`）。删除 ≠ 设为 `off`。
- **写后推事件失效插件缓存**：配置变更后向该 Bot 投递一条合成事件，插件据此即时
  重拉 profile，消除「改了开关要等插件 TTL」的窗口（对齐
  `sendMentionPrefNotification` 的动机）。账号级配置无频道上下文，用既有的
  `robot.IService.EnqueueBotEvent`（`modules/robot/api.go:81-86`）投进
  `/v1/bot/events` 队列，**不**照抄 mention_pref 的群频道消息。
  best-effort：异步 + recover，投递失败只记日志，绝不影响写接口返回 200。

## Out of scope

- `profiles` 按 Bot 裁剪。`pkg/cardmsg/profiles.go:16-35` 明确 `acceptedProfiles` 是
  校验器接受集与 D12 清单的**单一权威**，按 Bot 裁剪会打破该不变量，且推理卡的
  active/error 两个 view 本身是 `octo/v2`，裁掉会把卡砍成只剩终态。单独立项。
- 静态内置模板的 block 通道（`Blocked` 只存在于动态 artifact meta）。
- per-bot 模板可见性 / grant 模型。本任务的可见性 == `template_ref` 必须在部署广告集内。
- 平台管理端（`/v1/manager/robots/*`）的配置写入口与「管理员覆盖 owner」的优先级。
  本任务只做 owner 自助写；管理端若需要，是后续一期。
- 存量 `robot` 表配置列（`inline_on`/`auto_approve`/…）的迁移或读路径收口。
- `bot_mention_pref` 的迁入。它是 `(robot_id, group_no)` 二维配置，不属于
  `bot_setting` 的一维模型，保持独立。
- 配置读缓存。v1 走单行主键查询，与 send 路径已有的 DB 查询同量级。
- **配置项标题/副标题的服务端本地化下发**。读接口只返回 `key` / `type` / `options`
  等结构信息，展示文案由客户端按 key 自持（现状「Bot 管理」页的免@回答等条目已是
  客户端文案）。服务端下发本地化文案需要一套非错误码的 i18n 消息注册面，本任务不建。
  客户端遇到不认识的 key 应跳过，不阻塞渲染。
- 插件侧改造（Model B 废弃、`cardProgress` 移除等）。

## Acceptance

- 无 `bot_setting` 行且无 `system_setting` 行 → 有效 `mode=="off"`；profile 下发
  `reasoning_progress.mode=="off"`；模板发送被拒（`err.server.bot_api.card_invalid`）。
- Bot A 写 `mode=on` → profile 下发 `on` + `template_ref`；模板发送成功；Bot B 无记录
  仍为 `off`，互不影响。
- `system_setting` 全局设 `on`、Bot B 显式覆盖 `off` → B 有效值仍是 `off`（bot 层优先）。
- 覆盖行被删除（写空 value）→ 该 Bot 回落全局默认，而非落到代码默认。
- `cardmsg.BotEnabled()==false` 时，无论 bot/全局配置为何，profile 的
  `reasoning_progress.mode` 恒为 `off`。
- `mode=="on"` 时，profile 里的 `template_ref` 必然出现在同一响应的
  `templating.templates` 中（断言两者一致，防止自相矛盾）。
- Bot A 用不等于自己有效配置的 `template_ref` 发送 → 400 `err.server.bot_api.card_invalid`，
  且响应体不透出「该模板属于其他 Bot」之类可枚举信息。
- 未注册 key 写入 → 400；已注册 key 的非法值（`mode` 非 off/on）→ 400。
- owner 读接口返回全部已注册键；每个键的 `value` / `effective_value` / `source`
  三者可区分：未设覆盖时 `value==""` 且 `source!="bot"`；显式设为与全局同值时
  `value` 非空且 `source=="bot"`。
- 新注册一个键后，owner 读接口立即返回它（客户端无需发版即可发现新配置项）。
- 非 owner 读/写该 Bot 配置 → 403；robot 不存在/无 `creator_uid` → 404；DB 故障 → 500
  且不被伪装成 404。
- 删除不存在的覆盖 → 200（幂等）；删除后有效值回落全局默认而非代码默认。
- 写入成功后向该 Bot 的 `/v1/bot/events` 队列投递一条配置变更事件；事件投递失败
  不影响写接口返回 200。
- `mode==off` 时 `POST /v1/bot/message/edit` 的模板分支仍可把已发卡编辑到终态。
- 新增 handler 文件已加入 `modules/robot/api_i18n_test.go` 与
  `modules/bot_api/api_i18n_test.go` 的 `NoLegacyResponseError` 守卫列表。
- `go build ./...`、相关模块 `go test`、`make i18n-extract-check`、`make i18n-lint` 通过。
