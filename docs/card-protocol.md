# Octo 卡片消息协议（InteractiveCard, ContentType 17）

> **权威声明**：本文档是 `pkg/cardmsg/` 的**镜像**。octo/v1 白名单、大小/结构
> 上限、plain 派生规则的唯一强制权威是该 Go 包 —— 两者如有出入，以代码为准并
> 视为本文档的 bug。规范源头：`.octospec/tasks/card-message-protocol/brief.md`
> （P1 展示）与 `.octospec/tasks/card-message-interaction/brief.md`（P2 交互）；
> 本文档与两份 brief 同步修订，不单独漂移。
>
> 分期：**P1（展示）已实现**；**P2（交互）契约已冻结、随 sibling 实现 PR 落地**
> —— 客户端按本文档一次性架构，P2 是开关不是二次适配。

## 1. 信封（wire envelope）

```json
{
  "type": 17,
  "card": { "…": "标准 Adaptive Cards 1.5 JSON（octo profile 白名单子集）" },
  "plain": "服务端权威纯文本（客户端提交值一律被服务端重算覆盖）",
  "card_version": "1.5",
  "profile": "octo/v1"
}
```

- `type` = **17**（`InteractiveCard`）。⚠️ 与 `ContentType 7`（名片 `Card`）无关，
  不要把任何新逻辑接到 7 上。
- `card` 是**标准 AC 1.5 JSON**（不是改名 DSL）：可用 AC SDK 或自研渲染器互换
  渲染，渲染选型可逆（当前决策：**三端自研渲染器**）。服务端从不改写 card 树
  （mention 等 enrich 只发生在信封顶层键）。
- `plain` 由服务端在派发出口按 §5 规则生成，**永不为空**——离线推送、搜索、
  会话摘要、置顶、引用、复制、降级显示全部以它为准。
- `card_version` 当前固定 `"1.5"`；`profile` 见 §3。
- 未知的**额外**顶层字段被容忍（前向兼容）。P2 起信封新增可选字段：
  `card_seq`（整数，乱序防护）、`transient`（bool，进度帧不入变更历史）。

## 2. octo/v1 profile（P1 展示白名单）

服务端 write-strict 校验（`cardmsg.Validate`），白名单外一律 400：

| 类别 | 允许 |
|---|---|
| 元素 | `TextBlock`（markdown 子集，§2.1）、`RichTextBlock`、`Image`、`ImageSet`、`Container`、`ColumnSet`（含 `Column` 列）、`FactSet`、`Table`、`ActionSet`（`RichTextBlock`/`ImageSet`/`Table`/`ActionSet` 为 P3-3 Tier 1 追加，均 AC ≤1.5：ImageSet 1.0 / RichTextBlock 1.2 / ActionSet 1.2 / Table 1.5） |
| 动作（本地，无服务端回调） | `Action.OpenUrl`、`Action.ToggleVisibility`（折叠/展开，见 §2.2）、`Action.CopyToClipboard`（octo 自定义本地复制，见 §2.2）；元素/整卡 `selectAction` 携带上述任一本地动作亦可（分期继承） |
| 元素通用属性 | `id`（帧内唯一，与 `Action.Submit`/`Input.*` 共享同一 id 空间）、`isVisible`（bool；隐藏子树仍完整校验、计入预算，见 §2.2） |
| P2 起（octo/v2） | `Action.Submit`（含 selectAction/ActionSet 携带）、`Input.Text` / `Input.Toggle` / `Input.ChoiceSet` / `Input.Number` / `Input.Date` / `Input.Time`（id 必填且帧内唯一；后三者 P3-3 追加，均 AC 1.0、仍在 `card_version:"1.5"` 内，为白名单增量而非版本升级） |
| 暂不支持（后续按需） | `Media`(1.1)、`Action.ShowCard`(1.0)、`Action.Execute`(1.4)、模板/数据绑定，以及 AC 1.6 元素 |

