# Summary-Notify 通知卡片（`internal/carddispatch` 首个生产者）

> **权威声明**：本文档描述 `summary-notify` 生产者当前的卡片形态与组装规则。
> 权威源码：`pkg/cardtmpl/resource.go`（AC JSON 组装）、`modules/notify/card.go`
> （文案 / attribution / excerpt / 文本降级）、`internal/carddispatch/`（派发流水线）、
> `pkg/cardmsg`（wire 协议与校验）。规范源头：
> `.octospec/tasks/card-message-internal-dispatch/brief.md`（Decisions 1/3/4/11/14）与
> 同目录 `summary-notify-contract.md`（跨仓 ingress 契约）。两者如有出入，以代码为准。
>
> 相关 PR：#577（dispatch 基座）、#579（`summary-notify` pilot）、#580（复用
> `notification` bot 身份）。

## 1. 概览

`summary-notify` 是 `internal/carddispatch` 落地的**第一个**内部卡片生产者。业务侧
（`octo-smart-summary` worker）任务终态后经**既有** `POST /v1/internal/notify` 内网
接口投递**结构化字段**（`NotifyReq.Card` = `SummaryCardFields`），octo-server 侧完成
**卡片组装 → 派发 → 降级**闭环：

```
smart-summary worker
   │  POST /v1/internal/notify  (X-Internal-Token, 一请求一收件人)
   ▼
modules/notify.deliverCardNotification
   │  dedup → actor 排除 → live member 校验 → readiness 检查
   ▼
cardtmpl.BuildSummaryResourceCard        （组装 AC JSON，deep-link 由 External 决定）
   │
   ▼
carddispatch.Sender.Send                 （producer-bound；DM/octo/v1/system-notification）
   │
   ▼  单次 SendMessageWithResult 送 WuKongIM，from_uid = notification User Bot
   (卡片建卡/校验/派发失败 → 整请求降级为纯文本 DM，仍走同一 notification 身份)
```

- **发送方身份**：复用已有 `notification` User Bot（`ensureNotifyBot` provision 的
  `user`+`app`+`robot.status=1`）；卡片与文本降级同一 DM 会话，无独立 summary Bot（#580）。
- **接口层不变**：仍是 `POST /v1/internal/notify` + `X-Internal-Token`，body 里
  `Payload` 与 `Card` **二选一**。Decision 14：`Payload` 若被 `cardmsg.IsCardPayload`
  判为卡片则 400 拒绝——「凡卡片必须走结构化 `Card`」是硬性契约。
- **生产者信息**：`ProducerID="summary-notify"`，`SenderUID=NotifyBotUIDValue`，
  `AllowedChannelTypes=[Person]`，`AllowedProfiles=[octo/v1]`，
  `SpacePolicy=SystemNotification`，`MaxInFlight=20/process`。

## 2. 卡片形态（Adaptive Card 1.5）

`summary-notify` 使用 `pkg/cardtmpl` 的 `ResourceCard` 家族（`octo/v1` 展示白名单
子集），两种 `Kind`：

| `Kind` | 场景 | attribution | excerpt | FactSet |
|---|---|---|---|---|
| `"completed"` | 总结生成成功 | `labels.completedBanner` | 空 | 有效字段 |
| `"failed"` | 生成失败 | `labels.failedBanner` | `labels.failedPrefix + Reason` | 有效字段（通常为空） |

**公共结构**：
- Header：无 `IconURL` 时是 `Container`，含 `TextBlock(Title, weight=Bolder)` 与
  可选的 `TextBlock(Attribution, isSubtle, spacing=None)`；有 icon 时用
  `ColumnSet(auto Image + stretch title stack)`。`summary-notify` 目前不设置 icon。
- Excerpt：可选 `TextBlock(wrap=true)`，超过 `MaxExcerptRunes=300` 时截断加省略号。
- FactSet：仅当 `TimeRange`/`Members>0`/`MsgCount>0`/`GeneratedAt` 至少一个非空时出现。
- ActionSet：至少 `Action.OpenUrl("查看详情", deepLink)`；可选 `Action.CopyToClipboard`
  （`summary-notify` 未使用）。
