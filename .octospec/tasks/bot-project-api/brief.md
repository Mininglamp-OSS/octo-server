---
type: Task
title: "Task: bot-project-api"
description: 给 Bot API 增加 Project 只读端点 GET /v1/bot/projects 与 GET /v1/bot/projects/:project_id；可见范围 = bot 自身有效 Project 席位 ∪ 其 owner（robot.creator_uid）的有效席位，复用 modules/project 现有读查询与 Resp 投影。
tags: ["bot-api", "project", "space", "isolation", "auth", "acl", "wire-contract", "testing", "commit"]
timestamp: 2026-09-18T00:00:00+08:00
# --- octospec extension fields ---
slug: bot-project-api
upstream: none (owner request — 权限口径在会话子区由 owner 确认)
source: user
---

# Task: bot-project-api

## Goal

Bot（User Bot）具备 Project 只读能力，仅两个端点：

- `GET /v1/bot/projects?space_id=&keyword=&page=&limit=` — 列出该 Space 内此 bot 可见的项目，
  200 + 项目数组 + `X-Total-Count`（与人类侧 list 同形）。
- `GET /v1/bot/projects/:project_id` — 取单个项目，200 + 单个 `Resp` 对象；Space 由项目行推导，
  客户端不传、也传不了。

两个路由挂在既有 `botAPI` 组内（`authBot` + `requireBotIdentity` + per-bot 业务限流），
与 `GET /groups` / `GET /space/members` 同一中间件链，不新增专用 IP 桶。除 list/get
外的 Project 能力（写操作、成员、群关联等）一律不做；用户侧 API 行为不变。

## Background

- 人类侧读接口：`GET /v1/space/:space_id/projects`（`modules/project/api_read.go:12`）与
  `GET /v1/projects/:project_id`（`api_read.go:28`），挂载见 `modules/project/api.go:195`
  （AuthMiddleware → SharedUIDRateLimiter → space/project 中间件）。
- 人类侧读授权（`modules/project/service_read.go`）：调用者须有效账号
  （`user.status=1 AND COALESCE(user.is_destroy,0)<>2`）+ Space active + `space_member` active；
  list 只列调用者自己持有 active 席位（`pm.status=active AND pm.removing=0`）的项目；
  get 同样要求席位，且不存在 / 已解散 / 跨 Space / 无席位一律折叠成同一个 not-found（防枚举）。
- 分页：`page`/`limit`（默认 50、上限 200、页上限 100000）+ `X-Total-Count`；`keyword` 对 name 做
  转义 LIKE。响应投影 `Resp`（project_id/space_id/name/description/logo/creator/discoverability/
  max_members/member_count/human_member_count/agent_member_count/member_epoch/
  collaboration_role_epoch/status/all_member_group_no/pinned/my_role/capabilities/created_at/updated_at）。
- Bot 数据事实：User Bot 同时有 `user(robot=1)` + `robot` 行（`modules/project/db_agent.go:66`
  的账号资格口径与读路径一致），并由 botfather 绑定写 `space_member`；`robot.creator_uid` 是其 owner。
  App Bot 只有 `app_bot` 行、结构上禁止同名 `user`/`robot` 行（`modules/app_bot/db.go:59`），
  不可能持有 Project 席位。
- Owner 确认（本会话子区）：bot 目前无法作为成员加入 Project，可见范围走 **owner 名下项目**；
  调用身份仅 User Bot；list 的 `space_id` 必填。
- Bot 侧读取不能直接调用人类侧 handler/service 入口：`readProjects`/`readProject` 是包内方法，
  且查询以「调用者自己的席位」为过滤条件。跨模块先例是
  `ListProjectGroupRelationsByProjectIDs`（`modules/project/db_group.go:243`）：由 modules/project
  导出读函数、自建 DB façade，业务模块只消费结果。

## Load-bearing list

- **可见范围与 Space 隔离**（`touches: space, isolation, acl, auth, bot-api`）：
  可见项目集合 = `{p | p.space_id = :space_id AND p.status = 正常 AND (bot 有 active 席位 OR owner 有 active 席位)}`。
  owner 席位是 owner 名下项目在 bot 视角下的唯一新增可见面；不带 owner 展开、不跨 Space、不看 discoverability
  （该列是遗留展示字段，不承担安全边界，见 `modules/project/model.go:49`）。
  bot 自身席位保留：bot 真被加入为成员（组织目录内的分身路径）时它仍应看到该项目，且这是 `seat_only` 口径的严格子集。
