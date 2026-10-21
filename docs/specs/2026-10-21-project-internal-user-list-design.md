# Internal 指定用户 Project 全量列表设计

日期：2026-10-21

状态：已实现；已完成一轮独立规格审查、代码集成审查、相关包全量测试及独立 HTTP smoke，尚未部署。

## 背景与目标

服务间调用方需要在不持有目标用户 session、也不知道 Project ID 的情况下，查询指定用户在指定 Space 下有效加入且已完成激活的全部 Project，获得基础展示信息及该用户的 Project 角色。已完成激活指数据库字段 `activated_at IS NOT NULL`，沿用现有两阶段创建机制。

现有用户态列表 `GET /v1/space/:space_id/projects` 仅面向登录用户并采用分页；现有 internal membership 接口需要先提供 Project ID。本设计在 `modules/internal_membership` 增加反向列表查询，不改变这两类既有接口。

本次 Internal API 扩展独立于 `2026-09-10-project-prd-alignment-design.md` 的实施范围，沿用其中已确认的 Project 成员与 Space 资格语义。

## 范围

### 包含

- 一个服务间只读接口：指定 `space_id` 和 `uid`，完整返回有效加入且已完成激活的 Project。
- 基础信息与目标用户角色投影、服务凭据鉴权、Space 隔离、错误响应及行为验证。
- 复用既有 internal membership 配置、启用门槛和限流。

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
| 数据一致性 | 同一短生命周期 RR 只读事务 | 资格、项目状态和角色描述同一快照 |

## 接口契约

### 请求

`GET /v1/internal/projects?space_id=space-a&uid=user-a`

- `space_id` 和 `uid` 为必填、单值 query 参数，空值或带首尾空白的值返回参数错误；重复参数返回参数错误，不选择其中某一个值。先一次性解析完整 `RawQuery`，任一键或值的编码无法解析时整请求返回 `400`，不使用部分解析结果判定 selector。
- ID 沿用既有存量标识语义，不新增 UUID 格式约束，不在 Go 中用精确大小写比较替代数据库的标识判等。
- 凭据通过 `X-Internal-Token` 提供，值来自 `OCTO_MEMBERSHIP_INTERNAL_TOKEN`。
- 复用现有 `OCTO_MEMBERSHIP_INTERNAL_PEER_MAX_CACHE_AGE_SECONDS` 配置门槛及凭据互斥检查；既有配置不能启用 membership 模块时，本接口也不可用。
- 路由沿用具体路由上的执行顺序：既有请求读取边界中间件、共享 strict IP 限流、internal token 鉴权、参数解析和数据查询。与现有 membership 路由共用限流桶，不新增独立配额。
- 不接受用户 session 或其他能力的 internal token 作为替代凭据。`uid` 是经服务凭据授权的查询目标，不是客户端自报的登录身份；`space_id` query 是唯一 Space selector，其他 header 不得覆盖它。

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

| 字段 | 类型与含义 |
| --- | --- |
| `project_id` | string，数据库中的 Project ID |
| `space_id` | string，该 Project 所属 Space ID |
| `name` | string，现有名称原值 |
| `description` | string，现有描述原值，包含已支持的长描述，不截断 |
| `logo` | string，现有头像值，空值使用已存储的空字符串 |
| `role` | number，目标 `uid` 的 Project 角色：`0=成员`、`1=管理员`、`2=Owner` |

字段均存在；`role=0` 是有效成员角色，不得被省略。每个 Project 只出现一次，按 `octo_project.id DESC` 排序，顺序不受目标用户的置顶和隐藏偏好影响。无匹配结果返回 `[]`，不是 `null`。

## 成员资格、隔离与一致性

一次成功查询必须在同一个 `REPEATABLE READ` 只读事务中同时满足：

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

## 错误与边界情况

