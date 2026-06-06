# Incoming Webhook 推送契约

外部服务通过带 token 的 URL 向指定群推送消息。管理端点（创建/列出/更新/删除/重置）
由群主或管理员调用，详见 `api.go`。本文聚焦**推送端点**的请求契约。

```
POST /v1/incoming-webhooks/:webhook_id/:token
Content-Type: application/json
```

鉴权走 URL 内的 token（SHA-256 存储、常量时间比对），无需登录态。所有鉴权失败统一
返回 401（反枚举），并受多层限流约束。

## 消息形态

由 `msg_type` 选择，**缺省即纯文本**，与历史行为完全一致：

> **兼容性提醒**：`msg_type` 现在严格校验——只接受省略 / `"text"` / `"richtext"`，其它非空值
> （如 `"markdown"`）返回 400 `reason=msg_type`。历史上未知 JSON 字段会被忽略，因此若有旧
> 客户端误带了别的 `msg_type` 值，升级后需要去掉它（带合法 `content` 也会被拒）。`msg_type`
> 大小写不敏感（内部做了 lower+trim），但块 `type` 大小写敏感、须为精确小写（`text`/`image`）。

### 1. 纯文本（`msg_type` 省略或 `"text"`）

`content` 必填，客户端按 markdown 渲染。

```json
{
  "content": "Build **#123** passed ✅  https://ci.example.com/123",
  "username": "CI Bot",
  "avatar_url": "https://example.com/ci.png"
}
```

- `content`：必填，非空；语义长度上限 4000 rune（`DM_INCOMINGWEBHOOK_MAX_CONTENT_RUNES`）。
- `text`：`content` 的别名（Slack 等平台习惯用 `text`）。`content` 为空时回退到 `text`，
  降低从既有集成迁移的改造成本；两者都填以 `content` 为准。
- `username` / `avatar_url`：可选，覆盖该条消息的展示发送者名/头像（不改 webhook 本身配置）。

### 2. 富文本 / 图文混排（`msg_type` = `"richtext"`）

`blocks` 承载**有序**的图文块，数组顺序即图文穿插顺序。服务端翻译为内部 RichText 消息，
客户端复用既有富文本渲染链路。

```json
{
  "msg_type": "richtext",
  "blocks": [
    { "type": "text",  "text": "Build #123 passed ✅" },
    { "type": "image", "url": "https://example.com/chart.png", "width": 800, "height": 400 },
    { "type": "text",  "text": "耗时 42s" }
  ],
  "username": "CI Bot",
  "avatar_url": "https://example.com/ci.png"
}
```

块类型：

| `type`  | 必填字段 | 约束 |
|---------|----------|------|
| `text`  | `text`   | 非空（纯文本，不渲染 markdown） |
| `image` | `url`、`width`、`height` | `url` 仅接受 `http`/`https`（禁 `data:`/`base64`）；`width`/`height` 必须 > 0（供端上占位排版，避免抖动） |

约束：

- `blocks` 必填且非空；块数量上限默认 50（`DM_INCOMINGWEBHOOK_MAX_BLOCKS`）。
- **实际生效的上限是 8KB body cap**（`DM_INCOMINGWEBHOOK_MAX_BYTES`）：请求体在解析前即被
  截断，超出按 413 拒绝。由于图片仅 URL 引用（不内嵌 base64），8KB 足以承载数十个文本/
  图片块；多图文消息请用 URL 引用，不要内联大体积内容。
- 服务端另有 1MB 的 RichText 硬上限（octo-lib 契约）兜底，但在默认 8KB body cap 下不会
  先触达——它是上调 body cap 后才会成为约束的二级护栏。

## 通用字段与安全

- `username` / `avatar_url`：两种形态通用，服务端裁剪到字节上限（名 64B / 头像 255B）。
- 其它任意字段（含 `extra`、`space_id`）一律**丢弃**：消息归属的 Space 由服务端从群派生，
  不接受调用方覆盖，防止伪造到其它 Space。

## 响应

| 场景 | HTTP | 说明 |
|------|------|------|
| 成功 | 200 | `{"status":0,"message_id":<int>}` |
| 鉴权失败 | 401 | 统一响应，不区分原因（反枚举） |
| 限流 | 429 | 带 `Retry-After` |
| 请求非法 | 400 | `details.reason` ∈ `body`/`json`/`content`/`blocks`/`msg_type` |
| 体积过大 | 413 | 超 body cap 或富文本 >1MB |
| 投递失败 | 502 | 下游发送失败 |
| 功能停用 | 404 | 全局开关 `incomingwebhook.enabled=0` |

## 管理端点（群主 / 管理员）

需登录态 + 群管理员权限，路径前缀 `/v1/groups/:group_no/incoming-webhooks`。
除创建/列出/更新/删除/重置外，Phase 2 新增两个：

### 测试推送

```
POST /v1/groups/:group_no/incoming-webhooks/:webhook_id/test
```

向群里发一条测试消息，端到端验证配置（群可达、消息能投递）。文案按出站语言本地化
（en-US / zh-CN，由 `i18n.OutboundLanguage` 协商）。返回 `{"status":0,"message_id":<int>}`。
会记一条 `adapter=test` 的投递（成功或失败都记，便于在 deliveries 里与真实流量区分），
且**不**计入 `call_count` / `last_used_at`（测试不是真实流量）。

### 投递记录（排障）

```
GET /v1/groups/:group_no/incoming-webhooks/:webhook_id/deliveries?limit=50
```

倒序返回该 webhook 最近的投递记录（**成功 + 失败**），供发送方排障。`limit` 默认 50、
上限 100。失败记录的 `reason` / `http_status` 与 push 响应一致，便于对照定位。

```json
{
  "list": [
    {
      "status": 2, "reason": "blocks", "http_status": 400, "adapter": "native",
      "byte_size": 84, "message_id": 0, "created_at": 1749200000
    },
    {
      "status": 1, "reason": "", "http_status": 200, "adapter": "native",
      "byte_size": 42, "message_id": 123456, "created_at": 1749199900
    }
  ]
}
```

- `status`：`1`=成功，`2`=失败。
- `reason`（失败时）：`body` / `json` / `content` / `blocks` / `msg_type` / `too_large` /
  `delivery_failed`。
- `adapter`：`native`（推送端点）/ `test`（测试推送）。
- `http_status`：返回给调用方的状态码。**迁移前的历史成功行为 `0`（未知）**——不伪造成 200。
- **不返回调用方 `ip`**：审计表仍存 ip 作排查上下文，但出于隐私不向群管理员下发（review 决定）。

> **限流（429）不入审计**：`rate_limited` 是天然高频失败，逐条落库会在重试风暴时放大
> DB 写入、反噬限流的廉价丢弃；429 + `X-RateLimit-*`/`Retry-After` 头已把信息给到调用方。
> 节流可从 deliveries 里成功记录的稀疏/中断间接观察。

> **反枚举不变量**：鉴权失败（未知 webhook / 错 token / 已解散群）**不记入** deliveries，
> 只进 IP 失败预算——只有「鉴权通过后」的投递结果才落审计。