结构与大小上限（全部 ingress 一致）：

- 完整 payload 序列化 ≤ **512 KiB**（发送/编辑路由另有 2 MiB pre-decode body 上限；取 2 MiB 而非 1 MiB 是为不误伤同路由上恰好 1 MiB 的合法 RichText，见 `cardmsg.MaxSendBodyBytes`）；
- 递归节点数 ≤ **200**，嵌套深度 ≤ **16**；
- **URL 正向 allowlist**：仅绝对 `http`/`https`。作用于**全部会被渲染的 URL 面**：
  `Image.url`、`Action.OpenUrl.url`、`selectAction`、`AdaptiveCard`/`Container`/
  `Column`/`ColumnSet` 的 `backgroundImage`（字符串或 `{url:…}` 对象形）、
  `Action.OpenUrl.iconUrl`、以及 markdown 链接/图片目标（内联 `[t](url)`、引用式
  `[t][l]`+`[l]: url`、图片 `![alt](url)`、autolink `<scheme:…>`，经完整 CommonMark
  解析提取，详见 §2.1）。`data:` / `javascript:` / `intent:` /
  相对路径等一律拒绝。**app 深链（`octo://` 等）在 P1 被有意排除**——首方 scheme
  名单是 P3 议题。
- 服务端**不解引用**任何卡内 URL（无图片代理/unfurl/预取）——引入前必须先过
  SSRF-safe fetcher 决策。

### 2.1 TextBlock markdown 子集

粗体 `**`、斜体 `*`、列表、链接 `[text](url)`。链接目标走 §2 的同一 URL
allowlist —— 服务端用**完整 CommonMark 解析器**（非模式匹配）提取一段文本里所有会
被渲染成活链接的目标：内联链接 `[t](url)`、引用式链接 `[t][l]`+`[l]: url`、图片
`![alt](url)`、autolink `<scheme:…>`，包括嵌套/转义方括号 label（`[a [b]](url)`、
`[x\]](url)`）与转义 scheme 引用定义等边角形式，确保**校验面 ≥ 渲染面**。任何非
`http(s)` 目标（含反斜杠破坏 scheme 的 `javascript\:…`）一律拒绝。plain 派生时剥离
语法字符（链接降为链接文本）。

### 2.2 本地折叠与复制（octo/v1 本地动作）

均为**纯端上、无服务端回调**的动作（不走 `message/card/action`、不入 `card_action`
事件、无幂等/权限），与 `Action.OpenUrl` 同档，故落 **octo/v1**。

**`Action.ToggleVisibility`** —— 折叠/展开目标子树（如「收起推理 / 展开推理」）：

```json
{ "type": "Container", "id": "reasoning", "isVisible": false, "items": [] }
{ "type": "Action.ToggleVisibility", "title": "展开推理", "targetElements": ["reasoning"] }
```

- `targetElements` 必填、非空数组；每项是元素 `id` 字符串，或 AC `TargetElement`
  对象 `{ "elementId": "id", "isVisible": true|false }`。
- 引用的 `id` **必须存在于本卡**（全卡遍历后统一解析 —— 前向引用安全，target 可先于/
  后于其 toggle 出现）；悬空引用整卡拒。
- 元素 `id` **帧内唯一**（与 `Action.Submit`/`Input.*` 共享同一 id 空间，对齐 AC 的
  card-global id 模型）。`id` 可声明在**任意 id 承载节点**——顶层展示/输入元素、`Column`、
  `TableRow`/`TableCell`、`ImageSet` 子 `Image`——且**任一均可作 `targetElements` 目标**
  （校验器逐节点登记，不留缺口）。`isVisible` 出现时必须是 bool。
- **隐藏内容不豁免任何校验**：`isVisible:false` 的子树仍完整走白名单/URL allowlist、
  计入节点/深度预算（隐藏节点不得成为绕过通道 —— 校验面 ≥ 渲染面）。

**`Action.CopyToClipboard`**（octo 自定义，标准 AC 无）—— 本地复制一段明文：

