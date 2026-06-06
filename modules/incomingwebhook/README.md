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
