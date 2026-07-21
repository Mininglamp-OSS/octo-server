# L2b 卡片 owner 清单

> 本文档是 L2b 业务自定义卡片(external card providers)owner 命名空间的**权威清单**,
> 与 `pkg/cardtmpl/registry.go` 里的 owner 白名单校验互为镜像。
>
> **状态**:占位。L2b 通道本 PR (`.octospec/tasks/cardtmpl-registry-pilot`) 不开放,
> 清单为空。开放条件见 `docs/platform-card-base.md` §2.2 约束 5。

## 清单

| owner | vendor | contact | callback_domain_allowlist | created_at | notes |
|---|---|---|---|---|---|
| _(none)_ | — | — | — | — | L2b 通道未开放 |

## 变更流程

1. 业务方以 PR 形式提交条目;
2. **两名平台组维护者** review + approve;
3. 条目字段全部必填,不允许 `TBD`;
4. `owner` 值必须以 `ext.` 前缀开头,格式 `ext.<vendor>.<domain>`(小写、点分隔、无下划线);
5. `callback_domain_allowlist` 是业务方 callback URL 的**域名白名单**,`RouteSpec.URL` 必须匹配;
6. 条目登记后,同步在 `main.go` composition root 里显式 Register 对应 Template(仅当 L2b 通道已开放);
7. 回收 owner:删除清单条目 + 从 composition root 移除 Register + 保留 6 个月降级观察窗口。

## 与代码的镜像关系

- `pkg/cardtmpl/registry.go` 的 `l2aOwnerAllowlist` 内含 L2a 平坦 owner(`docs`/`summary`/`notify`/`action`);
- `l2bOwnerPrefix = "ext."` 常量固定,凡以 `ext.` 开头的 owner 会走 L2b 分支,注册期检索本清单;
- 本 PR 阶段 L2b 分支**默认拒绝**(任何 `ext.*` owner → 注册期 panic),清单空;
- 未来 L2b 开放 PR 需同时:①解锁注册期允许 `ext.*`;②本清单加条目;③启动期 self-check 断言清单与代码同步。