```json
{ "type": "Action.CopyToClipboard", "title": "复制", "text": "SELECT ..." }
```

- `text` 必填、字符串、≤ **4 KiB**（`cardmsg.MaxCopyTextBytes`，按 **UTF-8 字节**计、非字符数
  ——与 `Input.Text` 同口径；4096 字节 ≈ 1365 个 CJK 字符；manifest `limits.max_copy_text_bytes`
  同步下发，供 producer 按阈值 feature-detect）；`title` 可选字符串。
- `text` **逐字复制、不渲染** —— 无 URL/markdown 面，不过 allowlist。「勿复制隐藏/敏感
  字段」是生产者/客户端职责，非服务端结构校验。

## 3. profile 协商与降级链

- **服务端**：P1 只接受 `profile:"octo/v1"` + `card_version:"1.5"`，其它值
  （含 `octo/v2`，分期）→ 400。P2 服务端接受集变为 {octo/v1, octo/v2}。
- **客户端**（协商在渲染侧，不在服务端）：
  1. 认识该 `profile` → 渲染 `card`；
  2. 不认识 `profile`（更新的服务端/更旧的客户端）→ 渲染 `plain`；
  3. 连 `type:17` 都不认识（存量客户端）→ octo-lib 未知类型兜底文案。
- P2 产者能力发现：`GET /v1/bot/card/profile`（D12，随 P2 落地）返回部署的
  `enabled` / `card_version` / `profiles` / `elements` / `inputs` / `actions` / `limits`
  清单（只增不改）；`elements`/`inputs`/`actions` 是本部署接受的展示元素 / 交互输入 /
  **本地动作**白名单（源自 `pkg/cardmsg` 权威列表；`elements` 只列**顶层可放置**元素——
  `Column` 是 `ColumnSet` 的子列、由 `ColumnSet` 涵盖，不单列；`actions` 只列 octo/v1
  本地动作 `Action.OpenUrl`/`ToggleVisibility`/`CopyToClipboard`——`Action.Submit` 属
  octo/v2、经 `profiles` 隐式发现，不在 `actions` 内），供 producer 按**元素/动作粒度**
  前向兼容协商——即便 `card_version` 停在 `"1.5"`、`profiles` 不变，也能探测是否接受
  `Input.Number/Date/Time`、`ToggleVisibility`/`CopyToClipboard` 等 additive 新增能力。
  P1 期间生产者以发送被 400/`card_disabled` 拒绝为「未启用」信号。

## 4. 信任模型（谁能发卡、谁能信卡）

三层，各自独立必要：

1. **服务端 HTTP ingress**：只有 bot（`/v1/bot/sendMessage`、robot API）与
   incoming webhook（`msg_type:"card"`）能发卡。用户 `/v1/message/send` 的
   type-17 → 403 语义拒绝；**bot OBO（`on_behalf_of`）路径的卡片一律 400**
   （按请求意图拦截，先于 grant 校验）。
2. **客户端 from_uid 渲染门禁（协议契约的一部分）**：只有当消息的 `from_uid`
   是 bot / webhook 身份时才渲染 `card`；否则**降级为 `plain` 文本展示**。
   原因：WuKongIM 通知 webhook 是存储后无否决权的，直连长连接可绕过服务端
   ingress —— 渲染门禁是残余风险的最后防线。`from_uid` 由 IM 连接鉴权绑定，
   不可伪造。
3. **P2 动作端点**再次服务端复验目标消息 sender 身份。

**服务端展示面的对应纪律**：推送 / 搜索命中 / 摘要 / 置顶 / 引用等由服务端
产出文案的面，对 sender 非 bot/webhook 身份的 type-17 一律显示 `[卡片]`，
绝不透出存储 `plain`（该 plain 未经 Finalize，攻击者可控）。

**Rollout 开关**：`OCTO_CARD_MESSAGE_ENABLED`（默认关闭，部署级总闸）。客户端
渲染门禁发布前，生产环境不得开启。

