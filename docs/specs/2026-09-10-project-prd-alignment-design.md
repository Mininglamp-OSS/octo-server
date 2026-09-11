# Project 行为与权限对齐设计

日期：2026-09-10

状态：设计已确认；实现已在本工作区落地并完成针对性测试、真实依赖验收和 TCP HTTP smoke。外部客户端契约切换及部署仍由对应维护者负责。

## 背景与目标

基于最新 main 改造现有 Project，使其满足 `.local/assets/prd.md` 第 3.1.5、3.3.2、3.4.1–3.4.4 节的协作单元要求。产品、接口、数据模型和本文统一使用 Project 名称。PRD 是行为与权限依据，当前实现只提供可复用的工程基础；与目标冲突的现有规则需要修改。

用户已确认现有 Project 有唯一 Owner，沿用现有所有权和 ID。原开发工作区保持不变，实施在从 main 建立的独立分支进行；开始实施时重新核对 main 的变化。

## 范围

### 包含

- Project 创建、详情、修改、列表搜索。
- 成员读取、添加、移除、角色调整、Owner 转让和退出。
- 群与 Project 的关联查询、绑定、换绑、解除关联，以及关联群列表和创建群入口。
- 所需数据库迁移、调用方契约调整、错误响应、行为验证及文档同步。

### 不包含

- 除下文明确的 Drive provisioning boundary 外，不包含其他 Internal API、outbox、Redis 事件队列。
- 资源侧授权引擎、权限同步和撤销通知。资源侧实时查询关系判权，关联变化自然改变授权依据。
- Drive 资源内容、文件权限、ACL 和资源侧授权不在本设计范围；Project→Drive provisioning boundary 见下文，Drive ID 不作为 Project 响应事实。
- 扩展 Project 删除、归档产品能力。既有生命周期执行链是否与目标发生冲突，应在实施时按本文件权限不变量核对；不借本次添加新的生命周期机制。

## 核心决策

| 项目 | 目标 |
| --- | --- |
| 标识与存储 | 复用现有 Project ID 和业务数据；不建立第二套协作实体 |
| 名称 | 必填，最多 30 个 Unicode 字符；同组织允许重名 |
| Owner | 始终一名；只能由真人席位担任，并只能通过专门的身份转让改变 |
| 管理员 | 可以管理非 Owner 成员，包括其他管理员；最高授予管理员 |
| 组织与 Project | 组织身份是访问前提，不自动授予 Project 管理权限 |
| Project 创建方式 | Project 必须显式创建；列表是纯读，不因首次进入或读取自动创建默认 Project |
| 群关联 | 原生群管理权限与 Project 成员资格独立校验；仅 `all_member_group_no` 指向的群是 Project 专属全员群 |
| 全员群 | 专属群实时投影有效 Project 成员和真人 Owner；普通 Project 关联群保持原生快照 |
| 资源判权 | 消费当前关系，不增加异步授权同步体系 |
| Project 与 Drive | 以 `project_id` 管理 provisioning；不要求 Project 响应返回 Drive ID |

## 基础信息与列表

1. 创建、改名和数据库约束共同允许重名。通过新增迁移解除有效名称唯一约束，不重写已应用迁移；名称不能用作身份键。
2. 新建和修改名称最多 30 个 Unicode 字符；已有更长名称保持原值，只有提交名称变更时执行新长度校验，其他字段更新不受影响。
3. 支持名称搜索；`%`、`_` 和转义字符按字面匹配，兼容数据库 SQL 模式。
4. 列表只返回当前组织内本人有效加入的 Project。详情和成员信息同样校验有效组织身份及 Project 成员资格，不以组织管理员身份绕过。
5. 列表和详情提供名称、描述、头像、成员数、本人角色等展示信息。分页计数与列表使用同一过滤条件和快照。
6. Project 列表是纯读操作：GET 列表不初始化、不创建默认 Project 或其他 Project。符合组织资格但没有已创建且有权访问的 Project 时，返回空数组和零计数；Project 只能通过显式创建入口产生。

