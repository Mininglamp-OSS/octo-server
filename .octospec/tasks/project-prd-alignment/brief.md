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

除下文明确的 Drive provisioning boundary 外，本实现不添加其他 Internal API、outbox/Redis 事件队列、ACL 同步或资源侧权限副本。资源授权仍由当前 Project/群关系实时查询决定。

Group relations are entry metadata only: association/listing never grants chat read, send, or subarea access; resource authorization continues to use the current independent relationship checks.

## Acceptance references

- Spec 1, 4: duplicate Project names are valid; names are required and at most 30 Unicode characters; literal `%`, `_`, and escape characters in search are preserved; existing names longer than 30 remain readable and only submitted renames are length-checked.
- Spec 5: list/detail/roster authorization requires active organization identity and active Project membership; pagination count and rows share filters/snapshot; arrays remain arrays and total uses `X-Total-Count`.
- Spec 6: Owner/admin/member permissions match the matrix; admins manage all non-Owner members including peer admins; ordinary members cannot use personnel add/remove; role updates never mint Owner; dedicated atomic Owner transfer makes the former Owner admin; Owner can only be a human seat and cannot leave, demote, or be removed.
- Spec 7: member additions use `{members:[{uid,role}]}` with roles 0/1, lock current organization and Project state in one transaction, deduplicate identical requests, reject role conflicts/invalid targets atomically, and safely re-admit removed rows with the requested role; organization removal denies access while preserving the Owner role record.
- Project 专用成员候选接口 `GET /v1/projects/:project_id/member-candidates` 仅面向有效组织真人目录，Owner/Admin 才能访问；单次 RR 读快照完成权限、三状态（`current_user`、`already_member`、`invitable`）分类、字面姓名搜索、Project 分页和 `X-Total-Count`，active 且 `removing=0` 的 Project 席位才算已在，离开者可再次邀请，不暴露 email 等额外个人信息。
- Spec 10/11/12 where touched: no partial local mutation on failed core transactions, controlled lock order/current reads, migrated callers/tests/error responses, and documented external contract cutover.
- Project 建群服务的 `BotUID` 必须在同一建群事务中复核目标 Space 的 active 席位；复用现有候选人锁顺序。验收包括跨 Space Bot 拒绝且无部分写入、Space 内非 Project Bot 正常入群并设置 `bot_admin`、已在 Project 快照内的 Bot 保持单条成员记录及管理权限。普通成员准入、Bot 的 Project 成员资格要求和提交后 IM 策略不扩展。

## Sidebar and relation metadata contract

- Unified Sidebar Project sections are visible only to the caller's active Project
  membership (`Project.status=normal`, member `status=active`, `removing=0`).
  Space membership or a historical non-member pin does not create or repair a
  section; it never grants Project or native group access.
- Project `groups[]` is a relation-only projection, pinned first and capped at 50
  rows per Project in SQL. It intentionally does not filter native `group_member`
  status, including non-native or blacklisted relations, and does not grant native
  chat read/send/subarea/member permissions.
- The relation-only `groups[]` shape is an external client cutover. Coordinate the
  client migration before deployment; this repository does not claim that rollout
  is complete.

## Project–Drive provisioning boundary（2026-09-11）

- 经授权的 Drive provisioning 以 `project_id` 作为唯一管理与幂等依据；Project 侧不要求返回 Drive ID。
- 远端支持后由 `POST /v1/internal/drive/spaces` 接收 `X-Internal-Token` 和 `name`（使用完整 Project 名称、最多 30 个 Unicode 字符）、`octo_space_id`、当前 Owner `super_admin_uid`、`project_id`。只有同一 `project_id` 的完全相同重复请求可按幂等成功处理；其他 `409`、`401`、`500` 按重试/失败策略处理。
- `OCTO_DRIVE_INTERNAL_TOKEN` 必须与 Fleet HMAC 凭据分离；功能关闭时不得向远端出站。远端接口及其 30 字符名称支持是启用/部署前提，远端尚未提供时不得宣称已部署。

## 2026-09-11 关联群个人置顶与纯读收口

- 新增 `PUT /v1/projects/:project_id/groups/:group_no/setting`，要求显式布尔 `pinned`；写事务重新校验有效 Space、Project 成员和当前群关联。Project 成员即使不是原生群成员也可置顶，偏好不授予聊天读取、发送或子区权限。