**bot 侧子开关**：`OCTO_BOT_CARD_ENABLED`。总闸开启后，可用它单独控制 **bot 侧**
是否允许生成卡片（`bot_api` 发/改卡、legacy `robot` ingress）——默认（未设/非法值）
为开，向后兼容；显式设 `false` 时 bot 一律发不了卡（返回 `card_disabled`），但内部
通知 / incoming webhook / card_action 仍按总闸照常发卡。有效门禁
`BotEnabled() = 总闸 AND 子开关`；`GET /v1/bot/card/profile` 的 `enabled` 字段取
的正是它，故清单与实际发卡门禁**同源**，绝不出现「报 enabled 却发被拒」。

## 5. plain 派生规则（服务端权威）

按文档序遍历 `card.body`：

- `TextBlock` → 剥离 markdown 后的文本；
- `RichTextBlock` → 拼接 `inlines[]` 的文本（字符串 run + `TextRun.text`；TextRun 非 markdown，不剥离）；
- `FactSet` → 逐条 `"title: value"` 行；
- `Image` → `[图片]`；`ImageSet` → 每张图一个 `[图片]`；
- 容器类（`Container`/`ColumnSet`/`Column`、`Table` 的 `rows→cells→items`）→ 递归；
- 动作（按钮，含 `ActionSet`）**不参与**（按钮是操作面不是内容）。

段落以换行拼接；结果为空 → `[卡片]` 兜底。**客户端提交的 `plain` 值一律被
覆盖**。incoming webhook 的 `text` 字段是**兜底种子**：仅当派生结果为空时
使用，卡体产出文本时被忽略。

## 6. P1 生产者 API

```json
// POST /v1/bot/sendMessage（既有端点,新 payload 类型;OBO 不可用）
{ "channel_id": "g_9f2c...", "channel_type": 2,
  "payload": { "type": 17, "card": { "…": "…" }, "plain": "ignored — server recomputes",
               "card_version": "1.5", "profile": "octo/v1" } }

// POST /v1/incoming-webhooks/:webhook_id/:token（既有端点,新 msg_type;body ≤ 8KB）
// "text" 仅当派生 plain 为空时作种子;错误:结构非法 400 reason=card,
// 未启用 400 reason=card_disabled,超 512KiB → 413
{ "msg_type": "card", "card": { "…": "AC JSON" }, "text": "optional plain seed" }
```

robot API（legacy）`/robot/sendMessage` 同样接受 type-17（校验与 bot ingress
对称；错误形状是该 API 的单一 content-invalid 400）。

**P1 卡片不可变**：所有编辑入口（用户 `/v1/message/edit`、bot
`/v1/bot/message/edit`、robot 编辑）对 type-17（目标消息或编辑体）一律 400。
用户编辑路径**永久**关闭；bot 编辑路径由 P2 解锁。撤回与普通消息一致。

## 7. P2 交互契约（已冻结,随 sibling PR 落地）

完整规范见 `.octospec/tasks/card-message-interaction/brief.md`（D1–D12）。
客户端与 bot SDK 现在就按此架构：

### 7.1 交互闭环

```
客户端点按钮 → POST /v1/message/card/action     （鉴权/防伪造/幂等,即时 ack）
            → 服务端按 stored sender + data.owner/action_type 分支：
              - 注册的一方 sender/route：内部可靠队列 → HMAC callback
                → octo-server 写终态卡并通知申请人
              - 其它 Bot：bot 事件队列 event_type="card_action"
                （/v1/bot/events 轮询,至少一次）→ Bot 调 /v1/bot/message/edit
            → message_extra + CMD /v1/message/extra/sync
            → 三端重渲染新帧（消息列表/sync 响应即卡片状态权威,无独立状态 API）
```

