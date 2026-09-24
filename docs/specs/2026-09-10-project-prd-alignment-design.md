# Project 行为与权限对齐设计

日期：2026-09-10

状态：设计已确认；普通关联群服务端实现待审查，客户端切换与发布待联合验收。

## 背景与目标

现有 Project 作为 Space 内协作单元，遵循 `.local/assets/prd.md` 第 3.1.5、3.3.2、3.4.1–3.4.4 节的行为与权限约束；产品、接口、数据模型统一使用 Project 名称。2026-09-24 确认创建 Project 不附带群，历史自动创建的群按普通关联群管理。

Project 沿用现有 ID、业务数据与唯一真人 Owner。实施前保全工作区并盘点服务、客户端和历史群的受影响契约。

## 范围

### 包含

- Project 创建、详情、修改、列表搜索。
- 成员读取、添加、移除、角色调整、Owner 转让和退出。
- 群与 Project 的关联查询、绑定、换绑、解除关联，以及关联群列表和创建群入口。
- 所需数据库迁移、调用方契约调整、错误响应、行为验证及文档同步。

### 不包含

- 除下文明确的 Drive provisioning boundary 外，不扩展其他 Internal API、outbox 或 Redis 事件队列。
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
| 群关联 | 原生群管理权限与 Project 成员资格独立校验；Project 不指定或管理专属群 |
| 原生群成员 | 从 Project 发起创建时仅初始化一次成员快照；之后与 Project 成员、Owner 和名称变更独立 |
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

### Project 关联群的统一语义（2026-09-24 确认）

- Project 创建不自动创建群；群只通过显式建群或关联操作产生 `group.project_id` 关系。所有关联群使用同一套普通群规则。
- 存量由 Project 自动创建的群保留原生群、消息、成员、群主和 `group.project_id` 关联，直接作为普通关联群继续出现在关系列表。数据库删去 Project 上的专属指针不会删除或重建原生群，也不改写群名、关联人或原生群角色。
- Project 添加、移除、退出、重新加入成员或转让 Owner、改名时，不自动更改任何普通关联群的原生成员、群主、名称和 IM 订阅。退出 Project 不等于退出群；群成员资格与聊天权限仍由原生群及 Space 资格决定。Space 撤权或 Project 解散仍执行各自已有的原生群清理或关联回落，不以 Project 成员变动取代 Space 安全清理。
- Web 和 Bot 的原生群操作不再受专属群保护；继续执行普通群原有的身份、权限、Space 和 AI 容器约束。Project 关系元数据的可见性仍不授予群聊天或子区权限。

### 从 Project 创建群

- Project 有效成员可以发起；创建时自动关联当前 Project。
- 从 Project 发起创建群时，将创建事务确认的有效 Project 成员作为初始原生成员一次性写入；后续 Project 成员变化不自动同步群成员。
- 快照仅初始化普通关联群的原生成员，不是后续 Project 成员或关联的权威来源，也不替代资源侧实时关系查询。
- 群、关联字段和初始成员在同一业务事务内写入；实际 IM 创建位于提交后，使用现有补偿机制。
- 所需序列及配置在持锁前准备；事务内重新验证当前资格。候选成员扩展导致不能安全复核时，释放锁重新准备，有界失败不留下部分本地数据。
- 建群服务传入 `BotUID` 时，该 Bot 必须具有目标 Space 的 active 席位；Bot 加入现有候选人集合，在每轮快照重试中复用相同的 Space 席位锁顺序，并在群/成员写入前校验事务内结果。Bot 不必是 Project 成员；已在 Project 快照中的 Bot 只保留一条有效原生成员记录，成功建群后按既有流程设置 `bot_admin`。不合格 Bot 导致整个建群拒绝，不留下群、关联或成员的部分写入。

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

