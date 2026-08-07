---
type: Task
title: "Task: bot-setting-owner-contract"
description: Close the owner-facing contract gaps left open in the bot_setting store — master-switch projection, null semantics on PUT, ownership ordering, and the retry/propagation facts operators and adapters need.
tags: ["robot", "bot-api", "config", "wire-contract", "auth"]
timestamp: 2026-08-07T02:04:20+00:00
# --- octospec extension fields ---
slug: bot-setting-owner-contract
upstream: "Mininglamp-OSS/octo-server#706 合并时留下的 P2 清单"
source: review
---

# Task: bot-setting-owner-contract

## Goal

把 #706 合并时明确留下的 owner 侧契约缺口收掉：让 owner 目录返回的值与实际能力一致、
让读改写回的最自然流程不再 400、把属主校验提到 body 解析之前，并把两条**只写在 Go 注释
里、消费方看不到**的事实（profile 新失败模式的重试语义、全局层的传播窗口）落到契约与运维
文案上。

全部是 #706 评审判定为 non-blocking、且当时刻意不做的项。没有一条改变卡片能力本身。

## Background

#706 引入了 `bot_setting` 三层解析与四个卡片开关。合并时三位 reviewer 全部 approve，
并一致建议「先合，P2 走 follow-up」。以下是那份清单里**属于 owner/消费方契约**的部分
（鉴权类的在 `robot-stream-authz-symmetry`）。

**P2-1 · owner 目录的 `effective_value` 没经过总闸与下限投影。**
`listBotSettings` 直接回传 `resolveBotSettings` 的结果；`applyCardMasterSwitch` 与
`AllowsRawDisplayCard`/`AllowsRawInteractiveCard` 都在 `botCardConfigFrom` 里，而它只喂
profile 与发送门。两处同部署同 Bot 会给出互相矛盾的答案：

- 总闸未开时：目录报 `bot.display_enabled.effective_value=true`，profile 报
  `display_enabled=false`，发卡 400。owner 在管理页看到「展示卡：开」，一张也发不出去。
- `display=false, interaction=true` 时：目录报 `interaction_enabled=true`，而发送门要求
  两者同时成立，拒绝。

这正是 #706 在 `botCardConfigResponse` 里刻意消灭的「清单说行、发送说 400」矛盾，被搬到了
owner 界面上。brief 当时写明 `AllowsRaw*` 是「发送门、编辑门与 profile 清单三处共用的唯一
判定」——owner 目录是第四个消费方，而它什么都没组合。

**P2-5 · `PUT` 拒绝 `null`，而读接口对未配置项恰恰下发 `null`。**
`normalizeBotSettingValue` 把 `null` 判为非法，但 `GET` 对每个未设置的键都返回
`"value": null`，且 `botSettingUpdateReq` 用 `json.RawMessage` 的理由写的就是支持
「读目录 → 改一项 → 写回」。整份回灌必然带上未改动的 `null` 项，每一项都校验失败，而批量
是全有或全无，于是整个写入 400。同时也没有任何办法用 `PUT` 表达「删除这个覆盖」——只能
逐键 `DELETE`，尽管 `null` 正是读侧已经在用的那个形状。
现有的 `TestBotSettings_CatalogValueRoundTrips` 抓不到：它只回灌过**非 null** 值。

**P2-3 · `updateBotSettings` 的 `BindJSON` 与空 `items` 检查早于属主校验。**
逐项校验确实在属主门之后（#706 round-2/3 的修复），但两处整体检查在它之前。对他人或不存在
的 Bot 发 `{}` 或 `{"items":[]}` 会得到 400，而不是端点自述的 403/404。
两条独立评审腿各自命中同一处，是那轮置信度最高的发现。注意评审同时**驳回**了「信息泄露」
的定性：这个 400 无论目标存在与否、属主是谁都字节相同，泄露的比刻意的 403/404 拆分更少。
它是三轮评审推动的那个排序里最后一个洞，不是安全问题。

**P2-2 · profile 新失败模式的线路状态是 400，真实 500 在 body 里。**
`ResponseErrorL` 把传输状态钉死 400（D14 兼容），真实状态只在 `error.http_status`。#706 的
说明写着「producers must retry rather than read an error as capability off」——但按线路状态
分支的消费方看到的是 4xx，即「你的请求有问题，别重试」，与要求相反。这个端点在 #706 之前
根本不会失败，对 `openclaw-channel-octo` 是全新的失败面。

**P2-4 · 全局层是 best-effort，而一个能力开关现在骑在它上面。**
`EnsureSystemSettings` 容忍启动 `Load()` 失败并留下空快照，而空快照与「未配置」在
`SettingBoolOK` 里不可区分——某副本启动抖一下，就会落到代码默认 `true`，把运维想关的能力
开着，最长一个 reload TTL（60s），且期间与同伴副本结论不一致。
这在 `SettingBoolOK` 的注释和 brief 里都披露了，执行层是 fail-closed 的环境总闸。问题是
**运维不读 Go 注释**：`system_setting_schema.go` 里那三个 `botcard` 键的描述只提了总闸支配，
没提传播与 fail-open 窗口。

## Load-bearing list