- `GET /v1/projects/:project_id/groups` 以当前用户的 Space/Project/群四元偏好返回 `pinned`，置顶项先于分页并按服务端时间倒序；同一请求重复置顶不刷新时间，取消后重置，解绑隐藏后同一 Project 重绑恢复。
- GET Project 列表为纯读；没有已创建且有权访问的 Project 时返回空数组和 `X-Total-Count: 0`，不自动创建默认 Project。
- `GET /v1/projects/:project_id/members/:uid` 补齐用户态单成员读取：沿用成员列表 `MemberResp` 投影和 `0/1/2` Project 角色，直接按 `project_id + uid` 有界命中，不走分页列表或内存筛选；调用者、当前组织和 Project 成员资格在同一 RR 只读事务复核，未知/已移除目标为既有 not-found，数据库错误不能降级。目标筛选与列表一致，不额外将目标账号或 Space 席位状态当作 Project 关系事实；响应只是成员事实，Drive 仍须自行做目标资格和 ACL 判定。Drive 需要关系判权时使用同一用户 session `token`（兼容 Bearer）和路径中的 `project_id`、目标 `uid` selector，不使用 internal token 或自报身份。
- `GET /v1/group/my` 角色筛选收口：继续直接返回 `GroupResp[]`；无参数/仅 `space_id` 保持原保存群/Space 已加入群语义；`role=owner|admin|owner,admin` 可与 `space_id` 组合并先校验 active Space，排除解散群、非活跃成员、外部管理脏角色及已撤权 Space；无 `role` 仅补回填 role，不改变旧查询。
- 已验证：Project 定向回归和完整 `go test ./modules/project -count=1`、group Project 关系测试、真实 TCP HTTP pin/list/cancel 与解绑重绑 smoke、`go build ./...`、i18n extract-check/lint。

## PR887 审查收口（2026-09-11）

- B1：置顶配额只统计与当前 membership-only 读取一致的 Project（正常状态且调用者 Project 席位 active、`removing=0`）。成员移除/closing 后的历史 pin 不再占用槽位，因此在达到上限后仍能置顶新的可见 Project；偏好行保留以支持重新加入后的恢复。
- B2：Project-backed `POST /v1/group/create` 将预期准入拒绝映射为本地化错误 envelope：未知 Project 的语义状态为 `404`，非成员/禁用目标为 `403`，跨 Space 为 `409`，同时维持 D14 legacy wire `400`；未知数据库/IM 故障仍为内部错误。
- N1：`/v1/group/create` 在认证后使用共享 UID 限流；N2a 关系 bind/unbind 同时复核 Project 成员与原生群 owner/admin；N2b 已有 active 成员的角色冲突使整批添加原子失败。
- N3：钩子数量、I2、名称上限和管理员移除语义的注释已与当前独立原生群成员模型及实现同步。
- N4 是未修改的日期/时区基线问题；MySQL `SYSTEM=+0800` 时 Group 回归按 `TZ=Asia/Shanghai` 运行，Project 回归按 `TZ=UTC` 运行。
- 本轮以真实 TCP `http.Server` + MySQL 记录 B1 移除后换 pin、B2 非成员/跨 Space 4xx 响应；`go build ./...`、`make i18n-extract-check`、`make i18n-lint` 均通过。

- Sidebar review closure: membership-only Project visibility ignores historical non-member
  pin rows; relation `groups[]` keeps metadata-only semantics without native
  `group_member`/blacklist filtering and is SQL-capped at 50 rows per Project. The
  client field-shape cutover remains coordinated work and is not claimed deployed.

