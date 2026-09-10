---
type: Task
title: "Task: project-prd-alignment"
description: Align Project names, membership mutations, permissions, Space-removal persistence, personal pins for Project group relations, and the 2026-09-11 explicit-create/pure-read revision with the confirmed design.
tags: ["project", "space", "auth", "wire-contract", "testing", "pin"]
timestamp: 2026-09-10T00:00:00Z
source: self
---

# Task: project-prd-alignment

本任务覆盖 Project 核心 HTTP 契约及验证：`/v1/space/:space_id/projects`、`/v1/projects/:project_id`、Project 成员添加/退出/Owner 转让入口、`/v1/groups/:group_no/project` 关联入口，以及个人置顶、纯读列表、成员单查和群角色筛选。Project 必须显式创建；GET 列表仅返回已有且当前有权访问的 Project，空列表不得产生 Project。

## Authority and scope

`docs/specs/2026-09-10-project-prd-alignment-design.md` is authoritative over the Project implementation. This task covers core Project service, HTTP models/handlers, shared route wiring, core DB access, error contracts, name migration, and Space-removal cascade behavior. Read behavior and group relation implementations are integrated through their exact contracts; no default-Project initializer or mapping is retained.

The implementation does not add Internal APIs, outbox/Redis event queues, ACL synchronization, or resource-side permission copies. Resource authorization remains a realtime query of current Project/group relationships.

Group relations are entry metadata only: association/listing never grants chat read, send, or subarea access; resource authorization continues to use the current independent relationship checks.

## Acceptance references

- Spec 1, 4: duplicate Project names are valid; names are required and at most 30 Unicode characters; literal `%`, `_`, and escape characters in search are preserved; existing names longer than 30 remain readable and only submitted renames are length-checked.
- Spec 5: list/detail/roster authorization requires active organization identity and active Project membership; pagination count and rows share filters/snapshot; arrays remain arrays and total uses `X-Total-Count`.
- Spec 6: Owner/admin/member permissions match the matrix; admins manage all non-Owner members including peer admins; ordinary members cannot use personnel add/remove; role updates never mint Owner; dedicated atomic Owner transfer makes the former Owner admin; Owner cannot leave, demote, or be removed.
- Spec 7: member additions use `{members:[{uid,role}]}` with roles 0/1, lock current organization and Project state in one transaction, deduplicate identical requests, reject role conflicts/invalid targets atomically, and safely re-admit removed rows with the requested role; organization removal denies access while preserving the Owner role record.
- Spec 10/11/12 where touched: no partial local mutation on failed core transactions, controlled lock order/current reads, migrated callers/tests/error responses, and documented external contract cutover.

## 2026-09-11 关联群个人置顶与纯读收口

- 新增 `PUT /v1/projects/:project_id/groups/:group_no/setting`，要求显式布尔 `pinned`；写事务重新校验有效 Space、Project 成员和当前群关联。Project 成员即使不是原生群成员也可置顶，偏好不授予聊天读取、发送或子区权限。
- `GET /v1/projects/:project_id/groups` 以当前用户的 Space/Project/群四元偏好返回 `pinned`，置顶项先于分页并按服务端时间倒序；同一请求重复置顶不刷新时间，取消后重置，解绑隐藏后同一 Project 重绑恢复。
- GET Project 列表为纯读；没有已创建且有权访问的 Project 时返回空数组和 `X-Total-Count: 0`，不自动创建默认 Project。
- `GET /v1/projects/:project_id/members/:uid` 补齐用户态单成员读取：沿用成员列表 `MemberResp` 投影和 `0/1/2` Project 角色，直接按 `project_id + uid` 有界命中，不走分页列表或内存筛选；调用者、当前组织和 Project 成员资格在同一 RR 只读事务复核，未知/已移除目标为既有 not-found，数据库错误不能降级。目标筛选与列表一致，不额外将目标账号或 Space 席位状态当作 Project 关系事实；响应只是成员事实，Drive 仍须自行做目标资格和 ACL 判定。Drive 需要关系判权时使用同一用户 session `token`（兼容 Bearer）和路径中的 `project_id`、目标 `uid` selector，不使用 internal token 或自报身份。
- `GET /v1/group/my` 角色筛选收口：继续直接返回 `GroupResp[]`；无参数/仅 `space_id` 保持原保存群/Space 已加入群语义；`role=owner|admin|owner,admin` 可与 `space_id` 组合并先校验 active Space，排除解散群、非活跃成员、外部管理脏角色及已撤权 Space；无 `role` 仅补回填 role，不改变旧查询。
- 已验证：Project 定向回归和完整 `go test ./modules/project -count=1`、group Project 关系测试、真实 TCP HTTP pin/list/cancel 与解绑重绑 smoke、`go build ./...`、i18n extract-check/lint。

## Non-goals

Do not introduce a default-Project initializer or mapping, a second relationship/authorization source, or remediation for invalid historical Owner data. Read lists are pure reads; group relation storage remains on the dedicated implementation path.