## 成员与权限

| 操作 | Owner | 管理员 | 成员 |
| --- | --- | --- | --- |
| 查看 Project、成员列表 | 可以 | 可以 | 可以 |
| 修改 Project 设置 | 可以 | 可以 | 不可以 |
| 添加人员并指定管理员或成员 | 可以 | 可以 | 不可以 |
| 移除非 Owner 成员 | 可以 | 可以 | 不可以 |
| 调整非 Owner 角色 | 可以 | 可以 | 不可以 |
| 转让 Owner | 可以 | 不可以 | 不可以 |
| 直接退出 | 不可以，必须先转让 | 可以 | 可以 |

- 添加对象来自当前组织通讯录，逐人指定管理员或成员，默认成员；一次确认的批量添加在一个事务中完成。
- Project 成员添加前的候选读取使用 `GET /v1/projects/:project_id/member-candidates?keyword=&page=&limit=`。接口从该 Project 所属当前组织的有效真人目录取数，在同一 RR 只读快照中完成 Owner/Admin 权限复核、分类、搜索、分页和总数，返回 `uid`、组织目录语义的 `name` 以及 `current_user`、`already_member`、`invitable` 状态；有效 Project 席位只在 `status=active` 且 `removing=0` 时标为 `already_member`，已移除/移除中的席位可再次邀请。接口沿用认证、Space 隔离、UID 限流和既有错误 envelope，不返回 email 等额外个人信息；添加服务仍对候选 UID 做最终资格校验。
- 普通角色更新最多授予管理员，不能借此产生第二个 Owner，也不能直接修改 Owner 的角色或移除 Owner。
- Owner 转让对象必须是当前 Project 内有效的真人管理员或成员；机器人/Agent 席位不能成为 Owner。新 Owner 就任与原 Owner 降为管理员在一个事务中完成；转让完成后原 Owner 可另行退出。
- 主动退出使用独立入口；成员不能借移除接口获得管理权限。
- 重复添加相同角色的有效成员视为无变化；重复添加不能作为隐式角色修改，角色不同则返回明确冲突。无效目标使整批失败。
- 任一资格失败、已有角色冲突或请求内同 UID 的角色冲突都回滚整个批次；请求内同 UID 同角色重复项去重，不产生部分成功。
- 已移除成员重新加入时应用本次指定角色，并重新校验当前组织资格。现有清理流程不得在重新加入后继续删除其新资格。
- 成员记录同时保留首次记录时间 `created_at` 和当前加入轮次时间 `joined_at`；首次创建（含初始 Owner/Agent）两者相同。新添加、退出后重新加入以及移除流程中的重新加入写当前 UTC 时间到 `joined_at`，有效成员幂等添加和角色调整不改变它；成员列表与单人读取统一返回与 `created_at` 相同格式的 `joined_at`。历史记录无法还原真实轮次，本版本迁移仅以 rolling expand 新增可空 `DATETIME(3)`，不回填、不改为 `NOT NULL`；所有读取统一使用 `COALESCE(joined_at,created_at)`，旧二进制可省略该列，后续收缩迁移不在本版本。
- 用户有效性、组织资格、Project 资格均在写事务中重新校验。账号停用不改变其他有效成员的角色。
- 组织级强制撤权立即使其组织身份不满足访问条件，同时保留唯一 Owner 的身份记录，不删除或降级该记录。其他有效成员保持原权限；组织身份恢复前，该 Owner 无法执行操作。本次不提供管理员越权转让或自动选任机制。

## 群关联

### 关系与鉴权

