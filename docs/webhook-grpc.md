# Webhook gRPC 回调通道

WuKongIM 通过 gRPC 把事件回调给 octo-server，由 `modules/webhook` 处理：

| 事件 | 作用 |
|---|---|
| `msg.notify` | 消息写入 octo-server 数据库，并通知各业务模块 |
| `msg.offline` | 给离线用户发手机推送 |
| `user.onlinestatus` | 更新在线状态并通知好友 |

监听地址由 `grpcAddr` 配置（默认 `0.0.0.0:6979`）。它和 HTTP API 的 `addr`（默认 `:8090`）是**两个独立端口**。

## 部署建议

这个端口只需要 WuKongIM 能访问，客户端不需要。

- WuKongIM 与 octo-server 同机部署：`grpcAddr` 绑定 `127.0.0.1:6979`。
- 跨机部署：绑定内网地址，并用安全组 / 防火墙只放行 WuKongIM。
- Docker：不要把 6979 映射到宿主机（不要写 `ports: "6979:6979"`），用容器内部网络。
- Kubernetes：Service 使用 `ClusterIP`，并用 NetworkPolicy 只允许 WuKongIM 的 Pod 访问 6979。

## 认证配置（仅环境变量）

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TS_GRPC_AUTH_TOKEN` | 空 | 共享 token。配置后，调用方必须在 gRPC metadata `auth_token` 中携带相同的值，否则返回 `Unauthenticated`。 |
| `TS_GRPC_AUTH_REQUIRED` | `false` | 设为 `true` 表示强制认证：未配置 `TS_GRPC_AUTH_TOKEN` 时服务**拒绝启动**。取值必须是合法布尔值（`true`/`false`/`1`/`0` 等），否则同样拒绝启动。 |

**默认值 `false` 不改变现有部署的行为**：和之前一样，只有配置了 `TS_GRPC_AUTH_TOKEN` 才校验。

## 行为对照（已用 WuKongIM `v2.2.4-20260313` 实测）

| `TS_GRPC_AUTH_REQUIRED` | `TS_GRPC_AUTH_TOKEN` | octo-server | WuKongIM 回调 |
|---|---|---|---|
| 未设置 / `false` | 未设置 | 正常启动 | ✅ 正常（与之前一致） |
| 未设置 / `false` | 已设置 | 正常启动 | ❌ 全部被拒绝（之前就是如此） |
| `true` | 未设置 | ❌ **拒绝启动**，进程退出 | — |
| `true` | 已设置 | 正常启动 | ❌ 全部被拒绝 |
| 非法值（如 `yes`） | 任意 | ❌ **拒绝启动**，进程退出 | — |

原因：WuKongIM `v2.2.4-20260313` 调用 webhook gRPC 时**不携带** `auth_token`，也没有对应的配置项。

回调被拒绝时：消息不会写入 octo-server、离线推送不会发出、在线状态不会更新；WuKongIM 日志中出现 `code = Unauthenticated`，并按它自己的策略重试。

> ⚠️ 在 WuKongIM 支持发送 `auth_token` 之前，**不要**配置 `TS_GRPC_AUTH_TOKEN`，也**不要**设置 `TS_GRPC_AUTH_REQUIRED=true`。当前请依靠上面的「部署建议」限制访问来源。

## 开启认证的步骤（WuKongIM 支持发送 token 之后）

1. 在 WuKongIM 侧配置与 octo-server 相同的 token。
2. 在 octo-server 设置 `TS_GRPC_AUTH_TOKEN`，确认消息落库、离线推送、在线状态都正常。
3. 再设置 `TS_GRPC_AUTH_REQUIRED=true`，防止以后 token 丢失时服务在无认证状态下运行。

回滚：先去掉 `TS_GRPC_AUTH_REQUIRED`，再去掉 `TS_GRPC_AUTH_TOKEN`。

`TS_GRPC_AUTH_TOKEN` 的值不要与其他内部 token 相同。启动时会检查重复，发现时输出 Error 日志（不会阻止启动），见 `main.go` 的 `fixedInternalTokenEnvs`。

## 启动日志

| 情况 | 日志 |
|---|---|
| 已配置 token | Info：`gRPC server auth enabled` |
| 未配置 token，监听回环地址 | Info：`gRPC server auth not configured, listening on loopback only` |
| 未配置 token，监听非回环地址 | **Warn**：`gRPC server auth not configured on a non-loopback address ...` |
| 认证配置无效 | Error + 进程退出（错误信息只包含环境变量名） |

token 的值不会写入日志。

## 回调数据上限

单个事件超过以下上限时，整个事件返回错误（不截断、不部分处理）。取值远高于正常批次，只用来限制单个事件的处理量。

| 项目 | 上限 |
|---|---|
| `msg.offline` 压缩收件人列表解压后大小 | 32 MiB |
| `msg.offline` 收件人数（去重后） | 200,000 |
| `user.onlinestatus` 条目数 | 100,000 |

`msg.offline` 中重复的收件人只推送一次。这些上限对 HTTP webhook（`/v1/webhook`）同样生效。
