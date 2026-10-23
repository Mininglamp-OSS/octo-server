# Internal Project 查询设计

日期：2026-10-21

更新：2026-10-23

状态：用户查询模式与按 ID 批量查询模式均已实现；Server、Drive 与现有 CLI 已完成本地联合验收，尚未部署。

## 背景与目标

服务间调用方需要在不持有目标用户 session、也不知道 Project ID 的情况下，查询指定用户在指定 Space 下有效加入且已完成激活的全部 Project，获得基础展示信息及该用户的 Project 角色。已完成激活指数据库字段 `activated_at IS NOT NULL`，沿用现有两阶段创建机制。

现有用户态列表 `GET /v1/space/:space_id/projects` 仅面向登录用户并采用分页；现有 internal membership 接口需要先提供 Project ID。本设计在 `modules/internal_membership` 增加反向列表查询，不改变这两类既有接口。

Drive 的网盘成员关系独立于 Project 成员关系。Bot 可以直接获得某个网盘的权限，而不具备对应 Project 的成员身份。为使调用方在完成资源授权后按关联 Project ID 读取当前资料，本次在同一路由增加按 ID 批量查询；原用户查询模式及其响应保持不变。

本次 Internal API 扩展独立于 `2026-09-10-project-prd-alignment-design.md` 的实施范围，沿用其中已确认的 Project 成员与 Space 资格语义。

## 范围

### 包含

- 同一个服务间只读接口支持两种互斥选择条件：指定 `uid` 查询用户的全部有效已激活 Project，或指定 `project_ids` 批量查询基础资料。
- 基础信息与目标用户角色投影、服务凭据鉴权、Space 隔离、错误响应及行为验证。
- 复用既有 internal membership 配置、启用门槛和限流。
- 按 ID 查询返回相同六个字段，`role` 明确为 `null`，表示本次未查询用户角色；不检查任何用户或 Bot 的 Project 成员关系。

### 不包含

- 修改用户态列表、成员验证、epoch 或生命周期事件接口。
- 搜索、分页、角色筛选、个人置顶排序或隐藏偏好。
- 成员数、capabilities、群关系、Drive 信息及资源 ACL。
- 数据库迁移、数据写入、成员补建、自动创建 Project 或缓存失效体系扩展。

## 关键决策

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 用户的 Project | 当前有效加入且 `activated_at IS NOT NULL` 的 Project，包含成员、管理员和 Owner | 成员资格与既有 Project 读取一致，激活边界与 internal membership 一致 |
| 查询规模 | 单次返回全部匹配项，不分页、不截断 | 调用方无需预先知道项目或循环翻页 |
| 服务鉴权 | 复用 internal membership 凭据及启用条件 | 属于同一服务间成员事实查询能力 |
| 响应 | 直接 JSON 数组，仅含基础信息及目标用户角色 | 满足列表用途，不引入用户态完整 DTO 的个人偏好和计算字段 |
| 无有效资格 | `200 []` | 与 internal membership 的否定关系事实语义一致 |
| 按 ID 查询 | 同一路由增加 `project_ids` 选择条件，每批最多 50 个不同 ID | 复用现有鉴权与批量 ID 解析，不新增路由或查询模式参数 |
| 选择条件 | `uid` 与 `project_ids` 必须且只能出现一种 | 不设置隐含优先级，不用一个条件的错误或空值触发另一查询 |
| 按 ID 的角色字段 | 固定输出 `role: null` | 基础资料查询没有用户角色上下文；不以 `0` 表示未查询 |
| 数据一致性 | 同一短生命周期 RR 只读事务 | 资格、项目状态和角色描述同一快照 |

## 接口契约

### 请求

用户查询：`GET /v1/internal/projects?space_id=space-a&uid=user-a`

按 ID 查询：`GET /v1/internal/projects?space_id=space-a&project_ids=project-a,project-b`

