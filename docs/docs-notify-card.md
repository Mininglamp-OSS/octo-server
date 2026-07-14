# Docs-Notify 通知卡片（`internal/carddispatch` 第二个生产者）

> **权威声明**：本文档描述 `docs-notify` 生产者当前的卡片形态与组装规则。
> 权威源码：`pkg/cardtmpl/resource.go`（`BuildDocsResourceCard` + `docsDeepLink`）、
> `modules/notify/card.go`（`deliverDocsCardNotification` / `buildDocsCard` /
> `docsLabelsFor` / `docsAttributionAndVariant`）、`modules/notify/model.go`
> （`DocsCardFields`）、`internal/carddispatch/`（派发流水线）、
> `pkg/cardmsg`（wire 协议与校验）。规范源头：
> `.octospec/tasks/card-message-internal-dispatch/brief.md`（Decisions 1/3/4/11/14）与
> 同目录 `docs-notify-contract.md`（跨仓 ingress 契约）。两者如有出入，以代码为准。
>
> 兄弟文档：`docs/summary-notify-card.md`（`summary-notify` 生产者）— 组装管线、
> 降级链、Deep-link 前置、`metadata.octo.{variant,source}` 保留词表、错误分类**完全一致**，
> 本文档只呈现 docs 特有的字段/文案/deep-link 差异。

## 1. 概览

`docs-notify` 是 `internal/carddispatch` 落地的第二个内部卡片生产者，服务于
**docs-backend 的自动化通知**（分享、评论、访问申请）。业务侧（`octo-docs-backend`）
经既有 `POST /v1/internal/notify` 内网接口投递**结构化字段**
（`NotifyReq.DocsCard` = `DocsCardFields`），octo-server 侧完成
**卡片组装 → 派发 → 降级**闭环。

- **发送方身份**：与 `summary-notify` 共用同一 `notification` User Bot
  （`ensureNotifyBot` provision）；用户会话列表里不产生第二个系统 Bot 会话。**能力
  隔离在 producer 粒度**：不同 producer 各自的 `MaxInFlight` / 允许 profile / 允许
  channel type 独立配置，`docs-backend` 不因共用 Bot 身份而获得跨 producer 越权。
- **接口层**：仍是 `POST /v1/internal/notify` + `X-Internal-Token`。body 里
  `Payload` / `Card`（summary） / `DocsCard`（docs） **三选一**，见 §3。
- **生产者信息**：`ProducerID="docs-notify"`，`SenderUID=NotifyBotUIDValue`，
  `AllowedChannelTypes=[Person]`，`AllowedProfiles=[octo/v1]`，
  `SpacePolicy=SystemNotification`，`MaxInFlight=20/process`。

**用户主动分享** 不是 docs-notify 的范围（brief Out-of-scope）：那是一条独立的
「用户认证 + 服务端 mint」资源分享通路（`.octospec/tasks/user-resource-share-card/`
brief），发送方为分享用户本人，与本 producer 无关。docs-notify 覆盖的是
**自动化系统通知**：文档被分享给你、评论了你、有人请求访问你的文档等。

## 2. 卡片形态（Adaptive Card 1.5）

`DocsCardFields.Kind` 决定 attribution / variant；`ActorName` 决定 attribution
的主语（缺省用「有人」/「Someone」兜底）。

| `Kind` | 场景 | attribution（含 `ActorName`） | attribution（无 `ActorName`） | `metadata.octo.variant` |
|---|---|---|---|---|
| `"shared"` | 文档被分享 | `{Actor} 分享了文档` | `有人分享了文档` | `docs.shared` |
| `"commented"` | 有新评论 | `{Actor} 评论了文档` | `有新评论` | `docs.commented` |
| `"access_requested"` | 访问申请 | `{Actor} 请求访问文档` | `有人请求访问文档` | `docs.access_requested` |

**公共结构**（与 summary 卡完全对齐）：
- Header：无 `IconURL` 时是 `Container` + 加粗 `TextBlock(Title)` + 可选
  `isSubtle` attribution TextBlock。
- Excerpt：可选 `TextBlock(wrap=true)`，超过 `MaxExcerptRunes=300` 截断。
- FactSet：`ActorName`（若非空）→ 「操作人 / By」；`UpdatedAt`（若非空）→ 「时间 /
  At」。两者都空则整块省略。
- ActionSet：`Action.OpenUrl("查看详情", deepLink)`。
- **入卡文本一律经 `escapeMarkdown` 转义**（同 summary，`* _ [ ] ( ) < > \` \# ~ |`
  逐字符加反斜杠），阻断外部字段被 CommonMark 解析成活链接／图片。

### 2.1 shared 完整 AC JSON

示例入参（跨仓契约见 `docs-notify-contract.md`）：

```jsonc
{
  "space_id":  "spc_xxx",
  "service":   "docs-service",
  "targets":   ["uid_recipient"],
  "actor_uid": "",
  "docs_card": {
    "doc_id":     "d_20260713_abcd",
    "kind":       "shared",
    "title":      "产品设计方案",
    "actor_name": "Alice",
    "excerpt":    "Q3 上线计划已确认",
    "updated_at": "2026-07-13 15:04"
  }
}
```