- P2：AI session container（`purpose=ai_session_container`）在 Group 关系 PUT/DELETE 路由和 service 层均拒绝；关系真实变更同步推进 `group.version`，而重复绑定/解绑不制造版本噪音。Project 关系读列表、分页计数及 Sidebar 批量读取同样排除历史绑定的 AI 容器，普通群关系不受影响。Project 关系与原生群模型保持独立，预设群可与 Project 关系并存，不新增警告或限制。
- 置顶写入错误保持 `%w` 链；`pinned=1,pinned_at=NULL` 的历史行在重复置顶时修复时间戳，正常重复置顶仍保持原时间。`GET /v1/group/my` 角色列表的成员计数查询失败直接返回 `query_failed`，不再以成功的 0 掩盖数据库错误。已删除无引用的关系辅助函数。
- F2：Project 对账保留昂贵的 `ReconcileEnabled` 门禁，恢复专属全员群 I4-A 缺群与 I4-B 缺员扫描；采用游标分页、宽限期、Space 撤权/移除/封禁/系统 bot 豁免，完整轮转后发布 gauge，普通 `group.project_id` 关联群保持独立成员快照且不触发该缺员监控。
- 移除 `project_i2_violations_total`、`group_admission_rejected_total` 及全员群 guard failure 计数器后的运维影响：旧面板/告警应删除或允许序列缺失，缺失这些指标本身不是服务故障。
- Bot/IM 提示、订阅和其他提交后通知按 best-effort 处理；已提交的数据库事实是权威，通知失败只记录并由既有补偿/重试路径处理，不应让客户端重复已成功的核心写入。
- Migration 采用 `joined_at` rolling expand：仅新增可空 `DATETIME(3)`，不做回填、不改为 `NOT NULL`；旧二进制可省略该列，读取以 `COALESCE(joined_at,created_at)` 统一，新增/重新加入写真实 UTC 时间，后续收缩迁移不在本版本。Cascade 保留 active human Owner 及其角色；仅在有 active non-Owner agent rider 时处理 rider，并对 rider 只执行一次 member_epoch/清理队列过渡，Owner-only 行不进入分页。
- F3/F4：Space 撤权时，在同一事务中降级并移除专属群的原生 Owner 成员，保留 Project Owner 身份。恢复按有效账号、active Space、active Project、当前专属指针和当前 Project Owner 投影最新真人 Owner，跨表 JOIN 显式使用兼容 collation；Space ID 由数据库按 `space` 表实际 collation 解析并返回存储值，不用 Go `ToLower`/`TrimSpace` 或字节比较。旧 `rejoined` durable 任务在 worker 边界解析原始 selector，解析/查询错误沿原 lease 重试；Space 缺失/解散时终止该 rejoined 恢复责任。普通 Space removal 保留原始 selector，即使 Space 缺失/解散也继续 fail-safe 清理，不因 canonical resolver 无结果静默 no-op。Project 恢复先前置复核 `Project.space_id` 归属，投影写事务再次校验 Space/Project/当前专属群绑定；Space `0→1` 与既有 Project seat 重新准入同事务写入 `reason=rejoined`；仅复用 pending、空 lease、`attempts=0`、`last_error=''` 的初始任务，已分页、已失败、claimed 或终态任务均建立新责任。迟到 `IMRemove` 的补订阅失败由清理回调持久化；通用 admission 返回错误，由已有投影任务重试，避免派生新任务。纯成功分页持久化 cursor 并归还 attempt；D4 仍取消全部旧 Project pending 任务，普通关联群不进入专属投影。
- 清理工单立即入队与分页续跑使用 UTC 毫秒精度，避免数据库四舍五入使工单短暂落在未来；队列租约和退避仍按原有规则执行。验证使用真实 WuKongIM，Space 包包含重复 `-race` 运行。
- 最新审查修复保持现有产品规则：Bot 守卫使用数据库规范群号；Space 撤权在旧专属指针失效后继续按普通群清理，Project-only 清理仍限于当前专属群；I1/abandoned 扫描仅在 Project 正常时豁免 Space 撤权或 Space 解散后写侧保留的 Owner 身份，Project 已解散或不存在但仍 active 且无有效 Space 席位的 Owner 仍须报告，普通成员仍报告；两类查询保持 LIMIT inspected base 分页，生命周期/席位状态留在 SELECT flag 而非过滤返回行的 WHERE。
- Rejoin 真实失败同时保留失败页的输入游标和错误摘要，沿用原有 attempt/退避；普通成员清理、Owner 身份保留、普通群继任者选择、通知 best-effort 和 Drive 契约保持既有语义。已应用迁移保持不变，当前监控及任务原因口径记录于原规格。
- 验证（本轮）：`^TestReconcileOwnerLifecycleEligibility$` 先红后绿，确认正常 Project/Space 解散仍豁免 Owner、Project 解散/不存在的 active Owner 与普通成员由 I1/abandoned 报告；`TestSpaceRemovalRejoinRestoresProjectOwnerForRawSpaceID` 的真实 HTTP raw Space ID roundtrip 隔离 `-race` 通过，混合 Owner fixture 隔离验证通过。完整 `-race`：Group 68.086s（`TZ=Asia/Shanghai`）、Project 48.546s、Space 19.633s、Bot API 79.816s、`pkg/space` 1.901s；build、受影响包 vet、i18n extract-check/lint 均通过。
- Drive client 名称上限保持 64 字符，使用 Unicode 字符计数；30 字符中文 Project 名称与 64 字符混合中文/emoji 原样通过 HTTP 发送，65 字符在出站前拒绝。该边界回归先失败后通过，`internal/projectprovision` 完整 `-race` 与全仓 build 通过。

## Non-goals

Do not introduce a default-Project initializer or mapping, a second relationship/authorization source, or remediation for invalid historical Owner data. Read lists are pure reads; group relation storage remains on the dedicated implementation path.