- 两种模式均要求单值、非空、无首尾空白的 `space_id`。先一次性解析完整 `RawQuery`；任一键或值编码错误时整请求返回 `400`，不使用部分解析结果。
- 根据 query 中键是否出现选择模式，而非根据其值是否非空。`uid` 与 `project_ids` 同时出现，或同时缺失，均返回 `400 err.shared.param.invalid`，details 的 `field` 为 `uid|project_ids`。即使其中一个值为空，仍按键存在判定冲突。
- 用户模式沿用原 `uid` 的单值、非空、无首尾空白规则；重复值仍返回参数错误。
- 按 ID 模式复用 `parseIDList` 与 `maxBatchProjectIDs=50`：支持逗号分隔和重复 query 参数，修剪分隔项两侧空白，忽略空项并去重。解析后为空或超过 50 个不同 ID 时返回 `400`，details 的 `field` 为 `project_ids`；不新增另一套批量参数语法。大小写匹配沿用既有 `FoldID` 规则。
- `page`、`limit` 等参数不参与模式选择，不引入分页或额外查询模式。
- ID 沿用既有存量标识语义，不新增 UUID 格式约束，不在 Go 中用精确大小写比较替代数据库的标识判等。
- 凭据通过 `X-Internal-Token` 提供，值来自 `OCTO_MEMBERSHIP_INTERNAL_TOKEN`。
- 复用现有 `OCTO_MEMBERSHIP_INTERNAL_PEER_MAX_CACHE_AGE_SECONDS` 配置门槛及凭据互斥检查；既有配置不能启用 membership 模块时，本接口也不可用。
- 路由沿用具体路由上的执行顺序：既有请求读取边界中间件、共享 strict IP 限流、internal token 鉴权、参数解析和数据查询。与现有 membership 路由共用限流桶，不新增独立配额。
- 不接受用户 session 或其他能力的 internal token 作为替代凭据。用户模式的 `uid` 是经服务凭据授权的查询目标，不是客户端自报的登录身份；按 ID 模式由服务凭据授权读取指定项目资料，不携带用户身份。`space_id` query 是唯一 Space selector，其他 header 不得覆盖它。

### 成功响应

HTTP `200`，body 直接为数组，不包 `data` 或 `projects`，不返回分页元信息及 `X-Total-Count`。

```json
[
  {
    "project_id": "project-a",
    "space_id": "space-a",
    "name": "研发协作",
    "description": "项目说明",
    "logo": "",
    "role": 2
  }
]
```

上例是用户查询模式。按 ID 查询的每一行保持相同六个字段，仅 `role` 固定输出 JSON `null`；不省略字段，不输出字符串 `"null"`，不以 `0` 替代。该值表示“未查询用户角色”，不能解释为“不是成员”或用于授权。

| 字段 | 类型与含义 |
| --- | --- |
| `project_id` | string，数据库中的 Project ID |
| `space_id` | string，该 Project 所属 Space ID |
| `name` | string，现有名称原值 |
| `description` | string，现有描述原值，包含已支持的长描述，不截断 |
| `logo` | string，现有头像值，空值使用已存储的空字符串 |
| `role` | 用户模式为 number：`0=成员`、`1=管理员`、`2=Owner`；按 ID 模式固定为 JSON `null` |

两种模式的六个字段均存在；用户模式的 `role=0` 是有效成员角色，不得被省略。每个 Project 只出现一次。用户模式保留 `octo_project.id DESC` 排序，顺序不受目标用户的置顶和隐藏偏好影响。按 ID 模式不承诺输入顺序，调用方按 ID 匹配。无匹配结果均返回 `[]`，不是 `null`。

## 用户查询模式：成员资格、隔离与一致性

以下保持原用户查询契约不变。一次成功用户查询必须在同一个 `REPEATABLE READ` 只读事务中同时满足：

1. 目标账号符合既有 `pkg/user.ActiveAccounts` 有效账号条件：`status=1` 且 `is_destroy<>2`。注销冷静期不等于注销完成，不额外按真人或 Bot 类型过滤目标。
2. 指定 Space 存在且 `status=1`。
3. 目标用户在指定 Space 有 `status=1` 的 `space_member` 席位。
4. 返回的 Project 属于指定 Space，且 `octo_project.status=1`、`activated_at IS NOT NULL`。
5. 目标用户的 `octo_project_member` 满足 `status=1`、`removing=0`，且成员行的 `space_id` 与 Project 所属 Space 一致。

前三项任一不满足时返回 `200 []`；后两项决定各 Project 是否进入结果。Space Owner/Admin 不会自动得到未加入的 Project，保留的失效 Owner 身份记录也不构成访问资格。

激活条件与现有 internal epochs / `_verify` 一致；相比用户态 Project 列表，本接口增加 `activated_at IS NOT NULL` 过滤。只读取 Project 行的既有标记，不根据当前进程配置或 provisioning 任务状态推断。已有机制在无需 Fleet 确认时于创建阶段置位，需要确认时在成功确认后置位；本接口不改变其写入方式，也不将该标记解释为远端服务的实时健康状态。

