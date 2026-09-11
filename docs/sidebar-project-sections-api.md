# Follow Sidebar Project Sections API

本文档面向客户端，描述 `sidebar-project-sections` 引入的接口与兼容性变化。
对应服务端实现见 PR #878。

## 1. 客户端接入概览

1. 使用 `GET /v1/spaces/{space_id}/sidebar-sections` 获取关注页顶层结构。
2. 顶层结构同时包含手动分类 `category` 和 Project `project`，按响应数组顺序渲染。
3. 拖动顶层条目后，将当前完整列表提交给
   `PUT /v1/spaces/{space_id}/sidebar-sections/sort`。
4. 使用 `POST /v1/sidebar/sync` 获取会话、未读数等动态数据；请求必须携带
   `X-Space-ID`，客户端才能获得 `project_id` / `project_name`。
5. `PUT /v1/projects/{project_id}/setting` 设置 `pinned=true` 后 Project 进入关注；
   设置 `pinned=false` 后从关注移除，即使用户仍是该 Project 的成员。

所有接口沿用现有登录鉴权。新建的 Sidebar Sections 接口还要求当前用户是路径中
`space_id` 对应 Space 的有效成员。

## 2. 数据类型

以下 TypeScript 定义表达实际 JSON 结构。

```ts
type SidebarSection = CategorySection | ProjectSection;

interface CategorySection {
  type: "category";
  id: string;                  // 等于 category.category_id
  sort: number;
  category: CategoryPayload;
  project?: never;
}

interface ProjectSection {
  type: "project";
  id: string;                  // 等于 project.project_id
  sort: number;
  project: ProjectPayload;
  category?: never;
}

interface CategoryPayload {
  category_id: string;
  name: string;
  sort: number;
  is_default: boolean;
  groups: CategoryGroup[];
}

interface CategoryGroup {
  group_no: string;
  name: string;
  category_sort: number;
}

interface ProjectPayload {
  project_id: string;
  project_name: string;
  logo: string;
  all_member_group_no: string; // 可能为空串
  groups: ProjectGroup[];
}

interface ProjectGroup {
  group_no: string;
  name: string;
  project_id: string;
  linked_by: string | null;
  pinned: boolean;
}

interface SidebarSectionSortItem {
  type: "category" | "project";
  id: string;
}
```

约束：

- `type` 是判别字段；`category` 与 `project` 只会出现一个。
- `id` 必须等于对应对象中的 `category_id` 或 `project_id`。
- 客户端应直接采用响应数组顺序。`sort` 用于表达服务端顺序，但不应假设其永久连续或唯一。
- `groups` 始终是数组；没有数据时为 `[]`，不是 `null`。
- 对当前 Project 成员，Project 的 `groups` 与 `GET /v1/projects/{project_id}/groups` 默认第一页口径一致：
  返回当前 Project 的全部存活关联群，不按调用者是否有原生 `group_member` 席位过滤，最多返回 50 条。需要更多群时继续调用原 Project groups 分页接口。
- `groups[0]` 不保证是全员群。如 UI 要固定全员群在首位，请比较
  `group_no === all_member_group_no` 后由客户端排序。

## 3. 获取关注页顶层结构

```http
GET /v1/spaces/{space_id}/sidebar-sections
```

### 成功响应

HTTP `200`，响应体为数组，不额外包裹 `data`：

```json
[
  {
    "type": "category",
    "id": "category-001",
    "sort": 0,
    "category": {
      "category_id": "category-001",
      "name": "其他会话",
      "sort": 0,
      "is_default": true,
      "groups": [
        {
          "group_no": "group-direct-001",
          "name": "临时讨论群",
          "category_sort": 0
        }
      ]
    }
  },
  {
    "type": "project",
    "id": "project-001",
    "sort": 1,
    "project": {
      "project_id": "project-001",
      "project_name": "供应链运营协同",
      "logo": "",
      "all_member_group_no": "group-all-001",
      "groups": [
        {
          "group_no": "group-all-001",
          "name": "全员群",
          "project_id": "project-001",
          "linked_by": null,
          "pinned": false
        }
      ]
    }
  }
]
```

