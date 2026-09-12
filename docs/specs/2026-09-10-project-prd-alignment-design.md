# Project 行为与权限对齐设计

日期：2026-09-10

状态：设计已确认；在 `feat/project-prd-alignment` 当前实现上修复，并以 main 为专属全员群机制的保留基线。现有实现与修复后版本分别验收；外部客户端契约切换及部署由对应维护者负责。

## 背景与目标

基于最新 main 改造现有 Project，使其满足 `.local/assets/prd.md` 第 3.1.5、3.3.2、3.4.1–3.4.4 节的协作单元要求。产品、接口、数据模型和本文统一使用 Project 名称。PRD 是行为与权限依据，当前实现只提供可复用的工程基础；与目标冲突的现有规则需要修改。

现有 Project 有唯一真人 Owner，沿用所有权和 ID。实施前保全当前工作区并记录本地及远端 head；当前分支持续承载本设计，main 用于逐项比对原有机制与必要差异。

## 范围

### 包含

- Project 创建、详情、修改、列表搜索。
- 成员读取、添加、移除、角色调整、Owner 转让和退出。
- 群与 Project 的关联查询、绑定、换绑、解除关联，以及关联群列表和创建群入口。
- 所需数据库迁移、调用方契约调整、错误响应、行为验证及文档同步。

### 不包含

- 除下文明确的 Drive provisioning boundary 和专属群生命周期恢复所需的最小持久化任务适配外，不扩展其他 Internal API、outbox 或 Redis 事件队列。
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
- 专属全员群的群主由当前有效真人 Project Owner 投影；机器人/Agent 不得成为群主。Owner 转让时原群主降为普通成员，目标真人 Owner 升为群主；没有合格 Owner 时允许暂时无有效群主，不提升其他成员。Owner恢复资格时先恢复其有效原生成员关系，再同步群主；`group.creator` 的历史创建归属不作为当前群主权威。
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
- 远端支持后调用 `POST /v1/internal/drive/spaces`，使用 `X-Internal-Token`，请求包含完整 Project `name`（最多 30 个 Unicode 字符）、`octo_space_id`、当前 Owner `super_admin_uid` 和 `project_id`。201 成功；409 仅在合法 JSON 同时满足 `error=conflict` 和 `message=workspace_id "<本次project_id>" already bound to a space` 的既定精确契约时视为该 Project 已绑定。其他冲突和畸形响应按失败策略处理；每次重试重读当前名称和合格真人 Owner，不要求与第一次请求的 Owner 相同，不保存或依赖远端 Drive ID。
- `OCTO_DRIVE_INTERNAL_TOKEN` 与 Fleet HMAC 凭据分离；功能关闭时不得向远端出站。远端接口及 30 字符名称支持是启用/部署前提，远端尚未提供时不得宣称已部署。
- Drive 出站名称校验按 Unicode 字符计数，上限 64 字符；Project 用户 API 仍限制 30 字符，合法 Project 名称完整发送。名称校验不按 UTF-8 字节数截断或拒绝。

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

## 当前分支修复与完整保留方案（2026-09-11）

### 本轮具体事项与范围门禁

本节是本轮执行清单；全文其他API、PRD和发布清单用于保留既有契约，不代表重新实现或新增功能授权。