Project 创建、列表、详情及 Sidebar 的 Project 条目不再返回 `all_member_group_no`；关联群仍经原有 `groups[]` / 关系接口展示，不添加替代指针或固定首项。调用方同步移除依赖该字段的入口、排序及“全员群”标识；服务端不保留空串字段或兼容别名。

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
- 专属指针与建群租约列由 `scripts/project-native-groups.sql` 在发布后的独立运维阶段手动删除，不放入模块启动时自动执行的迁移目录；已应用的迁移保持原样。新版本先在保留旧列的 schema 上滚动部署，暂停受影响写入口并排空旧服务实例及在途建群/投影/清理任务的执行。确认所有流量由新版本承接、旧版本无法再次启动，备份 schema 后，由运维对目标库执行 `mysql --host=<host> --user=<user> --database=<database> < scripts/project-native-groups.sql`，然后核对列已移除、旧群原生记录与关联未变。切换后的未完成 Project 成员移除工单只关闭 Project 席位，不处理普通群成员；Space 原生清理仍处理失去 Space 资格的群成员。删列后旧二进制不可作为原样回滚包；回滚方案需使用兼容新模式的版本，不靠恢复指针重建专属语义。
- 停用仅为专属投影服务的 Space `reason=rejoined` 生产与消费钩子；已有待处理的该类工单应安全终结而非运行旧群投影，不阻断普通 Space 撤权清理。保留 Space 席位变化需要的 Project epoch 失效和 Project 成员清理任务的重入防护。

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
13. 创建 Project 后没有自动产生的群，创建、列表、详情和 Sidebar 不含 `all_member_group_no`，普通关联群没有特殊排序或专属标识。
14. 切换前已有群仍在 Project 关联列表和原生聊天中；Project 改名、成员增删/重新加入/退出及 Owner 转让后，其群名、群主、成员和 IM 订阅均不随之改变；具备原生群权限的 Web/Bot 操作按普通群规则执行。
15. Project 成员退出后，其原生群资格不因退出自动消失；Space 撤权仍清理其原生群资格，Project 解散仍使群回落 Space 且保留群成员。停用投影后旧待处理工单、并发写入及跨重启重试不得再次修改普通群成员或订阅。

## Project 关联群契约切换（2026-09-24）

### 实施边界

本轮切换 Project 自动建群与专属投影契约，同时保持既有 Project 管理、普通关联群、Space 撤权、解散和资源判权能力。调用方及文档中的专属群入口与字段一起切换；迁移不批量改写历史群消息、成员或关联记录，后续原生群操作仍按各自权限生效。

| 边界 | 目标 |
| --- | --- |
| 创建与读取 | 显式创建 Project 不附带建群；Project DTO 和 Sidebar 移除专属字段，关系群列表继续反映 `group.project_id` |
| Project 成员与 Owner | 保留唯一真人 Owner、席位状态/轮次、成员权限和重入防护；撤下群成员、群主及 IM 的专属投影 |
| 群操作 | Web/Bot 使用普通群权限；关联/换绑/解除和原生 Space 清理仍按各自规则执行 |
| 后台任务 | Project 成员移除工单只负责关闭席位，保留仍承载该责任的幂等、租约和取消机制；停用专属补建、改名、rejoin 投影与缺群/缺员扫描和指标 |
| 数据与发布 | 新版与旧列共存完成滚动部署；旧实例及在途任务排空后，手动运行 `scripts/project-native-groups.sql` 删除 Project 专属指针和租约列；保留历史群及群关系 |

实施时核对每个专属钩子、Web/Bot 守卫、Space rejoin 生产/消费、Project/Group/Category DTO、错误码/翻译、配置/指标和测试的唯一消费者；清除仅供专属群使用的路径，保留共享的 Space 撤权、Project epoch 失效和普通群能力。历史排队任务不能在新版本重放出群成员或 IM 变化。

不扩展一般 IM 订阅可靠性、解散清理、资源 ACL、目录搜索或 Drive 远端部署；不因切换而改写其他用户接口、路由、权限和群成员快照语义。

### API 完整保留清单

以下清单覆盖需要保留的入口及本次调整的响应字段；不改变未列明的请求、路由和中间件。

