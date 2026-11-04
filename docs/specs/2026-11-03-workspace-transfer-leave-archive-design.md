# 工作空间转让、退出与归档：Server 契约

日期：2026-11-03

## 背景与确认依据

本规格记录 OCTO-59 已确认的后端设计，依据为[工作空间转让、退出与归档 PRD](https://im.deepminer.com.cn/d/d_35cb0296849311b8f105a571)及本区最新产品确认：归档期间允许所有权转让和非所有者自行退出；归档取消排队及正在执行的任务；恢复按各自动化、触发器原启停状态继续，错过的触发不补跑。

Server 的 Project 是成员、所有权与归档状态事实源；Fleet 承载执行；Drive 计算独立授权与 Project 同步授权；Docs 合并文档独立授权与 Drive 派生读取权。实现采用现有成员、生命周期与事件机制，权限变化触发重验，活跃文档合并检查授权源版本。本文为设计约定，不表示业务代码、联合验收或部署已完成。

## 范围

### 包含

- 复用现有所有权转让、主动退出、角色修改和成员读取接口；补齐归档状态下的用户写入限制。
- 归档、取消归档、活跃/已归档列表及成员级归档提醒的后端契约。
- Server 与 Fleet 的生命周期读取、事件和准入版本契约，以及 Drive 消费成员与归档事实的边界。
- 保留 Project 成员资格、历史和独立资源授权；真实成员撤权、组织和账号强制撤权继续生效。
- 四个后端之间的契约、依赖及联合验收标准。

### 不包含

- Buddy 前端、样式、交互或 UI 测试；这些由客户端团队实现。本区 Buddy worktree 仅只读定位。
- 修改 Drive/文档的独立 ACL、资源归属或数据；不新增权限复制服务。
- 修改群原生关系、组织级所有权接管、资源移除、回收站或已取消执行的恢复。
- 生产部署或 Loop 状态/指派/评论修改；提交与评审请求按用户的独立发布授权执行。

## 现有转让与退出的核对结论

| PRD 行为 | 已有机制 | 本单处理 |
| --- | --- | --- |
| 当前所有者转让给现有合格真人成员 | `PUT /v1/projects/:id/owner`，body `{"uid":"..."}`；事务复核操作者、目标组织席位、Project 席位及真人资格 | 复用接口及事务 |
| 接任者为唯一所有者，原所有者成为管理员并留在空间 | `transferProjectOwnerOnce` 在 Project 行锁内提升目标、降级原所有者后提交，失败回滚；不调用退出 | 复用；保留并发唯一所有者回归 |
| 成员、管理员独立退出 | `POST /v1/projects/:id/leave`；本人有效席位进入移除状态，事务内更新 epoch、撤权事件及清理工单 | 复用，不耦合基本信息编辑权限 |
| 所有者不能直接退出 | HTTP 层 `can_leave=false`，服务层有 last-owner 保护 | 保持既有错误契约；归档期间同样先转让、再由本人单独退出 |
| 退出后失去 Project 访问 | `removing=1` 已不具备成员资格，随后清理完成关闭席位；缓存立即失效 | 复用；重新加入仍走现有邀请/管理员添加流程 |
| 退出保留其他成员内容 | Server 关闭本人及随本人的 Agent 席位，Fleet 延续既有执行撤权；不调用工作空间解散或共享内容删除 | 保持既有撤权，不删除共享任务、文件或历史 |
| 归档期间转让和自行退出 | 复用现有接口及成员事务 | 当前所有者可转让；管理员、普通成员可自行退出；转让后仍归档，不放开邀请、改角色或移除其他成员 |

代码依据：`modules/project/api_member.go:194-225,280-320`、`service.go:118-127,1435-1484,1567-1646,1800-1933`、`db_removal.go:14-27,78-91`。现有回归包含 `TestOwnerTransferIsDedicatedAndAtomic`、`TestConcurrentOwnerTransfersKeepOneOwner`、`TestOrdinaryMemberCanLeaveWithoutOwnerTransfer`、`TestOwnershipCannotTransferToExSpaceMember`；存在测试代码不代表本轮或线上已通过。

## 状态、权限与数据

### 状态事实

- 保留现有 `octo_project.status` 的 normal/disbanded 语义。可逆归档使用独立 `archived_at`，不借用 disbanded、不增加第三个 status 值。
- `archived_at IS NULL` 为活跃；非空为归档。归档/恢复动作本身不改变成员、角色或 `member_epoch`；归档期间发生的转让、退出和安全撤权正常改变名册及成员版本。
- 复用 `lifecycle_version`；每次真实归档或恢复在同一事务内递增一次并写入 outbox。相同状态重复请求不改变版本、不重复发事件。
- 新增 `archive_version`，记录最近一次进入归档时的 `lifecycle_version`，作为稳定的归档轮次。恢复不清零；下次归档替换为更高版本。新字段使用可空时间及非负 BIGINT，现存 Project 默认活跃、归档轮次为 0。
- Project 列表、详情及内部全量关系读取增加 `archived_at`、`archive_version`、`lifecycle_version`；不改变已有数字 `status`、成员计数或角色编码的含义。

### 读取与业务写入

- 有效账号、Space 资格、Project 成员资格及资源原有权限仍是读取前提。归档不会授予新的读取权，也不会将现有成员伪装为非成员。
- 归档时保留详情、成员列表、关系事实和历史读取。`_verify` 仍返回实际成员和实际 `member_epoch`，epochs 不因归档返回 0；Space 禁用、组织撤权、账号停用及 Project 解散的原有拒绝语义不变。
- 归档时禁止基本信息、成员添加/移除、角色/协作角色和工作空间关系等一般业务写。既有路由的最终写事务按原锁顺序复核状态与当前身份。
- 显式允许：当前所有者转让给现有合格真人成员；管理员、普通成员自行退出；当前所有者恢复；有效成员记录本人的提醒关闭状态及取消本人置顶。取消置顶只释放个人配额，不改变成员或工作空间业务数据；归档期间仍不允许新增置顶。转让不改变归档状态，原所有者成为管理员后可单独退出；所有者不能直接退出。组织/账号强制撤权及清理工单继续执行。
- `can_transfer_owner` 对当前有效所有者为真，与是否归档无关；`can_leave` 对当前有效非所有者成员为真，与是否归档无关；`can_archive` 仅活跃空间的所有者为真，`can_restore` 仅归档空间的所有者为真。其他业务写能力在归档期间为假，读取仍按当前资格判定。
- 转让与恢复均在 Project 行锁内复核当前所有者；接任者仍须具备有效组织/Project 席位及真人资格。恢复后不恢复已退出成员，不把归档时的名册或所有者快照覆盖到当前关系。

### 网盘与文档

Server 始终返回真实成员、角色与版本，不通过伪造 owner/admin 角色实施资源只读。Drive 将 Project 同步权限在归档期间封顶到读取，再与真实独立授权合并；退出/移除只撤销同步来源，其他有效 Project、挂载及独立授权仍有效。恢复依据当前有效成员和角色重新派生。Docs 的 Drive 来源仍只授予 `reader|none`，独立 owner、doc_member、share 授权不随 Project 归档或退出被改写，也不把 Project 归档映射为文档自身归档。具体鉴权、存量连接重验及授权源版本见 Drive/Docs 配套规格。

## 用户 API

所有路由复用现有用户认证、Space/Project 资格、UID 限流、i18n 错误 facade 和事务锁顺序；不接受客户端自报操作者 UID 或角色。

归档/恢复同时要求既有 Project 写开关与生命周期集成有效，且目标已有匹配的 Fleet Workspace；未配置投递或未完成绑定时返回既有禁用/未就绪语义，不改变归档状态。转让/退出继续遵守各自原启用条件，归档不是它们的拒绝理由。

| 接口 | 契约 |
| --- | --- |
| `PUT /v1/projects/:id/archive` | body 必填 `{"archived":true}` 归档或 `{"archived":false}` 取消归档；仅当前有效真人所有者。重复目标状态幂等，返回当前 Project DTO |
| `GET /v1/space/:space_id/projects` | 增加 `archive_status=active|archived|all`；默认 active。沿用 page/limit、数组及 X-Total-Count；过滤、计数、角色、capabilities 来自同一只读快照 |
| `GET /v1/projects/:id` | 归档项目仍对有资格的用户可读，返回归档字段及当前 capabilities |
| `GET /v1/space/:space_id/projects/archive-notices` | 当前用户尚未手动关闭的本轮归档提醒；仅当前有效成员且项目仍处归档，沿用 page/limit、数组及 X-Total-Count。条目含 project_id、name、archive_version、archived_at，不返回其他成员偏好 |
| `PUT /v1/projects/:id/setting` | 扩展可选 `archive_notice_dismissed_version`，仅关闭本人当前归档轮次；归档期间允许 `pinned:false` 取消本人置顶，仍拒绝 `pinned:true`。两项可同请求提交，沿用设置响应，仅更新请求指定的本人偏好 |

`octo_project_user_setting` 增加 `archive_notice_dismissed_version`，默认 0。关闭请求与当前归档轮次相等时记录，重复关闭幂等；过期轮次不能关闭新的提醒，返回归档轮次冲突。既有 pin upsert 必须保留该字段，提醒 upsert 必须保留 pin。`pinned` 为三态：1 置顶、0 显式取消、NULL 未设置；提醒首次建行写 NULL，pin upsert 写 0/1，侧边栏的 `pinned = 0` 过滤只命中显式取消。提醒读取不预先为所有成员批量建记录；离线成员下次查询自然取得尚未关闭的提醒。恢复后不返回本轮提醒，再次归档使用新的轮次。

个人置顶配额继续统计有效成员现存的置顶记录，包括归档项目；本人可取消归档项目置顶以释放配额，再置顶其他活跃项目。恢复空间不复活已取消的个人置顶，也不引入额外配额恢复策略。对已经置顶的活跃项目重复提交 `pinned:true` 仍校验当前身份，但不刷新 `pinned_at`、不改变列表顺序；其他同时提交且有效的偏好照常独立处理。

错误语义：不存在/无访问资格保持现有反枚举结果；无管理权保持 permission_denied；归档下的一般业务写返回 `err.server.project.archived`，不用于拒绝合法转让、非所有者退出或本人取消置顶；旧轮次关闭返回 `err.server.project.archive_version_conflict`。沿用 wire 400 与嵌套语义状态约定。数据库失败不能返回空列表或成功；所有者直接退出沿用既有错误，不另立一套字符串协议。

## Server → Fleet 契约

### 权威读取与事件

- 新增内部 `GET /v1/internal/projects/:project_id/lifecycle?space_id=...`，使用现有 membership internal token、scope 校验、strict-IP 限流和超时策略。
- 成功返回 project_id、space_id、lifecycle_version、archive_version、archived_at。只有有效、已 activation 且父 Space 有效的 normal Project 返回 200；不存在/跨 Space/未激活/父 Space 不可用返回 404，凭证失败 401，存储失败 500。响应 no-store，不携带名称、描述、成员名册或用户内容。
- 该接口仅回答生命周期事实，不授予成员资格。Fleet 仍使用既有成员判权；不把生命周期读取结果当作用户授权。
- 真实状态变更在 Project 事务中复用 `project.archived`（payload `{"reason":"owner_requested"}`）和 `project.restored`（payload 示例 `{"archive_version":12}`，引用本次恢复对应的归档版本），带现有 `project_version=lifecycle_version`。恢复引用的 archive_version 必须为正且小于本次恢复版本。
- 沿用已有 HMAC、event_id、outbox、逐 Project 顺序、重放与重试终态。Fleet 同时校验恢复引用的归档轮次已经完成副作用，不能只依赖传输顺序。事件里不增加项目名称和描述；不新增第二套队列、凭据或后台作业系统。
- `project.metadata_updated` 的 payload 冻结同一事务的 archive_version（首次归档前为 0）与 archived 布尔值，不包含名称/描述。它与其他会推进 external_version 的事件均受归档/恢复前序约束：归档轮次必须已经应用，本地状态还须匹配事件快照；缺少前序时可重试 503、不推进版本或固化回执。不能让更高版本的资料更新绕过漏投的取消/恢复动作。

### 准入与异步副作用

归档成功响应表示 Server 的归档状态和 outbox 已提交。Fleet 的取消任务等副作用按既有事件链收敛，不向用户增加待处理作业入口，也不声称 HTTP 成功意味着所有进程已物理退出。

为防止事件尚未消费时继续接收新业务写/执行，Fleet 在业务写、新调度与执行准入时读取权威 lifecycle：归档直接拒绝；Server 为活跃但 Fleet `external_version` 尚未追上时暂不准入，返回明确暂不可用。普通成员资格缓存不能替代该状态读取。

权威读取不直接推进 Fleet 的 `external_version`，也不绕过事件副作用。Fleet 在归档副作用成功的同一事务中保存 `last_applied_archive_version`；恢复事件必须匹配该归档轮次才能推进版本。若先收到恢复而前序归档尚未应用，返回可重试 `503 PROJECT_ARCHIVE_NOT_APPLIED`，回滚该次事件接收事务、不固化失败回执，也不改变 workspace 状态/版本；前序归档处理完成后，同一恢复 event_id 可重试成功。只有本地状态、已应用版本与权威活跃状态一致，才恢复业务写及新增执行。既有 outbox 重试耗尽也不能越过这个屏障。已在归档提交前完成准入的请求遵循原事务边界；本单不引入跨 MySQL/PostgreSQL 的分布式事务。

metadata_updated 在 workspace 行锁内要求 payload.archive_version 等于 last_applied_archive_version、payload.archived 等于本地归档状态；不满足时返回可重试 503 PROJECT_WORKSPACE_NOT_READY。这样 active/v1 不能直接跳到归档/恢复后的 metadata/v4；已经 archived/v2 但漏掉 restored/v3 时，也不能让 metadata/v4 抬高版本后丢弃恢复。生命周期权威读取仍不能替代应用这些副作用。

## Server → Drive 授权变化提示

成员移除、退出、角色/所有权等真实关系变更继续同事务推进 member_epoch。Drive 每次新判权读取 epochs、到期批量核验成员；epochs、_verify 与 lifecycle 的成功及鉴权、限流错误响应均 no-store，epochs 每批最多 50 Project，_verify 每批最多 50 UID。账号封禁不推进 Project epoch，不能靠版本相等延长成员证明；本次 peer 硬上界为 30 秒，Drive 成员缓存最长 15 秒。

增加 Drive 提示目标 URL（由 `OCTO_PROJECT_DRIVE_ACCESS_CHANGED_URL` 配置），复用既有 Server→Drive 内部 token，调用 POST /internal/v1/docs/project-access-changed；发送 event_id、space_id、project_id、subject_uid、member_epoch。它是来源失效提示，不是生命周期 HMAC 接收端，也不要求 Drive 接收 Fleet 的归档/恢复事件。

在既有 member_revoked 出站尝试旁路发送提示。Fleet 仍是该 outbox 的唯一权威目标：先完成 Fleet 本次投递及状态记录，Drive 提示不占成员事务/事件行锁；失败不改 Fleet 的 success/attempt/last_error，不阻塞后续事件的语义推进，不把单目标 outbox 宣称为多目标可靠投递。调用沿既有短超时、逐条有界处理，不创建无界 goroutine 或第二套持久队列；联调计入实际 worker 新增耗时。通知关闭、失败或被丢弃时，Drive/Docs 主动读取真实来源版本仍须收敛。

Drive 接收及 Docs 版本/会话契约分别见其配套的 2026-11-03 规格。归档/恢复不改成员事实，Drive 写权按 lifecycle 读取约束；不能为了产生通知把角色改成 reader 或增加虚假成员变更。


## 失败、恢复与发布

- Server 状态/outbox 任一写入失败，整笔事务回滚；没有成功响应便不能由客户端预先改变成员或空间状态。
- 网络中断不能被客户端当作一定失败；通过既有 Project 详情读取当前归档状态，再幂等提交目标状态，不新增持久化作业界面。
- Fleet 事件失败按已有 outbox 策略重试/终止；未追平期间阻止新写与执行，历史读取在资格有效时继续。既有事件永久失败的处理沿现有运维机制，不静默放开准入。
- 恢复开放原权限下的写和新执行，不复活已取消任务或已撤销凭据。Fleet 保留自动化 status、触发器 enabled 原配置，归档状态控制能否执行；恢复时重新计算未来调度时间，不补跑。
- 先完成兼容字段迁移、Fleet 事件消费、Drive 授权和 Docs 重验链路，再启用相应后端能力。启用前排空忽略归档语义的旧实例，不能只依赖调用方限制写操作。
- 归档能力启用前，旧格式 metadata_updated 事件须排空，旧生产/消费实例须退出；随后使用冻结 archive_version/archived 的新线协议。不得在投递时按当前状态补写旧 payload，破坏事件发生时的事实。
- 新字段为扩展迁移，不回填或改写成员、历史、权限和现有 ID。迁移和应用回滚均不能把已有归档状态静默变成活跃；存在归档数据时，不允许回滚到忽略归档的旧写服务。

## 验收标准

1. 活跃及归档空间均可转让给有效真人管理员或成员；目标唯一 owner，原 owner 为仍有效的 admin；并发两次转让最多一次成功。非法目标或事务失败不改变角色，转让不隐含退出或恢复空间。
2. 活跃及归档空间的普通成员、管理员可独立退出；所有者在 HTTP 和服务层均不能直接退出。退出后的下一次 Project 访问无权，其他成员及共享内容保留，再加入走既有邀请流程。
3. 归档/恢复动作不改变成员、角色或 member_epoch；期间的转让/退出正常更新当前关系。归档仍对有资格成员提供详情、成员及历史读取，不向非成员新增授权。
4. 归档时仅指定的转让、本人退出、恢复、本人提醒确认、本人取消置顶及原有安全撤权例外可写；邀请、改角色、移除其他成员、改资料及新增置顶仍拒绝。直接 API、CLI 与服务层判定一致。覆盖达到置顶上限、所有者归档、成员取消个人置顶后为其他活跃项目释放配额，以及恢复后不复活原置顶；重复置顶不得改变原顺序。
5. 活跃/已归档过滤、计数与详情一致；Drive 查询用户 Project 列表需显式包含归档项，不能因默认 active 丢失仍有效的网盘入口。
6. 仅当前 owner 可归档/恢复；归档后转让再恢复必须由新 owner 操作，缓存中的旧身份不能放行；重复目标状态不增版本或重复副作用。
7. 离线成员重新进入工作空间能读到未关闭提醒；关闭只影响本人当前归档轮次；跨设备保持，重复关闭幂等，旧轮次请求不能关闭新轮次。
8. 延迟、重复和乱序事件、快速归档/恢复及 Fleet 暂不可用时，不能越过生命周期准入版本门禁；特别覆盖 active/v1 先收到 restored/v3（引用 archive_version=2）、后收到 archived/v2：恢复首次不推进版本，归档副作用提交后恢复重试才成功。读取状态不能跳过归档取消副作用。
   另覆盖 archived/v2、restored/v3 投递耗尽后收到 metadata_updated/v4：仍拒绝推进，应用 v2/v3 后 v4 才可成功；仅漏恢复时也不得由 metadata 抬高版本把恢复丢弃。
9. 联合验收覆盖仅同步授权、仅独立授权、两者并存及多挂载：归档限制同步写、保留读取；退出仅移除本 Project 来源；独立编辑权和其他有效来源保留，恢复不复活已退出成员。
10. 归档取消排队及运行执行，恢复保留每条自动化和触发器原开关且不补跑。Docs 新请求、旧 token 与已连接会话都不得无限保留已撤销的同步读取权；通知缺失须能由来源版本检查发现，不能只验证当前 HTTP 请求。

## 规格归属与验证状态

既有转让/退出语义继续由 [Project 行为与权限对齐设计](2026-09-10-project-prd-alignment-design.md) 管理；本规格新增归档及其对旧接口的约束。服务间权威契约见 [项目生命周期契约](../project-lifecycle-contract.md)；Fleet 消费实现及路由验收见 `octo-fleet/docs/specs/2026-11-03-workspace-archive-readonly-design.md`，不在两个仓库重复定义同一 API。

本区本轮实现补齐归档期间 Owner 转让、非 Owner 退出及 Owner 禁止退出，metadata_updated 同事务冻结 archive_version/archived，并在 Fleet lifecycle 结果入账后为 member_revoked 旁路提示 Drive；epochs、_verify 与 lifecycle 的成功及鉴权、限流错误响应设置 no-store。隔离 MySQL/Redis 上的 modules/project、modules/internal_membership、pkg/project 非缓存回归已通过；真实本地 HTTP 已核对 no-store、归档后转让及退出、生命周期事件重复/乱序、按成员及归档轮次关闭提醒。Server、Fleet、Drive 与双节点 Docs 的联查已覆盖权威成员变化、提示丢失、来源账号停用及有限期证明到期；各消费端的执行与协作验收记录由对应规格管理。生产部署须另行批准，隔离验证不代表运行环境已启用。

审查修复后，`modules/project` 整包回归通过；真实本地 HTTP 确认首次关闭归档提醒后项目仍在侧边栏，返回的 `pinned` 仍为布尔值。提醒偏好使用 `pinned=NULL` 区分未设置与显式取消置顶。原生隔离 MySQL 已演练本次迁移 Down/Up，显式置顶与取消置顶均保留，重新升级后未设置偏好仍默认 NULL。`modules/category` 整包回归遇到既有限流响应 429，未修改该包或限流规则，不声明整包通过。

置顶审查修复在真实隔离 MySQL/Redis 上通过 `modules/project` 整包非缓存回归（81.300s），覆盖满配额后归档、取消本人置顶、置顶其他项目及恢复不复活偏好；重复置顶保留时间和排序；取消置顶可同时关闭本轮提醒。实际本地 HTTP 另行验证配额拒绝 → 归档取消置顶 → 新项目置顶成功、归档新增置顶仍拒绝及重复置顶不改时间，成员席位保持不变。错误响应隐私与 Common appconfig 两种响应分支的容器标识不泄露由真实 HTTP 路由回归验证。

全局限流回归先在真实 Redis 上复现三条权限事实路由 429 缺少 `no-store`，修复后通过；原生 loopback HTTP 另行验证同一全局桶耗尽后 epochs、`_verify`、lifecycle 均返回 429 和 `no-store`，普通路由仍受限流且不新增缓存策略，既有 health 豁免保留。本轮 smoke 仅启动生产全局限流 middleware 与 HTTP 路由，不启动业务模块。Project 错误呈现回归覆盖所有已注册 Project 5xx：仅安全固定的 `not_ready` 就绪提示公开，其余保持通用内部响应，raw error、uid、token 及私有详情均不出站。

原生生命周期证据：[`../evidence/2026-11-03-project-lifecycle-native.json`](../evidence/2026-11-03-project-lifecycle-native.json)。

`modules/project` 回归使用真实 MySQL/Redis，必须指向隔离验收服务。只读关系、生命周期、权限源变化及多服务异常需联合验收；不能借用默认业务数据库清表。
