# Octo API 详细规范

OCTO 项目 OpenAPI 接口规范的完整定义。

| 章节 | 主题 |
|---|---|
| [A. URL 设计](#a-url-设计r6--r10) | R6 + R10 |
| [B. 响应 Envelope](#b-响应-enveloper1) | R1 |
| [C. 字段与参数命名](#c-字段与参数命名r3--r7--r8) | R3 + R7 + R8 |
| [D. 错误码](#d-错误码r2) | R2 |
| [E. swag 注释](#e-swag-注释r13) | R13 |
| [F. 分页](#f-分页r5) | R5 |
| [G. 批量操作](#g-批量操作r11) | R11 |
| [H. Deprecate 流程](#h-deprecate-流程) | — |

### R1–R13 速查

引用规则编号时按此表定位章节；R4 / R9 / R12 无独立章节，由 lint 直接把守。

| R | 含义 | 章节 |
|---|---|---|
| R1 | 响应 envelope wire shape（2xx 顶层 `data`，4xx/5xx 顶层 `error`） | B |
| R2 | 错误码 12 项固定 enum + `details` 结构化 | D |
| R3 | 时间字段 `_at` 后缀（禁 `_time` / `_ts`） | C |
| R4 | 鉴权 endpoint 声明 401/403（404/500 warn 级） | D / E |
| R5 | 分页二选一：cursor `{has_more, next_cursor}` / offset `{total, page, page_size}` | F |
| R6 | URL 设计：snake_case 路径段 + 四角色段 | A |
| R7 | path 参数 `{<resource>_id}`，禁 `uid` / `_no` 等遗留 | A / C |
| R8 | 字段命名：snake_case / `is_` 布尔 / `_url` 后缀 | C |
| R9 | spec 必须 OpenAPI 3.1 | —（lint） |
| R10 | operationId `<resource>[.<sub>].<verb>` 2–3 段 | A |
| R11 | 批量走 `POST /<resource>/_batch`，all-or-nothing | G |
| R12 | 版本：`/v1` 唯一，`@Router` 相对路径（禁 `/v{n}` 前缀进 path） | A / E |
| R13 | swag 9 个必带标签（@Summary ≤80 / @Description 不重复 / 单 @Tags …） | E |

---

## A. URL 设计（R6 + R10）

> **本节是设计建议**。spectral 在 URL 上只 lint 字符级格式（snake_case 路径段、`_id` 后缀参数、operationId 三段格式等）。"该不该复数 / 前缀是否合理 / 该不该走中间件"是 PR review 范畴。

### A.1 仓库 = 服务命名空间

每个 OCTO 模块仓库 = 独立服务，客户端通过 `OCTO_API_BASE_URL` 网关分流。

**URL 分层**: 客户端看到的是 `<host>/[<service>/]api/v1/<resource>`，但 **service spec 只描述 `/v1/<resource>` 这一段**。`<service>/` 与 `/api/` 由网关层加（部署管），不进 OpenAPI；Go 内部 `r.Group("/v1")` + swag `@BasePath /v1`。

| 服务 | 网关挂载 | `servers:` URL | 资源域（示例）|
|---|---|---|---|
| `octo-server` | `/api/` | `https://<host>/api` | 核心 IM / 协作资源（matters / users / groups / messages / threads / ...）|
| `octo-matter` | `/matter/api/` | `https://<host>/matter/api` | matters + 子资源动作（`matters/{id}/_extract` / `.../timeline` / `.../assignees` 等）|
| `octo-smart-summary` | `/summary/api/` | `https://<host>/summary/api` | summaries + 配套（templates / schedules / `_infer`）+ internal/* |

各服务自有资源域，仓库自决；新增模块按本规范设计自己的 `/v1/<resource>`。单服务内**没有命名空间**——`/v1/internal/...` `/v1/admin/...` 等前缀按 A.2 四角色归 audience / domain / 动作。规范**逐仓库适用**，每仓库 CI 独立跑。

### A.2 URL 段的四角色

URL 一般形态（可叠加，每种最多一段）:

```
/v1/[<audience>/][<domain>/]<resource_plural>[/{id}][/<sub_plural>...][/_action]
```

| 角色 | 形态 | 何时用（heuristic）| 例 |
|---|---|---|---|
| **资源** | 复数 snake_case | 核心 entity（OCTO 各服务已知资源见 A.1 表格）| `/v1/users` `/v1/matters` |
| **域限定** | 单数 / 复数均可 | "前缀拿掉，资源名**在本服务内**是否歧义？"歧义 → 加。跨服务同名靠 base URL 路由消歧，**不需要**前缀 | `/v1/obo/grants`（octo-server 内 `grants` 歧义：OIDC / RBAC / 文件权限 / OBO）`/v1/auth/sessions`（同服务内 `sessions` 歧义：用户 / 语音 / 认证）`/v1/oidc/clients` |
| **受众标记** | 单数 / 复数均可 | "API 契约对不同消费方是否本质不同？"（SLA / 文档可见性 / SDK 生成）契约不同 → 加；纯权限差 → 走中间件 | `/v1/internal/notifications` `/v1/admin/users`（仅当与 `/v1/users` 契约真不同）|
| **资源动作** | `_` 前缀 | 非 CRUD 动词 | `/v1/users/_search` `/v1/matters/_batch` |

组合例:
- `/v1/users` —— 纯资源
- `/v1/matters/{matter_id}/assignees` —— 资源 + 子资源
- `/v1/users/_search` —— 资源 + 动作
- `/v1/obo/grants` —— 域限定 + 资源
- `/v1/internal/notifications` —— 受众 + 资源
- `/v1/internal/auth/sessions/{session_id}` —— 受众 + 域限定 + 资源 + id（罕见但合法）
- `/v1/matters/{matter_id}/close` —— 资源 + id + RPC 动词（状态机用动词原形，不加 `_`）

### A.3 格式约束（lint 阻断）

| 约束 | 触发规则 |
|---|---|
| 路径段 **snake_case**，禁 camelCase / PascalCase / kebab-case | `octo-path-snake-case` |
| Path 参数命名 `{<resource>_id}`，禁 `uid` / `_no` / `short_id` 等遗留 | `octo-path-param-id-suffix` + `octo-path-param-no-uid` |
| operationId `<resource>.<verb>` 或 `<resource>.<sub>.<verb>`，2–3 段，lowercase snake_case，`.` 分隔 | `octo-operation-id-format` |
| 路径**不得带 `/v{n}` 前缀**（`@Router` 相对路径，否则与 `servers:/v1` 叠成 `/v1/v1/...`）| `octo-path-no-version-prefix` |

### A.4 设计建议（doc 级，规则不报）

- URL 以 `/v1/` 开头（R12 — 当前唯一版本）
- 嵌套层级建议 ≤ 3 级（含 `/v1/` 起算）
- 状态 / 枚举值**不进路径** —— 走 body 或 query
- CRUD 用标准 HTTP 动词；**只有状态机**用 RPC 动词原形（close / reopen / archive / extract）
- 存量历史前缀（`/v1/manager/...` `/v1/admin/...` 等）若本质只做权限分流（与 `/v1/<resource>` 同契约），按模块逐步迁移到"资源 + 中间件"，见 `adoption.md` "存量仓库接入"

> swag `@Router` 写**相对路径**（不含 `/v1`），完整写法见 E 章节。

### A.5 operationId 例

格式约束已在 A.3 列；本节给具体例 + verb 选择 + swag 对齐：

| 层数 | 格式 | 例 |
|---|---|---|
| 2 层（标准 CRUD） | `<resource>.<verb>` | `matter.create` / `matter.list` / `matter.delete` |
| 3 层（子资源 / 状态机） | `<resource>.<sub>.<verb>` | `matter.assignee.add` / `matter.close` / `matter.transition` |

- verb 动词原形（CRUD: create / list / get / update / delete；子资源: add / remove；状态机: close / archive 等）
- 跟 swag `@ID` 标签完全一致

### A.6 反模式（doc 反例，PR review 拦截）

格式问题由 A.3 的 lint 规则抓；下列是规则抓不到、需 review 判断的设计错误：

| ⚠️ 反例 | ✅ 建议 | 错在哪 |
|---|---|---|
| `/v1/user/{id}` | `/v1/users/{user_id}` | 已知资源单数化 + 裸 id |
| `/v1/manager/backup/{id}` | `/v1/manager/backups/{backup_id}` | 嵌套资源单数 |
| `/v1/manager/adduser` | `POST /v1/manager/users` | 动词进 URL |
| `/v1/admin/users`（同契约） | `/v1/users` + 鉴权中间件 | audience 仅做权限分流，无契约差 |
| `/v1/manager/login` | `POST /v1/auth/sessions` | 认证流程跑错 audience |
| `/v1/summary/templates`（octo-smart-summary 内）/ `/v1/summary/summary_templates`（同 domain 重复）| `/v1/templates` / `/v1/summary/templates` | 网关挂载不进 spec；domain 段已说明域则资源段不再重复 |
| `/v1/common/appconfig` | `/v1/app_configs` | "common" 不表 audience / domain |

---

## B. 响应 Envelope（R1）

**契约是响应的 wire shape（JSON 结构），不是某个具体 Go 类型**。lint 校验生成 spec 的结构 —— 2xx 顶层必须有 `data`，4xx/5xx 顶层必须有 `error`；类型叫什么、内部怎么实现由各仓库自定。永远不要裸返 `[...]` / 自造 `{msg: ...}` 结构。

### 5 种响应形态（契约）

| 形态 | wire shape |
|---|---|
| 单条对象 / 创建后返回新建对象 | `{ "data": {...} }` |
| cursor 列表 | `{ "data": [...], "pagination": { "has_more": bool, "next_cursor": "..." } }` |
| offset 列表 | `{ "data": [...], "pagination": { "total": n, "page": n, "page_size": n } }` |
| 空成功（delete / 状态机动作） | `{ "data": {} }` |
| 失败（所有 4xx / 5xx） | `{ "error": { "code", "message", "details", "hint" } }`（字段语义见 D 章） |

swag `@Success` / `@Failure` 写法（类型为参考命名，见下节）：

| 响应形态 | swag 写法 |
|---|---|
| 单条 | `{object} envelope.Data[MatterResp]` |
| cursor 列表 | `{object} envelope.CursorList[MatterResp]` |
| offset 列表 | `{object} envelope.OffsetList[MatterResp]` |
| 空成功 | `{object} envelope.Data[EmptyResp]` |
| 失败 | `{object} envelope.Error` |

### cursor vs offset 选型

| 用 cursor 当 | 用 offset 当 |
|---|---|
| 数据量大，无限滚动 UI | 数据量小到中，需要跳页 |
| 列表会插入新数据，offset 漂移 | 列表稳定，靠 page 跳 |
| 客户端不需要总数 | 客户端要显示"共 N 条" |
| 例：消息流 / 通知 / event log | 例：matter 列表 / 用户管理表格 |

> 请求参数 / 响应 schema 见 F 章节。

### 统一返回抽象（官方实现：octo-lib `pkg/envelope`）

官方共享实现在 `github.com/Mininglamp-OSS/octo-lib/pkg/envelope`（零依赖、无 gin，非 wkhttp 服务也能引用）：

- 类型：`envelope.Data[T]` / `envelope.CursorList[T]` / `envelope.OffsetList[T]` / `envelope.Error` / `envelope.EmptyResp`
- wkhttp helper（成功侧）：单对象走 Context 方法 `c.ResponseData(x)` / `c.ResponseCreated(x)` / `c.ResponseEmpty()`（空成功 `{"data":{}}`；不叫 ResponseOK —— legacy `ResponseOK` 输出 `{"status":200}` 且有存量客户端依赖）；列表走泛型函数 `wkhttp.ResponseCursor(c, items, hasMore, nextCursor)` / `wkhttp.ResponseOffset(c, items, total, page, pageSize)`（`[]T` 编译期校验，nil 切片归一化为 `"data":[]`）
- R5 入参：`c.GetCursorParams()` / `c.GetOffsetParams()`（page_size 默认 20、上限 100）
- 失败侧无 helper：渲染走各服务注入的 `ErrorRenderer`（如 octo-server 的 `httperr.ResponseErrorL`，见 D 章）；`envelope.Error` 供 swag/客户端引用

swag 注解引用 octo-lib 类型的两个前提（缺一不可）：

1. 该 Go 文件 import envelope 包（handler 用 helper 时代码不直接引用类型，加 blank import 即可：`_ "github.com/Mininglamp-OSS/octo-lib/pkg/envelope"`）
2. swag 带 `--parseDependencyLevel 1`（`openapi-gen` 已内置，实测仅慢 ~1s）

边界与现实：

- lint **只看结构不看名字** —— 任何能让生成 schema 顶层出现 `data` / `error` 的实现都合规；不便依赖 octo-lib 的仓库可自定义同形态类型
- **不要**在注解里引用不存在的类型，也**不要**让注解声称与实际响应不符的结构
- 存量裸返接口会被 `octo-response-success-shape` / `octo-response-error-shape` 报 error —— 这是预期，迁移节奏见 `adoption.md`「存量仓库接入」

### 反模式

```go
❌ c.JSON(200, gin.H{"matters": matters})           // 裸返，顶层无 data
❌ c.JSON(200, matters)                             // 裸返数组
❌ c.AbortWithStatusJSON(400, gin.H{"msg": "..."}) // 失败响应顶层无 error
❌ 注解写 envelope.Data[X]、实际裸返                  // 注解与响应不符，spec 撒谎
✅ 统一抽象产出 { "data": ... } / { "error": ... }
```

---

## C. 字段与参数命名（R3 + R7 + R8）

所有 schema 字段、path/query 参数都必须遵守命名约定。spectral lint 会强制检查，不合规则 PR 阻断。

### 字段命名 + Go 类型 + swaggertype 对照

| 字段类型 | json tag 规则 | Go 类型 | swaggertype | 例 |
|---|---|---|---|---|
| 路径 / 响应 ID（R7）| `<resource>_id` | `string` | `"string,uuid"`（如需要）| `matter_id` / `user_id` |
| 普通字段（R8）| snake_case | 对应原生类型 | — | `title` / `page_size` |
| 布尔字段（R8）| `is_` / `has_` / `can_` 前缀 | `bool` | — | `is_active` / `has_more` |
| 时间字段（R3）| `_at` 后缀 | `string` / `*string` | `"string,date-time"`（RFC3339）| `created_at` / `due_at` |
| URL 字段（R8）| `_url` 后缀 | `string` | `"string,uri"` | `avatar_url` / `download_url` |
| 数组字段 | 跟元素相关的复数名 | `[]T` | — | `assignee_uids` / `tag_ids` |
| 大整数 | snake_case | `int64` | `"integer,int64"` | `total_bytes` |

> `creator_uid` 是历史保留——对应 octo-lib 的 user/uid 跨域语义；新字段一律用 `_id`，不新造 `_uid`。

### Go struct 示例

涵盖所有约束：必填 / 可选 / 时间 / URL / 数组 / bool / id。

```go
type CreateMatterReq struct {
    Title        string   `json:"title"          binding:"required,max=200"`
    AssigneeUIDs []string `json:"assignee_uids,omitempty"`
    DueAt        *string  `json:"due_at,omitempty"   swaggertype:"string,date-time"`
}

type MatterResp struct {
    MatterID  string  `json:"matter_id"`
    Title     string  `json:"title"`
    IsUrgent  bool    `json:"is_urgent"`
    AvatarURL string  `json:"avatar_url,omitempty"   swaggertype:"string,uri"`
    CreatedAt string  `json:"created_at"             swaggertype:"string,date-time"`
}
```

要点（struct 里不一眼可见的约束）：
- Go 标识符 PascalCase（`AssigneeUIDs` / `AvatarURL`）；**OpenAPI yaml 里的字段名取自 json tag**，必须 snake_case
- 可选字段：json tag 加 `omitempty`；语义上可 null → Go 类型用指针（`*string`）
- 入参校验：`binding:"required,max=200"` 等 gin 标签

### 反模式

| ❌ 错 | ✅ 对 | 违反 |
|---|---|---|
| `pageSize` / `createTime` | `page_size` / `created_at` | R8 camelCase |
| `active` / `more` | `is_active` / `has_more` | R8 无前缀布尔 |
| `avatar` | `avatar_url` | R8 URL 无后缀 |
| `created_time` / `create_at` / `created_ts` / `created` | `created_at` | R3 时间字段必须 `_at` 后缀 |
| `id` (path param) | `matter_id` | R7 裸 id |
| `uid` / `short_id` / `*_no` | `<resource>_id` | R7 历史命名全禁 |
| json tag `MatterID` / `matterId` | json tag `matter_id` | R8 — json tag 必须 snake_case（Go 字段名是 PascalCase 没事）|

---

## D. 错误码（R2）

12 项固定 enum + `details` 双层结构。**永远** 从这 12 项选，不要新造 code。子分类通过 `details` 字段表达。

### 12 项错误码 enum

| HTTP | code | 何时用 | details 建议字段 |
|---|---|---|---|
| 401 | `AUTH_REQUIRED` | token 缺/过期/无效 | `details.reason: missing\|expired\|invalid` |
| 403 | `FORBIDDEN` | 鉴权过但无权限 | `details.required_role` / `details.resource` |
| 404 | `NOT_FOUND` | 资源不存在 | `details.resource: "matter"` |
| 409 | `CONFLICT` | 状态/版本冲突 | `details.conflict_reason` / `details.current_state` |
| 409 | `DUPLICATE` | 重复创建 | `details.existing_id` |
| 400 | `VALIDATION_ERROR` | 入参校验失败 | `details.field` / `details.reason` |
| 413 | `PAYLOAD_TOO_LARGE` | body / 文件过大 | `details.max_bytes` / `details.actual_bytes` |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Content-Type 不支持 | `details.expected` / `details.actual` |
| 426 | `CLIENT_VERSION_TOO_OLD` | 客户端版本低 | `details.min_version` |
| 429 | `RATE_LIMITED` | 频控 | `details.retry_after_seconds` |
| 500 | `INTERNAL_ERROR` | 兜底 | 不暴露内部细节 |
| 503 | `UPSTREAM_UNAVAILABLE` | 上游故障 | `details.upstream` / `details.upstream_status` |

### 响应结构

```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "Matter not found",
    "details": { "resource": "matter" },
    "hint": "Verify the matter_id and try again."
  }
}
```

字段语义：
- `code` — 12 项 enum 之一
- `message` — 一句话英文描述（i18n 在客户端做）
- `details` — 结构化子分类信息，机器读
- `hint` — 给用户的修复建议，人读（可选）

### handler 用法

格式：`httperr.ResponseErrorL(c, errCode, detailsMap, hintMap)`。`details` / `hint` 都是 `map[string]any{...}`，传 `nil` 跳过。

```go
import "github.com/Mininglamp-OSS/octo-server/pkg/errcode"
import "github.com/Mininglamp-OSS/octo-server/pkg/httperr"

httperr.ResponseErrorL(c, errcode.ErrValidation,
    map[string]any{"field": "title", "reason": "exceeds 200 chars"},
    map[string]any{"hint": "Title must be ≤ 200 chars"})
```

### swag @Failure 标签

每个鉴权 endpoint 至少声明这些响应：

```go
// @Failure 400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure 401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure 403 {object} envelope.Error "FORBIDDEN: not owner"
// @Failure 404 {object} envelope.Error "NOT_FOUND"
// @Failure 429 {object} envelope.Error "RATE_LIMITED"
```

根据 endpoint 的实际错误集合补 409 / 413 等。

### 反模式

```
❌ 自造 code: "BAD_REQUEST" / "INVALID_PARAMETER" / "USER_NOT_FOUND"
   → 都属 VALIDATION_ERROR 或 NOT_FOUND；details 字段区分

❌ HTTP code 全用 400 或 500 一把梭
   → 404/403/409 等是协议含义，客户端依赖

❌ details 写成字符串: details: "field title too long"
   → 必须结构化对象，机器要读

❌ message 写中文
   → i18n 在客户端做，server 返英文
```

---

## E. swag 注释（R13）

每个 gin handler 上方必须有完整的 swag godoc 注释。9 个必带标签全部写齐，spectral lint 会强制检查。

### 9 个必带标签

| 标签 | 必带 | 规则 |
|---|---|---|
| `@Summary` | ✅ | ≤80 字符，英文动词大写开头 |
| `@Description` | ✅ | 1-3 句补充，不重复 Summary；含幂等性/副作用提示 |
| `@Tags` | ✅ | 单值，lowercase snake_case，跟 module 名一致 |
| `@ID` | ✅ | operationId，`<resource>.<verb>` 或 `<resource>.<sub>.<verb>` |
| `@Accept` / `@Produce` | ✅ | **响应只允许 `application/json`**（lint 强制：`octo-response-success-json-only` / `octo-response-error-json-only`）。请求侧文件上传照常 `multipart/form-data`。真实字节流端点（文件/语音下载）对 2xx 走仓库级 spectral override 显式豁免并在 PR review 说明；**错误响应必须 JSON，无豁免**；302 重定向无 body 不受影响 |
| `@Security` | ✅（鉴权时）| 用 `Bearer` |
| `@Param` | ✅（有参时）| 所有 path/query/body 参数都列 |
| `@Success` | ✅ | 至少 1 个 + envelope 类型 |
| `@Failure` | ✅（鉴权时）| 至少 401/403/404/500 |
| `@Router` | ✅ | **相对路径**（不带 `/v1` 前缀）+ method |

> ⚠️ **`@Router` 必须相对路径**：main.go 的 `@BasePath /v1` 已转成 OpenAPI `servers: [{url: /v1}]`，`@Router` 再写 `/v1/...` 会让客户端请求变 `/v1/v1/...` 重复。
>
> ✅ `@Router /matters/{matter_id} [delete]`　　❌ `@Router /v1/matters/{matter_id} [delete]`

### 完整模板

```go
// MatterDelete godoc
// @Summary       Delete matter
// @Description   Delete a matter the caller owns. Idempotent: returns 200 even if already deleted.
// @Tags          matter
// @ID            matter.delete
// @Accept        json
// @Produce       json
// @Security      Bearer
// @Param         matter_id path string true "Matter ID"
// @Success       200 {object} envelope.Data[EmptyResp] "Matter deleted"
// @Failure       400 {object} envelope.Error            "VALIDATION_ERROR"
// @Failure       401 {object} envelope.Error            "AUTH_REQUIRED"
// @Failure       403 {object} envelope.Error            "FORBIDDEN: not owner"
// @Failure       404 {object} envelope.Error            "NOT_FOUND"
// @Failure       429 {object} envelope.Error            "RATE_LIMITED"
// @Router        /matters/{matter_id} [delete]
func (h *MatterHandler) Delete(c *wkhttp.Context) { ... }
```

> 客户端实际请求是 `DELETE /v1/matters/{matter_id}`（servers `/v1` + path `/matters/{matter_id}` 拼接）。
>
> `envelope.*` 为参考命名（B 章「建议的统一返回抽象」）—— lint 校验解析后的 schema 形态（顶层 `data` / `error`），类型名与实现由仓库自定。

### @Param 写法

格式：`@Param <name> <in> <type> <required> "<description>"`

`<in>` 取值: `path` / `query` / `body` / `header` / `formData`（文件上传）。

```go
// @Param matter_id path   string true  "Matter ID"
// @Param page_size query  int    false "Page size, default 20"
// @Param body      body   CreateMatterReq true "Request body"
```

### @Success / @Failure 写法

格式：`@Success|@Failure <http_code> {object} <envelope_type>[<inner_type>] "<message>"`

`<envelope_type>` 从 5 种 envelope 中选（见 B 章选型表）。`<message>` 可省略；失败响应一般写 code（如 `"NOT_FOUND"`）便于阅读。

### 全局 main.go 必带

每个仓库的 `main.go` 必须有一次性的全局注解：

```go
// @title       Octo Server API
// @version     1.0.0
// @host        api.octo.example
// @BasePath    /v1
// @tag.name        matter
// @tag.description Task and todo management
// @tag.name        message
// @tag.description IM messaging
// @securityDefinitions.apikey Bearer
// @in     header
// @name   Authorization
```

每个 tag 必须在 `@tag.name` 声明 + `@tag.description` 描述，handler 用 `@Tags <name>` 引用。

### 反模式

| ❌ 错 | ✅ 对 |
|---|---|
| `@Summary 创建 matter`（中文） | `@Summary Create matter` |
| `@Summary` > 80 字符 | ≤ 80；写不下挪 `@Description` |
| `@Description` 跟 `@Summary` 重复 | `@Description` 补幂等性 / 副作用 / 默认值等 |
| `@ID createMatter`（camelCase）| `@ID matter.create` |
| `@Tags matter, admin`（多 tag）| 单 tag |
| 漏 `@Failure` | 401/403/404/500 至少齐 |
| `@Success 200 {object} MatterResp` | `@Success 200 {object} envelope.Data[MatterResp]` |

---

## F. 分页（R5）

列表 endpoint 必须选 cursor 或 offset 之一，跟 B 部分的 envelope 选型对应。

### 请求参数

| 模式 | query 参数 | Go struct |
|---|---|---|
| cursor | `cursor` (string, optional) + `page_size` (int, optional, default 20, max 100) | `Cursor string \`form:"cursor"\`` + `PageSize int \`form:"page_size,default=20" binding:"min=1,max=100"\`` |
| offset | `page` (int, optional, default 1) + `page_size` (int, optional, default 20, max 100) | `Page int \`form:"page,default=1" binding:"min=1"\`` + `PageSize int \`form:"page_size,default=20" binding:"min=1,max=100"\`` |

### 响应 pagination 字段 + swag

| 模式 | `pagination` 字段 | swag |
|---|---|---|
| cursor | `has_more` (bool) + `next_cursor` (string, 可省略 / null when `has_more=false`) | `// @Success 200 {object} envelope.CursorList[MatterResp]` |
| offset | `total` (int) + `page` (int) + `page_size` (int) | `// @Success 200 {object} envelope.OffsetList[MatterResp]` |

`next_cursor` 是**不透明字符串**（base64 编码的服务端状态），客户端原样回传，不可在客户端解析。

> lint 强制：`octo-pagination-shape`（error，pagination 形态必须二选一且 cursor 禁带 `total`）+ `octo-pagination-params-match`（warn，cursor 响应须声明 `cursor` query 参数 / offset 须声明 `page`）。

swag 入参注释（除上面 @Success 外）：

```go
// cursor 模式
// @Param cursor    query string false "Cursor for next page"
// @Param page_size query int    false "Page size, default 20, max 100"

// offset 模式
// @Param page      query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
```

### 反模式

```
❌ cursor 是 JSON 字符串明文（客户端可破解）
✅ base64 编码 + 服务端签名 / 加密

❌ has_more 不返回，客户端拿空数组判断结束
✅ 永远显式返 has_more 字段

❌ page_size 无上限（DOS 风险）
✅ 设上限（一般 100）+ binding 校验

❌ cursor 模式还返 total（多余且性能开销）
✅ cursor 只返 has_more + next_cursor
```

---

## G. 批量操作（R11）

批量操作走 `_batch` 后缀的独立 endpoint，**全或无**（all-or-nothing）语义。

### URL 模板

```
POST /v1/<resource_plural>/_batch
```

例：
- `POST /v1/matters/_batch` — 批量创建 matter
- `POST /v1/messages/_batch` — 批量发消息

operationId 用 `<resource>.batch_create` / `<resource>.batch_update` 等。

> lint 强制：`octo-batch-post-only`（error，`_batch` 路径仅允许 POST）+ `octo-batch-requires-body`（error，必带 requestBody）。

### all-or-nothing 语义

- 入参数组里**任一项**校验失败 → 整批拒绝，返 400 `VALIDATION_ERROR`，`details.failed_index` 指向第一个失败项
- 入参数组里**任一项**业务失败 → 整批回滚，返对应 error code，`details.failed_index` 指向失败项
- 全部成功 → 返 200 + 全部新建对象数组

不允许"部分成功部分失败"的混合结果 —— 简化客户端错误处理。

### 请求/响应结构

```go
type BatchCreateMattersReq struct {
    Items []CreateMatterReq `json:"items" binding:"required,min=1,max=100,dive"`
}

type BatchCreateMattersResp struct {
    Items []MatterResp `json:"items"`
}
```

```go
// @Param body body BatchCreateMattersReq true "Batch request"
// @Success 201 {object} envelope.Data[BatchCreateMattersResp]
// @Failure 400 {object} envelope.Error "VALIDATION_ERROR: details.failed_index"
```

### 错误响应（部分失败时）

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "Batch validation failed",
    "details": {
      "failed_index": 3,
      "field": "title",
      "reason": "exceeds 200 chars"
    }
  }
}
```

### 反模式

```
❌ 部分成功部分失败的混合响应（{succeeded: [...], failed: [...]}）
✅ all-or-nothing，要么全成要么全失败

❌ 批量上限不设（一次塞 10000 个）
✅ 设上限（一般 100）+ binding 校验

❌ 用 query 参数传批量数据
✅ 永远走 POST body

❌ 没有 _batch 后缀，跟单条创建用同一个 URL
✅ 独立 endpoint，URL 显式标 _batch
```

---

## H. Deprecate 流程

废弃一个 endpoint 或字段而不直接删除（保留过渡期，让客户端有时间迁移）。

### swag 注释

```go
// MatterCreateLegacy godoc
// @Summary       Create matter (legacy)
// @Description   Deprecated as of 2026-06-01. Removal planned 2026-09-01. Use POST /v1/matters instead.
// @Deprecated    true
// @Tags          matter
// @ID            matter.create_legacy
// @Router        /matter [post]
// ... 其它标签照常
func (h *MatterHandler) CreateLegacy(c *wkhttp.Context) { ... }
```

swag 看到 `@Deprecated true` 会在生成的 OpenAPI 里给该 operation 加 `deprecated: true`，客户端 SDK 生成器看到这个标志会发出 warning。

> lint：`octo-deprecated-needs-guidance`（warn）要求 deprecated operation 的 description 写明替代方案 / 移除计划（含 instead / removal / sunset 等关键词）。

### HTTP 响应 header

废弃 endpoint 的 handler 实际响应时**必带**两个 header，告诉客户端何时停止用：

```go
c.Header("Deprecation", "true")
c.Header("Sunset", "Wed, 01 Sep 2026 00:00:00 GMT")  // RFC 3339 / IMF-fixdate
c.Header("Link", `</v1/matters>; rel="successor-version"`)  // 替代 endpoint
```

- `Deprecation: true` — 标记已废弃（RFC draft）
- `Sunset: <HTTP-date>` — 计划移除日期（RFC 8594）
- `Link: <url>; rel="successor-version"` — 替代方案

客户端拿到这些 header 应该记录日志 / 提示用户升级。

### 字段级 deprecate

字段单独废弃（保留 endpoint，但某字段要移除），在 Go struct 的 swag 注释 / json tag 描述里标：

```go
type MatterResp struct {
    MatterID  string  `json:"matter_id"`
    Title     string  `json:"title"`
    // Deprecated: use `creator_id` instead. Removal planned 2026-09-01.
    CreatorUID string `json:"creator_uid,omitempty" extensions:"x-deprecated=true"`
    CreatorID  string `json:"creator_id"`
}
```

swag 会把 `extensions` 透传到 spec property 上（如 `x-deprecated: true`）。

### 移除时机

| 类型 | 推荐过渡期 |
|---|---|
| endpoint 整体废弃 | ≥ 90 天 + 客户端确认升级完成 |
| 字段废弃 | ≥ 60 天 |
| 错误码改 | 不允许直接改（属 breaking）；新增 code 不算 |

在 `Sunset` 到期之前不要删 —— 实际删的 PR 是另一次 breaking change。

### 反模式

```
❌ 直接删 endpoint / 字段（没标 deprecate 就移除）
✅ 先 deprecate → 等过渡期 → 再删

❌ swag 标 @Deprecated 但 handler 没加 Deprecation / Sunset header
✅ 标记 + header 一起做，客户端运行时能感知

❌ Description 没说替代方案 / 移除日期
✅ Description 写明：何时废弃 + 何时移除 + 替代是什么
```