- **入卡文本一律经 `escapeMarkdown` 转义**（`* _ [ ] ( ) < > \` \# ~ |` 逐字符加
  反斜杠），阻断外部字段中的 markdown 链接/图片/自动链接被 CommonMark 解析器解为
  可点击链接（否则会额外过 URL allowlist 或走渲染面）。

### 2.1 completed 完整 AC JSON

字段来源：见跨仓契约 `summary-notify-contract.md`。示例入参：

```jsonc
{
  "space_id":  "spc_xxx",
  "service":   "summary-service",
  "targets":   ["uid_recipient"],
  "actor_uid": "",
  "card": {
    "task_no":     "TN_20260713_abcd",
    "kind":        "completed",
    "title":       "产品周会纪要",
    "time_range":  "2026-07-06 10:00 ~ 2026-07-13 10:00",
    "members":     5,
    "msg_count":   128,
    "generated_at":"2026-07-13 15:04",
    "reason":      ""
  }
}
```

服务端组装的 `card` 文档（AC 1.5，`profile=octo/v1`，`card_version="1.5"`）：

```json
{
  "type": "AdaptiveCard",
  "version": "1.5",
  "metadata": {
    "webUrl": "https://im.example.com/s/TN_20260713_abcd?sp=spc_xxx",
    "octo": {
      "variant": "summary.completed",
      "source": { "label": "智能总结" }
    }
  },
  "body": [
    { "type": "Container", "items": [
      { "type": "TextBlock", "text": "产品周会纪要", "weight": "Bolder", "wrap": true },
      { "type": "TextBlock", "text": "总结已生成完成", "isSubtle": true, "spacing": "None", "wrap": true }
    ]},
    { "type": "FactSet", "facts": [
      { "title": "时间范围", "value": "2026-07-06 10:00 ~ 2026-07-13 10:00" },
      { "title": "参与成员", "value": "5 人" },
      { "title": "消息数量", "value": "128 条" },
      { "title": "生成时间", "value": "2026-07-13 15:04" }
    ]},
    { "type": "ActionSet", "actions": [
      { "type": "Action.OpenUrl", "title": "查看详情",
        "url": "https://im.example.com/s/TN_20260713_abcd?sp=spc_xxx" }
    ]}
  ]
}
```

发送到 WuKongIM 时的完整 wire 信封（`plain` 由服务端权威派生，见 `card-protocol.md` §5）：

```jsonc
{
  "type": 17,
  "card_version": "1.5",
  "profile": "octo/v1",
  "space_id": "spc_xxx",       // 由 carddispatch 权威注入
  "plain":    "产品周会纪要\n总结已生成完成\n时间范围: 2026-07-06 10:00 ~ ...\n参与成员: 5 人\n...",
  "card":     { ... /* 上方 body */ }
}
```

渲染效果（示意）：

```
┌───────────────────────────────────────────────┐
│ 产品周会纪要                                    │
│ 总结已生成完成                                  │
│                                                │
│ 时间范围   2026-07-06 10:00 ~ 2026-07-13 10:00 │
│ 参与成员   5 人                                 │
│ 消息数量   128 条                               │
│ 生成时间   2026-07-13 15:04                     │
│                                                │
│ [ 查看详情 ]                                    │
└───────────────────────────────────────────────┘
```

### 2.2 failed 完整 AC JSON

入参（失败一般只有 title + reason，其它字段空/零 → 服务端省略）：

```jsonc
{
  "space_id":  "spc_xxx",
  "service":   "summary-service",
  "targets":   ["uid_recipient"],
  "card": {
    "task_no": "TN_20260713_abcd",
    "kind":    "failed",
    "title":   "产品周会纪要",
    "reason":  "upstream LLM 5xx"
  }
}
```

服务端组装：

```json
{
  "type": "AdaptiveCard",
  "version": "1.5",
  "metadata": {
    "webUrl": "https://im.example.com/s/TN_20260713_abcd?sp=spc_xxx",
    "octo": {
      "variant": "summary.failed",
      "source": { "label": "智能总结" }
    }
  },
  "body": [
    { "type": "Container", "items": [
      { "type": "TextBlock", "text": "产品周会纪要", "weight": "Bolder", "wrap": true },
      { "type": "TextBlock", "text": "总结生成失败", "isSubtle": true, "spacing": "None", "wrap": true }
    ]},
    { "type": "TextBlock", "text": "失败原因：upstream LLM 5xx", "wrap": true },
    { "type": "ActionSet", "actions": [
      { "type": "Action.OpenUrl", "title": "查看详情",
        "url": "https://im.example.com/s/TN_20260713_abcd?sp=spc_xxx" }
    ]}
  ]
}
```

