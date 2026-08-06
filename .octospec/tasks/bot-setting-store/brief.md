---
type: Task
title: "Task: bot-setting-store"
description: Generic per-bot config store (bot_setting) with a bot → global → code-default resolution chain; first consumers are the four bot-level card switches.
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
2. **adapter 消费面**（`GET /v1/bot/card/profile`，botToken）——新增 `config` 对象，
   下发**已经 AND 完的有效值**，插件直接用、不自行推断。
3. **发送端**（`POST /v1/bot/sendMessage`）——按同一份有效配置二次校验。

首批四个键：

| 键 | 管什么 | 存储 | 代码默认 |
|---|---|---|---|
| `bot.card_enabled` | 该 Bot 的卡片总闸 | **不存**，取 `cardmsg.BotEnabled()` | 随 env（未设 = false） |
| `bot.display_enabled` | **raw 展示卡**（bot 自拼 octo/v1 卡） | `bot_setting` | `true` |
| `bot.interaction_enabled` | **raw 交互卡**（bot 自拼 octo/v2 卡） | `bot_setting` | `true` |
| `bot.reasoning_enabled` | **Registry 推理进度卡** | `bot_setting` | `true` |

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

**三个子开关默认 `true` 是安全的**，因为安全性由总闸承担：`OCTO_CARD_MESSAGE_ENABLED`
未设即 `false`（`pkg/cardmsg/cardmsg.go:168-175` fail-closed），`card_enabled` 为假时
三个子开关的有效值恒为假。上游诉求里的「安全默认关闭推理卡」由总闸满足，子开关不必
再各自默认关——否则运维开了总闸还要逐 Bot 再开一次，反而制造困惑。

## Load-bearing list

- **`bot_setting` 稀疏语义**：只存偏离全局默认的覆盖项；无行 / 空串 value = 未配置，
  回退下一层。删除覆盖 == 回落上一层，不是「设为 false」。
- **解析优先级**：bot 覆盖 → `system_setting` 全局默认 → 代码默认（三个子开关均为
  `true`）。未升级/未配置的部署行为不变，零回归。
- **`card_enabled` 是派生值，不可写**：它恒等于 `cardmsg.BotEnabled()`
  （`OCTO_CARD_MESSAGE_ENABLED` AND `OCTO_BOT_CARD_ENABLED`），不落 `bot_setting`。
  owner 目录里以只读项出现（`editable:false`，`source:"env"`），写入必须 400 ——
  否则会出现「库里写着 true、env 关着」的假象。
- **总闸支配子开关**：`card_enabled==false` ⟹ 三个子开关的有效值全为 `false`。
  profile 绝不能报某个子开关为 true 而发卡被 `card_disabled` 拒
  （`pkg/cardmsg/cardmsg.go:177-200` 已确立的「清单与发卡门禁同源」不变量）。
- **display / interaction / reasoning 三者正交**：`display_enabled` 与
  `interaction_enabled` 只管 **raw 卡路径**（bot 自拼 card JSON），**绝不可**实现成
  「按 wire profile 一刀切」。推理卡自身横跨两档
  （`ai.reasoning-process@0.3.0/manifest.json`：active/error = `octo/v2`，
  result = `octo/v1`），按 profile 切会把它砍成只剩终态或只剩过程。模板卡只看
  `reasoning_enabled`。
- **键白名单是可查询的**：同一份注册表既是写入校验的白名单，也是 owner 读接口
  返回的「可配置项目录」——客户端据此渲染「Bot 管理」列表，新增一个键不需要客户端
  发版。未注册的 key 写入必须 400，不得长出野键（同 `system_setting` 的 `settingDef`）。
- **回显三态必须可区分**：读接口对每个键同时返回 `value`（该 Bot 的显式覆盖，未设为
  `null`）、`effective_value`（解析后的有效值）与 `source`（`bot`/`global`/`default`/`env`）。
  只返回一个合并值，UI 就无法区分「我显式设成了 false」与「我没设、跟随全局默认
  false」，也就无法正确渲染「恢复默认」。
- **`reasoning_template_ref` 由服务端解析，非 owner 可配**：`reasoning_enabled==true`
  时下发的 ref 必在同一响应的 `templating.templates` 内（即部署广告集
  `defaultBotTemplatePolicy().AdvertisedSend`）；`false` 时必须为 `null`。
- **`/v1/bot/card/profile` wire 契约 additive-only**（`modules/bot_api/card_profile.go:23-24`）：
  只增 `config` 对象，现有字段（含 `enabled`）不改名/不删/不改类型/不改语义。
- **发送端二次校验**：profile 是能力清单，`sendMessage` 必须独立按有效配置校验
  （模板分支 `reasoning_enabled==false` 拒绝；raw 分支按 `display_enabled` /
  `interaction_enabled` 拒绝），拒绝走单一泛化码 `err.server.bot_api.card_invalid`，
  具体原因只进日志（防枚举）。
- **关闭只拦新发**：门加在 `send.go`，`botMessageEdit` 的模板分支不加 —— 已发出的卡
  必须能编辑到终态，否则线上残留「永久处理中」的卡。
- **写入口鉴权 = bot owner 自助**：对齐 `setAutoApprove` / `setMentionPref` 的
  `assertRobotOwner`（`creator_uid == loginUID`，robot 不存在 → 404、非 owner → 403、
  DB 故障 → 500 且不得伪装成 404）。挂在 `/v1/robot/:robot_id/*`（user session）。
  `/v1/bot/card/profile` 保持 botToken —— 它的语义是「bot 读自己的有效配置」，
  owner 化等于废掉它（插件没有 user session）。
  `updated_by` 记录操作者 UID（即审计，不另开审计表）。