- 一个群同时关联零个或一个 Project；关联保存在群的现有归属字段中，补充关联人字段，不建立重复关系来源。
- 绑定要求调用者具有原生群管理权限及目标 Project 有效成员资格，群和目标必须属于同一组织。
- 换绑同时校验源和目标 Project 资格；解除关联校验当前源资格。Project 的普通成员满足上述群权限时即可操作。
- 同一目标重复绑定是幂等操作，保留原关联人。关联目标和关联人原子更新；解除时一起清空。
- 关联列表返回当前 Project 的关联群及数量，支持名称字面搜索，不以调用者已经是原生群成员作为关系列表的筛选条件。关系 DTO 不承载聊天内容。
- 解除关联后，该群不再出现在 Project 关联列表，原生群成员关系保持不变。
- 群内容访问、子区访问及资源派生权限由相应资源的判权路径负责；关系接口不替代这些判权路径。
- 资源侧以当前有效关联和成员资格实时判断 Project 派生权限；解除关联后该授权依据消失。原生群成员资格属于独立权限来源，不与派生权限混为一体。本规格只约定关系事实，不实施资源侧判断逻辑。
- 预设群组的原生群成员模型与 Project 关系是两个独立维度；预设群可以同时保留自身成员并关联 Project，既不自动同步成员，也不因并存关系增加警告或限制。

### Project 专属全员群

- 只有 `octo_project.all_member_group_no` 指向且仍关联该 Project 的原生群属于专属全员群；普通 `group.project_id` 关联群、预设群和历史快照群均不进入本节同步。
- 专属全员群的有效原生成员集合跟随 Project 当前有效席位（`status=active` 且 `removing=0`）收敛；Project 添加、重新加入、Space 成员恢复后补入，Project 移除、退出或 Space 撤权时先从专属群移除再清理 Project 席位。重试必须幂等，并以 Project 行锁和成员锁防止旧清理任务删除新资格。
- 专属全员群的群主由当前有效真人 Project Owner 投影；机器人/Agent 不得成为群主。Owner 转让、Owner 失效和重试收敛时，旧 creator 降为普通成员，目标真人 Owner 升为群主；没有已入群的合格 Owner 时不凭空提升其他成员。
- 专属群的群面 disband、退出、移除成员、手动转让群主、blacklist-add，以及手动 add/invite/scan-join（含 Bot API add/remove）均返回 `all_member_group_protected`，必须改走 Project 成员/Owner 入口；blacklist-remove 允许。Project/Space/BotFather 的系统级级联和同步钩子使用服务层原语，不受 HTTP 守卫阻断。
- 专属群判定以 Project 指针为权威，并同时校验群的 `project_id` 与有效状态；普通关联群不得因 `project_id` 字段而获得上述保护或同步。

### 从 Project 创建群

- Project 有效成员可以发起；创建时自动关联当前 Project。
- 普通 Project 发起群复用原开发设计中已明确的建群成员快照：将创建事务确认的有效 Project 成员作为初始群成员一次性写入，后续成员变化不自动同步原生群成员表。专属全员群只通过 `all_member_group_no` 指针进入上一节的实时投影语义。
- 快照仅初始化普通关联群的原生成员，不是后续 Project 成员或关联的权威来源，也不替代资源侧实时关系查询。
- 群、关联字段和初始成员在同一业务事务内写入；实际 IM 创建位于提交后，使用现有补偿机制。
- 所需序列及配置在持锁前准备；事务内重新验证当前资格。候选成员扩展导致不能安全复核时，释放锁重新准备，有界失败不留下部分本地数据。

### 关联群个人置顶（2026-09-11 确认）