一方 callback 路由是静态启动配置，卡片不携带 URL。路由键必须同时匹配消息表中的
`sender_uid`、服务端模板写入的 `data.owner` 和 `data.action_type`。外部 Bot 即使复制相同
`data` 仍走原有 pull 队列；已注册的一方 sender 若提交未知/缺失 owner/action，则
fail-closed，不会落入无人消费的 Bot 队列。传输、队列与运维合同见
[`card-action-callback-dispatch.md`](./card-action-callback-dispatch.md)。

一方消费方的 route/notify 配置、标准 action 卡、终态约束、回调验签、幂等、重试与 DLQ
统一以 [`card-action-callback-dispatch.md`](./card-action-callback-dispatch.md) 和
[`card-action-callback-consumer.md`](./card-action-callback-consumer.md) 为准；本文不重复维护
消费方接入合同。

- **状态在卡片内容里**：防重复操作 = 服务端幂等（业务身份键
  `message_id+action_id+operator_uid`，`client_token` 只是关联 ID）+ bot 重写
  卡片（按钮随帧消失/置灰）+ 客户端瞬时 loading（**10 s 超时恢复可点**，
  不得持久化本地动作状态）。**幂等判定先于生效帧校验**（PR#548 P1-4）：已受理
  动作的重试即使 bot 已重写移除该按钮，也回 `replay`（而非 stale-frame 400）；
  但**从未受理**的按钮的迟到点击仍 fail-closed 400。
- **`action_id` 命名逻辑动作实例**：新帧重新提供同一逻辑动作必须换新 id
  （如 `approve#2`），否则同人 24h 内的合法再操作会撞已消费的幂等桶。
- **帧模型**：每帧都是完整 type-17 信封、独立过白名单，连续帧结构可任意不同、
  可在 v1/v2 之间迁移；跨类型变异（卡→文/文→卡）拒绝。存储只留最新帧；
  变更历史入侧表（`GET /v1/message/card/revisions`，成员可见、可抹除但留
  tombstone、`transient:true` 的进度帧不入史）。
- **乱序防护**：可选 `card_seq`（单调递增）；带上时旧帧 → 409。
- **inputs 信任边界**：提交键必须命中生效帧声明的 `Input.*` id（未声明
  fail-closed），值为字符串、逐类型校验、总量 ≤ 16 KiB。P3-3 起 `Input.Number`
  校验为有限数、`Input.Date`（YYYY-MM-DD）/`Input.Time`（HH:MM）校验格式，三者 `""`
  均视为未填放行；`min`/`max` 区间与 `isRequired`/`regex` 一样**不服务端强制**（AC 规范
  定义 min/max 为可忽略 hint，区间校验交 bot 业务逻辑，PR#556 定稿）。
- **`event_data.space_id`**（PR#548 P1-3）：卡片**来源 Space**，服务端从存储行
  解析（群/子区取群 SpaceID；DM 取发送时注入 payload 的 `space_id`），**非**操作者
  请求上下文 Space；无权威值时省略该键（fail-closed），消费方按可选字段处理。
- **交互只对 bot-sender 卡片开放**：webhook 卡片是展示-only（无事件消费端）。
- **时延预期**：bot 侧收事件是游标轮询（快慢 = bot 轮询节奏）；客户端刷新走
  CMD 是实时的。
- **重写节奏**：里程碑级（≥2–5 s 或 ≥25% 进度步进），不要每秒进度条。

### 7.2 频率与配额

发送/编辑走 bot API 既有配额；`card/action` 挂标准登录用户限流。

## 8. 客户端渲染器职责（决策 B：三端自研）

- 实现 §2 白名单元素 + §3 降级链 + §4 渲染门禁 + §7.1 的瞬时交互态；
- 变更历史视图 = 只读复用同一渲染器（每帧都是完整可渲染信封）；
- 不认识的元素/动作：整卡降级为 `plain`（P1/P2 无逐元素 fallback —— 那是 P3
  议题）。协议 wire 格式是标准 AC，未来切换 AC SDK / DivKit 不动协议。
