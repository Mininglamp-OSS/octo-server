---
type: Task
title: "Task: file-extension-policy-dynamic-config"
description: Move the file-upload extension policy and size cap from process-init env / scattered constants to an immutable snapshot backed by system_setting, and surface the effective limits through /v1/common/appconfig.
tags: ["trust-boundary", "external-content", "error-response", "i18n", "wire-contract", "bot-api", "testing"]
timestamp: 2026-08-26T00:00:00Z
slug: file-extension-policy-dynamic-config
upstream: 无（用户口头提出：上传白名单能否免重启）
source: user
---

# Task: file-extension-policy-dynamic-config

## Goal

把文件上传的**扩展名策略**与**大小上限**从「进程 `init()` 读 env 改写包级 map」
和「散落三处的硬编码常量」改成 **不可变策略快照 + `system_setting` 动态配置**，
并通过 `/v1/common/appconfig` 下发有效值供客户端预校验。

三个 `system_setting` 键：

| 键 | 方向 | 约束 | 回退链 |
|---|---|---|---|
| `file.extra_blocked_extensions` | 封堵（安全方向） | 叠加在内置黑名单之上；**不可撤销** baseline | DB ∪ env → baseline |
| `file.extra_allowed_extensions` | 放开 | 可放开**任意**扩展名；内置黑名单不可撤销（写侧拒绝 + 读侧压制） | DB ∪ env → baseline |
| `file.max_size_kb` | 上限 | `Positive: true` + 服务端硬上限 clamp | DB → code default（**无 env 层**） |

两个方向都不需要重启、不需要发版。唯一的硬约束是**内置黑名单不可撤销**
（`.exe` / `.php` / `.sh` / `.js` / `.apk` …）：写侧当场拒绝，读侧永远压制。

`DM_FILE_EXTRA_ALLOWED` / `DM_FILE_EXTRA_BLOCKED` 作为兼容回退层保留，
**DB 未配置时解析结果与本次改动前逐字节相同**。

appconfig 新增字段（与既有 `sticker_upload_limits` 并列、同单位）：

```json
"file_upload_limits": {
  "max_size_kb": 102400,
  "allowed_extensions": [".jpg", ".png", "…", ".zip"]
}
```

## Background

### 扩展名策略现状（已核实）

- `modules/file/const.go:174,209` 两张包级 `map[string]bool`；`const.go:246` 的
  `init()` → `loadExtensionsFromEnv()` 读 env 后**直接写这两个 map**。
  函数注释自陈：`// 仅在 init() 中调用，不可在运行时重复调用（map 无并发写保护）`。
- `IsAllowedExtension` / `IsBlockedExtension`（`const.go:286,295`）是**包级函数**。
  生产调用点 **6 处 / 3 个模块**：`file/api.go:348,886`、
  `bot_api/file.go:172,274`、`robot/api.go:2118,2221`。
- baseline 允许集 **74 项**（JSON 序列化约 546 字节）。

### 大小上限现状（已核实）：100MB 有三份独立定义

| 检查点 | 当前来源 | 路径 |
|---|---|---|
| `file/api.go:293` | `MaxFileSize` + 1MB 余量 | multipart body reader |
| `file/api.go:307` | `MaxFileSize` | multipart 校验 |
| `file/api.go:859` | `MaxFileSize` | 预签名签发 |
| `bot_api/file.go:70` | **本地 `const maxSize`** | bot multipart |
| `bot_api/file.go:266` | `file.MaxFileSize` | bot 预签名 |
| `robot/api.go:2058` | **本地 `const maxSize`** | robot multipart |
| `robot/api.go:2213` | `file.MaxFileSize` | robot 预签名 |

**7 个检查点、3 个来源。** 只动 `file.MaxFileSize` 会让 bot / robot 的 multipart
路径继续用写死的 100MB —— 配置调小了那两条路径不生效。本任务顺带消除这三份重复。

`MaxFileSize` 是跨包引用的 `const`（`file.MaxFileSize`），动态化后必须变成函数，
**三个模块的调用点都要改**——这与扩展名键「保持包级函数签名不变」的策略不同。