实现接缝：

- `modules/internal_membership` 负责路由、现有鉴权/限流复用、参数、错误 facade 和最小响应投影；在既有 store 接缝增加对应列表能力。
- 数据查询放在 `pkg/project` 的只读事实层；该包不得导入 `modules/project`，不借用用户态 handler 或其带默认分页的服务来获取“全部”结果。
- 账号、Space 和 Space 席位资格使用相同事务连接，遵循现有独立查询方式，避免跨 legacy 表和 `octo_*` 表直接 JOIN 导致混合 collation 故障。Project 与 Project 成员通过 Project ID 及 Space ID 关联，并以指定 Space 和目标 UID 过滤。
- 返回数据库存储的 ID，不改写其大小写；跨读取结果匹配 ID 时沿用已有 ASCII-only `FoldID` 语义。同一事务中先通过 `pkg/space.ResolveSpaceID` 按 legacy 表的 collation 解析实际 Space ID，再用该 ID 校验 Space/席位资格并限定 Project 查询；不能以原始 selector 的字面值代替已解析身份。查询使用私有投影保留实际 `pm.uid` 和 `pm.space_id`，仅将席位 UID 与已合格目标 UID、Project 和席位双方的 Space ID 与已合格实际 Space ID 分别 `FoldID` 一致的行投影到六字段响应；SQL 的 collation 判等不能替代此身份绑定。这样保留 ASCII 大小写兼容，同时不将 legacy `0900_ai_ci` 下不同的账号或 Space 按 Project `general_ci` 的宽松匹配合并。查询不依赖个人偏好记录，不做逐项目查询。
- 沿用请求 context 开启 `Isolation: sql.LevelRepeatableRead, ReadOnly: true` 的事务，并设置已有 membership 单语句读取期限（当前为 5 秒）。所有读取使用能应用 `tx.Timeout` 的 `Load` / `LoadOne` 调用形态，不使用忽略该期限的 `Rows` / `Iterate` 路径。超过读取期限时整个请求返回 `500`，不截断结果、不增加重试；任何查询或事务错误都不返回已收集的部分结果。

响应只表示该事务快照时的有效成员关系及基础资料，不承诺在 HTTP 响应后持续有效。列表不返回 epoch；调用方不得把它作为长期授权快照，具体资源访问仍由资源侧校验，后续成员判定继续使用既有验证契约。

## 按 ID 查询模式：资料、隔离与一致性

1. 仅由 internal 服务凭据授权查询，不读取目标账号、Space 席位或 Project 成员关系，不将 `uid` 或 Bot owner 作为隐含选择条件。
2. 同一短生命周期 `REPEATABLE READ` 只读事务内，通过现有 `ResolveSpaceID` 解析实际 Space ID，并确认 Space 有效，再按该 Space 与请求的 ID 集合批量读取 `octo_project`。沿用现有请求 context 和 membership 单语句读取期限，不新增超时配置。
3. 只返回 `status=1` 且 `activated_at IS NOT NULL` 的项目。未知、失效 Space 返回 `[]`；未知、其他 Space、已解散或未激活的项目不进入结果。成功响应可能少于请求 ID 数量，这不是数据库查询失败。
4. 查询显式投影 `project_id`、`space_id`、`name`、`description`、`logo`，从 `octo_project` 读取当前存储值；不调用用户态分页接口，不逐项目查询，不读取完整成员或权限资料。
5. 保留数据库中的 ID 拼写。候选行的实际 Space ID 必须与已解析 Space 按既有 ASCII-only `FoldID` 规则绑定；Project ID 也必须按该规则属于请求集合，不能仅依赖宽松 SQL collation 匹配。重复或大小写变体不产生重复项目行。
6. 数据访问继续位于 `pkg/project`；在 `membershipStore` 增加独立的按 ID 读取方法，用户模式继续调用原方法。响应投影保证用户模式角色为整数，按 ID 模式角色为显式 `null`；不为统一 Go 类型而改写原用户查询的资格判断。
7. 不缓存、不回写名称、不发布同步事件、不增加重试。任一读取或事务失败返回错误，不转为空数组或已收集的部分结果。

本接口返回资料而非资源授权结果。调用方必须先按自己的资源权限确定可读取的资源，再读取关联项目资料；不得据此增加资源、推导成员身份或提升角色。

## 错误与边界情况

