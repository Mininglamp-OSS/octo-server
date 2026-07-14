# docs-notify 卡片通知 — 跨仓 ingress 契约

> 仓内契约，供 octo-server `docs-notify` 生产者与 `octo-docs-backend` 对齐,PR
> 打开后整理成英文 issue/comment 同步给 `Mininglamp-OSS/octo-docs-backend`。
> 关联:brief.md(本目录)、summary-notify-contract.md(姊妹契约)、
> handoff.md、`docs/docs-notify-card.md`。
> 状态:契约草案,由本 PR 落地 `pkg/cardtmpl` / `modules/notify` 侧。
> 最后更新 2026-07-14。

## 背景一句话

`docs-notify` 是 `internal/carddispatch` 的**第二个**生产者（首个见
`summary-notify-contract.md`）,把 docs-backend 想推给用户的**自动化系统通知**
（分享、评论、访问申请）升级为 `octo/v1` 展示卡片,复用 `notification` User Bot
身份 → 用户会话列表**不新增**系统 Bot 会话。docs-backend 不构造 type-17 map、
不发文本兜底、不做卡片模板 —— 只发**结构化字段** `DocsCardFields`,一切文案 /
布局 / deep-link / i18n 由 octo-server owning。

**用户主动分享** 不在本契约内 —— 那是独立的用户认证 + 服务端 mint 通路
（见 `.octospec/tasks/user-resource-share-card/brief.md`）,发送方为分享用户本人。

## 契约要点（锁定项）

1. **同端点、同 token、同粒度**:仍是 `POST /v1/internal/notify`,头
   `X-Internal-Token`（复用 `NOTIFY_INTERNAL_TOKEN`;fail-closed）,**一请求
   一收件人**（`targets` 单元素）。`space_id` / `service` / `actor_uid` 语义
   与 summary 完全相同,octo-server 已有的 dedup / actor 排除 / memberCache
   / ensureNotifyBotReady / carddispatch 派发全部复用。
2. **三选一互斥**:`Payload` / `Card`（summary） / `DocsCard`（docs）**只能
   出现一个**。多于一个 → 400 `err.server.notify.card_invalid`;全都缺失
   → 400 legacy「payload不能为空」。字段级检测:
   - `req.Payload != nil` 计入(包括空 `{}` —— 呼应「显式意图」)。
   - `req.Card != nil` 计入。
   - `req.DocsCard != nil` 计入。
3. **禁止客户端手搓 type-17 map**:Decision 14 仍生效 —— `payload` 若被
   `cardmsg.IsCardPayload` 判为卡片(`type=17`)一律 400
   `err.server.notify.card_not_allowed`,无论走 `Card` 还是 `DocsCard`。
4. **响应契约不变**:仍返回 `NotifyResp{delivered:[], filtered:{uid:reason}}`。
   reason 词表与 summary 一致(`not_space_member` / `target_denied`
   / `dispatch_failed` / `busy` / `send_failed`) —— docs-backend 可**照抄**
   smart-summary 的 dedup / retry / sweep 状态机。
5. **模板/文案/链接归属 octo-server**:docs-backend **只发原始字段**;卡片布局、
   按钮文案(查看详情)、FactSet 标签(操作人 / 时间)、attribution
   (「Alice 分享了文档」)、`/d/{doc_id}?sp={space_id}` deep-link、
   `metadata.octo.variant` / `metadata.octo.source` 全部由 octo-server
   `pkg/cardtmpl` + `modules/notify.buildDocsCard` + `i18n.OutboundLanguage`
   生成。docs-backend 不再拼「XX 分享了 <link>」这类降级文本(旧字段作废;
   octo-server 侧的 `buildDocsFallbackText` 负责纯文本降级)。
6. **批量端点 `/notify/batch` 拒绝 `DocsCard`**:与 `Card` 同,卡片必须走单条端点
   `/notify`;batch 一条含 `DocsCard` 即 400,不做任何投递。

## 请求体（卡片模式）

```jsonc
POST /v1/internal/notify
X-Internal-Token: <NOTIFY_INTERNAL_TOKEN>
{
  "space_id":  "spc_xxx",              // 现状:必填,server 用 memberCache 校验收件人
  "service":   "docs-service",         // 现状不变
  "targets":   ["uid_recipient"],      // 现状:单收件人
  "actor_uid": "",                     // 现状:通知场景一般留空
  "docs_card": {                       // 新增:非空即走 docs-notify 卡片 producer
    "doc_id":     "d_20260713_abcd",   // ★ 见下「标识」
    "kind":       "shared",            // "shared" | "commented" | "access_requested"
    "title":      "产品设计方案",         // 原始标题(server 负责转义/截断)
    "actor_name": "Alice",             // 预格式化的操作人显示名;空则用「有人」/「Someone」兜底
    "excerpt":    "Q3 上线计划已确认",    // 可选预览/评论/申请说明;≤ 300 runes 截断
    "updated_at": "2026-07-13 15:04"   // 已格式化的时间字符串;空则省略「时间」行
  }
}
```

`payload` / `card` / `docs_card` 三者同时缺失 → 400 legacy;多于一个 → 400
`err.server.notify.card_invalid`。

## 三个约定（对齐 summary-notify）

- **标识 `doc_id`(不是自增 `id`)**:deep-link `/d/{doc_id}?sp={space_id}` 用
  docs-backend 侧不可枚举的文档标识 —— 与 octo-web `/d/:docId` 独立路由
  (已在线;冷加载 + 登录跳转 + XIN-398 多会话 sid 恢复)对齐。**这是 docs-notify
  与 summary-notify 的关键差异:docs 侧「查看详情」按钮开箱可用**,
  summary 侧仍在等 `/s/:taskId` 上线。
