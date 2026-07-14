# summary-notify 卡片通知 — 跨仓 ingress 契约(草稿）

> 仓内草稿,供 octo-server P2(`summary-notify` pilot）实现时对齐,PR 打开后整理成英文
> issue/comment 同步给 `Mininglamp-OSS/octo-smart-summary`。
> 关联:brief.md(本目录)、handoff.md、octo-server PR #577(P1 地基)。
> 状态:待 smart-summary owner 确认字段清单。最后更新 2026-07-14。

## 背景一句话

今天 smart-summary worker 在任务终态后 `POST /v1/internal/notify` 发**纯文本 DM**
(`payload:{type:1,content:<整段文案>}`),per-recipient 一请求一收件人,dedup/retry 靠
自己的 `summary_notification` 表 + `Sweep`。P2 把这条 DM **从纯文本升级为 `octo/v1`
展示卡片**,发送方复用现有 `notification` User Bot,经 octo-server
`internal/carddispatch` 边界派发。卡片及其纯文本降级都进入同一个系统通知 DM 会话。
该决定于 2026-07-14 覆盖 2026-07-13 的专用 `summary` Bot 方案。
**不新增 HTTP 路由**(brief Goal / Out-of-scope),而是**扩展现有
`/v1/internal/notify` 的 `NotifyReq`**。

## 契约要点(锁定项)

1. **同端点、同 token、同粒度**:仍是 `POST /v1/internal/notify`,头 `X-Internal-Token`,
   **一请求一收件人**(`targets` 单元素)。smart-summary 的 claim/markSent/markFailed/
   Sweep 状态机、`space_id`/`service`/`actor_uid` 语义**全部不变**。
2. **`payload` 与 `card` 二选一**:
   - 发文本(现状,其它 service 继续用):填 `payload:{type:1,content:...}`,`card` 省略。
   - 发卡片(summary pilot):填结构化 `card`,`payload` 省略(server 侧 binding 从
     `required` 放宽为二者其一)。
3. **禁止客户端手搓 type-17 map**:Decision 14 仍然生效——`payload` 若被
   `cardmsg.IsCardPayload` 判为卡片(`type=17`),**照旧 400 拒绝**。卡片只能走
   结构化 `card` 字段,由 octo-server 用模板生成 type-17 wire,smart-summary 永远不构造
   Adaptive Card JSON。
4. **响应契约不变**:仍返回 `NotifyResp{delivered:[], filtered:{uid:reason}}`。卡片派发
   成功 → uid 进 `delivered`;`target_denied`/`dispatch_failed`/建卡失败 → uid 进
   `filtered`。smart-summary 现有「只认 `delivered[]` 判真送达 + 非成员进 filtered 重试」
   的逻辑**一行不用改**(`recipientIsActiveMember` 预检可保留,carddispatch 的 target
   校验会 fail-closed 兜底)。
5. **模板/文案/链接归属 octo-server**:smart-summary **只发原始字段**;卡片布局、按钮
   文案(查看详情/复制)、FactSet 标签、`/s/{task_no}?sp={space_id}` deep-link,全部由
   octo-server `pkg/cardtmpl` + `i18n.OutboundLanguage` 生成。smart-summary 不再拼
   「查看结果:<link>」(旧字段 `payload.result_url` 作废)。

## 请求体(卡片模式)

```jsonc
POST /v1/internal/notify
X-Internal-Token: <SUMMARY_NOTIFY_TOKEN>
{
  "space_id":  "spc_xxx",          // 现状:必填,server 用 memberCache 校验收件人
  "service":   "summary-service",  // 现状不变
  "targets":   ["uid_recipient"],  // 现状:单收件人
  "actor_uid": "",                 // 现状:DM 通知留空
  "card": {                        // 新增:非空即走 summary-notify 卡片 producer
    "task_no":    "TN_20260713_abcd",   // ★ 见下「标识」
    "kind":       "completed",          // "completed" | "failed"
    "title":      "产品周会纪要",         // 原始标题(server 负责转义/截断)
    "time_range": "2026-07-06 10:00 ~ 2026-07-13 10:00", // 见下「时间/计数」
    "members":    5,                    // 参与人数;<=0 省略该行
    "msg_count":  128,                  // 消息条数;<=0 省略该行
    "generated_at": "2026-07-13 15:04", // 完成时间字符串;空则省略
    "reason":     ""                    // failed 时的脱敏原因;completed 留空
  }
}
```

`payload` 与 `card` 同时缺失 → 400;两者同时存在 → 400(避免歧义)。

## 三个约定(建议默认,待确认)

- **标识 `task_no`(不是自增 `id`)**:deep-link `/s/{task_no}?sp={space_id}` 用
  `summary_task.task_no`(varchar unique)。理由:不可枚举 + 与前端
  `WKApp.openSummaryDetail` 对齐。**待 smart-summary 确认前端 detail 入口用的是 task_no
  还是 id。**
- **时间字段传「已格式化字符串」**:`time_range` / `generated_at` 由 smart-summary 用它
  自己的 `internal/timezone`(东八区)格式化后传字符串,octo-server 原样填进 FactSet 值。
  理由:octo-server 无该业务时区配置,避免时区漂移。**标签(“时间范围”)仍由 octo-server
  按收件人语言本地化。**
- **计数传 int**:`members` / `msg_count` 传整数,octo-server 按 `i18n.OutboundLanguage`
  组装「参与成员:N 人」「消息数量:N 条」,单位随语言。

## 字段来源(smart-summary 侧现成)

| card 字段 | smart-summary 来源 |
| --- | --- |
| `task_no` | `SummaryTask.TaskNo` |
| `kind` | `kindForStatus(status)`(completed/failed) |
| `title` | `SummaryTask.Title` |
| `time_range` | `formatTimeRange(task)`(现有) |
| `members` | `participantCount(task)`(现有) |
| `msg_count` | `summary_result.total_msg_count`(现有 `resultMeta`) |
| `generated_at` | `summary_result.generated_at` 格式化(现有 `resultMeta`) |
| `reason` | 现有 `errorSanitizer` 脱敏后的失败原因 |

空间名(现在 smart-summary 自己 resolve)在卡片里不再需要——octo-server 侧可按 space_id
自行渲染或省略,减少两边重复。

## octo-server 侧(本 PR 落地,cross-repo 之外)

- `summary-notify` producer 绑定已有 `notification` Bot
  (`user`+`app`+`robot.status=1),卡片和纯文本降级复用其 provisioning/readiness;
  不新增专用 `summary` Bot,也不自动删除环境里可能已存在的旧 `summary` 身份。
- 注册 `summary-notify` producer(DM/`octo/v1`/system-notification/MaxInFlight 20),
  经 `SenderFromContext` 注入 notify。
- `card` 分支:`memberCache.verify` → `cardtmpl` 建卡 → 每个成员 `sender.Send` → 汇总
  `delivered/filtered`。**建卡/配置(如 deep-link 非 https)失败降级为原纯文本 DM**,
  保证通知必达(此为 octo-server 侧策略,brief 未强制,PR 说明)。

## enablement 门(端到端上线前必须齐)

1. **octo-web** 上线 `/s/:taskId` 路由(否则「查看详情」死链)——独立 PR;
2. octo-server 落 `cardtmpl` snapshot/Validate + deep-link 形状测试(本 PR);
3. **smart-summary** 切换发送从 `payload` 文本 → `card` 字段(独立 PR,最后做)。

在 (1)(3) 到位前,octo-server 侧行为惰性:无 `card` 请求即无卡片流量;全局
`OCTO_CARD_MESSAGE_ENABLED` 为总开关,回滚可移除 producer spec 或关该 env。

## 明确不在本契约内

原群/thread 回发(member-exempt 群发)、A2 用户转发卡片、docs 分享卡片、per-channel
限速与 cluster cap——均为后续独立任务(brief Out-of-scope)。