渲染效果（示意）：

```
┌───────────────────────────────────────────────┐
│ 产品周会纪要                                    │
│ 总结生成失败                                    │
│                                                │
│ 失败原因：upstream LLM 5xx                      │
│                                                │
│ [ 查看详情 ]                                    │
└───────────────────────────────────────────────┘
```

### 2.3 AC `metadata`（跨 producer 共享的约定）

每张卡都在 AdaptiveCard 根上带上标准 AC 1.5 `metadata` 对象，内嵌 **保留命名空间**
`octo` 供渲染层区分卡片家族／样式、显示来源角标。字段稳定、低基数，值由服务端生成：

```json
"metadata": {
  "webUrl": "<deep-link>",              // AC 1.5 标准字段；与主 Action.OpenUrl.url 同源
  "octo": {
    "variant": "<family>.<kind>",       // 卡片家族+种类标识，见下表
    "source":  { "label": "<来源名>", "iconUrl": "<可选 https 角标图标>" }
  }
}
```

- **`variant`** —— 稳定标识，渲染端**可以但不必**读；未识别值等同「泛用卡片」。
- **`source`** —— 来源能力，供渲染端显示「来自 XX」chip / 角标。`label` 由 producer
  **按出站语言**传入（`i18n.OutboundLanguage` → `summaryLabelsFor`/`docsLabelsFor` 的
  `sourceLabel`），与卡片其它文案同一套 i18n；`iconUrl` 可选（当前 pilot 只填 `label`）。
  模板**不自动**把 `source` 渲进卡体——是否显示、以何形态显示（chip / 前缀 / 静默）由
  渲染端决定，机器消费方则已拿到来源信号。

`variant` / `source.label` 保留词表（`{producer_family}.{kind}`，只增不改）：

| producer | Kind | variant | `source.label`（zh-CN / en-US） |
|---|---|---|---|
| `summary-notify` | `completed` | `summary.completed` | 智能总结 / Smart Summary |
| `summary-notify` | `failed` | `summary.failed` | 智能总结 / Smart Summary |
| `docs-notify` | `shared` / `commented` / `access_requested` | `docs.shared` / `docs.commented` / `docs.access_requested` | 文档 / Docs |
| 后续新增 producer | 自定 kind | `<family>.<kind>` | 该 producer 的 i18n `sourceLabel` |

产者永远不塞用户输入到 `metadata`（只塞服务端选定的 `variant`/`source` + 服务端拼的
`webUrl`），避免绕开主体 URL/markdown 白名单（`pkg/cardmsg.Validate` 不解析
`metadata` 子字段——正因如此，`metadata` 里没有任何调用方可控的输入面）。
`source.iconUrl` 若设置，`cardtmpl` 会走 `requireHTTPS` 正向校验。

## 3. Ingress 契约（`POST /v1/internal/notify`）

完整跨仓契约见 `.octospec/tasks/card-message-internal-dispatch/summary-notify-contract.md`
（本仓草稿，PR 打开后同步到 `Mininglamp-OSS/octo-smart-summary` 的
issue/comment）。摘要：

- 头 `X-Internal-Token`（`SUMMARY_NOTIFY_TOKEN` / `NOTIFY_INTERNAL_TOKEN`；fail-closed）。
- 一请求一收件人（`targets` 单元素），smart-summary 侧
  claim / markSent / markFailed / Sweep 状态机与 `space_id` / `service` / `actor_uid`
  语义不变。
- **`Payload` 与 `Card` 二选一**：文本仍走 `Payload{type:1,content:...}`；卡片填
  `Card`。两者同时缺失 → 400；同时存在 → 400（`err.server.notify.card_invalid`）。
- **`Payload` 里禁止手搓 type-17**：`cardmsg.IsCardPayload` 匹配即 400（Decision 14）。
- 响应仍为 `NotifyResp{delivered:[], filtered:{uid:reason}}`：卡片派发 uid 进
  `delivered`，`target_denied` / `dispatch_failed` / `busy` / 建卡失败 → uid 进
  `filtered`。smart-summary 现有「只认 `delivered[]` 判真送达 + 非成员进 `filtered`
  重试」的逻辑一行不用改。