- **三态返回（`value` / `effective_value` / `source`）是 owner UI「恢复默认」的前提。**
  P2-1 的修法**不得**把三者折叠或让 `value` 失去「未设置=null」的语义——否则会破坏
  #706 的核心契约。只能动 `effective_value` 的算法，或新增字段并给旧字段改名。
- **`AllowsRawDisplayCard` / `AllowsRawInteractiveCard` 是 raw 卡两档的唯一判定**，
  发送门、编辑门、profile 清单三处共用。owner 目录若要投影，必须经它们，绝不能再写一份
  布尔组合——#706 round-2 的 P1 就是这么来的。
- **`bot.card_enabled` 是派生只读键**（`source: "env"`, `editable: false`）。任何投影方案
  都必须保持它可被客户端单独读到，否则 UI 失去置灰依据。
- **`normalizeBotSettingValue` 同时是写入白名单的一部分**：放宽 `null` 会改变「批量全有或
  全无」的语义边界，需要明确 `null` 是 no-op 还是「删除覆盖」，两者对 `DELETE` 端点的定位
  影响不同。
- **`assertRobotOwner` 的 404/403 拆分是 brief 明文契约**，与 `mention_pref` 端点共享。
  上移它会让形状错误多付一次 DB 查询——这个代价 #706 的注释已经接受过一次。
- **`ResponseErrorL` vs `ResponseErrorLWithStatus` 的选择是全仓 D14 约定**。改 profile 那
  一处会让它与 `bot_api` 其余内部错误不一致；不改则需要把「按 `error.code` 判重试」写进
  契约。两条路都要，不能默认。

## Out of scope

- **鉴权改动**：`stream/end` 归属、`allowSendToChannel` 群分支谓词 →
  `robot-stream-authz-symmetry`。
- **App Bot 支持**。App Bot 在 `app_bot` 表、没有 `robot` 行，`assertRobotOwner` 读 `robot`，
  因此 `PUT /v1/robot/<appBotUID>/settings` 永远 404，无法为其写任何覆盖。不是回归（改动前
  也没有 per-Bot 开关），全局层对它仍生效。**是否支持是产品决定，不是本任务的实现细节**，
  需先有结论才能立项。
- **字符串 `type` 在 `bot_api`/`message` ingress 上绕过卡片谓词**：跨模块、且严重度取决于
  仓外事实。
- **`AdvertisedRef` 第二次循环冗余**、`send.go` 中英混排注释：纯整洁度，随手可做但不值得
  单开任务。
- **`notifyBotSettingChanged` 缺 context 超时**、**deleteBotSetting 零行也推事件**：
  防御纵深/微优化，非契约问题。

## Acceptance

- **P2-1**：总闸关闭时，`GET /v1/robot/:id/settings` 与 `GET /v1/bot/card/profile` 对同一个
  Bot 的同一能力**不再给出相反答案**。测试同时断言两个端点，缺一条就抓不到矛盾本身。
  若采用「新增字段 + 改名」而非直接投影，则契约文档必须写明 UI 该读哪个。
- **P2-5**：把读接口返回的完整 `list` 原样 `PUT` 回去成功（含未配置项的 `null`），且
  `null` 的语义（no-op 或删除覆盖）在代码注释与契约里一致。现有 round-trip 测试扩展为
  回灌**含 null 的整份目录**，而不只是单个非 null 值。
- **P2-3**：对他人 / 不存在的 Bot 发 `{}` 与 `{"items":[]}` 分别得到 403 / 404。
  `TestBotSettings_OwnershipGuard` 增加这两个子用例（现有子用例只覆盖了「格式正确但 key
  非法」）。
- **P2-2**：profile 的重试契约有明确结论并落地——要么改用 `ResponseErrorLWithStatus`，
  要么在客户端契约文档里写明「按 `error.code == err.shared.internal` 判重试，不要按 HTTP
  状态码」。两者都需要 adapter 侧确认。
- **P2-4**：`system_setting_schema.go` 中三个 `botcard` 键的 `Description` 补上传播窗口与
  「要真正关闭请用环境总闸或 per-Bot 覆盖」的运维指引。
- `go test -race -shuffle=on ./modules/robot/ ./modules/bot_api/ ./modules/common/` 通过。
- `go vet ./...`、两个 i18n gate、两个 `NoLegacyResponseError` guard 通过。
- 每条修复用「只回退该修复、确认其测试失败」验证过。

## Open questions（需人确认）

1. **P2-1 取哪种修法？** (a) `effective_value` 直接经 `botCardConfigFrom` 的投影——UI 不用
   改，但「这一层的解析结果」这个语义丢失；(b) 保留现字段、新增 `enforced_value`——语义更
   准，但客户端要改。已在客户端对接说明里让 UI 自行 AND 作为过渡，两种方案都不会让它出错。
2. **`null` 是 no-op 还是删除覆盖？** 后者更符合读侧形状，但会让 `DELETE` 端点变成冗余。
3. **profile 的重试契约**：改 wire status（与 `bot_api` 其余错误不一致）还是写文档
   （依赖消费方正确实现）？需要 `openclaw-channel-octo` 侧一起确认。