### 可见性规则

- 用户创建或加入 Project 后，Project 默认进入关注。
- 用户置顶一个可见的 Space-listed Project 后，该 Project 进入关注。
- 非 Project 成员置顶 Space-listed Project 时，Project 可见，但 `groups: []`；
  置顶不会授予 Project 席位或群成员权限。
- 用户显式取消置顶后，该 Project 从关注移除，即使用户仍是 Project 成员。
- 再次置顶会恢复该 Project，保留此前的顶层排序位置。
- 已退出、已解散或不再可见的 Project 不会返回。
- Project 群只会出现在 Project 下，不会同时出现在手动分类中。

## 4. 更新关注页顶层排序

```http
PUT /v1/spaces/{space_id}/sidebar-sections/sort
Content-Type: application/json
```

请求体必须包含当前用户在该 Space 下的全部可见顶层条目，每个条目恰好一次；数组顺序
就是目标顺序。

```json
{
  "items": [
    {"type": "category", "id": "category-001"},
    {"type": "project", "id": "project-001"},
    {"type": "category", "id": "category-002"}
  ]
}
```

成功响应：

```json
{
  "status": 200
}
```

注意：

- 这是完整替换，不支持只提交被拖动的一个条目。
- 不允许重复项、未知 `type`、空 `id`、缺项或多项。
- 如果其他设备同时改变了可见列表，服务端会返回列表不匹配错误；客户端应重新 GET，
  基于最新列表重新执行排序。
- 当前接口没有客户端版本号或 CAS 参数，客户端不应在失败后盲目重试旧请求体。

## 5. Project 置顶与取消置顶

沿用 #861 已有接口：

```http
PUT /v1/projects/{project_id}/setting
Content-Type: application/json
```

置顶：

```json
{"pinned": true}
```

取消置顶：

```json
{"pinned": false}
```

成功响应为现有完整 Project 对象，其中 `pinned` 是写入后的状态：

```json
{
  "project_id": "project-001",
  "space_id": "space-001",
  "name": "供应链运营协同",
  "pinned": false
}
```

上例省略了 Project 对象中未变化的已有字段；真实响应仍返回完整对象。

行为说明：

- `pinned=true`：Project 加入关注；重复调用幂等。
- `pinned=false`：Project 从关注移除；重复调用幂等。
- 取消置顶不退出 Project、不退出群，也不改变任何成员权限。
- 再次置顶会恢复此前保留的顶层排序位置。
- 设置接口不返回完整 Sidebar Sections。调用成功后，客户端可以先更新本地 UI，随后
  重新请求 `GET /v1/spaces/{space_id}/sidebar-sections` 校准服务端状态。

## 6. Sidebar Sync 增量字段

现有接口：

```http
POST /v1/sidebar/sync
X-Space-ID: {space_id}
Content-Type: application/json
```

本次只为 `items[]` 增加 `project_name`，并保留已有 `project_id` 的字符串类型：

```ts
interface SidebarItemProjectFields {
  project_id?: string;
  project_name?: string;
}
```

示例：

```json
{
  "items": [
    {
      "target_type": 2,
      "target_id": "group-001",
      "channel_type": 2,
      "channel_id": "group-001",
      "space_id": "space-001",
      "project_id": "project-001",
      "project_name": "供应链运营协同",
      "timestamp": 1788955200000,
      "unread": 3,
      "is_pinned": false,
      "is_followed": true
    }
  ],
  "version": 123,
  "follow_version": 45
}
```

字段规则：