- **删除覆盖 = 回落上一层，幂等**：删不存在的覆盖也返回 200（对齐 `deleteMentionPref`）。
- **写后推事件失效插件缓存**：配置变更后向该 Bot 投递一条合成事件，插件据此即时
  重拉 profile，消除「改了开关要等插件 TTL」的窗口（对齐
  `sendMentionPrefNotification` 的动机）。账号级配置无频道上下文，用既有的
  类型化事件通道 `robot.IService.EnqueueBotTypedEvent`（`modules/robot/api.go`），
  **不**照抄 mention_pref 的群频道消息——那条走群频道是因为免@偏好本身是群维度的。
  事件**不携带具体新值**：带值就意味着事件与 profile 两条下发路径各自维护同一份
  语义，一旦漂移，adapter 会拿事件里的旧形状覆盖 profile 的权威结果。
  best-effort：异步 + recover，投递失败只记日志，绝不影响写接口返回 200。
- **多副本一致性分两档，且是刻意的**：per-Bot 覆盖**不做进程内缓存**，每次解析直读
  MySQL，因此 owner 在副本 A 的改动对副本 B 立即可见；全局默认层继承
  `system_setting` 的进程内快照 + `defaultReloadTTL=60s` 轮询，故改**全局默认**
  最多 60s 才在所有副本生效。加 per-Bot 缓存会把即时一致换成 TTL 漂移，而失效事件
  只到得了 Bot、到不了其它副本（要做得上 Redis pub/sub）——一致性优先于这点开销。
- **热路径不得取进程级锁**：`common.EnsureSystemSettings` 每次调用都取全局 mutex
  （`modules/common/system_settings.go`），而 `SystemSettings` 的设计前提是
  「readers 走 atomic.Pointer、永不取锁」。解析器必须在**构造期**解析一次并持有实例
  （`Robot.systemSettings` / `Service.systemSettings`），绝不可在每次请求里调
  `EnsureSystemSettings`，否则等于把全局锁加到发卡路径上。
- **门只加在新建路径**：`sendMessage` 的门在 `if cardIntent` 之内，非卡片消息零开销；
  推理卡的流式更新走 `message/edit`，该路径**刻意不加门**（关闭只拦新建，已发卡要能
  进终态）。故新增成本是「每张卡创建一次索引单行读」，而该路径本就有多次 DB 查询。

## Out of scope

- `profiles` 按 Bot 裁剪。`pkg/cardmsg/profiles.go:16-35` 明确 `acceptedProfiles` 是
  校验器接受集与 D12 清单的**单一权威**，按 Bot 裁剪会打破该不变量。本任务改用
  `display_enabled` / `interaction_enabled` 两个显式布尔表达同一意图。
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

**解析链**

- 无 `bot_setting` 行且无 `system_setting` 行 → 三个子开关有效值均为 `true`（在
  `card_enabled==true` 前提下）。
- Bot A 显式写 `bot.reasoning_enabled=false` → 只影响 A；Bot B 无记录仍为 `true`。
- `system_setting` 全局设 `false`、Bot B 显式覆盖 `true` → B 有效值为 `true`（bot 层优先）。
- 删除 Bot B 的覆盖 → 回落**全局默认**而非代码默认；删不存在的覆盖 → 200（幂等）。

**总闸与派生值**

- `cardmsg.BotEnabled()==false` 时，无论 bot / 全局配置为何，profile 的
  `config.*` 四个布尔恒为 `false`，且 `reasoning_template_ref==null`。
- 写 `bot.card_enabled` → 400（派生值不可写）。
- owner 目录中 `bot.card_enabled` 以 `editable:false` / `source:"env"` 返回。

**profile wire**

- `config.reasoning_enabled==true` ⟹ `reasoning_template_ref` 非 null 且必然出现在
  同一响应的 `templating.templates` 中（断言两者一致，防止自相矛盾）。
- `config.reasoning_enabled==false` ⟹ `reasoning_template_ref==null`。
- 现有字段 `enabled` / `card_version` / `profiles` / `elements` / `inputs` / `actions` /
  `templating` / `limits` 的值与形状不因本任务改变。

**发送端**

- `reasoning_enabled==false` 时模板发送 → 400 `err.server.bot_api.card_invalid`，
  且响应体不透出可枚举信息。
- Bot A 用不等于自己有效配置的 `template_ref` 发送 → 同上单一泛化码。
- `reasoning_enabled==false` 时 `POST /v1/bot/message/edit` 的模板分支仍可把已发卡
  编辑到终态。
- `display_enabled==false` 时 raw octo/v1 卡被拒；`interaction_enabled==false` 时
  raw octo/v2 卡被拒；两者均**不影响**模板渲染出的推理卡。

**owner 面**

- 读接口返回全部已注册键；`value` / `effective_value` / `source` 三者可区分：
  未设覆盖时 `value==null` 且 `source!="bot"`；显式设为与全局同值时 `value` 非 null
  且 `source=="bot"`。
- 新注册一个键后，读接口立即返回它（客户端无需发版即可发现新配置项）。
- 未注册 key 写入 → 400；已注册 bool 键的非法值 → 400。
- 非 owner 读/写 → 403；robot 不存在/无 `creator_uid` → 404；DB 故障 → 500 且不被
  伪装成 404。
- 写入成功后向该 Bot 的 `/v1/bot/events` 队列投递一条配置变更事件；事件投递失败
  不影响写接口返回 200。

**工程门**

- 新增 handler 文件已加入 `modules/robot/api_i18n_test.go` 与
  `modules/bot_api/api_i18n_test.go` 的 `NoLegacyResponseError` 守卫列表。
- 新增 owner 路由挂 `SharedUIDRateLimiter`（`AuthMiddleware` 之后）。
- `go build ./...`、相关模块 `go test`、`make i18n-extract-check`、`make i18n-lint` 通过。
