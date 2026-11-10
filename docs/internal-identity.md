# 内部身份查询（update1.1.0）

`POST /v1/internal/auth/identity`，operationId `identity.resolve`。
调用方以 `X-Internal-Token` 提交 `OCTO_MEMBERSHIP_INTERNAL_TOKEN`；不接收
业务 Token、用户登录会话或 Bot Channel Key 作为服务身份。

请求 `{user_id, space_id, bot_id?}`；ID 最长 40 个字符，不接受空白包装。
`bot_id` 可选，未绑定时不传或留空。请求必须为 JSON 对象，拒绝未知字段、
错误字段类型及超大 body。

成功返回 `{data:{user,space,membership,bot}}`，`bot` 为单个对象或 null。
`membership.role` 为用户在该 Space 的角色（0 成员、1 管理员、2 所有者），
不是 Space 自身属性或功能权限。名称来自当前用户／Space 资料。

先查询有效用户、Space 及用户成员关系，返回名称和成员角色；仅在提供
`bot_id` 时，再查询属于该用户的有效 BF Bot 及其资料名称。不查询应用 Bot，
不重复检查 Bot 的 Space 成员关系；同 Space 由 Bot 创建与关联流程保证。

两次查询共用一个只读 repeatable-read 快照，总计 3 秒超时（包含连接池等待）。
不绑定 Bot 时只执行一次 SELECT；绑定时执行两次。ID 按字节精确匹配，
保留索引定位条件，不因不同 collation 合并身份。

用户、Space、用户成员关系或指定的 Bot 不可用时统一返回 404，不返回部分
身份。未提供 `bot_id` 时返回 `bot: null`；成功查询的 Bot 为 `is_active=true`。

身份接口直接使用模块原有的 `internalAuthMiddleware()` 和 `internalToken`，
沿用相同的 Secret 校验、跨用途冲突检查及启用配置（包括 peer-cache-age 声明）。
服务端查询当前身份；调用方自行决定结果缓存策略和有效期。
接口复用模块现有 `ipLimit`，在 Secret 校验前限流，与成员及项目接口共享
同一 IP 配额和配置，不新增独立限流器。

错误返回沿用现有内部接口协议：`error.{code,message,details,http_status}`
及兼容字段 `msg/status`，保留已注册错误码和真实 HTTP 状态码。

数据库用例通过 `OCTO_IDENTITY_TEST_MYSQL_DSN` 指定隔离环境，未配置时跳过；
HTTP 用例无需外部依赖。