| 场景 | HTTP 状态及行为 |
| --- | --- |
| 必填参数缺失、空值、首尾空白或重复 | `400`，`err.shared.param.invalid`，details 使用对应 `field` |
| 完整 query 解析失败（含畸形重复值或键） | `400`，`err.shared.param.invalid`；不归因到单一 field，不使用部分解析结果，不返回 parser 内部原因 |
| 凭据缺失、错误或模块未启用 | `401`，`err.shared.auth.token_invalid`，不透露具体配置状态 |
| 超过现有 strict IP 限额 | `429`，沿用既有限流响应与 headers |
| 账号/Space 不存在、失效或不具备有效 Space 席位 | `200 []` |
| 项目未完成激活、已解散、其他 Space 的项目、目标未加入或席位移除中 | 不进入结果 |
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

## 实施与发布边界

本设计只新增读取入口，不需要数据迁移或历史回填。发布依赖现有 membership 服务凭据和 peer cache age 配置已正确启用；本次不更改其现有安全条件，也不声称任何外部消费方已经完成集成。

## 实施与验证记录

- 路由及 store：`modules/internal_membership/user_projects.go`、`api.go`、`store.go`；只读查询及六字段类型：`pkg/project/user_projects.go`。账号资格调用现有 `ActiveAccounts`；Space 先经 `ResolveSpaceID` 解析存储身份，再由 `ActiveMembers` 校验 Space 状态与 Space 席位；四条查询均在同一个显式 RR 只读事务中执行。
- 回归红灯：实现前 `TestInternalUserProjectsHTTPActivatedMembership` 在真实模块路由上得到 `404`，与预期 `200` 不符；实现后同一回归通过。
- `go test -p 1 ./pkg/project ./modules/internal_membership ./modules/project -count=1`：三包全量通过，执行期间持有 `/tmp/octo_testdb_lock`。
- 数据库回归覆盖 205 项目全量返回、角色与激活/撤权状态、混合 collation、大小写标识、语句故障和确定性跨连接快照；快照测试以默认 READ COMMITTED 的独立连接运行。HTTP 回归覆盖真实模块路由、strict 桶共享及拒绝路径读取边界、selector、服务凭据与真实 socket 响应。
- 独立一次性 Go 程序在隔离临时库和动态 loopback 端口上实际调用同一模块：观察到激活前仅返回一个项目，激活后带 `limit=1` 仍完整返回两个项目；跨 Space 查询、字面 `[]`、`401`、重复 selector `400`、移走项目表后的 `500` 均符合契约。程序结束后释放 listener 并删除临时库。
- `go build ./...`、`go vet ./pkg/project ./modules/internal_membership ./modules/project`、`make i18n-extract-check i18n-lint` 均通过。
- 审查修复回归：`TestUserProjectsInSpaceRejectsDistinctLegacySeatIdentity` 在修复前实际返回另一账号的 Owner 席位，畸形重复 selector 两例在修复前实际返回 `200` 而非 `400`；修复后这些回归、ASCII 大小写正例及上述三包全量测试再次通过。独立 HTTP 程序在真实混合 collation 临时库中验证 `s`/`ß` 账号隔离、停用与恢复、六字段响应及坏编码值/键的 `400`，输出 `REVIEW_FIX_HTTP_SMOKE_OK`；全仓库 build、相关包 vet 和 i18n 门禁再次通过。
- Space 身份回归：`TestUserProjectsInSpaceBindsQualifiedSpaceIdentity` 在修复前查询 `s` 实际返回 banned `ß` 的项目描述及 Owner 角色；修复后验证两侧 Space 独立绑定、ASCII 大小写、legacy selector 解析和 Space 恢复后隔离。`go test -race -shuffle=on -p 1 ./pkg/space ./pkg/project ./modules/internal_membership ./modules/project -count=1` 全量通过。独立程序在真实混合 collation 临时库和生产模块路由上验证相同 HTTP 行为，输出 `SPACE_IDENTITY_HTTP_SMOKE_OK`；全仓库 build、相关包 vet 和 i18n 门禁通过。

未执行全仓库测试或外部消费方联合验收。本机 WuKongIM `127.0.0.1:5001` 未运行，相关包测试初始化有 IM 连接失败日志；本接口的真实 MySQL、Redis、HTTP 路径及上述测试均通过，未使用 IM 替身。

## 未决事项

无。