### `system_setting` 可复用的部分

`modules/common/system_settings.go` 已在生产验证：schema 白名单（65 键 / 19 category）、
`atomic.Pointer` 不可变快照无锁读、超管写入 + 单事务批量 upsert + 按类型校验、
管理台 `effective_value` 回显。

**两个直接照抄的范式**：

1. **`SpaceDisableUserCreate`（`system_settings.go:643`）—— 三层链的标准写法。**
   用独立 `lookup` 的 `ok` 区分「DB 缺行 → 回退 env」与「DB 显式写 0 → 压制 env」，
   而不是直接调 `getBool`。两个扩展名键照此实现。
2. **`StickerUploadAllowedFormats`（`:1779`）—— 只能收窄的白名单。**
   读侧求交、非法 token 丢弃、**全部非法时回退完整默认集**（bad config 不得暗关功能）。

### 三条不能想当然的事实（评审已纠正）

1. **不存在通用的「DB → env → yaml → default」回退链。**
   `lookup()`（`:259`）只读 DB 快照；`getBool/getString/getInt` 的 fallback 是
   **调用方传参**。env 层由各 getter 自己实现。
   → 必须自己写 `fileExtra{Allowed,Blocked}FromEnv()`；且
   `threadAutoArchiveDaysFromEnv`（`:820`）的注释确立了约束：
   **env 兼容层不得重新解释既有部署的配置语义**。
2. **「当前实例即时生效」不成立。** `api_manager_system_setting.go:484` 的
   `loadWithGeneration` 失败时只 `m.Warn(...)`，随后照常 `c.ResponseOK()`。
   对「紧急封堵」是致命的——运维拿到 200 却未生效。见 O4。
3. **跨实例收敛约 60s**（`defaultReloadTTL`）。本仓不存在 `modules/featuregate`，
   `system_setting` 是唯一动态配置层。

### 写侧的一个硬约束

`settingTypeInt` 默认范围是 `[settingIntMin, settingIntMax] = [0, 3650]`
（`system_setting_schema.go:27`）。`file.max_size_kb` 的值是 102400，
**必须设 `Positive: true`** 才能跳过该上界（走「必须正整数」校验），
与 `sticker.upload_max_size_kb` 那组同样处理；真正的上界由读侧 clamp 承担。

### 现有敞口（不由本任务引入，也不得被本任务扩大）

- `.html` / `.htm` **已在内置允许集**（`const.go:181`）；`.svg` 今天也能由旧 env 放开。
- `ValidateMagicNumber`（`const.go:74`）对**没有魔数定义的扩展名直接返回 `true`**，
  「放开一个扩展名」= 同时「跳过内容校验」。这是早期设计要求候选集的理由，
  实施评审否掉了（见 D8）：`.html` / `.htm` 本来就在内置允许集里，候选集防不住
  这个口子，而真正的边界（内置黑名单不可撤销）在另一层。
- **OSS V1 签名不覆盖 `Content-Length`**（`api.go:809` 已记录）：预签名路径上
  OSS 的大小上限只是 advisory。**动态化不改变这一点**——运维调小 `max_size_kb`
  在 OSS 部署上仍挡不住超量 PUT，必须在键的 Description 里写明，否则会产生虚假安全感。
- 下载侧不受上传白名单约束，且不能只看 `getFile`（`api.go:716` 是 302，头不生效）；
  真正能强制 attachment 的是 `/v1/file/download/url`（`api.go:1071`）签发的
  `response-content-disposition` + 上传时写入的对象元数据。属独立任务。

### appconfig 现状

- `/v1/common/appconfig` 挂在 `commonNoAuth`（`common/api.go:87`）—— **公开端点，无鉴权**。
- handler 有 **version 短路分支**（`api.go:389`）：新字段必须**两个分支都填**，
  否则老客户端命中短路后拿不到最新值（`sticker_upload_limits` 的注释即为此写）。
- 既有 `sticker_upload_limits` 用 `max_size_kb`，故本任务统一用 kb，零单位转换。