- **时间字段传「已格式化字符串」**:`updated_at` 由 docs-backend 用其自身时区
  格式化后传字符串,octo-server 原样填进 FactSet 值。理由:octo-server 无 docs
  业务时区配置,避免时区漂移。**标签(「时间」/「At」)仍由 octo-server 按收件
  人语言本地化。**
- **`actor_name` 传预格式化的显示名**:显示名由 docs-backend 侧解析(可含姓+
  称谓 / 组织信息 / 匿名替换等业务规则);octo-server 侧 `escapeMarkdown` 后
  嵌入 attribution。理由:避免 card 路径依赖额外 DB 查询,一致性与 summary
  的 `Title` 同一纪律。

## 字段来源（docs-backend 侧现成 / 需要新增）

| card 字段 | docs-backend 来源(建议) |
| --- | --- |
| `doc_id` | 文档实体的 `id` 字段(与 `/d/:docId` 路由 taskId 同义) |
| `kind` | 触发场景:分享 = `shared`;评论 = `commented`;访问申请 = `access_requested` |
| `title` | 文档 `title` 字段(空则用「无标题」兜底 —— docs-backend 侧决策) |
| `actor_name` | 触发者的显示名(docs-backend 已 resolve;匿名场景可留空) |
| `excerpt` | 分享:留空 / 简短介绍;评论:评论内容摘要;访问申请:申请理由 |
| `updated_at` | 触发事件的时间戳格式化字符串(docs-backend 时区) |

## octo-server 侧（本 PR 落地）

- `docs-notify` producer 绑定已有 `notification` Bot(`user`+`app`+
  `robot.status=1`),卡片和纯文本降级复用其 provisioning/readiness;
  不新增专用 `docs` Bot。
- 注册 `docs-notify` producer(DM/`octo/v1`/system-notification/MaxInFlight
  20),经 `SenderFromContext` 注入 `modules/notify`。与 `summary-notify` 同
  `ProducerSpec` shape,仅 ID 不同(见 `main.cardDispatchProducerSpecs`)。
- `docs_card` 分支:`memberCache.verify` → `cardtmpl.BuildDocsResourceCard` →
  每个成员 `sender.Send` → 汇总 `delivered/filtered`。**建卡/配置(如
  deep-link 非 https)失败降级为原纯文本 DM**,保证通知必达(此为 octo-server
  侧策略,brief 未强制,PR 说明)。
- **metadata 预留**:每张卡在 AdaptiveCard 根塞 `metadata.webUrl` +
  `metadata.octo = {variant, source}`;`variant` 保留词表见
  `docs/summary-notify-card.md` §2.3;`source.label` 供渲染端做「来自文档」
  角标 —— docs-backend 无需管这些字段,server 侧根据 `kind` 决定 variant,
  根据 producer 决定 source.label。

## enablement 门（端到端上线前必须齐）

1. **octo-web** 上线 `/d/:docId` 路由 —— **已在线**(既有 standalone
   doc 通路);相较 summary,docs 端「查看详情」链接开箱可用;
2. octo-server 落 `cardtmpl` snapshot/Validate 测试 + `docsDeepLink` 形状测试
   + pilote2e E2E(**本 PR 落地**);
3. **octo-docs-backend** 侧新增 outbound IM notify 客户端(仿造
   `octo-smart-summary/internal/notify/`),按上述 body 结构 POST。
   建议实现要点:
   - 每场景一个函数(`NotifyDocShared` / `NotifyDocCommented` /
     `NotifyDocAccessRequested`),内部塞 `Kind` 常量;
   - 复用 `X-Internal-Token`(`NOTIFY_INTERNAL_TOKEN` = 同一 token,
     docs-backend 与 smart-summary 共用);
   - per-recipient dedup(避免同一评论触发多次通知);
   - 消费 `NotifyResp{delivered,filtered}` —— 只认 `delivered[]` 判真送达,
     `filtered` 内标记为 `not_space_member` / `busy` / `dispatch_failed` 的
     可延时重试;
   - **不实现自身文本降级路径** —— server 侧已负责;发失败即上报错误让
     调用方按业务重试(与 smart-summary 侧的 `Sweep` 一致)。

在 (1)(3) 到位前,octo-server 侧行为惰性:无 `docs_card` 请求即无 docs 卡片
流量;全局 `OCTO_CARD_MESSAGE_ENABLED` 为总开关,回滚可移除 producer spec
或关该 env。

## 明确不在本契约内

- 用户主动分享文档到 DM/群/子区 —— 见 `../user-resource-share-card/brief.md`,
  发送方为分享用户本人,非 Bot proxy。
- 访问申请的交互(「同意 / 拒绝」按钮) —— 需要 producer 升级到 `octo/v2` +
  绑定一个跑 `/v1/bot/events` 的 action-owner;docs-backend 目前是
  TS/Hocuspocus,不跑 Go bot 事件轮询,升级为独立议题。
- 群/子区自动通知(如「这份文档被评论了」推到关联群) —— 需要 producer
  widening 到 group/thread channel type,同步需要 per-channel rate rule +
  cluster-wide cap 决策(brief › Industry practice alignment)。
- 文档缩略图 / 富预览(Image / RichTextBlock) —— 目前只用 TextBlock + FactSet
  +  Optional Excerpt。未来若加缩略图,需要 docs-backend 自建 SSRF-safe
  fetcher 并提供绝对 https URL。