| 场景 | HTTP 状态及行为 |
| --- | --- |
| `space_id` 或用户模式 `uid` 缺失、空值、首尾空白或重复 | `400`，`err.shared.param.invalid`，details 使用对应 `field` |
| 完整 query 解析失败（含畸形重复值或键） | `400`，`err.shared.param.invalid`；不归因到单一 field，不使用部分解析结果，不返回 parser 内部原因 |
| `uid` 与 `project_ids` 同时出现或同时缺失 | `400`，`err.shared.param.invalid`，details 的 `field` 为 `uid|project_ids` |
| 按 ID 模式解析后 ID 列表为空或超过 50 个不同 ID | `400`，`err.shared.param.invalid`，details 的 `field` 为 `project_ids` |
| 凭据缺失、错误或模块未启用 | `401`，`err.shared.auth.token_invalid`，不透露具体配置状态 |
| 超过现有 strict IP 限额 | `429`，沿用既有限流响应与 headers |
| Space 不存在或失效 | 两种模式均返回 `200 []` |
| 账号不存在、失效或没有有效 Space 席位 | 用户模式返回 `200 []`；按 ID 模式不查询账号和席位 |
| 项目未完成激活、已解散、位于其他 Space | 两种模式均不进入结果 |
| 目标未加入项目或席位移除中 | 用户模式不进入结果；按 ID 模式不查询成员关系 |
| 数据库或事务失败 | `500`，`err.shared.internal`；记录内部原因，不以空列表或部分列表替代失败 |

错误沿用 `modules/internal_membership/api_i18n.go` 的真实 HTTP 状态及 i18n envelope，不套用用户态历史 wire 400。

## 验收标准

1. 同一 Space 下，目标用户分别以成员、管理员和 Owner 加入且已完成激活的项目均出现且角色正确；仅由目标创建的历史关系或 Space 管理员身份不扩大结果。
2. 同一用户加入另一 Space 的项目不会出现在当前 Space 的结果；Project 成员行与 Project 的 Space 不一致时也不出现。混合 collation 下，legacy Space `s` active、`ß` banned，目标在两者均保留 active Space 席位，只有 `ß` 存在有效 Project/Owner 席位时，查询 `s` 和 `ß` 均返回 `[]`。恢复 `ß` 后，每个 Space 只返回自身 Project/席位均合格的行；Project 与成员行任一侧实际 Space 不符均不能进入结果。保留 ASCII 大小写以及经 legacy 表解析到同一实际 Space 的 selector，响应仍返回 Project 的存储拼写。
3. `status=1`、成员资格有效但 `activated_at IS NULL` 的 Project 不出现；确认流程置位后，后续请求返回该项目。已解散、Project 席位非 active 或 `removing=1` 时不出现；退出后消失，重新有效加入且已完成激活后恢复并返回当前角色。
4. 账号被停用或注销完成、Space 失效、Space 席位撤销时，即使仍保留 active Project 席位或 Owner 记录也返回 `[]`；注销冷静期且其余资格有效时仍正常返回。
5. 合格用户无符合条件的项目、所有已加入项目均未完成激活、未知用户、未知 Space、非 Space 成员均返回 `200 []`，不返回 `null`。
6. 构造超过现有用户态默认及最大单页数量的匹配项目，单次请求完整返回，不受 `page`、`limit` 或分页默认值截断；重复读取顺序符合 `id DESC`。
7. 用户设置项目置顶或隐藏后，返回集合及顺序不变；`role=0` 和空 `logo` 字段保留，描述原样返回。
8. 缺失、空白及重复 selector 返回 `400`；畸形重复 selector 或畸形 query 键同样在业务查询前返回 `400`。用户 session 不能替代 internal token；配置不满足启用条件或凭据错误时返回 `401`；限流继续返回 `429`，拒绝请求不执行业务查询。
9. 通过资格读取与列表读取之间的确定性同步点，控制另一连接提交账号/Space 撤权或角色变更，验证当前响应仍描述同一旧快照、随后新请求反映新状态；用例在连接默认隔离为 READ COMMITTED 时也必须成立，不依赖调度时机或仅检查事务配置。
10. 资格查询、列表查询或事务失败返回 `500`，不会输出成功空数组或部分结果；混合 legacy/Project 表 collation 及大小写不同的有效存量 ID 不产生错误放行或精确字符串比较导致的误拒绝。混合 collation 下，以 active `s` 和 banned `ß` 两个不同 legacy 账号、仅 `ß` 持有 Owner 席位的夹具查询 `s`，必须返回空数组；恢复 `ß` 的账号资格后，也只返回其自己的项目。加入 `s`/`S` 的独立合法席位后，ASCII 大小写查询均保留其自身项目，两个账号不得互相串席位或角色。
11. 新路由使用真实模块路由挂载完成 HTTP smoke，核对凭据、完整数组、Space 隔离和故障状态；运行时覆盖新路由的读取边界→strict IP 限流→鉴权顺序，验证错误凭据仍消耗 strict IP 配额，401/429 拒绝路径仍受已有读取边界约束；不能仅依赖手工挂载的两条旧路由测试。现有 membership 与用户态 Project 列表契约保持不变。
12. 按 ID 查询在未提供用户 session、目标用户或 Bot 不具备 Project 成员身份时，仍返回指定有效已激活项目的基础资料；每行六个字段完整，`role` 是显式 JSON `null`。
13. 同一路由的原 `space_id+uid` 调用仍返回原集合、顺序和整数角色，含 `role=0`；新模式不改变现有成员资格、错误和全量列表契约。
14. `uid` 与 `project_ids` 同时存在时返回 `400`，包含任意一方为空的情况；两者都缺失、空 ID 列表、超过 50 个 ID 或完整 query 解析失败也返回参数错误，不执行业务查询。
15. 按 ID 模式覆盖逗号分隔、重复 query、空分隔项和去重；恰好 50 个不同 ID 可完整返回。不受 `page` 或 `limit` 截断，每个项目只出现一次。
16. 混合请求有效、未知、跨租户、已解散和未激活项目时，仅返回符合查询条件的项目；全部不匹配返回字面 `[]`。不因为目标用户缺少成员关系而丢失有效资料。
17. 覆盖现有混合 collation 下的实际 Space 身份绑定与 Project ID 匹配：ASCII 大小写兼容，非等价 ID 不扩大结果，响应保留存储拼写。
18. 项目改名提交后，下一次按 ID 查询返回新名称；不需要改动 Drive 数据、重启服务或清理新缓存。
19. 查询或事务失败返回 `500`，不是成功空数组或部分资料。复用真实模块路由完成 HTTP smoke，覆盖两种选择条件、鉴权、Space 隔离和显式空角色；不把历史测试记录当作新模式的验证。