| 方法与路径 | 必须保留的契约 |
| --- | --- |
| POST `/v1/space/:space_id/projects` | 显式创建；名称必填、30 Unicode 字符、允许重名；有效组织真人创建者成为唯一 Owner；现有头像/描述及配置配额保留；启用的 provisioning 照常执行，不自动建群 |
| GET `/v1/space/:space_id/projects` | 当前有效 Project 成员范围；名称字面搜索、page/limit、数组与 X-Total-Count；纯读，不含专属群字段 |
| GET `/v1/projects/:project_id` | 有效账号/Space/Project 成员读取；名称、描述、头像、成员数、本人角色与 capabilities 使用一致快照，不含专属群字段 |
| PUT `/v1/projects/:project_id` | Owner/Admin 设置；新名称30字符，未修改名称时兼容历史长名称；不联动群名 |
| DELETE `/v1/projects/:project_id` | 保留既有解散入口及 Owner 权限；普通关联群回落 Space 直属，原生成员不因 Project 解散而删除 |
| PUT `/v1/projects/:project_id/setting` | 本人 Project 偏好；有效成员可见性、Project 置顶配额及重复请求语义；保留既有设置字段 |
| GET `/v1/projects/:project_id/members` | 成员数组、分页和计数；角色、robot、owner_uid、collaboration_roles、created_at、joined_at |
| GET `/v1/projects/:project_id/members/:uid` | 单个 MemberResp；同一 RR 权限边界，有界点查；关系事实不替代资源 ACL；目标不存在与数据库失败分开 |
| GET `/v1/projects/:project_id/member-candidates` | Owner/Admin；当前组织有效真人目录；keyword/page/limit；uid/name/status，current_user、already_member、invitable；数组与计数 |
| POST `/v1/projects/:project_id/members/add` | `members:[{uid,role}]`，role为0或1，缺省成员；整批原子；同角色幂等、异角色冲突；不接受 Owner 授予 |
| POST `/v1/projects/:project_id/members/remove` | 保留 `uids` 请求及逐人结果数组，不擅自改为添加接口的原子契约；Owner/Admin可管理非Owner，自身退出走leave；移除任务及轮次保护保留 |
| POST `/v1/projects/:project_id/leave` | 成员/Admin可退出；Owner必须先专门转让；保留清理任务 |
| PUT `/v1/projects/:project_id/owner` | `uid`；仅当前Owner，目标为有效真人成员；原Owner降Admin；事务内唯一性，不联动群主 |
| PUT `/v1/projects/:project_id/members/:uid/role` | `role`为0或1；Owner/Admin管理非Owner；不承担Owner转让 |
| GET、POST `/v1/projects/:project_id/collaboration-roles` | 保留既有协作角色读取/创建、配额与维护逻辑；协作标签不授予管理权限 |
| PUT、DELETE `/v1/projects/:project_id/collaboration-roles/:role_id` | 保留既有协作角色改名/删除及权限检查 |
| PUT `/v1/projects/:project_id/members/:uid/collaboration-roles` | 保留成员协作角色替换及清理，不改变Owner/Admin/成员角色编码 |
| GET `/v1/projects/:project_id/groups` | 群关系DTO、linked_by、pinned、搜索、分页及计数；置顶先于分页；排除AI容器；不按原生群席位或黑名单过滤元数据 |
| PUT `/v1/projects/:project_id/groups/:group_no/setting` | 必填布尔pinned；个人Space/Project/群维度；不使用消息置顶配额；幂等及历史NULL排序时间修复 |
| GET `/v1/groups/:group_no/project` | 当前关系及关联人事实，为标题/管理关联使用；保持已确认读取授权，不泄漏聊天内容 |
| PUT `/v1/groups/:group_no/project` | project_id目标；原生群Owner/Admin且具备源/目标Project资格；同Space；关联人原子更新，幂等保留；AI容器保护 |
| DELETE `/v1/groups/:group_no/project` | 源Project资格与原生群管理权；只解除关系，不清理原生群成员；AI容器保护 |
| POST `/v1/group/create` | 保留原生建群请求，project_id可选；Project普通群初始化当前成员快照；本地群/关系/成员原子提交，IM提交后处理；UID限流及通用建群配额保留 |
| GET `/v1/group/my` | 直接GroupResp数组、不分页；role=owner/admin/owner,admin及space_id组合；原生角色编码不同于Project；旧无role查询兼容，计数/外部归属查询失败返回错误 |
| GET `/v1/space/:space_id/sidebar-sections` | 保留Category等既有条目；Project仅有效成员可见；groups为关系DTO，SQL按每Project最多50条，个人置顶排序，不含专属群字段 |
| PUT `/v1/space/:space_id/sidebar-sections/sort` | 保留统一排序入口、偏好和隐藏语义，排序记录不能制造Project访问资格 |
| POST `/v1/auth/verify` | 保留现有认证及Project关系/capabilities消费者；按新权限矩阵返回事实，不将原生群角色当Project角色 |
| GET `/v1/common/appconfig` | 保留project_on与服务端写开关同源；开关关闭保留读取和必要安全清理 |
| 出站 POST `/v1/internal/drive/spaces` | Drive内部Token及四字段协议、当前Owner重读、201/精确409处理、超时/重试；不是本仓库新增用户路由 |