### 决策（2026-08-26 已确认）

- **D1 blocked 合并语义 = 并集**（`env ∪ DB`，只增不减）。覆盖语义会让「封堵 A」
  意外解封 env 里已封的 B。代价：env 封的扩展名无法从管理台解封，只能改 env + 重启
  —— 刻意如此：安全方向给快通道，解封方向留在发布流程里。
- ~~**D2 allowed 合并语义 = DB 覆盖 env**~~ — 已由 D9 取代。
- **D9（2026-08-26 修订）：allowed 也改成 env ∪ DB 并集。**
  核对现网配置时发现覆盖语义是个事故源：生产 env 放开着
  `.tgz/.xlsm/.key/.numbers/.pages/.heic`，运维想再加一个格式时若在管理台只填
  新的那个，前六个**当场失效**、用户立刻传不了，而他不会想到这一层。
  加一个格式是高频操作，作废一批格式是事故。
  撤销能力没丢：黑名单优先级最高，收回某个 env 放开项写进 `extra_blocked` 即可。
  规矩因此统一成一句话 —— **「允许」栏只管加，「禁止」栏只管减**。
- **D8（2026-08-26 修订，取代原候选集设计）：放开方向不设代码候选集。**
  早期设计要求 `extra_allowed` 只能命中一张代码内候选集，理由是「放开一个扩展名 =
  同时跳过内容校验」。实施评审否掉：(a) 那道保险防不住主要风险 —— `.html` /
  `.htm` **本来就在内置允许集里**，浏览器可渲染这个口子今天就是敞的；
  (b) 真正的安全边界是内置黑名单不可撤销，那是 `modules/file/policy.go` 的派生
  层，与候选集无关；(c) 需求本身就是「不重启放开一个格式」，候选集把它变成
  「仍要发一次版」，等于没解决问题。
  配套：把内置黑名单项写进 `extra_allowed` 时**写侧直接 400**
  （`ErrFileExtensionNotAllowlistable`），否则值存进去、`effective_value` 不显示它、
  上传照样被拒，没有任何地方解释原因。「哪些扩展名不可放开」由 `modules/file`
  经探针注册给 `common`（依赖倒置，同 appconfig provider）。
- **D3 审计落在通用层**：加在 `updateSystemSettings`，65 个键统一受益。
- **D4 reload 失败可见性 = 响应体回带本实例生效状态**（新增字段，老前端忽略，
  向后兼容）。**不阻塞落库** —— 落库本身是对的，其他实例仍在 60s 内收敛；
  操作者需要知道的是「已入库、本实例未收敛」，而不是拿到一个无差别的 200。
- **D5 `fileMaxSizeKBHardCap = 524288`（512MB）**。更大的传输应走分片 / 网盘，
  不走 IM 附件通道。
- **D6 与 `sticker.upload_max_size_kb` 的组合约束 = 写侧拒绝**，用
  `ViolatesThreadArchiveOrdering` 的 merge-then-validate 范式（校验
  merge(当前快照, 入参) 而非任一半）。理由：两个键各自看都合法、组合却让贴纸
  永远传不上去，正是本仓已经踩过一次才加守卫的那类配置。
- **D7 公开端点下发有效白名单 = 可接受**（白名单可由试传枚举出来，低风险），
  但**不下发 blocked 列表**。

### 待补充（不阻塞实施）

- ~~**O1 候选集初始内容**~~ — 已由 D8 取消：不再有候选集，`extra_allowed`
  开箱即可放开任意非黑名单扩展名。
- ~~**O2 现网 env 实际值**~~ — 已核对（2026-08-26）：生产
  `.tgz,.xlsm,.key,.numbers,.pages,.heic`，测试 `.tgz,.xlsm`；均不命中内置黑名单，
  迁移后全部继续生效。其中 `.key/.numbers/.pages/.heic` 在 baseline 里本就存在，
  env 配置属历史冗余；真正只靠 env 撑着的是 `.tgz` 与 `.xlsm`。两组值已固化为
  `TestExtensionPolicy_DeployedEnvValuesRemainEffective`。
  设计上已规避：env 层与 DB 层走同一套语法清洗，且不再有候选集这类额外约束
  （D8），故现网 env 放开过的扩展名不会因本次改动失效。

