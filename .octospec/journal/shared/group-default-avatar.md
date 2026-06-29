---
type: Journal
title: "Journal: group-default-avatar (increment 1 — create API + data model)"
description: Server-rendered default group avatar groundwork — create-group API gains custom avatar text/color params, persisted via new columns; avatarrender gains group-name helpers.
tags: ["group", "avatar", "wire-contract", "default-avatar"]
timestamp: 2026-06-25T03:00:28Z
# --- octospec extension fields ---
task: group-default-avatar
source: self
---
# Journal: group-default-avatar (increment 1 — create API + data model)

PRD「群聊与个人默认头像」Module 2 的后端实现，第一增量。把个人默认头像
(PR #346 server-rendered) 的思路延伸到群头像：默认头像将由服务端用「色块圆 +
群名前 4 字 / 群组图标」渲染，取代现有「成员头像九宫格合成」。本增量只落**创建
接口 + 数据模型 + 渲染助手**；avatarGet 渲染分支、改群 API、拆合成事件留待后续。

## What was done
- **DB 迁移** `modules/group/sql/20260625000001_group_avatar_custom.sql`（可重入，
  INFORMATION_SCHEMA 守卫 + 存储过程，含 Up/Down）：
  - `avatar_text VARCHAR(16) NOT NULL DEFAULT ''` —— 自定义头像文字，''=未自定义。
  - `avatar_color TINYINT DEFAULT NULL` —— 自定义色板下标，NULL=未自定义。
- **模型** `modules/group/db.go` `Model`：加 `AvatarText string` + `AvatarColor *int`。
  `*int` 而非 `int`：0 是合法色板下标，无法兼作哨兵；且 `util.AttrToUnderscore`+
  dbr `Record()` 会写入结构体所有字段，`*int=nil → NULL` 让既有建群路径（service.go
  另有一处 `s.db.Insert`）无需逐处显式赋值即可落到“未自定义”。与既有 `GroupMd *string`
  同一处理范式。`QueryWithGroupNo` 用 `Select("*")`，新列自动读回。
- **渲染助手** `pkg/avatarrender/`（纯增量，未改既有 IndividualText/palette 值/顺序）：
  - `text.go`：`GroupText(name)` 取可见字符**前 4** rune（区别于个人的后两字）；
    `VisibleRuneCount(s)` 供校验；抽 `visibleRunes` 复用。
  - `palette.go`：`PaletteSize()` + `ColorByIndex(i)`（自定义色 + 校验越界）。
- **创建接口** `modules/group/api.go`：`groupReq` 加 `avatar_text`/`avatar_color`
  （可选、向后兼容）；`checkAvatar()` 校验（文字 ≤4 可见 rune、颜色 nil 或 [0,palette)，
  超限不静默截断、经 `respondGroupRequestInvalid(c, field)` 返回）；`groupCreate`
  落库。`CreateGroupServiceReq` + `CreateGroup` insert 透传字段。

## Rules honored
- **error-handling (load-bearing)**：新参数校验走 `httperr.ResponseErrorL` +
  复用 catch-all `ErrGroupRequestInvalid`（`SafeDetailKeys:["field"]`），**未新增
  errcode → 零 i18n 改动**。`make i18n-lint` / `i18n-extract-check` 通过；
  `TestGroupNoLegacyResponseError` 源码守卫通过。avatarGet 仍是二进制端点，未动。
- **space-isolation (load-bearing)**：仅给 groupCreate 加可选字段，未放宽任何
  WHERE、未改 Auth/Space 边界、未改 openGroups 免鉴权 avatar 路由。
- **testing**：新单测用 `testutil.NewTestServer` + `CleanAllTables`。

## Verification
- `go build ./...`、`golangci-lint run ./pkg/avatarrender/... ./modules/group/...`
  （0 issues）、`go vet`、`gofmt` 通过。
- 纯单测通过：`avatarrender` 的 `GroupText`/`VisibleRuneCount`/`ColorByIndex`、
  group 的 `TestGroupReqCheckAvatar`（11 例）。
- **集成测试本地实跑通过**（自建 MySQL 8.0 / Redis 7 / WuKongIM v2.2.4-20260313，
  与 ci.yml 一致）：`TestGroupAvatarCustomDBRoundTrip`（nil→NULL 读回 nil、自定义
  值原样读回）、`modules/group` 全套（19s 无回归）、`avatarrender` 全套、
  `base/event` avatar 测试。
- 迁移由 `testutil.NewTestServer` 自动 apply，测试库实测列：
  `avatar_text varchar(16) NOT NULL`、`avatar_color tinyint NULL DEFAULT NULL`。

## Learnings
- dbr `InsertInto(...).Columns(util.AttrToUnderscore(m)...).Record(m)` 会写入
  结构体**全部**字段，故新增列的 Go 零值会覆盖 DB `DEFAULT`。需要“缺省=未设置”
  语义且零值本身合法（如 0 是合法下标）时，用指针字段让 nil→NULL 落到 DB 默认，
  避免逐 insert 点补值。（候选 learning，见 pending/）

## Increment 2 — avatarGet 服务端渲染分支
把 `avatarGet`（`modules/group/api.go`）从「一律重定向对象存储」改为按 `is_upload_avatar`
分流：
- `is_upload_avatar==1` → 维持历史重定向版本化对象（自定义上传不回归）。
- 否则 → `writeGroupDefaultAvatar` 服务端实时出图：文字 = `avatar_text` 优先，否则
  群名；颜色 = `avatar_color` 优先，否则 `ColorForSeed(group_no)`；`GroupText` 取前 4 字、
  `Renderable` 判定可渲染则出「色块圆 + 文字」，否则回退群组图标。带内容相关弱 ETag
  （渲染版本 + group_no + 实际色 + 文字）+ `Cache-Control: public, max-age=300,
  must-revalidate` + 304，改名/换色后短时 revalidate 到新图。

`pkg/avatarrender` 新增（均不改既有 `Render`/`IndividualText`/palette，个人头像不受影响）：
- `group.go`：`GroupAvatarLines` 排版决策 + `RenderGroup` 两行渲染。**对齐设计稿**：
  含 CJK 且 ≥3 字排 2 行（上少下多）——「架构讨论」→2×2、「三个字」→1+2「三」/「个字」；
  纯拉丁或 ≤2 字单行。两行字号/留白经样张目检对齐设计稿（`groupMaxInkWidthRatio` 等）。
- `render.go`：`RenderIcon`（色块圆 + 群组图标）；图标为**占位资产**
  `icons/group-placeholder.png`（白色双人剪影），Figma 正式 SVG 到位后替换该文件即可。
- `etag.go`：共享 `ETag`/`IfNoneMatch`（行为同 user 历史 avatarETag，提升为共享实现）。

增量 2 验证：`avatarrender` 全套（GroupAvatarLines/RenderGroup/RenderIcon/ETag）+
`modules/group` 全套（19s 无回归）+ 4 个 avatarGet 集成测试（渲染字节级匹配
RenderGroup、空名→RenderIcon、自定义覆盖、上传→302、ETag→304）+ golangci 0 issues +
i18n-lint + 源码守卫。样张目检 4 字 2×2 / 3 字 1+2 / 拉丁单行均匹配设计稿。

## Increment 3 — 改群 API 支持自定义头像文字/颜色
群信息更新（`PUT /v1/groups/:group_no` → `groupUpdate`）新增对 `avatar_text` /
`avatar_color` 两个 map key 的处理，与 name/notice 同级（走 Service，不走
`groupUpdateActionMap`——那是群设置用的）：
- `modules/group/api.go groupUpdate`：解析两个 key，校验（文字 ≤4 可见 rune；颜色
  解析为 int，`""`/`"-1"` 清除、`[0,palette)` 设置，否则 400 经
  `respondGroupRequestInvalid`），调 `UpdateGroupAvatarCustom`。
- `service.go`：`UpdateGroupAvatarCustomServiceReq` + `UpdateGroupAvatarCustom`——
  合并现值（未提供字段不动）、bump 群版本、落库、`SendChannelUpdateToGroup` 通知端
  刷新（头像 URL 稳定，靠 avatarGet 的 ETag 取到新图）。接口加入 `IService`。
- `db.go`：`updateAvatarCustom`（专用 SetMap，`UpdateTx` 的显式列不含 avatar 列，
  故需独立方法）；**不触碰 `is_upload_avatar`**——自定义文字/色仍属「默认头像」。
- `const.go`：本地 key 常量 `attrKeyAvatarText`/`attrKeyAvatarColor`（octo-lib 无
  avatar attr key），与创建接口 JSON 字段名一致。

语义：`avatar_text=""` 清除自定义文字（回退群名前 4 字）；`avatar_color=""`/`"-1"`
清除自定义色（回退 `ColorForSeed(group_no)`）；只传一个则另一个保持现值。

增量 3 验证：`TestGroupUpdateAvatarCustom`（设置/部分更新/清除落库）+
`TestGroupUpdateAvatarCustomValidation`（文字超长/颜色越界/非数字 → 400，且不落库）+
`modules/group` 全套（18.8s 无回归）+ golangci 0 issues + i18n-lint + 源码守卫。

## Increment 4 — 拆除九宫格合成事件链路
默认头像改为 avatarGet 实时渲染后，「成员头像九宫格合成」整条异步链路已冗余（合成图
`is_upload=0`，新逻辑一律实时渲染、不再读取它），本增量整体移除：
- **5 个发布点**（2 处 api.go 内联 + 3 处经 `beginAvatarUpdateEvent`：CreateGroup、
  踢人、退群）连同各自 `EventCommit` 删除；公共助手 `beginAvatarUpdateEvent` 删除。
- **base/event**：`handleGroupAvatarUpdateEvent` + 注册 + `shouldComposeGroupAvatar`、
  `queryGroupAvatarState`、`updateGeneratedGroupAvatar`、`groupAvatarState`、
  `GroupAvatarUpdate` 事件常量、不再使用的 `Event.fileService` 字段（及 `file` import）
  全部移除。`handleEvent` 对未注册事件本就优雅处理（标记完成 + debug 日志），故即便
  有遗漏发布也不会 error-loop——但已无任何发布点。
- **group**：`queryGroupAvatarIsUpload` 及两处仅服务于旧守卫的 `memberCount`
  （含一处 `QueryMemberCountTx` FOR UPDATE——其结果只喂旧 `<9` 守卫、无其它容量校验
  消费，移除不丢任何强制）、`contains` 助手、`wkevent`/`event`/`dbr` import 清理。
- 删除过时测试 `avatar_test.go`/`avatar_db_test.go`（只测被删的合成逻辑）。
- **保留**：`DownloadAndMakeCompose`/`MakeCompose`（`file/api.go` 另有用途）、
  `CMDGroupAvatarUpdate`（客户端刷新 CMD，与合成无关）、`QueryMembersFirstNine*`。

**历史群兼容**：已合成群（`is_upload=0, version>0`）请求头像时直接走实时渲染，旧合成
对象成无害孤儿，无需刷库——满足「历史群不批量更新」。

增量 4 验证：`go build ./...` + golangci 0 issues + `go vet` + `modules/group` 全套
（19s，建群/加人/扫码入群/踢人/退群核心流无回归）+ `modules/base/event` 全套 +
i18n-lint + 源码守卫；全仓无残留引用（仅说明性注释）。

## Follow-ups
1. ~~`avatarGet` 服务端渲染分支 + 弱 ETag/304 + 群组图标占位~~ ✅ 增量 2。
2. ~~改群 API：`avatar_text`/`avatar_color`~~ ✅ 增量 3。
3. ~~拆除九宫格合成事件链路~~ ✅ 增量 4。
4. **外部素材待替换**：群组图标正式 SVG（现 `icons/group-placeholder.png` 占位）、
   自定义色板精确色值（现复用个人色板，已像素比对一致）。

## Review round 1 fixes (PR #478)
评审(Jerry-Xin / yujiawei 🔴, Octo-Q ✅)后修复,本轮范围 = P1 + 同源正确性 + 便宜整洁:
1. **P1 非原子部分写入**(`groupUpdate`):avatar 字段解析+校验**前置**到 name/notice/invite
   任何 mutation 之前,非法即 400、不再部分写入群名。+ `TestGroupUpdatePartialWriteRejected`。
2. **Lost-update 竞态**(`UpdateGroupAvatarCustom`/`updateAvatarCustom`):去掉读-改-写合并,
   改为**只 UPDATE 本次提供的列**;并发只改文字/只改色互不覆盖。+ `TestGroupUpdateAvatarColumnScoped`。
3. **存原始串截断**:`avatar_text` 落库前归一化为 `GroupText`(剔除不可见字符、≤4 可见 rune),
   create + update 两路一致,避免零宽字符撑爆 VARCHAR(16)。+ `TestGroupUpdateAvatarTextCleaned`。
4. **GroupResp 暴露** `avatar_text`/`avatar_color`(`from`/`fromModel`)。+ `TestGroupRespExposesAvatarFields`。
5. **不存在群 → 404**(`avatarGet`),与 UserAvatar 一致,消除枚举/无谓渲染面。+ `TestGroupAvatarGetNonexistentReturns404`。
6. 删死代码 `QueryMemberCountTx`、`QueryMembersFirstNineTx`(拆合成后无调用者)。

未纳入本轮(已与维护者对齐,单独处理):公开渲染端点限流(与 UserAvatar 一致,两端点一起)、
权限不对称(manager vs creator,加注释)、渲染失败 ETag 模式、CRC32→SHA 等 nit。

## Review round 2 fixes (PR #478)
两位人类 reviewer 在 round-1 修复后又发现一个新 P1(及若干 nit):
1. **P1 解散群泄露**(`avatarGet`):404 条件由 `groupInfo==nil` 扩为
   `|| Status==GroupStatusDisband`——否则公开端点会把已解散群的群名渲成 PNG(信息泄露 +
   「已解散」vs「从未存在」枚举)。沿用本 PR service 写路径已用的 disband 守卫。
   + `TestGroupAvatarGetDisbandedReturns404`。
3. **updated_at**(nit):`group.updated_at` 是 DEFAULT 但无 ON UPDATE,列级 UPDATE 不自动
   刷新,故 `updateAvatarCustom` 显式写 `updated_at=time.Now()`。
4. **哨兵不对称**(nit):`checkAvatar` 注释说明创建拒 -1、改群收 -1/"" 清除的刻意差异。
延后(reviewer 认可非阻断):version 并发单调性、渲染失败 304、VARCHAR(32) 留头、公开端点
限流(与 UserAvatar 一起)、强 ETag。

## Rebase onto #481 + shared render cache (PR #478 coordination)
#481(`46184555`,issue#480 修复)在 main 落了**进程级共享头像渲染缓存**
`avatarrender.GetOrRender`(LRU + singleflight + 单一渲染并发信号量)。群 avatarGet 同样
按需渲染,必须接入同一个缓存(共用那**一个**信号量才是真正的全机渲染上限;per-endpoint
各限 N 会变 2N、等于没限)。
- rebase `feat/group-default-avatar` 到含 #481 的 main(干净,#481 文件独立)。
- `writeGroupDefaultAvatar` 两处渲染(RenderGroup / RenderIcon)包进 `GetOrRender`,
  404/解散/304 仍在渲染**之前**。
- 缓存 key 用新导出的 `avatarrender.CacheKey`(长度分帧 injective,与 ETag 同因子但
  **非** CRC32——32 位碰撞会跨群串图;文字因子用户可控)。user 的私有 `avatarCacheKey`
  暂留,后续 extraction PR 统一(协调评论已约定)。
- 压测对标 #481 的 starvation repro,延伸到群路径(`pkg/avatarrender/group_starvation_test.go`):
  `TestGroupRenderCost`(RenderGroup ≈36ms / RenderIcon ≈163ms)、
  `TestGroupRenderCacheCollapsesRenders`(确定性:64 并发×6 轮/4 key → 仅 4 次真渲染)、
  `TestGroupRenderCacheEliminatesStarvation`(GOMAXPROCS=2 扇出下受害者放大 ≈1.5x,≤1.8x)。

## Review round 3 fixes (PR #478)
四位 reviewer 在 head `28aa56d8` 全部 APPROVE(Jerry-Xin、OctoBoooot、Octo-Q、yujiawei)。
本轮收尾:1 必修(我引入的 flaky 测试)+ 2 项 reviewer 一致推荐的便宜加固。
1. **flaky 测试 env-gate**(必修):`TestGroupRenderCacheEliminatesStarvation` 断言 wall-clock
   放大倍数(under/base ratio),在共享/受压 runner 上抖动误报(评审实测 1.9x/3.5x)。它
   **演示**饿死消除、但不能**钉死**并发安全。改为默认跳过,仅 `OCTO_TIMING_TESTS=1` 时跑
   (手动复现/演示);渲染收敛的**确定性**证明由 `TestGroupRenderCacheCollapsesRenders`
   承担,恒在 CI 跑。(#481 的同型 `TestCacheEliminatesStarvation` 共享同一脆弱断言,属其
   范畴,本 PR 不动,仅在注释中标注。)
2. **disband TOCTOU 关闭**(Jerry-Xin + yujiawei 一致推荐):`updateAvatarCustom` 的 WHERE
   加 `status<>disband` 并返回 `RowsAffected`;`UpdateGroupAvatarCustom` 据 `affected==0`
   返回 not-found/disbanded。关闭服务层「读到未解散」之后、写入之前群被并发解散的窗口
   (version 每次新值 → 匹配行必变更,RowsAffected 真实反映命中)。+ db 层回归
   `TestGroupUpdateAvatarCustomSkipsDisbanded`(已解散→0 行不写不 bump;正常→1 行落库)。
3. **Swagger 同步**(Octo-Q/yujiawei 🔵):`modules/group/swagger/api.yaml` create/update body
   补 `avatar_text`/`avatar_color`,`group` 响应定义补两字段,GET `/groups/{group_no}/avatar`
   契约更新为 image/png(原 stale `application/json`)+ If-None-Match/304/302/404。
延后(reviewer 一致非阻断,需人决策):**公开端点渲染群名前缀的产品政策**——yujiawei 将其
判为非阻断(group_no 是不可枚举 UUID;与已上线的 user-avatar 渲染昵称同型;本 PR 反而以
disband/不存在→404 收紧了面),但因 security-sensitive 标签,留待人类显式 ack。其余延后项
同前(groupUpdate 跨步原子性、强 ETag、公开端点限流与 UserAvatar 一起做)。

## Post-merge 跟进:透明四角(group + user 头像)
合并后线上观察:渲染的默认头像**圆外是不透明白底**,在不裁圆/深色 surface 上露白方块。
根因:`RenderGroup`/`RenderIcon`/`Render` 三个函数都先 `draw.Draw(canvas, …, color.White, …,
draw.Src)` 铺白底再画圆(当年为「任意背景不透底色」+ 输出无 alpha 的 RGB,与旧 ASCII/bot
头像一致)。`image.NewRGBA` 零值即全透明,故**删掉那行铺白底**即可:圆外保持透明,`png.Encode`
自动输出带 alpha 的 RGBA PNG。三函数各删一行 + 去掉随之 unused 的 `image/draw` import + 改文档
注释。**范围 group + user 一起**(`avatarrender.Render` 生产仅 `modules/user/api.go` 调,波及干净)。
- 方案选型:服务端直接透明 vs 客户端加遮罩 → 取**服务端透明**。描边圆环已把形状焊死成圆
  (裁成非圆会切坏环),客户端遮罩换形状是伪需求;服务端一次改、所有 surface 立即正确、不依赖
  各端正确裁圆、零副作用。
- 测试:`TestRenderOpaque`→`TestRenderTransparentCorners`(四角 alpha=0 + 整图非 Opaque);
  group/icon 各加圆外 alpha=0 断言;`inkBox` 仅采圆内核心区不受影响,仅更注释。group/user 端点
  逐字节比对(handler==Render*)两边同源保持一致。**不在本次范围**:未命名群→双人图标、取字规则
  (`Bug反`→`Bu`/`g反`,待设计)。

### PR #486 评审轮修复(6 位 reviewer)
PR #486 评审,**必修阻断项**:渲染字节变了但 **ETag/cache 版本 tag 没 bump** —— ETag 是对**因子串**
做 CRC32(非像素),同因子→同 ETag→已缓存旧图的客户端 `If-None-Match` 命中→304→`must-revalidate`
下**永远返旧白角图**(进程 LRU 同样陈旧到重启)。这正是 #349 当初加白底时 bump 到 `name-v3` 的同
类动作。修复:
- bump 渲染版本 tag(ETag + CacheKey 两处同步):`modules/group/api.go` `group-name-v2`→`v3`、
  `group-icon-v2`→`v3`;`modules/user/api.go` `name-v3`→`v4`。**`ascii-v1` 不动** —— ASCII 兜底
  `generateDefaultAvatar` 本次未改、字节没变(Octo-Q/QA/yujiawei 三方确认;只在像素真变时才 bump)。
  两处 ETag 站点加注释:视觉改动(像素变因子不变)必须 bump 版本段。
- 修两处 stale helper 注释(`render.go` `drawCircle`/`drawCircleFilledStroked`「调用方先铺白底」)——
  它们是 precondition 措辞,后人照做会把白底加回、静默回归。
- gate `starvation_repro_test.go` 两个 wall-clock timing 测试(`TestRenderFanoutStarvesVictim`、
  `TestCacheEliminatesStarvation`)到 `OCTO_TIMING_TESTS=1`,对齐 #478 的群版。QA 实测后者在 main
  基线一样挂(newFailures=0,既有噪声非回归),gate 后 `go test ./pkg/avatarrender/...` 不再 flake。
- 验证:`go build ./...`;avatarrender(timing 默认 skip)+ group + user 头像端点全绿;golangci-lint
  0 issues;i18n-lint 过。stale-test-DB 迁移 panic 是既有 harness 状态问题(非本改动),drop+recreate
  `test` 库后各模块单独跑均过。

### PR #486 评审第二轮:端点版本 pin 测试(Jerry-Xin 非阻塞建议)
d169ffc7 后 4 位 reviewer 全 Approve,唯一开放项是 Jerry-Xin(两轮均提)的非阻塞 🔵:现有测试覆盖
ETag **函数**,但没有任何测试 pin 端点实际接线的版本段。真实缺口——若后人把 handler 里
`group-name-v3`/`group-icon-v3`/`name-v4` 误改回旧版(或下次改渲染漏 bump),当前无测试会挂,客户端
会 304 到陈旧图(正是上一轮的阻断根因)。补两个端点级 pin:
- `modules/group/avatar_version_pin_test.go` `TestGroupAvatarGetPinsRenderVersion`:命名群→断言响应
  ETag == `avatarrender.ETag("group-name-v3", groupNo, "seed", GroupText(name))`;空名→图标兜底→断言
  == `ETag("group-icon-v3", groupNo, "seed")`。
- `modules/user/avatar_version_pin_test.go` `TestUserAvatarGetPinsRenderVersion`:可渲染昵称→断言响应
  ETag == `avatarETag("name-v4", uid, IndividualText(name))`。`ascii-v1` 故意不 pin(走未改的
  `generateDefaultAvatar`)。
- 设计:测试里的版本字面量与 handler 内联字面量**相互独立**,故单边漂移即不等→失败(非同源恒真);
  `GroupText`/`IndividualText` 复用 handler 同一函数,取字规则将来合法变更时测试与 handler 同步移动,
  **只 pin 版本段**。复用现有 `doAvatarGet`/`getAvatarForTest` 端点桩,无新 handler→不触 NoLegacy 守卫。
- 验证:`go vet`、`go build ./...`、两端点 pin + 既有 avatar 测试全绿、NoLegacyResponseError 守卫
  group/user 均 ok、golangci-lint 0 issues。