- 置顶仅影响本人在当前 Project 内的关联群列表，与消息列表的频道置顶独立。
- 有效 Project 成员可以置顶当前关联群入口，无须原生群成员身份；置顶不授予聊天读取、发送或子区访问权限。
- 新增 `PUT /v1/projects/:project_id/groups/:group_no/setting`，请求体为 `{"pinned": true}` 或 `{"pinned": false}`。`pinned` 必须提供且为布尔值；认证、UID 限流、组织资格及错误响应沿用本规格工程契约。
- 写入在同一事务内复核当前组织、Project 有效成员资格和群的有效关联，与解绑、换绑按既有锁顺序串行化。跨组织、非成员、群已解散或已不关联当前 Project 时拒绝，失败不得保存偏好。
- 个人偏好按组织、Project、用户和群唯一定位，使用 Project 所属的群用户设置存储，不复用消息频道置顶记录；不套用消息频道每组织七条的配额。
- `GET /v1/projects/:project_id/groups` 每项增加 `pinned` 布尔字段，缺省为 `false`。置顶项优先，置顶时间倒序，同时间以群 ID 稳定排序；未置顶项保持现有稳定排序。排序先于分页，搜索条件和 `X-Total-Count` 不变。
- 重复置顶成功且不刷新时间；重复取消成功；取消后再置顶使用本次时间。服务端产生排序时间，不接受客户端自报。
- 解除或换绑后旧 Project 不再展示该群，个人偏好保留但不构成访问依据。重新关联回同一 Project 时恢复本人原有偏好；新 Project 的偏好独立。
- 本次不增加拖拽排序，不改变消息列表置顶、群成员关系或聊天权限。

验收：置顶和取消改变本人列表顺序；其他用户、组织和 Project 不受影响；分页前排序不遗漏置顶项；重复请求保持顺序；非原生群成员可置顶关联入口但仍不能读取聊天；换绑后写入旧关联被拒绝，重新关联恢复对应偏好；存储失败返回错误而非成功。

### 统一 Sidebar Project 条目（2026-09-11 收口）

- 统一 Sidebar 的 Project 条目只对当前用户的有效 Project 成员席位可见
  （Project 正常、席位 `status=active` 且 `removing=0`）。Space 成员身份或历史
  非成员 `pinned=1` 偏好不能制造条目；读路径不回填这类历史排序行，也不授予
  Project、原生群或子区权限。成员显式 `pinned=false` 后仍按个人偏好隐藏条目。
- Project 条目中的 `groups[]` 是关系元数据投影，按置顶优先并在每个 Project 内由
  SQL 限制最多 50 条。它不按调用者的原生 `group_member` 席位过滤（包括非原生成员
  或原生黑名单关系），也不返回原生群成员字段或授予聊天读写、子区、成员权限。

- Sidebar 不定义或改变原生群成员同步；普通 Project 发起群和关联已有群保持各自独立
  的既有成员/关系语义。该边界不改变 `groups[]` 的元数据语义或任何原生聊天权限。
- `groups[]` 字段形状切换需要客户端与服务端协调上线；本规格记录服务端契约和迁移
  边界，不表示外部客户端已经完成切换或部署。

## API 与工程契约

- 复用 `/v1/space/:space_id/projects`、`/v1/projects/:project_id` 和现有 Project 成员入口；新增能力归入 Project，不引入另一套命名或兼容别名。
- 保留可复用的数字角色编码：成员 0、管理员 1、Owner 2；数值不是权限判定的替代品，服务端 capabilities 按本权限表计算。
- 优先在现有接口中扩展搜索及逐成员角色输入；添加 Owner 转让和群关联所必需的入口。整批原子添加取代当前逐目标部分成功语义，相关调用方和测试同步调整。
- 分页沿用现有 `page/limit`。既有数组响应不因增加计数被直接改为对象；总数通过 `X-Total-Count` 返回，实施时同步检查跨域暴露与消费契约。
- 用户接口使用认证、共享 UID 限流和有效组织校验；错误经过 i18n facade，沿用 D14 wire 400 与嵌套语义状态。
- 只读响应的授权、角色、计数和分页来自同一短生命周期 RR 只读快照，不使用写锁代替一致性。
- 写事务只使用当前连接；按相关组织成员、组织、Project、成员/群业务行顺序加锁。多个 Project 按确定顺序锁定，防止并发换绑反向取锁。
- 数据库失败不能降级为无权限、无成员或成功；提交结果不确定时不自动重做创建操作。