## Load-bearing list

<!-- touches: trust-boundary, external-content, error-response, i18n, wire-contract, bot-api, testing -->

- **上传扩展名白名单是信任边界本身**（`trust-boundary` / `external-content`）：
  「用户上传的字节能否落进对象存储」的唯一格式门，下游无第二道格式校验。
- **内置黑名单是不可撤销的安全基线**（`const.go:209`）。现有 env 逻辑已保证
  「extra_allowed 命中黑名单则忽略 + 打日志」（`const.go:257`），迁移后必须保持，
  且 DB 层不得成为绕过它的新通道。
- **`IsAllowedExtension` / `IsBlockedExtension` 包级函数签名**：6 个调用点跨三模块，
  目标是**签名不变**、调用方零改动。
- **`MaxFileSize` 跨包 const 引用**：动态化必然改签名，`bot_api` / `robot` 联动，
  且要消除两处本地 `const maxSize` 复制。**7 个检查点必须同源**。
- **`MaxBytesReader` 与校验阈值同源**（`api.go:293` 现为 `MaxFileSize + 1MB` 余量）：
  两者若不同源，超限会退化成 EOF / `request body too large`，而不是友好错误。
- **env 语义等价**：`TestLoadExtensionsFromEnv`（`const_test.go:577`）的 **11 条子用例**
  是 env 层现行规范（大小写/空格容错、无点号补全、纯点号与 `..` 丢弃、含路径分隔符丢弃、
  黑名单优先、同名同现以黑名单为准、空 env 不影响现有配置），逐条必须继续成立。
- **现有 env 逻辑的顺序依赖**（`const.go:270-273` 命令式 `delete`）：改成声明式派生
  `(base ∪ dbAllowed) − (baseBlocked ∪ dbBlocked ∪ envBlocked)` 会消掉它——需确认
  与上述第 11 条子用例结论一致，不得悄悄改变结果。
- **并发正确性**：读侧全在 HTTP handler，今天靠「只在 init 写」规避竞态。
  改后必须不可变快照原子发布，`-race` 无 data race。
- **贴纸是第二层门，顺序不能变**（`api.go:340-370`：先全局黑/白名单，再
  `stickerLimits.allowedFormats`；size 同理先全局后贴纸）。
- **`stickerLimits()` 的 nil-safe 范式**（`sticker_compress.go:109`）：未挂 settings
  回落硬编码默认值，老单测逐字节等价——新策略快照照此办理。
- **`system_setting` 写侧契约**：schema 未注册的 `(category,key)` 必须 400；超管校验；
  单事务；`Effective` 回显须反映 DB→env→baseline 的实际结果。
- **appconfig wire 契约**（`wire-contract`）：公开端点；**version 短路分支与全量分支
  都必须下发** `file_upload_limits`；下发的是**服务端算完的有效值**，不是 baseline；
  **不下发 blocked 列表**。
- **manager 写入响应契约**（`wire-contract`，D4）：`updateSystemSettings` 响应新增
  本实例生效状态字段，向后兼容（老前端忽略）；落库与生效状态解耦，reload 失败
  不得回滚已提交的事务。
- **客户端预校验只是 UX，服务端始终兜底**：封堵后客户端缓存必然滞后 ≤60s，
  「选了文件、上传被拒」是预期行为而非 bug。
- **i18n**（`error-response` / `i18n`）：**新增**用户可见错误走 `httperr` + `pkg/errcode`；
  不主动迁移本模块既有裸 `c.ResponseError`（那是独立的 i18n 迁移工作，避免范围蔓延）。
  `respondBotAPIFileTooLarge` / `respondRobotFileTooLarge` 已带 MB 参数，传动态值即可，
  i18n 侧零改动。

## Out of scope

