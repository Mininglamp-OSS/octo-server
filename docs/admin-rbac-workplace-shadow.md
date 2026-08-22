# octo-admin workplace RBAC shadow 观察说明

本阶段只对 octo-admin 的 workplace 全局管理面进行 RBAC shadow 求值，覆盖分类、分类应用、应用和横幅的 18 个已声明 operation。现有 `CheckLoginRole` / `CheckLoginRoleIsSuperAdmin` 仍是唯一的 HTTP 放行依据；shadow 结论不会返回新的 401/403，也不会替换 legacy gate。

## 开关与回滚

默认关闭。需要在受控环境显式设置：

```text
OCTO_ADMIN_RBAC_WORKPLACE_SHADOW=1
```

接受的开启值为 `1`、`true`、`yes`、`on`（不区分大小写）。清空、设置为 `0`、`false` 或其他值即可关闭。关闭后不执行 workplace RBAC 查询，也不产生 shadow 观测；回滚不需要数据库 schema 或授权绑定回滚。

## 观测字段与结果

事件只包含：`uid`、`operation_id`、`permission`、`legacy_allowed`、`rbac_allowed`、`outcome`，以及错误事件的 `error_kind`。

`outcome` 取值如下：

- `match`：legacy 和 RBAC 结论一致。
- `legacy_allow_rbac_deny`：legacy 放行、RBAC 不包含对应 permission。
- `legacy_deny_rbac_allow`：RBAC 包含对应 permission、legacy 拒绝。
- `rbac_evaluation_error`：effective-permissions、缓存或求值失败；继续沿用 legacy 结果。
- `mapping_error`：workplace operation 或 permission 映射异常；继续沿用 legacy 结果。

事件走进程内日志观测出口，不写角色变更、授权变更、敏感访问审计或其他持久化历史。请求体、token、业务对象内容、Group/Space/Robot ACL 和资源 scope 均不得进入事件。

## 灰度检查与处置

灰度期间按 operation、permission、结果类型观察映射完整性、差异比例、RBAC/cache/mapping 错误和请求延迟。发生异常时先关闭 `OCTO_ADMIN_RBAC_WORKPLACE_SHADOW`；由于 legacy gate 仍实际执法，关闭开关即可停止额外查询和观测，不影响 workplace 既有接口行为。

本阶段明确不观测和不改动 `/v1/groups/**`、`/v1/space/**`、`/v1/robot/**`、其他 manager module、`/v1/manager/me`、`/v1/manager/rbac/**` 或 marketplace Skill/MCP/Expert 权限。