关联调用链也必须保留：Space成员添加、邀请加入/审批、移除、退出、解散；BotFather账号删除及其Agent rider级联；Web群disband/exit/add/invite/scan-join/remove/transfer/blacklist及Bot群add/remove；消息发送、历史读取、子区访问仍走原生资源鉴权。上述既有入口不因Project关系而被移除、重命名或统一改成Project权限。

### PRD 行为与工程保留项

1. PRD 3.1.5：组织隔离；账号和Space资格是Project访问前提，Space管理员不绕过Project成员边界。
2. PRD 3.3.2、3.4.4：关联仅入口，解绑不改变原生访问；从Project发起建群只初始化当时的成员；非原生成员可以看关系但不能看聊天或子区；只关联群、不单独关联子区。
3. PRD 3.4.1–3.4.3：30字符名称、同组织重名、显式创建、成员数/角色、Owner/Admin设置权限、批量人员管理、唯一真人Owner及专用转让。
4. 人员添加对象资格复核、Agent owner_uid/rider生命周期继续保留；Owner/Admin可以添加符合目录资格的bot，不限定必须为操作者自己的bot；普通成员不能通过own-agent路径绕过人员管理权限。创建与转让都不得产生机器人Owner。
5. joined_at表示本轮加入：首次/重新加入更新，有效幂等添加和角色调整不更新；created_at保留首次记录。Owner-only Space撤权保留身份，nonOwner riders正常关闭，轮次和缓存失效幂等。
6. Project个人置顶只计当前有效可见成员Project；被撤权的历史偏好保留但不占槽位。关联群个人置顶与消息频道置顶独立；解绑隐藏、同Project重绑恢复个人偏好。
7. AI session container在关系读、计数、sidebar及变更服务中排除；实际关系变更推进group.version，无变化不推进。
8. 认证、共享UID限流、Space隔离、D14 wire400/i18n envelope、英文错误源和中文翻译同步保留；数据库失败不能降级为0计数/空结果/无权限。
9. RR只读快照、确定锁顺序、当前事务连接、仅重试可回放且未提交的DB事务保留；不能重试结果不确定的创建提交或盲重放IM副作用。
10. Fleet HMAC协议、既有outbox/lease/backoff/开关/监控保持；Drive使用独立OCTO_DRIVE_INTERNAL_TOKEN，配置凭据去重保持确定顺序，关闭时不出站；请求捕获测试须保持goroutine同步。

候选搜索当前确认契约为组织目录显示名的字面搜索。PRD 3.4.2 的“姓名、部门或邮箱”完整搜索不在当前已实现契约中，不能声明此条已全部覆盖；客户端应使用准确提示，部门/邮箱检索需对应目录数据与授权契约后另行对齐。任务移除影响计数、Loop任务/自动化/项目、文件及文档ACL、能力市场和客户端七Tab渲染由对应系统负责，不属于本次octo-server后端修复；Drive角色映射由Drive消费Project事实实现，不新增角色推送。

### 群与生命周期实现边界

- Project 自动建群和补建租约/CAS、成员/Owner/名称投影、专属判定与 Web/Bot 守卫、缺群/缺员对账不再运行。用户显式从 Project 建群继续可用；普通关联群可按原生权限添加、移除成员、转让群主、退出、解散、邀请和更改黑名单；Project 关系变更仍要求原生群权限和 Project 资格。
- Project 成员移除工单保留对 `removing`、重入取消、最终关闭席位和已有安全重试的职责；去掉仅服务于专属群的 Group 清理步骤，不能把旧工单视为对历史群的移除命令。Space 撤权的群面步骤仍遍历所有应清理的原生群，包括由旧自动建群留下的普通关联群；Space 席位恢复不自动把成员加回普通群。
- Space 席位变更造成的 Project epoch 失效、有效 Project Owner 身份保留、普通成员席位关闭仍按既有边界执行。停用仅为专属群服务的 `rejoined` 投影工单与其游标、重试钩子；对升级前已有工单只做安全终结，不执行旧 IM add/remove 或群主写入，也不误调用普通 Space removal 的破坏性步骤。
- Project 解散保留将所有关联群回落为 Space 直属的既有处理，群原生成员与消息保留；Space 撤权仍按普通群自身规则处理群主继任。Project Owner 不再决定任何群的群主；群现任群主失去 Space 资格时按原生群规则处理。
- 混合 collation 下的普通 Project/Space/群权限和关联查询继续使用数据库判等及既有空间隔离；专属指针判定与专属任务不再参与这些查询。Project/Space 资格或数据库查询失败不能放宽普通群原有的安全校验。