### Project 成员单查与 Drive 用户态判权（2026-09-11）

- `GET /v1/projects/:project_id/members/:uid` 是成员列表同一 `MemberResp` 投影的单对象版本。成功时直接返回一个成员对象，不返回数组、分页参数或 `X-Total-Count`；`role` 继续使用 Project 角色数字 `0=member`、`1=admin`、`2=owner`，`robot`、`owner_uid` 和 `collaboration_roles` 与列表保持一致。
- 单查沿用 `readProjectAccessTx`，在同一 `REPEATABLE READ` 只读事务中校验调用者的有效账号、Project 所属 Space 当前组织成员资格及当前 Project 成员资格，再用 `project_id + target uid` 的有界 point query 读取目标；不得读取分页列表后在内存筛选。目标有效性与成员列表一致，仅要求 Project 正常且目标席位 `status=active`、`removing=0`；不额外把目标 `user`/Space 席位状态当作此关系事实的过滤条件。目标未知、已移除或 `removing=1` 均返回既有 not-found 语义；数据库错误必须返回 query_failed，不能降级成 not-found。该响应只表示目标的 Project 成员事实，不是目标账号或 Drive 资源授权决定，Drive 必须继续自行校验目标资格和 ACL。
- Drive 关系判权需要用户态成员事实时，保留同一用户原始 session `token`（首选 `token` header，兼容 `Authorization: Bearer <session>`）调用 `GET /v1/projects/{project_id}/members/{uid}`；`project_id` 和目标 `uid` 是路径 selector，不要求 `X-Space-ID`，不接受 `X-Internal-Token`、客户端自报 UID 或角色。Drive 仍负责自己的映射、ACL 和撤权窗口，Project `role` 不替代 Drive 权限；没有用户 session 的后台任务不能借此用户路由。

### Project→Drive provisioning boundary（2026-09-11）

- 经授权的 provisioning 以 `project_id` 作为唯一管理与幂等依据；Project 创建/响应不要求返回 Drive ID。
- 远端支持后调用 `POST /v1/internal/drive/spaces`，使用 `X-Internal-Token`，请求包含完整 Project `name`（最多 30 个 Unicode 字符）、`octo_space_id`、当前 Owner `super_admin_uid` 和 `project_id`。只有同一 `project_id` 的完全相同重复请求可按幂等成功处理；其他 `409`、`401`、`500` 按重试/失败策略处理。
- `OCTO_DRIVE_INTERNAL_TOKEN` 与 Fleet HMAC 凭据分离；功能关闭时不得向远端出站。远端接口及 30 字符名称支持是启用/部署前提，远端尚未提供时不得宣称已部署。

### `GET /v1/group/my` 角色筛选收口（2026-09-11）

- 成功响应继续是直接 `GroupResp[]`，不分页、不包 `data`；无参数返回当前用户保存的群，仅 `space_id` 返回该 Space 下当前用户已加入的群。
- `role=owner`、`role=admin` 或 `role=owner,admin` 只返回当前用户有效群主/管理员身份的群；`role` 可与 `space_id` 组合，组合请求先校验调用者仍是 active Space 成员。筛选结果排除解散群、非活跃群成员、外部管理角色脏数据和已撤权 Space 资格。
- 不带 `role` 的旧查询保持既有兼容行为，只补齐群 `role` 回填，不套用新角色筛选的收窄语义；群数字角色保持 `0=member`、`1=owner`、`2=admin`。

## 迁移与发布

