# OPPO PUSH 服务端接入

## 当前实现

服务端通过 OPPO 国内 REST API 发送通知栏单推：

- 鉴权：`POST https://api-push-cn.heytapmobi.com/server/v1/auth`
- 单推：`POST https://api-push-cn.heytapmobi.com/server/v1/message/notification/unicast`
- 编码：UTF-8 `application/x-www-form-urlencoded`
- HTTP 总超时：5 秒
- auth token：按 AppKey 隔离，本地和 Redis 均按绝对时间提前 20 小时过期；正常发送不访问
  Redis，Redis 故障时使用进程内缓存降级
- token 失效：仅对 OPPO code `11` 强制鉴权并重试一次
- 去重：重试沿用同一个 `app_message_id`
- 可观测性：厂商业务拒绝以 Warn 记录 `oppo_code/retryable`，成功响应以 Debug 记录
  `oppo_message_id`；日志不包含 MasterSecret、auth token 或 registration_id

服务端会发送 `verify_registration_id=true`、24 小时离线 TTL，但不发送 `notify_id`（避免
不同会话中相同 `message_seq` 的通知互相覆盖），并把以下会话路由字段编码进
`action_parameters`：

```json
{
  "space_id": "space-id",
  "channel_id": "channel-id",
  "channel_type": 2,
  "message_seq": 42
}
```

通知标题和正文是 OPPO 必填字段。服务端对空值使用“您有一条新的消息”兜底，并分别按
50/200 个 Unicode 字符截断；`action_parameters` 编码后不得超过 4 KiB，超限时拒绝发送并
返回错误。

## 配置

基础配置由 octo-lib 加载。启用 OPPO pusher 至少需要：

| 环境变量 | 必需 | 说明 |
| --- | --- | --- |
| `TS_PUSH_OPPO_PACKAGENAME` | 是 | Android 包名，同时作为厂商 pusher 路由键 |
| `TS_PUSH_OPPO_APPKEY` | 是 | OPPO PUSH AppKey |
| `TS_PUSH_OPPO_MASTERSECRET` | 是 | OPPO PUSH 服务端密钥；不得写入日志或仓库 |

以下为 octo-server 本地扩展配置，当前版本**仅从环境变量读取**，写入 `tsdd.yaml` 不会生效：

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `TS_PUSH_OPPO_CATEGORY` | 空 | 新消息分类，例如 `IM`；仅在流量确实符合已获批分类时设置 |
| `TS_PUSH_OPPO_NOTIFY_LEVEL` | 分类为空时不发送；分类非空时为 `2` | 允许 `1`、`2`、`16`；`16` 需要 OPPO 强提醒权益 |
| `TS_PUSH_OPPO_CHANNEL_ID` | 空 | Android `NotificationChannel` ID，不是 IM 会话 ID |
| `TS_PUSH_OPPO_PRIVATE_MSG_TEMPLATE_ID` | 空 | OPPO 审核通过的私信模板 ID；平台要求时必须配置 |
| `TS_PUSH_OPPO_CLICK_ACTION_TYPE` | `0` | `0` 启动应用，`1/4` 打开 Activity，`2/5` 使用 URL/Intent URI |
| `TS_PUSH_OPPO_CLICK_ACTION_ACTIVITY` | 空 | action type 为 `1/4` 时必需 |
| `TS_PUSH_OPPO_CLICK_ACTION_URL` | 空 | action type 为 `2/5` 时必需 |

配置值不合法时，OPPO pusher 在启动构造阶段会被禁用并记录错误。不会回退为带错误分类的
请求。

### 分类边界

`TS_PUSH_OPPO_CATEGORY` 是当前应用级配置。octo-server 同一推送流里可能同时存在真人会话、
Bot 和系统消息，因此不能在所有环境中无条件默认 `IM`。只有确认该部署送往 OPPO 的消息都
符合 OPPO 已批准的 `IM` 分类时才设置；否则保持为空，或后续增加经过评审的逐消息分类器。

已配置 `category` 后不要在同一设备上混用旧分类和新分类 payload。若 OPPO 控制台要求
私信模板，同时配置 `TS_PUSH_OPPO_PRIVATE_MSG_TEMPLATE_ID`，避免 code `54`。

### 点击路由

服务端只负责生成可信 `action_parameters`。若需要点击后直达会话，应配置 Android Activity
或 Intent URI，并由客户端校验、解析上述四个字段。未配置时 `click_action_type=0`，只保证
启动应用，不宣称能够直达会话。

## 验证

无需真实凭据的契约测试：

```bash
go test -race ./modules/webhook -run '^Test(OPPO|ParseOPPO|LoadOPPO|NewOPPO)' -count=1
go vet ./modules/webhook
```

真实设备烟测需要专用测试应用、测试设备和有效 registration_id：

```bash
OPPO_APP_KEY='...' \
OPPO_MASTER_SECRET='...' \
OPPO_DEVICE_TOKEN='...' \
go test ./modules/webhook -run '^TestOPPOPush$' -count=1 -v
```

该烟测只证明 OPPO 接口接受请求。到达、展示和点击跳转还必须结合测试设备与 Android 客户端
日志验收。
