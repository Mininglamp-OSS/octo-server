# docs_proxy

octo-server 侧 `/v1/docs/proxy/*path` 反向代理，把已鉴权请求转发到 doc 服务
（`octo-docs-html`），供 doc 侧在同一 origin 下渲染。

## 路由

- 挂载点：`/v1/docs/proxy/*action`（`Any` method，含 OPTIONS）。
- 上游地址由环境变量 `OCTO_DOCS_UPSTREAM` 指定；未配置整个 module 跳过路由挂载。
- 路径映射：`/v1/docs/proxy/<rest>` → `<upstream>/<rest>`（`upstream` 若带 basepath 会保留）。
- 中间件链：先 `AuthMiddleware`（匿名请求 401 abort），后反代 handler。

## 反代注入的头（内网可信头）

反代在**已鉴权通过**、把请求转发到上游 doc 服务之前，注入以下头。**每个头都必须
先 `Del` 掉 caller 可能自带的同名头，再 `Set` 反代自己算出来的值**，防止外网 caller
伪造身份。

| Header | 语义 | 来源 |
| --- | --- | --- |
| `X-Octo-Token` | 当前用户的 octo token | caller 请求的 `token` header（AuthMiddleware 已校验合法） |
| `X-Octo-Uid` | 登录用户 uid | `wkhttp.Context.GetLoginUID()`（OCT-145 方案 C 新增） |
| `X-Octo-Name` | 登录用户显示名 | `wkhttp.Context.GetLoginName()`（OCT-145 方案 C 新增） |
| `X-Octo-Role` | 登录用户角色 | `wkhttp.Context.GetLoginRole()`，收敛到 `superAdmin` / `admin` / `member`（OCT-145 方案 C 新增） |

`X-Octo-Role` 的值域**只有** `superAdmin` / `admin` / `member` 三种：wkhttp 常量
里 `superAdmin` / `admin` 保留原样，其他一切字面量（空串、未识别 role）一律降级
`member` —— 保证 doc 侧按稳定字符串判权，不会把空串或未知值当合法 role 提权。

同时反代在转发前会剥掉以下 caller 头，防止 leak 到上游：

- `Authorization`（防外部凭据泄给 doc）
- `Cookie`（防会话跨域串）
- `token`（原始 token 只以 `X-Octo-Token` 内网头形式过去）
- RFC 7230 §6.1 hop-by-hop 头 + `Trailer` / `Upgrade`（请求 & 响应两侧都清）

## ⚠️ 部署硬约束：反代→doc 必须走内网

`X-Octo-Uid` / `X-Octo-Name` / `X-Octo-Role` 是**内网信任头**：一旦 doc 服务
在外网可达，外网 caller 直接把这三个头打给 doc 就是身份伪造缺口。部署方**必须**
保证：

1. doc 服务（`OCTO_DOCS_UPSTREAM` 指向的地址）**只在内网监听**，外网无法直连。
2. 外网入口只暴露 octo-server 的 `/v1/docs/proxy/*`，所有请求必经反代过 Auth。
3. 如果 doc 服务不得不暴露到外网（不推荐），必须自行加一层信任 IP 白名单，
   只接受来自反代的源 IP，且在 doc 侧再验一次 `X-Octo-Token`（回打 octo-server）
   做保险 —— 这时方案 C 的信任头就退化成方案 A/B，性能优势没了。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `OCTO_DOCS_UPSTREAM` | *(空)* | doc 服务地址。未配置 → module disable，`/v1/docs/proxy/*` 返 404。 |
| `OCTO_DOCS_PROXY_TIMEOUT_MS` | `30000` | 反代到 doc 的 response header 超时（毫秒）。非法值回落默认。 |

### 废弃变量（OCT-145 方案 C 落地后不再使用）

- `OCTO_OPENAPI_URL`：方案 A/B 里 doc 侧用来回打 octo openapi 拿 userinfo。方案 C
  下 doc 侧直接消费 `X-Octo-Uid/Name/Role`，不再需要。**doc 侧配置可移除**。
- `OCTO_USERINFO_URL`：同上，方案 C 下 doc 侧不再调 userinfo。**doc 侧配置可移除**。

octo-server 侧本来就没读这两个变量，改动只影响 doc 侧部署清单。