- 现有 Project ID、Owner、成员和群数据保留。新增迁移负责名称约束、关联人及成员轮次时间等必要存储；`joined_at` 采用仅扩展的可空列迁移，不在本版本回填或收缩为非空。
- 旧接口中能够创建多个 Owner、直接退出 Owner 或绕过目标权限的入口必须一起调整；不能只修新入口。
- 用户角色和添加批次语义是同端点的不兼容行为变更，采用协调切换：发布前完成所有受影响调用方适配与联合验收，切换期间暂停相关写入口，排空旧服务实例后统一启用新契约。无法协调时阻止该契约上线，不静默混用两种批次语义。实施交付列出实际受影响调用方；不能以本仓库测试替代外部客户端验收。
- 存量群缺失关联人时返回空值，不推测或伪造历史操作者；管理权限仍取实时资格，后续实际换绑时记录操作者。
- 应用回滚不等于业务数据回滚。允许重名后，旧唯一约束可能无法重建；发布前明确回滚包兼容性，不删除重名数据以强行回滚。

- Sidebar Project 条目与关系群 `groups[]` 的客户端字段变更采用独立协调切换；服务端
  membership-only 读门和 SQL 分组上限先行，历史非成员 pin 行只读忽略，不以清理旧偏好
  代替授权收口。

## 验收标准

1. 同组织内创建、改名为同一名称成功；名称边界和字面搜索正确。
2. 列表为空时不创建 Project；列表、详情和成员查询遵守当前组织及 Project 成员边界，分页数量和结果一致。
3. 管理员可调整、移除其他非 Owner 成员；普通成员无法执行人员管理。
4. 添加时逐人角色正确，任一非法目标导致整批无变化；重复添加不隐式改变角色。
5. Owner 不能被普通角色修改或移除，不能直接退出；转让后恰有一名 Owner，原 Owner 是管理员。
6. 并发转让、组织撤权、退出和重新加入不产生权限窗口或清理新资格。
7. 群管理员兼 Project 普通成员可以关联、换绑、解除；缺任一资格或跨组织时拒绝。
8. 重复关联保持关联人；并发换绑不能绕过源、目标校验；解除不修改原生群成员。
9. Project 建群成功时关联和初始成员完整，失败无部分本地状态；IM 失败按真实补偿结果返回。
10. 数据库写锁等待不使只读查询借用业务排他锁；受控小连接池下事务不依赖额外业务连接。
11. 受影响接口、调用方、测试、错误码和文档一起调整；使用真实依赖执行针对性测试及 HTTP smoke 后才能声称实现完成。

12. Sidebar Project 条目仅对有效 Project 成员可见；非成员历史 pin 不回填，关联
    `groups[]` 最多按每 Project 50 条 SQL 限制且不附带原生群权限；客户端完成字段
    切换后再联合验收。

## 实施状态（2026-09-10）

- 已落地核心 Project 与成员契约：`POST/GET /v1/space/:space_id/projects`、`GET/PUT/DELETE /v1/projects/:project_id`、`POST /v1/projects/:project_id/members/add`、`POST /v1/projects/:project_id/leave`、`PUT /v1/projects/:project_id/owner`。
- 已落地 Project 群关联与建群入口：`GET/PUT/DELETE /v1/groups/:group_no/project`、`POST /v1/group/create`；换绑和解除关联保持原生群成员席位不变。
- 已验证 Project 名称、成员权限、群关联/建群、个人置顶与纯读列表等契约，并完成真实 TCP HTTP pin/list/cancel 及解绑重绑 smoke；当前目标不包含默认 Project 自动初始化。

## 设计修订状态（2026-09-11）

- Project 不再由 GET 列表或首次进入自动创建默认 Project；列表只观察已有且当前有权访问的 Project，空列表返回空数组。
- 关联群个人置顶设计已实现：`PUT /v1/projects/:project_id/groups/:group_no/setting` 只写当前用户在当前 Space/Project/群关系下的偏好；`GET /v1/projects/:project_id/groups` 返回 `pinned` 并在分页前按置顶时间排序。偏好不授予原生群聊天权限，解绑后隐藏、同一 Project 重新关联后恢复。
- 已使用真实 MySQL、Redis、WuKongIM 完成 Project 定向测试、group 关系测试、真实 TCP HTTP pin/list/cancel 及解绑重绑 smoke；`go build ./...`、i18n 一致性检查和本地化 lint 通过。