- **下载侧强制 attachment / content-disposition 加固**（需逐存储后端 + 对象元数据）。
- **OSS V1 签名不覆盖 Content-Length 这一既有缺口**：本任务只记录，不修。
- **魔数表扩充**（`fileMagicNumbers`）与「无魔数定义即跳过校验」这一既有行为。
- **其它 env 迁移**（本仓 113 个字面 env）。已分档，另立 task：
  档 1「读路径已是每次读」约 20 个可合为一个 PR；
  档 2 含 `DM_AUTH_SLOWLOG_MS`（`pkg/auth/parser.go:24` 包级 var）、
  `DM_GROUP_INVITE_SLOW_LOG_MS`（`group/service.go:161` 包级 var）、
  `DM_DEFAULT_CATEGORY_NAME`（`category/config.go:15` sync.Once）等需先改读路径；
  登录失败 / OIDC callback 阈值属防暴破边界，须带不可突破上限 + 审计 + 回滚约束，单独立 task。
- **全局 IP/UID 限流参数动态化**（中间件启动期装配，需 octo-lib 支持）。
- **缩短 60s TTL / Redis pub/sub 秒级下发**。
- **贴纸相关键**（`sticker.*` 已动态，不动）。

## Review 修复记录（2026-08-26）

首版实现被 review 打回两个阻断项，均已修复并补了负向验证（撤掉修复 → 用例必红）：

- **P0 装配缺失。** `File.New()` 创建了 `SystemSettings` 却没调
  `SetPolicySettings()`，`currentPolicy()` 一直走「未挂载」分支
  （env + baseline + 默认 100MB）。管理台写入 `file.*` 照常落库、照常返回 200，
  但**对任何上传入口都不生效**，也不反映到 appconfig —— 功能整体是死的，
  且没有任何报错。
  **为什么没测出来**：当时每一条用例都用 `useSettings()` 手动注入 fake，
  没有一条走过真实装配路径（`module.Setup → New(ctx)`）。手动注入的测试再多，
  也证明不了生产上那根线接上了。
  修复：`New` 挂载 provider；新增 `policy_integration_test.go`（本包因此引入
  infra 依赖，接受）+ 零 infra 的 `TestNewMountsPolicySettingsSource` 双保险。

- **P1 两条 multipart 路径绕过扩展名门。** `bot_api.botUploadFile` 完全不校验
  扩展名，`robot.botUploadFile` 只拒空扩展名。封堵 `.pdf` 后主接口与预签名都
  拒绝，这两条路径仍能把文件写进对象存储 —— 与「统一动态封堵」的目标直接冲突，
  也违反 trust-boundary 规则的 sibling parity。
  **为什么没测出来**：parity 守卫只统计 `IsAllowedExtension` 调用次数，
  验证的是「调用点没变少」，不是「每个入口都有门」—— 一个压根没有门的入口
  照样 PASS。
  修复：两处补同一份策略校验；新增 handler 行为测试
  （`modules/bot_api/file_extension_gate_test.go`、
  `modules/robot/file_extension_gate_test.go`），断言被拒时 mock 的
  `UploadFile` **未被调用** —— 要防的是字节落进对象存储，不是状态码。
  守卫改为「行为测试为主 + 计数只盯入口数量变化」，并在注释里写明这个分工。

**行为变更（需在 PR 说明）**：`POST /v1/bot/file/upload` 与
`POST /v1/robot/file/upload` 现在会拒绝黑名单/白名单外的扩展名。此前这两条路径
可以上传任意扩展名（含 `.exe`），属于收紧。

## Review 修复记录（第二轮，2026-08-26）

- **P1 快照指纹碰撞。** `policyKey` 把 allow/block 拼成
  `"allowed|blocked|kb"`，而扩展名清洗只拒绝空 / `.` / `..` / 含路径分隔符 /
  含连续点的 token —— `|` 是合法字符。于是
  `allowed=[".a"] blocked=[".b|.pdf"]` 与 `allowed=[".a|.b"] blocked=[".pdf"]`
  指纹相同：从前者切到后者时缓存命中旧快照，`.pdf` 实际仍可上传、appconfig 也
  仍下发旧清单，而管理台已经返回 `applied=true` —— **紧急封堵静默失效**。
  修复：快照改存输入切片副本，用 `slices.Equal` 逐项比较，彻底消除编码歧义
  （任何拼接方案都可能碰撞，换分隔符只是把碰撞推远）。
  回归：`TestPolicy_SnapshotKeyDoesNotCollide` 及其反向、乱序变体。