| 编号 | 分类 | 本轮事项 | 完成标准 |
| --- | --- | --- | --- |
| F1 | 必修 | 专属群判定恢复main分表读取，修复混合collation错误 | 同构与混合库Web/Bot判定正确，无1267，普通群行为不变 |
| F2 | 必修 | 恢复main专属缺员I4-B扫描、游标、指标及有效测试 | 缺员可发现，恢复后归零，分页/宽限期正确，扫描只检测 |
| F3 | 必修 | 补齐保留Project Owner身份后的Space撤权与恢复投影 | 仅 active account、active Space、active Project 且当前 dedicated pointer 匹配时恢复；恢复以最新合格真人 Owner 收敛；撤权同事务将专属群 creator 降级并删除其成员记录，不 handover，保留 Project Owner 身份 |
| F4 | 必修 | 修复专属移除旧任务覆盖新加入订阅的竞态 | Space seat `0→1` 及既有 Project seat 重新准入，同事务写入 Space removal outbox 的 `reason=rejoined`。仅复用 pending、空 lease、`attempts=0`、`last_error=''` 的初始任务；已执行任务不能吸收新请求。独立 projection registry 只执行专属投影，持久化游标续跑；成功分页归还 attempt，真实失败由原任务重试。迟到 `IMRemove` 的补订阅失败由清理回调补写持久任务，通用 admission 不派生任务。D4 原子取消全部旧 Project pending 任务（含 claimed），独立投影任务承担补偿；所有早退分支复核当前指针及有效资格，普通关联群不进入此链路 |
| R1 | 保留/回归 | main中仍适用的创建、改名、补建租约/CAS、守卫、清理任务及测试 | 仅恢复与F1–F4直接相关的删除或必要适配；当前已正确的部分不改 |
| R2 | 保留/回归 | 下文API全清单、PRD核心行为、四份迁移、Drive及普通群独立性 | 检查路由/契约完整性并运行受影响回归；不逐项重写、不顺带扩展接口 |
| R3 | 保留/回归 | 显式Project/Space解散、指针转换、普通群关系回落 | 本轮不得引入回归；仅调整F3/F4必需的边界，不把既有解散失败恢复另立为本轮改造 |
范围控制：

- 实现改动必须归属于F1–F4、下节明确接受的review事项，或能指出其直接依赖；测试、配置和文档只随对应行为调整。main对照不是全模块清理授权。
- 优先使用现有持久任务设施。若完成F3/F4确需新表、新worker、新事件协议或一般化任务框架，先说明现有设施不足与最小方案，取得单独确认后再实现；本规格不预先授权这些新增架构。
- 回归检查发现与F1–F4及明确接受review事项无关的既有缺陷，单独报告，不自动修复。尤其不扩展一般IM订阅可靠性、解散清理系统、资源ACL、部门/邮箱搜索或Drive远端部署。
- 不要求所有接口新增测试或文档重写；复用有效现有测试，新增测试仅覆盖本轮真实故障与不确定边界。
- 分支重建、提交整理、squash和远端强推不属于本轮代码修复，另按用户明确指令执行。

### 最新 review 事项与处置（2026-09-11）

证据基于当前代码head `e4670dac0d49b41368b45eb4ae701eff71956736`：