## PR887 审查收口（2026-09-11）

- B1：置顶配额计数与 membership-only 列表谓词一致；仅 Project 正常且调用者席位 `status=active AND removing=0` 的已置顶项目计入。成员被移除或席位进入 closing 后，历史偏好保留但不再占用槽位，重新具备有效成员资格后可恢复。
- B2：`POST /v1/group/create` 只将预期 Project 准入拒绝映射为本地化 D14 envelope：不存在项目携带语义 `404`，非成员/禁用目标携带语义 `403`，跨 Space 携带语义 `409`；legacy wire status 仍为 `400`，未知数据库或 IM 失败仍走内部 `store_failed`。
- N1：Project-backed Group creation 在认证后挂载共享 UID 限流，与其他用户写入口使用同一 UID bucket。
- N2a/N2b：关系 bind/unbind 继续同时要求 Project 成员资格和原生群 owner/admin；缺少任一资格时不修改原生成员或关系。已有 active 成员的不同角色重复添加拒绝整批请求，不保留部分新成员或隐式改角。
- N3：同步修正钩子、I2、Project 名称上限及管理员移除语义的注释，注释与当前实现和规格保持一致。
- N4：管理群日期边界的环境敏感基线本轮不改；MySQL `SYSTEM` 为 `+0800` 时 Group 验证使用 `TZ=Asia/Shanghai`，Project 验证使用 `TZ=UTC`，不把基线波动归因于本变更。
- 本轮用真实 TCP `http.Server` 和 MySQL 验证了 B1“移除成员达到上限后仍可置顶新可见 Project”以及 B2 非 Project 创建者/跨 Space 的本地化 4xx 响应；随后 `go build ./...`、`make i18n-extract-check` 和 `make i18n-lint` 均通过。

- Sidebar 审查收口：Project 条目改为仅有效 Project 成员可见，历史非成员 pin 行只读
  忽略；关系群 `groups[]` 保持不按原生 `group_member`/黑名单过滤并由 SQL 按 Project
  限制 50 条；旧 native GroupResp 读路径移除，CORS 暴露仅在允许跨域响应时追加。
  客户端字段切换仍需协调，未在本规格中声称已部署。

- P2 review 收口：AI session container（`purpose=ai_session_container`）在 Group 关系 PUT/DELETE 路由和 service 层均拒绝；实际关系变更同步推进 `group.version`，重复绑定/解绑不制造版本噪音。Project 关系读列表、分页计数及 Sidebar 批量投影同样排除历史绑定的 AI 容器，普通群关系不受影响。预设群与 Project 关系保持独立并允许并存；无引用的关系辅助函数已删除。
- 置顶 upsert 保留 `%w` 错误链，并修复 `pinned=1,pinned_at=NULL` 的历史行；`GET /v1/group/my` 角色列表的成员计数查询失败直接返回 query_failed，不降级为成功的 0。
- 运维注意：`project_i2_violations_total`、`group_admission_rejected_total` 及全员群 guard failure 计数器已移除，旧面板/告警应删除或允许序列缺失，缺失这些指标本身不是服务故障。Bot/IM 提示、订阅和其他提交后通知按 best-effort 处理，数据库提交事实权威，通知失败只记录并由既有补偿/重试路径处理。
- Migration 采用 rolling expand：仅新增可空 `joined_at DATETIME(3)`，旧二进制仍可省略列，读侧 `COALESCE(joined_at,created_at)`，新写入使用真实 UTC 时间；后续收缩迁移不在本版本。Cascade 保留 active human Owner 及其角色；仅有 active non-Owner agent rider 时处理 rider，并使 member_epoch/清理队列过渡幂等，Owner-only 行不进入分页。

## 未决事项

无产品决策待确认。外部客户端契约切换、Drive 远端接口/30 字符支持的部署前提和资源侧实时判权验收由对应维护者负责，不属于本设计阶段已验证的外部部署事实。