- **P2 上限展示被整除截断。** `file.max_size_kb` 接受任意 KB 值，而提示一律
  `bytes/1024/1024`：配成 1536KB 时服务端实际放行 1.5MB，却告诉客户端
  「不能超过 1MB」—— 一个服务端并不执行的上限。四个出口全中
  （file 的 multipart 与预签名、bot_api、robot）。
  修复：新增 `file.FormatSizeLimit`（`"1.5 MB"` / `"100 MB"` / `"512 KB"`）与
  `file.SizeLimitDetails`；三个 errcode 的消息改用 `{{.max_size}}` 插值，
  `SafeDetailKeys` 增加**精确的** `max_size_kb`，`max_mb` 保留整除语义仅作兼容。
  file 模块那两处原本是裸 `c.ResponseError`，既然要改面向用户的文案，就按仓库
  规则一并迁进本地化信封（新增 `errcode.ErrFileUploadTooLarge`）。
  回归：`TestFormatSizeLimit*` / `TestSizeLimitDetails` /
  `TestPresignedOversizeReportsExactCap`（走真实 renderer 验证渲染后的文案与
  详情）/ 源码守卫 `TestSizeLimitIsNotTruncatedForDisplay`。

**连带调整**：`api_unit_test.go` 里超限那条直驱用例（`gin.CreateTestContext`，
无 route 故无 ErrorRenderer）只能看到兜底 `{msg,status}`、msg 是未插值模板，
其上限断言迁到有 renderer 的集成路径，覆盖比原先更强；
`respond{BotAPI,Robot}FileTooLarge` 的签名从 MB 改为 bytes，相应 helper 测试
改用 1.5MB 这种非整数 MB 的上限钉住精度。

## Review 修复记录（第三轮，2026-08-26）

第二轮 review 的 P1（`ErrFileExtensionListTooLarge` 渲染 `<no value>`）与 6 项
建议已修复。但**同一个 commit 里我还加了一样没人要的东西，并且它是破坏性的**：

- **`?path=` 与 filename 扩展名一致性校验 —— 已完整回退。**
  第一轮 review 把这个分歧标为 *"reasonable as a follow-up rather than a
  blocker"*，我却在修 P1 时一并加了进来，且没有做兼容性分析。两位 reviewer
  独立复现了后果：它打断了**服务端自己签发**的上传 URL ——
  `getFilePath` 的 `TypeWorkplaceBanner` / `TypeWorkplaceAppIcon` 分支发出的
  path 不带扩展名，`TypeMomentCover` 硬编码 `.png`（非 PNG 封面必挂），
  而 `api.go` 里那段「修复客户端上传路径缺少扩展名的问题」的兼容代码
  ——针对观察到的真实流量写的——被这道门变成了不可达的死代码。
  我的两个测试只覆盖了「不一致」与「完全一致」，没覆盖「path 无扩展名」，
  所以全绿。
  回退而不是按建议收窄谓词：收窄之后仍是行为收紧，而客户端可能构造出
  服务端之外的 `?path=` 形态（reviewer 也明确说未 grep 过客户端）。
  这一项若要做，应当是独立任务，先盘清真实流量形态。
  新增 `TestUploadFile_AcceptsServerGeneratedPathShapes` 把 getFilePath 签发的
  五种形态钉死 —— 未来任何收紧 `?path=` 的改动必须先让这张表全绿。

- `FormatSizeLimit` 的一位小数只在能精确表示时才用 MB，否则退回 KB：
  1100KB 渲染成 "1.1 MB" 会被读成 1153434 字节，与本函数要修的
  「1536KB 被整除成 1MB」是同一类错误。