**I1 与 abandoned-cleanup 监控。** 两类扫描均以写侧的 Space 席位和 Project 生命周期语义为准；Owner 豁免仅适用于 `Project.status=normal`。Space 撤权或 Space 解散后，若 Project 仍正常，写侧保留的 Owner 身份不算席位泄漏；若 Project 已解散或不存在，仍为 active 且无有效 Space 席位的 Owner 是 stale active seat，I1 与 abandoned 扫描均须报告。普通成员缺失有效 Space 席位且没有待执行清理任务时仍须报告。两类查询在 `LIMIT` 限制的 inspected base page 上以 SELECT flag 计算，不把生命周期或席位状态放进过滤返回行的 `WHERE`；该豁免只影响只读监控，不授予访问资格；已应用 core migration 中的 I1 注释是历史口径，现行口径以本节和扫描实现为准。

**Space 清理任务。** 升级前已持久化的 `reason=rejoined` 任务不再投影 Project 群；在新版本按非破坏性终态处理，不能转交普通 removal steps/finalizers。`bot_deleted` 仍执行账号删除级联；普通 Space 撤权仍由原有工单处理。旧迁移注释保留历史原文，运行期原因处理以新实现为准。

**投递与并发边界。** 普通群 IM add/remove 的原生流程和通知语义保持不变；不将旧专属投影重试改造成通用 IM 投递系统。Space 清理当前逐 UID 执行，原生群群主继任及死锁重试保持既有行为；原有 Project/Space 席位锁与重入栅栏不能因群投影移除而放宽。

### 数据迁移与发布清单

- 保留新增 `modules/project/sql/20260910000001_project_read_default.sql`：只解除有效名称唯一索引；同名数据存在时Down可能失败，不删业务数据强行回滚。
- 保留 `modules/space/sql/20260910000002_group_project_linked_by.sql`：可空关联人，历史不伪造。
- 保留 `modules/project/sql/20260911000001_project_group_user_setting.sql`：个人Project关联群置顶存储及唯一键。
- 保留 `modules/project/sql/20260911000002_project_member_joined_at.sql`：单条可空ADD COLUMN，读侧COALESCE，旧写入可省略，不做回填/NOT NULL收缩。
- 已应用迁移保持原样。追加 Project 前向迁移删除 `octo_project.all_member_group_no` 与 `all_member_group_lease_until`；升级前记录存量关联群及消息/成员抽样，升级后核对原生群与 `group.project_id` 均未变。若数据库不支持预期在线 DDL 算法，迁移失败关闭，不接受隐式大表重建。
- 同步 Project/Sidebar API 文档及真实客户端消费方，撤去仅供专属群使用的错误码、翻译、配置、对账指标和运维告警；保留 Project/Space 共用的清理、epoch 与相关监控。其他已确认的用户 API、Drive 前提及字段切换责任不变。
- 用户API批量添加/Owner权限和sidebar DTO切换需调用方联合验收；列出仓库内真实调用点及外部责任系统，不假设前端已发布。Drive接口与30字符支持仍是外部部署前提。

### 实施顺序与验收门禁

1. 枚举受影响的服务实例、客户端字段消费者、Project/Group/Space/Bot/Category 写读路径及尚未完成的 Project removal、Space `rejoined` 工单；确认原生 Space 清理和 Project 席位关闭仍可独立完成。
2. 适配客户端并联合验收新 DTO；冻结受影响写入口，排空旧服务与在途专属群外部调用。部署不再访问专属列的新服务，确认它不会发起投影或由旧工单修改群；保留旧列直至所有旧实例退出。
3. 使用新前向迁移删除专属列，核对存量群、群成员、消息和 Project 关联记录。此后只允许使用兼容新模式的应用回滚包，不回放旧专属群任务。
4. 实际验证创建 Project 无群、历史群正常聊天和群面操作、Project 成员/Owner/名称独立、普通群创建快照、Space 撤权清理、Project 解散回落、旧工单及跨重启边界。窄用例先行，再验证 Project/Group/Space/Category 和受影响 Bot 路径；API 响应形状、错误与数据库失败需实测。

## 未决事项

设计已确认。外部客户端字段消费方需配合上线；未完成联合验收和旧实例排空前，不删除专属列。Drive 部署及目录部门/邮箱检索仍由对应系统负责。