- **席位活口检查**（`touches: acl, isolation, auth`）：席位集合里的每个 uid（除 caller 自己，
  它的 Space 资格由调用方闸门证明）都必须**当前**仍满足 caller 同一套谓词 —— `space_member` active +
  账号有效（`user.status=1 AND COALESCE(is_destroy,0)<>2`）+ Space active。理由：`octo_project_member`
  行的关闭是异步级联（`space_member_removal.go`），移除成员后到级联跑完之间存在窗口，工单进终态后则不再收敛；
  账号注销（`modules/user`）完全不触碰 `robot` / `space_member` / `octo_project_member`，席位行会永久残留。
  没有这道检查，owner 被移出 Space 或注销后，bot 仍能继续读 owner 的项目 —— 可见性会比 owner 本人的读权限活得更久。
  同一谓词用点查复用（`projectReadSpaceAccessTx`），不做跨表 join（该模块明确禁止授权依赖 legacy/`octo_*` 表的
  collation 一致）。
- **调用身份**（`touches: bot-api, auth`）：仅 User Bot（`bf_` token）。App Bot 一律
  `err.server.bot_api.app_bot_unsupported`，与 group/voice 等非 DM 面既有口径一致；
  它既无 `space_member` 席位也无 Project 席位。
- **Space 维度**（`touches: space, isolation`）：list 必填 `space_id`；bot 必须在目标 Space 有
  active `space_member` 席位且 Space active（复用 `projectReadSpaceAccessTx`），否则
  `err.server.bot_api.not_space_member`。多 Space bot 不做隐式回落（区别于
  `GET /v1/bot/space/members`：那里静默取首个 Space 会让「只看到一部分」读起来像正常结果）。
  get 的 Space 由项目行推导（与人类侧 `GET /v1/projects/:id` 一致），客户端不提供。
- **失败语义**（`touches: wire-contract`）：get 的一切不可见（不存在 / 已解散 / 跨 Space /
  bot 无 Space 席位 / 席位集合里没有任何仍然活口的席位）→ 同一个 `err.server.project.not_found`
  （404，沿用人类侧防枚举折叠，`pkg/errcode/project.go:240`）；list 非 Space 成员 →
  `err.server.bot_api.not_space_member`（403）；
  缺 `space_id` / 空 `project_id` → `err.server.bot_api.request_invalid`（400，带 field detail）；
  内部错误 → `err.server.bot_api.query_failed`（500，先 `ba.Error(..., zap.Error(err))`）。
  均用 `ResponseErrorLWithStatus`（保留真实状态码，bot adapters 以此分支）；不新增错误码 → 无 i18n 迁移。
- **投影与分页**（`touches: wire-contract`）：复用人类侧同一 `Resp`（`toResp` 单一投影，
  my_role = 席位集合中首个有 active 席位的 uid 的角色，bot 优先、其次 owner；pinned = bot 自己的
  `octo_project_user_setting`，通常为 false）。分页/关键字/排序与人类侧 list 完全同语义。
  空结果回 `[]`（非 null），与 bot_api 其它列表端点一致。

## 实现要点

- `modules/project/principal_read.go`（新）：导出 `ReadProjectsForPrincipal` /
  `ReadProjectForPrincipal` + façade（`&Project{db, cfg, Log}`，**不**走 `New()`——它会注册
  Space 移除级联步骤并启动 worker，二次构造会重复注册；`cfg` 经包级 `sync.OnceValue` 解析一次，
  不在读路径上重复解析环境变量）；导出 `ErrProjectReadNotFound` / `ErrProjectReadForbidden`
  供跨模块 `errors.Is` 映射。查询复用 `projectReadSpaceAccessTx` /
  `principalSeatsWithLiveSpaceAccessTx`（席位活口过滤）/
  `queryProjectReadMemberRoleTx`（get 经 `queryProjectReadMemberRoleAmongTx` 按席位优先级取角色）/
  `projectReadSeatCountsTx` / `queryProjectPinnedReadTx` / `toResp`。
- `modules/project/db_read.go`：新增 `listProjectsReadForPrincipalsTx`（每席位一个
  `LEFT JOIN octo_project_member`，PK 是 (project_id, uid)，因此不会造成项目行重复；
  `COALESCE` 取角色优先级；其余 WHERE/ORDER/分页与既有 list 谓词同形）。
- `modules/bot_api/projects.go`（新）：两个 handler + 统一错误映射；owner uid 取自
  `authBot` 已写入 context 的 `*robotModel.CreatorUID`（无额外 DB 查询）。
- `modules/bot_api/bot_api.go`：两条路由加进 `botAPI` 组。
- `modules/bot_api/api_i18n_test.go`：`TestBotAPINoLegacyResponseError` 文件清单登记 `projects.go`。

## Out of scope