- 原始长度超限改用独立的 `ErrFileExtensionListTooLong`，不再复用条数/字符数的
  消息去描述一个运维并未触碰的上限。
- `clampIntUpper` 的 Warn 文案去掉 "sticker" 字样（它现在也服务 `file.max_size_kb`）。

**未采纳的建议**（在 PR 中说明理由）：`policyInputs` 跨三次 getter 读取、
上传 handler 未 pin 扩展名策略、跨键守卫的 check-then-act —— 三者都是既有形态
（sticker 同样如此）、窗口极小且自愈，改动涉及结构调整；在一个已经因为「顺手多做」
出过破坏性变更的 PR 里，不宜再扩大范围。

## Acceptance

- `go test ./modules/file/... -race -count=1` 通过；策略快照读写并发**无 data race**。
- **等价性**：DB 无对应行时，对 baseline 全集 + 一组代表性非法/边界扩展名，
  `IsAllowedExtension` / `IsBlockedExtension` 判定与改动前**逐条相同**
  （以 `TestIsAllowedExtension_AllEntries` / `TestIsBlockedExtension_AllEntries` 为基线）。
- `TestLoadExtensionsFromEnv` 的 **11 条子用例语义逐条保留**（可改写断言形式，
  结论不得放宽）。
- **黑名单不可撤销**：`extra_allowed` 写入任一 baseline 黑名单扩展名（`.exe`/`.php`），
  读侧仍判定 blocked 且不出现在有效白名单。
- **可放开任意扩展名**：`extra_allowed` 写入 `dwg,psd,step` 后立即生效，无需发版重启。
- **内置黑名单不可放开**：`extra_allowed` 含 `.exe` / `.php` 时写侧返回 400 +
  `file_extension_not_allowlistable`，详情指出具体扩展名；直改库的旁路在读侧被过滤，
  不会污染 `effective_value`。
- **不 dark-close**：`extra_allowed` 为空 / 全非法时有效白名单**回退 baseline 全集**。
- **封堵生效**：`extra_blocked` 写入后（本实例 reload 成功时）立即拒绝，
  6 个扩展名调用点行为一致。
- **size 单一真源**：7 个检查点全部读同一快照值；两处本地 `const maxSize` 消除；
  `MaxBytesReader` 上限跟随动态值（含 1MB 余量语义不变）。
- **size clamp**（2026-08-26 随 review 修订）：`file.max_size_kb` **≤0** 视为未配置、
  回退 code default；**超过硬上限钳到硬上限**，不是回退默认值 —— 后者会让运维填
  600000（想要 ~586MB）反而拿到 100MB，比编辑前还小、也比键上写明的 512MB 还小。
  与 sticker 那组键共用同一个钳位器（`clampIntUpper`），越界 Warn 按 (key, value)
  去重。写侧 `Positive: true` 拒绝非正整数。
- **appconfig**：`file_upload_limits` 在 **version 短路分支与全量分支都出现**，
  `allowed_extensions` 为有效值（含 DB/env extra、已扣除 blocked），
  `max_size_kb` 与服务端校验值一致；**不含 blocked 列表**。
- **写侧校验**：非法 token（含 `/` `\` `..`、纯点号）被拒绝或规范化，行为与 env 层
  清洗一致；未注册 `(category,key)` 仍 400。
- **审计**：一次写入产生一条含 操作者 uid / `category.key` / 旧值→新值 / request id
  的记录（落点依 O3）。
- 既有断言常量的 `TestMaxFileSize` / `TestMaxFileSize_Value`（`const_test.go:284,396`）
  改写为「默认值 = 100MB」而非「常量 = 100MB」。
- `go test ./modules/common -run '^TestSystemSettings_StickerUploadAllowedFormats_' -count=1`
  保持 PASS（回归基线，已在 `origin/main` 验证）。
- `make i18n-extract-check` + `make i18n-lint` 通过；新 errcode 有 zh-CN 翻译。
- `golangci-lint run ./modules/file/... ./modules/common/... ./modules/bot_api/... ./modules/robot/...` 无新增告警。