## 实施与发布边界

本次扩展同一读取入口，不需要数据库迁移、历史回填或新配置。先发布 Server 的按 ID 模式，再接入 Drive，避免旧 Server 拒绝缺少 `uid` 的新请求。发布依赖现有 membership 服务凭据和 peer cache age 配置已正确启用；不改变现有安全条件。

Drive 接入边界：先完成现有网盘授权，再从结果收集 `project_id`，按所属租户分组、去重并每批最多 50 个调用新模式；只用基础资料刷新显示名称，不使用 `role:null` 推导授权。列表和详情共用名称读取，不改 CLI/Web 命令或网盘角色规则。名称读取复用现有 Project availability gate 和 membership 服务凭据，不新增名称缓存；上游遗漏项目或读取失败时，保持既有显示名称。接入实现与本地联合验收已完成。

## 用户查询模式的历史实施与验证记录

- 路由及 store：`modules/internal_membership/user_projects.go`、`api.go`、`store.go`；只读查询及六字段类型：`pkg/project/user_projects.go`。账号资格调用现有 `ActiveAccounts`；Space 先经 `ResolveSpaceID` 解析存储身份，再由 `ActiveMembers` 校验 Space 状态与 Space 席位；四条查询均在同一个显式 RR 只读事务中执行。
- 回归红灯：实现前 `TestInternalUserProjectsHTTPActivatedMembership` 在真实模块路由上得到 `404`，与预期 `200` 不符；实现后同一回归通过。
- `go test -p 1 ./pkg/project ./modules/internal_membership ./modules/project -count=1`：三包全量通过，执行期间持有 `/tmp/octo_testdb_lock`。
- 数据库回归覆盖 205 项目全量返回、角色与激活/撤权状态、混合 collation、大小写标识、语句故障和确定性跨连接快照；快照测试以默认 READ COMMITTED 的独立连接运行。HTTP 回归覆盖真实模块路由、strict 桶共享及拒绝路径读取边界、selector、服务凭据与真实 socket 响应。
- 独立一次性 Go 程序在隔离临时库和动态 loopback 端口上实际调用同一模块：观察到激活前仅返回一个项目，激活后带 `limit=1` 仍完整返回两个项目；跨 Space 查询、字面 `[]`、`401`、重复 selector `400`、移走项目表后的 `500` 均符合契约。程序结束后释放 listener 并删除临时库。
- `go build ./...`、`go vet ./pkg/project ./modules/internal_membership ./modules/project`、`make i18n-extract-check i18n-lint` 均通过。
- 审查修复回归：`TestUserProjectsInSpaceRejectsDistinctLegacySeatIdentity` 在修复前实际返回另一账号的 Owner 席位，畸形重复 selector 两例在修复前实际返回 `200` 而非 `400`；修复后这些回归、ASCII 大小写正例及上述三包全量测试再次通过。独立 HTTP 程序在真实混合 collation 临时库中验证 `s`/`ß` 账号隔离、停用与恢复、六字段响应及坏编码值/键的 `400`，输出 `REVIEW_FIX_HTTP_SMOKE_OK`；全仓库 build、相关包 vet 和 i18n 门禁再次通过。
- Space 身份回归：`TestUserProjectsInSpaceBindsQualifiedSpaceIdentity` 在修复前查询 `s` 实际返回 banned `ß` 的项目描述及 Owner 角色；修复后验证两侧 Space 独立绑定、ASCII 大小写、legacy selector 解析和 Space 恢复后隔离。`go test -race -shuffle=on -p 1 ./pkg/space ./pkg/project ./modules/internal_membership ./modules/project -count=1` 全量通过。独立程序在真实混合 collation 临时库和生产模块路由上验证相同 HTTP 行为，输出 `SPACE_IDENTITY_HTTP_SMOKE_OK`；全仓库 build、相关包 vet 和 i18n 门禁通过。