## 4. 服务端组装管线（关键代码路径）

1. **Ingress 校验**：`modules/notify/api.go` bind → `Payload` / `Card` 互斥 → 若 `Payload`
   带 type-17 shape → 400（Decision 14）。
2. **业务组装**：`modules/notify/card.go` `deliverCardNotification`：
   - dedup → actor 排除；
   - `memberCache.verify(space_id, targets)`：非成员 → `Filtered`；
   - `ensureNotifyBotReady()`：readiness 未就绪 → 请求级失败（调用方重试）；
   - `buildSummaryCard`：按 `Kind` 组装 attribution / excerpt / facts，交给
     `cardtmpl.BuildSummaryResourceCard`；
   - `carddispatch.Sender.Send(ctx, Target{DM}, Card{octo/v1, document})` 逐收件人
     并发（内嵌 `sem` 20 与 producer `MaxInFlight` 20 一致）。
3. **派发管线**（`internal/carddispatch`，Decisions 7/8/11）：live bot identity →
   live Space/target ACL → envelope 组装（`type=17`, `card_version="1.5"`,
   `profile=octo/v1`, 服务端 `space_id`, 服务端 `plain`）→ `cardmsg.Validate` →
   `cardmsg.Finalize` → `cardmsg.RecheckPayloadSize` → **一次** `SendMessageWithResult`。
4. **文本降级**：`canCard` 在**请求级**决策；`cardmsg.Enabled()==false` /
   sender 未注入 / `buildSummaryCard` 失败（含非 https deep-link 前置） → 整请求走
   `sendSummaryText`（`config.NewPersonalMsgSendReq`，`from_uid=NotifyBotUIDValue`，
   `payload={type:1,content:buildSummaryFallbackText(...)}`）。文本兜底文案见
   `buildSummaryFallbackText`：completed / failed 分别按 headline + 键值行拼装。

## 5. 文案与 i18n（`summaryLabelsFor`）

`i18n.OutboundLanguage(ctx)` 决定；目前 `deliverCardNotification` 用
`context.Background()`（deployment 默认外语），与邮件模板 / botfather 出站文案同纪律。
后续如按收件人语言分发，改为按 uid resolve 即可，模板层无需改动。

| Key | zh-CN（默认） | en-US |
|---|---|---|
| `completedBanner` | 总结已生成完成 | Summary ready |
| `failedBanner` | 总结生成失败 | Summary failed |
| `timeRange` | 时间范围 | Time range |
| `members` / `membersValue` | 参与成员 / `%d 人` | Participants / `%d` |
| `msgCount` / `msgCountValue` | 消息数量 / `%d 条` | Messages / `%d` |
| `generatedAt` | 生成时间 | Generated at |
| `failedPrefix` | 失败原因： | Reason:  |
| `completedHeadline`（fallback text） | 你的总结「%s」已生成完成。 | Your summary "%s" is ready. |
| `failedHeadline`（fallback text） | 你的总结「%s」生成失败。 | Your summary "%s" failed to generate. |

按钮文案由 `pkg/cardtmpl` 的 `labelsForLanguage` 独立提供：`viewDetails=查看详情/View
details`、`copy=复制/Copy`（`summary-notify` 不使用后者）。

## 6. Deep-link（`/s/{task_no}?sp={space_id}`）

`cardtmpl.summaryDeepLink`：从 `External.WebLoginURL` 取 origin（`scheme://host`），
拼 `/s/` + `PathEscape(task_no)` + `?sp=` + `QueryEscape(space_id)`。origin 必须是
**绝对 https**，否则 `BuildSummaryResourceCard` 返回错误 → 请求级降级为纯文本 DM。

**前置**（enablement 门 (1)，见 brief）：**octo-web 需注册 `/s/:taskId` 独立浏览器
路由**，行为镜像 `/d/:docId`（冷加载 → 登录跳转 → 多会话 sid 恢复，XIN-398 测试套）；
登录态命中现有 `WKApp.openSummaryDetail(taskId)`。**目前 octo-web `main` 尚未上线该路由**
（本仓 handoff `P2 gating tests` 未收敛），因此生产环境「查看详情」按钮仍会 404。
smart-summary 切换 `Card` 前需要 octo-web 该路由与本仓 pilot 一起启用。