- Project 群：返回所属 Project 的 `project_id` 和 `project_name`。
- Project 群的 thread：继承父群的 `project_id` 和 `project_name`。
- 直属 Space 群、DM：不返回这两个字段。
- 不带 `X-Space-ID`：不返回这两个字段；客户端不得把缺失解释成直属 Space。
- Project 名称采用当前 Space 范围内的实时名称。
- 名称查询为 fail-soft：极端存储故障时，主 sidebar 请求仍可能成功且仅缺少
  `project_name`。因此客户端类型必须允许字段缺失，并使用无名称占位展示，不能用名称
  判定权限或身份。

## 7. 旧接口兼容性变化

### `GET /v1/spaces/{space_id}/categories`

响应结构不变，但分类顺序现在来自统一 Sidebar Sections 顺序。Project 条目不会出现在
该接口中，Project 群也不会进入任何手动分类。

### `PUT /v1/spaces/{space_id}/categories/sort`

请求和响应结构不变，但写入目标已切换到统一排序表。服务端只会在分类当前占据的
槽位内重排分类，不会移动夹在分类之间的 Project。该接口仍不能表达 Project 与分类之间
的相对拖动；新的混合关注页必须使用 `/sidebar-sections/sort`。

### `PUT /v1/groups/{group_no}/category`

请求结构不变：

```json
{"category_id": "category-001"}
```

如果目标群属于 Project，非空 `category_id` 会被拒绝：

```json
{
  "error": {
    "code": "err.server.category.project_group_cannot_categorize",
    "message": "A Project group cannot be placed in a manual category.",
    "details": {},
    "http_status": 409
  },
  "msg": "A Project group cannot be placed in a manual category.",
  "status": 400
}
```

兼容期内 HTTP transport status 仍为 `400`；客户端应优先读取
`error.code` 和 `error.http_status`。

`{"category_id":""}` 表示移出分类。即使目标群属于 Project，该清理请求也允许，
用于修复升级前遗留的异常分类关系。

### `POST /v1/group/create`

`project_id` 与 `category_id` 互斥。两者同时为非空时请求返回
`err.server.group.request_invalid`，不会创建群或写入分类关系。

## 8. Sidebar Sections 错误码

新接口沿用双层错误信封：HTTP transport status 在兼容期通常为 `400`，真实语义状态在
`error.http_status`。

| `error.code` | 语义状态 | 客户端处理 |
|---|---:|---|
| `err.server.category.request_invalid` | 400 | 修正请求；`details.field` 可能为 `items` |
| `err.server.category.sort_list_mismatch` | 400 | 重新 GET 后再排序 |
| `err.server.category.sort_list_duplicate` | 400 | 去重后重试 |
| `err.server.category.not_found` | 404 | 当前列表已变化，重新 GET |
| `err.server.category.space_member_required` | 403 | 退出当前 Space 页面或刷新成员状态 |
| `err.server.category.query_failed` | 500 | 可退避重试；不要覆盖本地已有数据 |
| `err.server.category.store_failed` | 500 | 排序未确认成功，重新 GET 校准 |

UID 限流触发时，沿用现有 `X-RateLimit-*` 响应头；客户端应按
`X-RateLimit-Retry-After` 退避。

## 9. 推荐客户端渲染流程

```text
进入 Space 关注页
  ├─ GET /sidebar-sections        -> 顶层分类/Project 结构及顺序
  └─ POST /sidebar/sync           -> 会话、未读、thread 等动态数据
          │
          ├─ project_id 匹配 ProjectSection.id
          └─ category_id 匹配 CategorySection.id

拖动顶层条目
  └─ PUT /sidebar-sections/sort   -> 提交完整可见顺序

置顶/取消置顶 Project
  ├─ PUT /projects/{id}/setting
  └─ 本地更新后重新 GET /sidebar-sections 校准
```

客户端不要从 `my_role`、`groups` 是否为空或 Project 是否置顶推导权限；权限继续以
Project 响应中的 `capabilities` 和具体操作接口返回结果为准。