- [yujiawei，2026-09-11 11:08 UTC](https://github.com/Mininglamp-OSS/octo-server/pull/887#pullrequestreview-5177970066)。
- [Jerry-Xin，2026-09-11 11:26 UTC](https://github.com/Mininglamp-OSS/octo-server/pull/887#pullrequestreview-5178110117)。

两份review的重复问题合并为一个执行项；review结论是待核实输入，不覆盖当前代码证据或已确认产品约束。

| 编号 | review事项 | 当前核实与执行边界 |
| --- | --- | --- |
| C1 | P2：关联写入的专属指针检查边界核实 | `modules/group/project.go:48-66` 沿既有 Project 锁→group 锁序执行；fresh UUID 群创建提交后，以 Project 空指针+lease CAS 发布，正常 API 无法把既有任意 `groupNo` 认领为其他 Project 专属。空指针查询不加 `FOR SHARE`；本轮不新增锁或改代码 |
| C2 | 非阻塞：MemberRole/PickActiveOwner无生产调用 | 当前 `pkg/project/all_member_group.go` 两函数仅发现定义。F3/F4及main恢复完成后重新查引用；仍无调用且为本PR新引入的辅助代码则删除，有真实消费者则保留。不要为保留函数造调用，不做全库死代码清理 |
| V1 | 非阻塞：pinned_at=NULL重复置顶不修复 | 当前 `modules/project/db_group_pin.go:82-85` 已包含 `pinned_at IS NULL` 分支；不新增修复，保留现有行为并在相关回归验证，回复review时引用当前证据 |
| V2 | 非阻塞：已应用project_user_setting迁移被改 | 当前指定文件 `modules/project/sql/20260908000001_project_user_setting.sql` 与main无diff；不新增修改，交付前保持与main一致 |
| V3 | 旧阻塞joined_at与Owner rider清理 | 两位最新review已确认修复；作为R2回归保留，不重新实现、不改成旧契约 |

review关闭条件：实施后逐项附代码/行为证据回复来源review；C1记录既有锁序与不可达边界，C2完成最终引用判定；V1–V3给已满足证据而非重复提交。回复、commit及push在用户授权实施交付时执行，本次仅更新事项文档。

### 执行边界

- 在 `feat/project-prd-alignment` 上修复；当前已确认的 PRD 行为和 API 是保留集合，main 的专属群实现与测试是恢复比对基线。
- 开始实施时记录当前 head、远端 PR head 和最新 main SHA；保全用户未提交改动。按行为选择改动，不整体覆盖共享文件。
- 对每个受影响 main 机制记录“原有保证、当前替代、必要差异、验证场景”。未改变语义的代码和有效测试尽量与 main 字节一致，不额外重命名、改注释或重排文件。
- 当前设计不自动提交或重写远端历史。后续若整理提交，先备份原提交链，保证整理前后最终文件树一致；更新已发布历史必须使用绑定预期远端 head 的 force-with-lease，远端发生变化时停止而非覆盖。
- PR 最终差异由文件树决定，squash 只整理历史。恢复成 main 原样的内容自然退出最终 diff；必要的产品契约差异仍保留。

### API 完整保留清单

以下清单同时覆盖新增、修改与必须继续存在的入口。保留入口不等于所有入口都需要改实现；禁止因恢复 main 丢失路由、请求字段、响应形状或中间件。

| 方法与路径 | 必须保留的契约 |
| --- | --- |
| POST `/v1/space/:space_id/projects` | 显式创建；名称必填、30 Unicode 字符、允许重名；有效组织真人创建者成为唯一 Owner；现有头像/描述及配置配额保留；创建触发专属群和启用的 provisioning |
| GET `/v1/space/:space_id/projects` | 当前有效 Project 成员范围；名称字面搜索、page/limit、数组与 X-Total-Count；纯读，无默认创建 |
| GET `/v1/projects/:project_id` | 有效账号/Space/Project 成员读取；名称、描述、头像、成员数、本人角色与 capabilities 使用一致快照 |
| PUT `/v1/projects/:project_id` | Owner/Admin 设置；新名称30字符，未修改名称时兼容历史长名称；专属群改名钩子保留 |
| DELETE `/v1/projects/:project_id` | 保留既有解散入口及 Owner 权限，清理指针、关系、任务与缓存按当前生命周期执行；不新增归档能力 |
| PUT `/v1/projects/:project_id/setting` | 本人 Project 偏好；有效成员可见性、Project 置顶配额及重复请求语义；保留既有设置字段 |
| GET `/v1/projects/:project_id/members` | 成员数组、分页和计数；角色、robot、owner_uid、collaboration_roles、created_at、joined_at |
| GET `/v1/projects/:project_id/members/:uid` | 单个 MemberResp；同一 RR 权限边界，有界点查；关系事实不替代资源 ACL；目标不存在与数据库失败分开 |
| GET `/v1/projects/:project_id/member-candidates` | Owner/Admin；当前组织有效真人目录；keyword/page/limit；uid/name/status，current_user、already_member、invitable；数组与计数 |
| POST `/v1/projects/:project_id/members/add` | `members:[{uid,role}]`，role为0或1，缺省成员；整批原子；同角色幂等、异角色冲突；不接受 Owner 授予 |
| POST `/v1/projects/:project_id/members/remove` | 保留 `uids` 请求及逐人结果数组，不擅自改为添加接口的原子契约；Owner/Admin可管理非Owner，自身退出走leave；移除任务及轮次保护保留 |
| POST `/v1/projects/:project_id/leave` | 成员/Admin可退出；Owner必须先专门转让；保留清理任务 |
| PUT `/v1/projects/:project_id/owner` | `uid`；仅当前Owner，目标为有效真人成员；原Owner降Admin；事务内唯一性和专属群Owner投影 |
| PUT `/v1/projects/:project_id/members/:uid/role` | `role`为0或1；Owner/Admin管理非Owner；不承担Owner转让 |
| GET、POST `/v1/projects/:project_id/collaboration-roles` | 保留既有协作角色读取/创建、配额与维护逻辑；协作标签不授予管理权限 |
| PUT、DELETE `/v1/projects/:project_id/collaboration-roles/:role_id` | 保留既有协作角色改名/删除及权限检查 |
| PUT `/v1/projects/:project_id/members/:uid/collaboration-roles` | 保留成员协作角色替换及清理，不改变Owner/Admin/成员角色编码 |
| GET `/v1/projects/:project_id/groups` | 群关系DTO、linked_by、pinned、搜索、分页及计数；置顶先于分页；排除AI容器；不按原生群席位或黑名单过滤元数据 |
| PUT `/v1/projects/:project_id/groups/:group_no/setting` | 必填布尔pinned；个人Space/Project/群维度；不使用消息置顶配额；幂等及历史NULL排序时间修复 |
| GET `/v1/groups/:group_no/project` | 当前关系及关联人事实，为标题/管理关联使用；保持已确认读取授权，不泄漏聊天内容 |
| PUT `/v1/groups/:group_no/project` | project_id目标；原生群Owner/Admin且具备源/目标Project资格；同Space；关联人原子更新，幂等保留；专属群及AI容器保护 |
| DELETE `/v1/groups/:group_no/project` | 源Project资格与原生群管理权；只解除关系，不清理普通群成员；专属群及AI容器保护 |
| POST `/v1/group/create` | 保留原生建群请求，project_id可选；Project普通群初始化当前成员快照；本地群/关系/成员原子提交，IM提交后处理；UID限流及通用建群配额保留 |
| GET `/v1/group/my` | 直接GroupResp数组、不分页；role=owner/admin/owner,admin及space_id组合；原生角色编码不同于Project；旧无role查询兼容，计数/外部归属查询失败返回错误 |
| GET `/v1/space/:space_id/sidebar-sections` | 保留Category等既有条目；Project仅有效成员可见；groups为关系DTO，SQL按每Project最多50条，个人置顶排序 |
| PUT `/v1/space/:space_id/sidebar-sections/sort` | 保留统一排序入口、偏好和隐藏语义，排序记录不能制造Project访问资格 |
| POST `/v1/auth/verify` | 保留现有认证及Project关系/capabilities消费者；按新权限矩阵返回事实，不将原生群角色当Project角色 |
| GET `/v1/common/appconfig` | 保留project_on与服务端写开关同源；开关关闭保留读取和必要安全清理 |
| 出站 POST `/v1/internal/drive/spaces` | Drive内部Token及四字段协议、当前Owner重读、201/精确409处理、超时/重试；不是本仓库新增用户路由 |

关联调用链也必须保留：Space成员添加、邀请加入/审批、移除、退出、解散；BotFather账号删除及其Agent rider级联；Web群disband/exit/add/invite/scan-join/remove/transfer/blacklist及Bot群add/remove；消息发送、历史读取、子区访问仍走原生资源鉴权。上述既有入口不因Project关系而被移除、重命名或统一改成Project权限。

### PRD 行为与工程保留项

1. PRD 3.1.5：组织隔离；账号和Space资格是Project访问前提，Space管理员不绕过Project成员边界。
2. PRD 3.3.2、3.4.4：关联仅入口，解绑不改变原生访问；普通发起群只初始化当时的成员；非原生成员可以看关系但不能看聊天或子区；只关联群、不单独关联子区。
3. PRD 3.4.1–3.4.3：30字符名称、同组织重名、显式创建、成员数/角色、Owner/Admin设置权限、批量人员管理、唯一真人Owner及专用转让。
4. 人员添加对象资格复核、Agent owner_uid/rider生命周期继续保留；Owner/Admin可以添加符合目录资格的bot，不限定必须为操作者自己的bot；普通成员不能通过own-agent路径绕过人员管理权限。创建与转让都不得产生机器人Owner。
5. joined_at表示本轮加入：首次/重新加入更新，有效幂等添加和角色调整不更新；created_at保留首次记录。Owner-only Space撤权保留身份，nonOwner riders正常关闭，轮次和缓存失效幂等。
6. Project个人置顶只计当前有效可见成员Project；被撤权的历史偏好保留但不占槽位。关联群个人置顶与消息频道置顶独立；解绑隐藏、同Project重绑恢复个人偏好。
7. AI session container在关系读、计数、sidebar及变更服务中排除；实际关系变更推进group.version，无变化不推进。
8. 认证、共享UID限流、Space隔离、D14 wire400/i18n envelope、英文错误源和中文翻译同步保留；数据库失败不能降级为0计数/空结果/无权限。
9. RR只读快照、确定锁顺序、当前事务连接、仅重试可回放且未提交的DB事务保留；不能重试结果不确定的创建提交或盲重放IM副作用。
10. Fleet HMAC协议、既有outbox/lease/backoff/开关/监控保持；Drive使用独立OCTO_DRIVE_INTERNAL_TOKEN，配置凭据去重保持确定顺序，关闭时不出站；请求捕获测试须保持goroutine同步。

候选搜索当前确认契约为组织目录显示名的字面搜索。PRD 3.4.2 的“姓名、部门或邮箱”完整搜索不在当前已实现契约中，不能声明此条已全部覆盖；客户端应使用准确提示，部门/邮箱检索需对应目录数据与授权契约后另行对齐。任务移除影响计数、Loop任务/自动化/项目、文件及文档ACL、能力市场和客户端七Tab渲染由对应系统负责，不属于本次octo-server后端修复；Drive角色映射由Drive消费Project事实实现，不新增角色推送。

### main 机制保留与必要适配

| main机制 | 处理 |
| --- | --- |
| 专属指针识别及分表读取 | 原样复用兼容性设计，保持有效Project/群/关联共同判定；提交时仍做事务内权威检查 |
| 专属创建、改名、补建claim、deadline CAS、释放及写回fence | 保留原有机制与有效行为测试；只适配当前真人Owner和有效成员定义 |
| Web/Bot专属群守卫及安全失败处理 | 保留有效保护与监控；普通关联群不获得专属保护；blacklist-remove允许恢复 |
| Project清理outbox、worker租约/心跳/取消和重入保护 | 保留；清理范围缩到专属指针；所有IM分支满足下述重新加入保证 |
| I4-A缺群和I4-B专属缺员对账 | 保留扫描、游标、宽限期、指标与有效测试；不将普通群成员差异当缺员 |
| 普通关联群（`group.project_id`）成员 | 保持与Project席位独立的成员快照；不纳入I4-B专属缺员监控；Space原生清理仍覆盖其应清理的群，不能误删组织撤权安全链 |
| Space最终专属Owner收敛 | 保留其目的并适配Owner身份保留/恢复；不能以普通群自动交接代替 |
| 错误码、配置、指标及有效测试 | 未改变语义者原样保留；仅删除已不适用普通群约束的项；每个删除项说明替代或失效原因 |

### 必须闭合的修复

**Space Owner撤权/恢复。** 唯一真人 Project Owner 记录保留；Space 撤权时在同一事务中将专属群 creator 角色降为普通成员并删除该成员记录，不执行 handover，专属群可暂时无有效群主，但 Project Owner 身份仍保留。恢复仅处理 active account、active Space、active Project 且当前 dedicated pointer 仍匹配的席位，补入当前专属群后以最新合格真人 Owner 收敛；不自动提升其他成员或解散 Project，已关闭普通成员席位不自动恢复。覆盖管理员添加、邀请/审批等所有现有 Space 重新加入路径；工作持久化并可跨重启重试，提交资格变化时即保证任务可恢复，不能只依赖内存事件回调。优先复用现有任务设施，不扩展一般资源授权。

撤权、重新加入和群主投影统一以当前有效账号、Space成员资格、有效Project及其当前成员席位共同判定；保留的 Owner 身份行不等于已重新加入，不能仅凭 Project status=active 跳过 Space 撤权退订。投影只能落到当前有效 dedicated pointer，并以最新合格真人 Owner 收敛；Owner 暂时失去资格、恢复任务重试或耗尽不得触发无继任者自动解散 Project，也不得解除普通关联群关系。只有现有显式 Project/Space 解散等真实终态才终止对应恢复责任；终态后的旧恢复任务不能重建或重新订阅。

恢复工作按稳定游标覆盖该用户全部仍有效 Project 席位，达到单次预算时持久化 `project_id` cursor 续跑点；纯成功分页归还本次 claim 的 attempt 并尽快继续，真实失败记录错误和 attempt 后重试。立即入队与分页续跑的到期时间按 UTC 毫秒精度截断，避免数据库舍入后晚于当前认领时刻；真实退避仍保留原有延迟。补群与群主写入事务复核当前 Project Owner、有效成员和 dedicated pointer；并发转让后旧恢复任务不得把群主写回原 Owner。显式解散只作为本轮恢复任务的终态边界与回归场景。

分页中的真实失败保留该页的输入游标，下一次从失败页起重试，不能越过失败 Project。复用 `last_error` 的首行 `rejoin_cursor:<project_id>` 保存续跑位置，后续行记录错误摘要；成功续页仅保留游标，终态按既有规则记录完成/耗尽原因。重试次数和指数退避保持不变。

**成员重新加入与IM订阅。** Space 成员席位 `0→1` 及既有 Project seat `status!=active` 或 `removing=1` 的重新准入，在成员事务中写入 Space removal outbox 的 `reason=rejoined`。`EnqueueMemberRejoinIntentTx` 仅复用 `status=pending`、`lease_owner=''`、`lease_until IS NULL`、`attempts=0` 且 `last_error=''` 的初始任务；已分页任务可能已经越过新请求对应的 Project，已失败任务可能耗尽预算，二者均不可吸收新请求，claimed 和终态任务同样建立新责任。迟到旧 `IMRemove` 的补订阅失败由 `reconcileDedicatedGroupProjection` 清理回调写入持久任务并返回 `ErrAdmittedButNotSubscribed`；通用 admission 将错误交给调用方，投影 worker 使用原任务重试，不递归派生新任务。独立 projection registry 不运行普通 removal steps 或 finalizers；普通关联群保持原生成员快照。原生成员行存在、不存在、已删除、并发删除及已重新准入等分支统一复核当前专属指针与有效 Project、Space 资格，`IMRemove` 返回后再次复核并补偿并发重新加入。测试必须把 IMAdd 故障放在迟到退订后的补偿调用上，验证持久责任和后续恢复。

专属指针变化必须触发任务作用域复核。旧群已经解除关联成为普通群时，旧专属任务不得再修改其原生成员或订阅；成员写入和关系变更共用事务栅栏，提交后IM操作与专属身份转换必须有顺序及补偿保证，不能只做一次无锁预查。旧群仍处于有效专属清理范围时，其退订责任不能因读到新指针而被遗忘；当前专属群补订阅按自己的资格和轮次执行。Project 重新准入按既有 D4 在同一事务原子取消所有 pending 的旧 Project removal job（含 claimed）并清 lease；独立 durable Space rejoin intent 负责迟到 `IMRemove` 的补偿，不能覆盖新的投影责任。任务被取消、替换或达到重试上限时，尚未完成的订阅责任仍可发现、告警并由既有运维重试机制重驱，不能将失败标为成功。验证需覆盖Space撤权与Project重入相交、连续两次重入、指针更换、解除关联成为普通群、显式解散和重试耗尽。

上述旧专属任务的作用域限制针对 **Project 成员清理**。**Space 撤权清理**仍覆盖该 Space 的所有原生群：锁内发现专属指针已清除、移动或 Project 已解散时，对仍存活的旧群按普通群规则移除已撤权成员，并保留普通群的群主继任语义；只有 Space 席位恢复时才跳过该成员的清理。专属身份在写事务内读取，不由调用方传入历史专属标志。

**数据库排序规则与 Space selector。** 恢复分表专属判定，不引入全库 collation 迁移；跨 Project、Space、成员及用户表的 JOIN 显式使用兼容 collation，并以隔离混合库验证实际角色更新。Space ID 不用 Go 的 `ToLower`/`TrimSpace` 或字节比较猜测规范值，而由数据库按 `space` 表实际 collation 解析请求 selector，并返回数据库存储值。旧 `rejoined` durable 任务在 worker 边界按该规则解析原始 selector；解析/查询错误沿原租约退避重试，Space 缺失或已解散时终止该 `rejoined` 恢复责任。普通 Space removal 保留原始 selector，即使 Space 缺失或已解散也继续按 fail-safe 清理语义执行，不因规范值解析无结果静默 no-op。Project 恢复先前置复核 `Project.space_id` 归属，投影写事务再次校验 Space、Project 和当前专属群绑定；Group 绑定在锁内以 Project 权威列做 SQL 判等，兼容历史大小写/PAD SPACE 跨表值，不能改为 Go byte comparison。数据状态查询失败保持安全拒绝，不伪装普通群。

Bot HTTP 守卫先按数据库排序规则解析请求群号，再使用查得的规范 `group_no` 判断专属身份。因此大小写变体及 PAD SPACE 排序规则下的尾空格不能绕过保护；普通关联群的可变更规则保持不变。

**专属缺员监控。** I4-B只检测有效专属群缺员；宽限期、正在移除、组织资格、系统bot和缺群去重规则与当前契约一致。保留跨页游标、完整轮转后发布及失败续扫。扫描只检测，写侧恢复不能依赖扫描自动修复；查询语义正确与生产排序形态下扫描开销分别验证，保留原有昂贵扫描开关和真实运行限制。

**I1 与 abandoned-cleanup 监控。** 两类扫描均以写侧的 Space 席位和 Project 生命周期语义为准；Owner 豁免仅适用于 `Project.status=normal`。Space 撤权或 Space 解散后，若 Project 仍正常，写侧保留的 Owner 身份不算席位泄漏；若 Project 已解散或不存在，仍为 active 且无有效 Space 席位的 Owner 是 stale active seat，I1 与 abandoned 扫描均须报告。普通成员缺失有效 Space 席位且没有待执行清理任务时仍须报告。两类查询在 `LIMIT` 限制的 inspected base page 上以 SELECT flag 计算，不把生命周期或席位状态放进过滤返回行的 `WHERE`；该豁免只影响只读监控，不授予访问资格；已应用 core migration 中的 I1 注释是历史口径，现行口径以本节和扫描实现为准。

**清理任务运维口径。** `space_member_removal_cleanup.reason=rejoined` 表示恢复投影，只运行 rejoin hooks，不运行破坏性 removal steps/finalizers；`bot_deleted` 表示账号删除级联。已应用 migration 的列注释保持原始版本，当前原因枚举以 `modules/space/member_removal.go` 为准。

**投递与并发边界。** Owner 角色提交后的 CMD/channel 通知维持 best-effort；角色已正确时重试不重发通知。IM 跨服务 add/remove 顺序及通用持久投递仍属于既有 #797 可靠性范围。Space 清理当前每次处理一个 UID；新增批量调用前须实现逐 UID 资格筛选和继任者排除。Space-seat 锁查询也锁住共享 Space 行，普通群继任者按创建时间锁定，可能串行或由数据库裁决死锁并交由现有工单重试；本次保留锁范围和选主规则。

### 数据迁移与发布清单

- 保留新增 `modules/project/sql/20260910000001_project_read_default.sql`：只解除有效名称唯一索引；同名数据存在时Down可能失败，不删业务数据强行回滚。
- 保留 `modules/space/sql/20260910000002_group_project_linked_by.sql`：可空关联人，历史不伪造。
- 保留 `modules/project/sql/20260911000001_project_group_user_setting.sql`：个人Project关联群置顶存储及唯一键。
- 保留 `modules/project/sql/20260911000002_project_member_joined_at.sql`：单条可空ADD COLUMN，读侧COALESCE，旧写入可省略，不做回填/NOT NULL收缩。
- main已应用迁移保持原样，不改历史DDL和迁移编号。四份现有新增迁移仅保留与回归验证；若F3/F4确需新增存储，按本轮范围门禁另行确认，不直接新增迁移。
- 同步本规格及实际受影响的现有任务brief、changelog、错误注册/翻译/提取标记和运维说明；sidebar等API文档仅在对应契约确实改变时调整。原有专属监控继续提供时不得提示运维删除。
- 用户API批量添加/Owner权限和sidebar DTO切换需调用方联合验收；列出仓库内真实调用点及外部责任系统，不假设前端已发布。Drive接口与30字符支持仍是外部部署前提。

### 实施顺序与验收门禁

1. 冻结接口/行为清单和工作区快照，以main差异逐项标记保留/恢复/最小适配，不改分支身份。
2. 恢复专属分表判定、对账/指标、租约/守卫及对应有效测试；共享文件仅应用必要变更块。
3. 完成Space Owner恢复和IM重新加入的持久化责任闭环，覆盖原有生命周期及所有提前返回分支。
4. 对本节API逐项记录route→handler→service→DB→DTO/错误→调用方→验证证据；既有协作角色、设置、解散和配置入口同样检查，不能只验新增路由。
5. 使用真实MySQL/Redis/WuKongIM串行验证相关模块。数据库测试持共享锁，按包恢复干净测试库；不清空无关Redis数据。
6. 确定性并发测试覆盖“旧回调读到移除→重新加入完成→旧退订”及退订/补订阅失败、取消、重复、跨重启；实际验证最终有效者能收发、最终无效者不能因残留订阅继续访问。
7. Space真人Owner撤权→恢复、转让并发、bot rider关闭、专属无合格Owner、缺群补建/失效租约必须验证；普通关联群成员独立性和Space原生撤权同时验证。
8. 真实混合collation守卫、I4-B缺员/宽限期/分页/恢复归零；joined_at旧二进制写兼容；关系/个人置顶/候选/单查/读快照及数据库失败传播全部验收。
9. 窄用例先行，再跑Project/Group/Space/Category和受影响Bot路径；Project及并发相关包使用-race，完成build/vet/i18n检查。实际HTTP调用证明响应形状、X-Total-Count及允许origin的CORS暴露；拒绝origin不增加暴露头。
10. 审查最终删除与替代矩阵、接口清单、配置与监控差异；每处仍有差异均有产品或正确性理由。CI通过只能证明现有测试，不替代main行为对照。

## 未决事项

方案已确认。实施交付必须重新提供修复后证据，当前分支历史验证不代表上述恢复路径已通过。外部客户端字段切换、目录部门/邮箱检索与Drive部署仍由相应系统维护者完成；未完成前只声明本规格明确范围内的后端能力。