## 7. 降级与错误分类

| 触发 | 结果 | 收件人级别处理 |
|---|---|---|
| `cardmsg.Enabled() == false` | 请求级 → 走文本 | delivered 记文本送达 |
| `n.cardSender == nil`（producer 未注入） | 请求级 → 走文本 | 同上 |
| `buildSummaryCard` 失败（如非 https base URL） | 请求级 → 走文本；`Warn` 日志 | 同上 |
| `ensureNotifyBotReady()` 未就绪 | **请求级失败**（500 类）| 调用方重试 |
| 收件人非 Space 成员 | `Filtered[uid]=<memberCache 原因>` | smart-summary 现有重试逻辑 |
| `carddispatch` 返回 `target_denied` | `Filtered[uid]="target_denied"` | 同上 |
| `carddispatch` 返回 `busy` | `Filtered[uid]="busy"` | 同上 |
| `carddispatch` 返回 `dispatch_failed` | `Filtered[uid]="dispatch_failed"` | 同上 |
| 文本降级路径 `SendMessage` 失败 | `Filtered[uid]="send_failed"` | 同上 |

Decision 8：dispatcher 单次 `SendMessageWithResult`，不隐式重试；transport-ambiguous
的重复由调用方接受（brief pilot table，2026-07-13 确认）。

## 8. 已知调优候选（open，可讨论后再改）

以下点是渲染 / 文案层面的**可选**改进；不改也满足契约。改动落在 `modules/notify/card.go`
的 `buildSummaryCard` / `summaryLabelsFor`，不涉及 `pkg/cardtmpl` 或 wire 契约。

1. **失败状态视觉区分**：目前 `failedBanner` 与 `completedBanner` 同用
   `isSubtle:true` 灰色文本，仅靠文案区分。可给 failed 的 attribution `TextBlock`
   增加 `color:"Attention"`（AC 1.5 内置枚举；`pkg/cardmsg/validate.go` TextBlock
   分支未对 `color` 做 allowlist，能过校验；三端自研 renderer 需实现）。示例：

   ```json
   { "type":"TextBlock","text":"总结生成失败",
     "isSubtle":true,"color":"Attention","spacing":"None","wrap":true }
   ```

2. **失败原因改用 FactSet 行**：把「失败原因：<reason>」从独立 `TextBlock` excerpt
   改为 `FactSet` 一行（`title="失败原因"`，`value=Reason`），视觉与 completed 卡的
   FactSet 一致。`escapeMarkdown` 已作用于 Fact.value，安全性不变。取舍：excerpt 换行
   自然、易读；FactSet 更规整、但长 reason 挤压 value 列。

3. **完成状态强调**：给 `completedBanner` 加 `color:"Good"` 也可行（AC 1.5 语义色），
   与失败态 `Attention` 对称。同样只需 renderer 支持。

三条都是**渲染层小改**，不涉及 `pkg/cardmsg` allowlist、wire 契约、ingress 协议。
在 octo-web `/s/:taskId` 落地前可以并行讨论、单独提 PR。

## 9. 参考

- 代码
  - `pkg/cardtmpl/resource.go` — 模板与 deep-link 组装、markdown 转义、rune 截断
  - `modules/notify/card.go` — Kind → attribution/excerpt/facts 映射、文本降级
  - `modules/notify/model.go` — `SummaryCardFields` 结构（跨仓契约字段）
  - `internal/carddispatch/` — dispatch pipeline（Decisions 3/4/7/8）
  - `pkg/cardmsg/` — wire 协议、`Validate` / `Finalize` / `RecheckPayloadSize`
- 契约与规范
  - `.octospec/tasks/card-message-internal-dispatch/brief.md`
  - `.octospec/tasks/card-message-internal-dispatch/summary-notify-contract.md`
  - `.octospec/tasks/card-message-internal-dispatch/handoff.md`
  - `docs/card-protocol.md` — wire 协议权威（§1 信封、§2 白名单、§5 plain 派生）
- 相关 PR：#577（dispatch 基座）、#579（`summary-notify` pilot）、#580（复用
  `notification` bot 身份）