- 写操作（创建 / 更新 / 解散 / 成员管理 / 群关联）与除 list/get 外的任何 Project 端点。
- 用户侧 API 行为、`modules/project` 既有读路径语义（仅新增并行入口，不改旧谓词）。
- 数据库迁移、新错误码、i18n 资源文件。
- Bot 侧 keyword/分页之外的新过滤（owner 继承之外的可见性扩展、跨 Space 聚合等）。

## Acceptance

- `go test ./modules/bot_api/... ./modules/project/...` 通过。
- 新增 handler 测试（`modules/bot_api/projects_test.go`）覆盖：
  - 成功：owner 席位项目 + bot 自身席位项目出现在 list（total/`X-Total-Count` 正确），
    get 与 list 中同一条对象一致；`my_role` 取席位角色（owner 项目上体现 owner 的角色优先级）。
  - 可见性边界：同 Space 无席位项目、跨 Space 项目都不出现（list）且 get 回 not_found；
    Space 内无 bot 席位时 list 回 `not_space_member`。
  - 失败/参数：无 token → 401（证明路由与 authBot 已挂载，缺路由是 404）；缺 `space_id` → 400
    request_invalid；App Bot token → 403 app_bot_unsupported；非法 `page`/`limit`（如 -5）不 500。
  - 分页：`limit=1&page=2` 返回第二页且 `X-Total-Count` 仍为总数；`keyword` 过滤生效；
    空结果是 `[]`（非 null）且拒绝请求不带 `X-Total-Count`。
  - 席位活口：owner 被移出 Space（`space_member.status=0`，项目席位行仍 active）后，
    owner 席位项目从 list 消失、get 回 not_found；Space 席位恢复后重新可见；
    owner 账号注销（`user.is_destroy=2`，Space 席位仍在）同样不再授权。
    （此用例在去掉活口过滤时必然失败 —— 反证覆盖有效。）
  - 已解散项目（`octo_project.status=0`）不在 list 且 get 回 not_found。
  - 置顶：bot 自己的 `octo_project_user_setting` 行让该行 `pinned=true` 并排在最前；
    别的 bot 的置顶行不影响本 bot 的行。
- 路由注册可调用（真机验收：部署后以 bot token 调这两个端点，与 Web 端项目列表对齐）。

## Review record

实现完成后做过两轮独立只读审查（`security-reviewer` + `reviewer`，各自独立读代码、不看对话）：

- 安全审查提出一处 medium：**owner 席位未锚定 owner 自身实时的 Space 在籍与账号有效性**，
  场景包括「成员被移出 Space 后席位关闭是异步的、工单终态后不再收敛」与「账号注销不触碰任何席位表」。
  已按建议落地为 `principalSeatsWithLiveSpaceAccessTx`（复用同一谓词、点查、不做跨表 join），
  并补上反向验证（去掉过滤该用例失败）。
- 安全审查的另外两条（`my_role`/`capabilities` 回显 owner 角色、per-bot 业务桶默认关闭
  只剩全局 per-IP 桶）不改代码：投影必须与人类侧同形且 bot 无写端点；限流姿态与既有
  `GET /groups` / `GET /space/members` 一致，已在 `projects.go` 头注释里写明。
- 逻辑审查结论为正确（无阻塞），指出 façade 每请求 `loadConfig()` 的重复解析 →
  改为包级 `sync.OnceValue`（与 `New()` 的构造期解析同语义）。

PR review（@Jerry-Xin，CHANGES_REQUESTED）指出导出 seam 的一处 fail-open：
`principalSeatsWithLiveSpaceAccessTx` 对长度 < 2 的席位集合直接放行，于是「caller 之外的
单个 uid」会仅凭席位行授权 —— 这正是活口检查要挡的过期席位。已修：除 caller 外的每个 uid
一律检查，不再有长度捷径（bot handler 的 `[bot]`/`[bot, owner]` 组合行为与查询数不变）；
并在 `modules/project/principal_read_test.go` 增加直接针对 seam 的用例（list + detail：
live / 移出 Space / 账号注销 / 恢复 / caller 自身单元素集合）。反向验证：恢复捷径时该用例失败。

同轮 review 的非阻塞项：席位集合此前无上界（每个 uid 一次 join + 一次点查），已在导出契约里
收紧为 `principalMaxSeatUIDs`（超限**拒绝**而非截断，避免静默丢掉调用方点名的 principal），
并补 `TestReadSeamsRejectAnOversizedPrincipalSet`（超限拒绝、恰好等于上界仍正常服务；
反向验证：关掉检查该用例失败）。另修掉 brief 末尾多余空行（`git diff --check` 报错）。
`check-sprint` 失败属流程项：该 job 校验的是 PR 在 Octo Board 上的 Sprint 字段
（`.github/workflows/check-sprint.yml` → 可复用工作流），与 `Closes #issue` 无关，
需要 Projects 写权限才能设置，不在本 pr 的代码改动范围内。