以下限制属于上述历史验证：未执行全仓库测试或外部消费方联合验收；当时本机 WuKongIM `127.0.0.1:5001` 未运行，相关包测试初始化有 IM 连接失败日志。上述 MySQL、Redis、HTTP 结果仅覆盖用户查询模式，不覆盖本次新增的按 ID 模式。

## 本次规格与实施记录

- 2026-10-23：完成确定性自检及一轮限定范围的独立规格审查，审查结果为通过，未发现阻塞问题。
- 2026-10-23：实现按 ID 批量读取；`modules/internal_membership/user_projects.go` 按 selector 键是否出现选择互斥模式，并沿用完整 query 解析及既有 `parseIDList`/50-ID 边界；`modules/internal_membership/store.go` 与 `pkg/project/user_projects.go` 增加独立 metadata 读取，在同一 RR 只读事务中解析并检查实际 Space，再批量筛选有效且已激活的 Project，按 ASCII-only `FoldID` 绑定 Space 和 Project ID，角色固定为显式 `null`。原用户查询资格逻辑与 `UserProject` 类型未改变。
- 回归红灯：实现前，真实路由的合法 `project_ids` 请求返回 `400` 而非 `200`；同时提供 `uid` 与 `project_ids` 的请求被错误接受。Drive 的 Bot 列表、详情及人类派生列表均实际返回旧名称；ASCII 大小写 ID 匹配和超过 1 MiB 的合法批量资料响应也分别观察到旧名和 JSON 截断失败。修复后对应回归通过。
- Server：`go test -race -shuffle=on -p 1 ./pkg/space ./pkg/project ./modules/internal_membership ./modules/project -count=1`、`go build ./...`、相关三包 `go vet`、`make i18n-extract-check i18n-lint` 均通过。
- Drive：专用 MySQL 测试库上的 `make build test vet`（全仓 race/shuffle 测试）及 Docker 构建通过，覆盖授权后刷新、租户分组、50-ID 分批、最终名称排序、ASCII ID 匹配及大响应读取。
- 独立本地进程挂载真实 Server internal 模块和真实 Drive space handler，通过未改动的 `octo-cli drive space list/get` 发起 HTTP 调用。连续两次提交 Project 改名后，下一次列表和详情均返回新名称；网盘可见数量、角色和本地来源不变。无权网盘详情返回 `403`，Drive 持久化名称仍为原快照，Bot 的 Project 成员记录数为 `0`。该烟测仅替换身份解析为既有 `auth.StubClient`，资料查询、Drive 授权、SQL 与 CLI 均使用真实实现。
- 独立 Server HTTP 调用另验证了六字段响应及显式空角色、跨 Space 空数组、selector 冲突 `400`、错误服务凭据 `401`，以及临时移走项目表后的 `500`。烟测使用本次专用临时 MySQL 容器；未执行生产环境验收或部署。

## 未决事项

无。