服务端组装（AC 1.5，`profile=octo/v1`，`card_version="1.5"`）：

```json
{
  "type": "AdaptiveCard",
  "version": "1.5",
  "metadata": {
    "webUrl": "https://im.example.com/d/d_20260713_abcd?sp=spc_xxx",
    "octo": {
      "variant": "docs.shared",
      "source": { "label": "文档" }
    }
  },
  "body": [
    { "type": "Container", "items": [
      { "type": "TextBlock", "text": "产品设计方案", "weight": "Bolder", "wrap": true },
      { "type": "TextBlock", "text": "Alice 分享了文档", "isSubtle": true, "spacing": "None", "wrap": true }
    ]},
    { "type": "TextBlock", "text": "Q3 上线计划已确认", "wrap": true },
    { "type": "FactSet", "facts": [
      { "title": "操作人", "value": "Alice" },
      { "title": "时间",   "value": "2026-07-13 15:04" }
    ]},
    { "type": "ActionSet", "actions": [
      { "type": "Action.OpenUrl", "title": "查看详情",
        "url": "https://im.example.com/d/d_20260713_abcd?sp=spc_xxx" }
    ]}
  ]
}
```

WuKongIM wire 信封（`plain` 由服务端权威派生）：

```jsonc
{
  "type": 17,
  "card_version": "1.5",
  "profile": "octo/v1",
  "space_id": "spc_xxx",              // 由 carddispatch 权威注入
  "plain":    "产品设计方案\nAlice 分享了文档\nQ3 上线计划已确认\n操作人: Alice\n时间: 2026-07-13 15:04",
  "card":     { ... /* 上方 body */ }
}
```

渲染效果（示意）：

```
┌───────────────────────────────────────────────┐
│ 产品设计方案                                    │
│ Alice 分享了文档                                │
│                                                │
│ Q3 上线计划已确认                                │
│                                                │
│ 操作人   Alice                                  │
│ 时间     2026-07-13 15:04                       │
│                                                │
│ [ 查看详情 ]                                    │
└───────────────────────────────────────────────┘
```

### 2.2 commented / access_requested 变体

字段与 `shared` 相同、只是 `kind` + attribution / variant 不同。省略 `ActorName`
时 attribution 走匿名兜底：

- `kind: "commented"` + 无 `actor_name` → attribution = `"有新评论"`，
  `metadata.octo.variant = "docs.commented"`
- `kind: "access_requested"` + `actor_name: "Bob"` → attribution =
  `"Bob 请求访问文档"`，`metadata.octo.variant = "docs.access_requested"`

**约定** `docs-backend` 端预格式化 `actor_name`（可含姓+称谓、显示名等，逐字符
`escapeMarkdown` 后落入 attribution）。octo-server 不做二次身份解析（避免 card 路径
额外 DB 依赖）；跨仓契约见 `docs-notify-contract.md` §3。

## 3. Ingress 契约（`POST /v1/internal/notify`）

完整跨仓契约见 `.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md`。
摘要：

- 头 `X-Internal-Token`（同 `NOTIFY_INTERNAL_TOKEN`，与 summary 复用；fail-closed）。
- 一请求一收件人（`targets` 单元素）；`space_id` / `service` / `actor_uid` 语义
  与 summary 完全对齐。
- **三选一**：`Payload` / `Card`（summary） / `DocsCard`（docs） 只能出现一个。
  多于一个 → 400 `err.server.notify.card_invalid`；全都缺失 → 400
  `payload不能为空`（legacy）。
- `Payload` 里禁止手搓 type-17（Decision 14；`cardmsg.IsCardPayload` 匹配即 400
  `err.server.notify.card_not_allowed`）。
- 响应仍为 `NotifyResp{delivered:[], filtered:{uid:reason}}`，reason 词表与
  summary 一致（`target_denied` / `dispatch_failed` / `busy` / `not_space_member`
  / `send_failed`），docs-backend 侧的重试/dedup 逻辑可**照抄** smart-summary。
- **批量端点 `/notify/batch` 拒绝任何 `Card` / `DocsCard` 条目** — 卡片只能走单请求
  路径（Preflight 400），因为 batch 走 legacy 文本 fan-out，不经 carddispatch。

## 4. 服务端组装管线

1. **Ingress 校验**：`modules/notify/api.go` bind → 三选一互斥（`present > 1` →
   `err.server.notify.card_invalid` 400；`payload==nil && card==nil && docs_card==nil &&
   len(payload)==0` → legacy 400）→ Decision 14 gate。
2. **业务组装**：`modules/notify/card.go` `deliverDocsCardNotification`：
   - `Kind` 合法性 → 400 unmapped；
   - dedup → actor 排除；
   - `memberCache.verify(space_id, targets)`；非成员进 `Filtered`；
   - `ensureNotifyBotReady()`（与 summary 共享）；
   - `buildDocsCard` → `docsAttributionAndVariant` → `cardtmpl.BuildDocsResourceCard`；
   - `carddispatch.Sender.Send(ctx, Target{DM}, Card{octo/v1, document})` 并发（内嵌
     `sem` 20 与 producer `MaxInFlight` 20 一致）。
3. **派发管线**（`internal/carddispatch`，Decisions 7/8/11）与 summary **完全相同**。
4. **文本降级**：`canCard` 请求级决策；`cardmsg.Enabled()==false` /
   `docsSender==nil` / `buildDocsCard` 失败（含非 https deep-link 前置） → 整请求走
   `sendSummaryText`（服务端复用同一 API，`from_uid=NotifyBotUIDValue`，
   `payload={type:1,content:buildDocsFallbackText(...)}`）。文本兜底格式：
   `{attribution}\n文档：{Title}\n{Excerpt}\n时间：{UpdatedAt}`（按存在与否行）。

## 5. 文案与 i18n（`docsLabelsFor`）

`i18n.OutboundLanguage(ctx)` 决定。当前 `deliverDocsCardNotification` 用
`context.Background()`（deployment 默认外语），与 summary / 邮件 / botfather 同纪律。

| Key | zh-CN（默认） | en-US |
|---|---|---|
| `sharedBanner` / `sharedBannerAnon` | `%s 分享了文档` / 有人分享了文档 | `%s shared a document` / A document was shared with you |
| `commentedBanner` / `commentedBannerAnon` | `%s 评论了文档` / 有新评论 | `%s commented on a document` / A new comment on a document |
| `accessRequestedBanner` / `accessRequestedBannerAnon` | `%s 请求访问文档` / 有人请求访问文档 | `%s requested access to a document` / Someone requested access to a document |
| `title` | 文档 | Document |
| `actor` | 操作人 | By |
| `updatedAt` | 时间 | At |

按钮文案由 `pkg/cardtmpl.labelsForLanguage` 提供（同 summary）：
`viewDetails=查看详情/View details`、`copy=复制/Copy`（docs-notify 未使用后者）。

## 6. Deep-link（`/d/{doc_id}?sp={space_id}`）

`cardtmpl.docsDeepLink`：从 `External.WebLoginURL` 取 origin（`scheme://host`），拼
`/d/` + `PathEscape(doc_id)` + `?sp=` + `QueryEscape(space_id)`。origin 必须是
**绝对 https**；否则 `BuildDocsResourceCard` 返回错误 → 请求级降级为纯文本 DM。

**前置**：octo-web `/d/:docId` 路由**已在线**（`packages/dmworkbase` 走的是既有
standalone doc 通路，含冷加载 → 登录跳转 → 多会话 sid 恢复的 XIN-398 测试套件）。
这是 docs-notify 与 summary-notify 的关键差异——docs 侧「查看详情」按钮开箱可用；
summary 侧仍在等 octo-web `/s/:taskId` 上线。

## 7. 降级与错误分类

与 summary 完全一致，见 `docs/summary-notify-card.md` §7。reason 词表：
`not_space_member` / `target_denied` / `dispatch_failed` / `busy` / `send_failed`。

## 8. 已知调优候选

同 `docs/summary-notify-card.md` §8（`color:"Attention"` / `color:"Good"` / 让
Excerpt 与 FactSet 位置可调）。多一个 docs 专属候选：

- **访问申请交互按钮**：`access_requested` 变体天生适合「同意 / 拒绝」的交互卡（`octo/v2`），
  但需要 producer 从 `octo/v1` 升级到 `octo/v2` 并绑定一个跑 `/v1/bot/events` 的
  action-owner（brief › 「Interactive cards」小节）。docs-backend 目前是 TS/Hocuspocus，
  不跑 Go bot 事件轮询——如果要走 `octo/v2` 交互，需要单独设计 executor 服务。**这是后续
  独立议题**，不在本 producer 范围。

## 9. 参考

- 代码
  - `pkg/cardtmpl/resource.go` — 模板家族（`BuildSummaryResourceCard` /
    `BuildDocsResourceCard` 共用 `buildResourceCard`）、`metadata` 组装
  - `modules/notify/card.go` — `deliverDocsCardNotification` / `buildDocsCard` /
    `docsAttributionAndVariant` / `docsLabelsFor`
  - `modules/notify/model.go` — `DocsCardFields` + `DocsCardKind*` 常量（跨仓契约字段）
  - `modules/notify/api.go` — 三选一 mutex、docs-notify sender 装配
  - `main.go` — `cardDispatchProducerSpecs`（summary-notify + docs-notify 同 shape）
  - `pilote2e/docs_card_wukongim_test.go` — E2E：POST → 真 WuKongIM → 读回验 type-17
- 契约与规范
  - `.octospec/tasks/card-message-internal-dispatch/brief.md`
  - `.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md`（跨仓）
  - `.octospec/tasks/card-message-internal-dispatch/summary-notify-contract.md`（姊妹）
  - `docs/card-protocol.md` — wire 协议权威
  - `docs/summary-notify-card.md` — 姊妹 producer 文档，共享管线细节
- 相关 PR：#577（dispatch 基座）、#579（`summary-notify` pilot）、#580（复用
  `notification` bot 身份）、本 PR（`docs-notify` producer + `metadata.octo.{variant,source}`）
