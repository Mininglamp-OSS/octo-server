# PR-B Harvest 映射表（POC → 合并后 P1 之上）

> 步骤 2 产物。核对 `origin/poc/card-message` 的 P2 `cardmsg` 增量 vs 已 squash-merge
> 进 main 的 P1（#543, `100570a1`）。**核心结论：P1 的 `pkg/cardmsg` 已把 P2 接缝
> 预埋好，harvest 是"填接缝 + 补新文件"，不是整包覆盖。** POC 的 `validate.go`/
> `plain.go`/`cardmsg.go` 是 P1 前的独立实现，多处已被 #543 review 反超，禁止整体搬。

## 决策约束（已锁定，2026-07-08）

- 分支从 **merged main** 起（P1 已在），不依赖已删的 `feat/card-message-p1-display`。
- **D9 card_seq 落 `message_extra`**（新增列 + 条件 CAS），非 POC 的独立 Redis 键 `cardseq:{id}`。
- 必修 POC 三缺陷：`event_data.data` 缺失、成员校验 O(n)、card_seq 存储位置。

---

## A. `pkg/cardmsg/validate.go` —— 在 P1 已留接缝里填 P2（改，不搬）

P1 版已具备的接缝（禁止用 POC 版覆盖，只做增量编辑）：

| P1 现状（`validate.go`） | PR-B 动作 | POC 参考 |
|---|---|---|
| `interactiveByProfile`（:102）只认 `ProfileV1 → (false,true)` | 加 `case ProfileV2: return true, true` | POC interactive.go 同名函数 |
| `walker` struct（:110）有 `interactive` 字段，无 `seenIDs` | 加 `seenIDs map[string]struct{}` + `registerID(kind,id)` 方法（D1 帧内唯一） | POC validate.go `registerID`(:101)/`seenIDs`(:95) |
| `element()` `case "Input.Text/Toggle/ChoiceSet"`（:242）仅 `!interactive` 拒绝 | 填：id 必填 + `registerID` 唯一 + ChoiceSet `choices` 数组校验 | POC validate.go :222-240 |
| `action()` `case "Action.Submit"`（:314）interactive 时裸 `return nil` | 填：id 必填 + `registerID` 唯一 + `data` 若存在须为对象 | POC validate.go :307-320 |

**保持不动**：`checkURL`/`checkNodeURLs`/`markdownLinkTargets`（P1 已含 #543 round-4
的 goldmark 反正则绕过加固，POC 无此）。

⚠️ 冲突点：POC `interactive.go` 里**重复定义** `interactiveByProfile`——harvest 时
**丢弃**该副本，改 `validate.go` 里的 P1 版。

## B. `pkg/cardmsg/` —— 新增 helper 文件（harvest + 去重）

| 目标文件 | 内容 | POC 来源 | 适配 |
|---|---|---|---|
| `interactive.go`（新） | `ProfileV2`、`EventTypeCardAction="card_action"`、`SubmitActionIDs`、`NormalizeContentEdit`、`CardSeq`、`CardSeqFromContentEdit` | POC interactive.go | **删掉** POC 里重复的 `IsCardRawPayload`（P1 cardmsg.go 已有）和 `interactiveByProfile`（归 A）。**新增** `SubmitAction(effective,id) (data map[string]interface{}, ok bool)` —— 兼返存在性 + 静态 `data`（POC 的 `SubmitActionIDs` 只返 id 集，**漏 data**，是 event_data.data 缺失根因；D11 要求服务端从生效帧提取 `data`）。 |
| `inputs.go`（新） | `MaxInputTextBytes`/`MaxInputsBytes`、`ValidateInputs`、`collectInputSpecs` | POC inputs.go | 依赖 sentinel `ErrCardInputInvalid`（见 C）。逻辑可直搬。 |

**`plain.go` 不碰**：P1 版严格更优（有 `RecheckPayloadSize` 供 mention 展开后复检；
用 goldmark 剥离 FactSet markdown）。POC 版用 regexp 且漏剥 FactSet = 已修回归。

## C. `pkg/errcode/` + `pkg/cardmsg` sentinel

P1 已注册（保留）：`ErrBotAPICard{Disabled,Invalid,OBOForbidden,EditForbidden}`、
`ErrMessageCard{SendForbidden,EditForbidden}`、`ErrRobotCardEditForbidden`。

PR-B 新增（**按 P1 惯例分散进对应模块文件，不建 `card.go`**）：

| 码 | 文件 | HTTPStatus | Internal | 用途 |
|---|---|---|---|---|
| `ErrMessageCardActionInvalid` | `pkg/errcode/message.go` | 400 | false | D3/D7/D11 归并的单一 invalid（防枚举） |
| `ErrMessageCardActionDenied` | `pkg/errcode/message.go` | 403 | false | 非成员（唯一 403 语义） |
| `ErrBotAPICardSeqConflict` | `pkg/errcode/bot_api.go` | 409 | false | D9 card_seq ≤ 已存 |
| sentinel `ErrCardInputInvalid` | `pkg/cardmsg/cardmsg.go`（var 块） | — | — | inputs.go 内部错误；端点映射到 `ErrMessageCardActionInvalid` |

每个新码补 `active.zh-CN.toml` + `make i18n-extract` 生成 en-US marker。

## D. `modules/robot/api.go` —— 类型化事件（D5）

- `IService`（:61）加 `EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error)`。
- `Service` + `*Robot` 两个实现，委托到新的 `enqueueBotTypedEventGeneric`，挂在与
  `enqueueBotEventGeneric`(:179) **同一 GenSeq/ZAdd/Expire chokepoint**（POC robot/api.go:162-258 已实现，可直搬）。
- 公开 `eventResp`（`bot_api/events.go:27-28`）已带 `EventType/EventData`，无需改 events.go。

## E. `modules/message/` —— 端点 + 幂等 + 接线

| 文件 | 来源 | 适配 |
|---|---|---|
| `api_card_action.go`（新） | POC 同名（229 行） | **必修**：① 成员校验 `switch group/topic` 分支用 `m.groupService.ExistMemberActive(groupNo, loginUID)`（service.go:590 已存在的单点查询）替换 POC 的 `GetMembers` 全量扫描；② `eventData` map **补 `"data"` 键** = `cardmsg.SubmitAction(effective, actionID)` 提取的静态对象（仅当 action 声明了 data）；③ 请求里若带 `data` 字段一律忽略。 |
| `card_action_claims.go`（新） | POC 同名（71 行） | 直搬（`pkg/redis.NewInstrumentedClient` + SetNX/SetXX/Del，与 OIDC 锁同模式）。 |
| `api.go` 路由 + `Message` struct | — | `/v1/message` 组（:291，已挂 Auth+SharedUIDRateLimiter+Space）加 `POST /card/action`（额外挂 64KiB `MaxBytesReader`）；struct 加 `cardClaims *cardActionClaimStore`；`robotService` 接口用到 `ExistRobot`(已有) + 新 `EnqueueBotTypedEvent`。 |

**需实现阶段确认的既有依赖**（POC 引用，须在 merged main 核实/补齐）：
`m.db.queryMessageByID(channelID, channelType, messageID)`、
`m.messageExtraDB.queryWithMessageID(messageID)`、`resolveParentGroupNo` ——
若 merged main 无同名方法，按现有 db 层风格补。

## F. `modules/bot_api/send.go` —— D6 type-17 分支 + D9 CAS

- **撤销点**：merged `:843-844` `if cardmsg.RejectsCardEdit(...) → ErrBotAPICardEditForbidden`。
  PR-B 改为：原消息是 card 或编辑体是 card 时，走 type-17 分支：
  - 跨类型变异检查（原 type-17 ⟺ 编辑体 type-17，否则 400）；
  - `cardmsg.NormalizeContentEdit(req.ContentEdit)`（validate + Finalize + canonical JSON）；
  - D9：`cardmsg.CardSeqFromContentEdit` 有值 → **条件 CAS 写**（见下）；无值 → last-write-wins。
- user 编辑路径（`message/api.go:815`）**保持** `IsCardContentEdit` 拒绝（永久）。
- `ErrBotAPICardEditForbidden` 撤用后成为孤儿码——**保留注册**（不删，避免动 i18n marker；
  robot/user 路径仍各自用自己的 EditForbidden 码）。
- **D9 存储（落 message_extra）**：`message_extra` 加 `card_seq BIGINT`（`modules/message/sql/` 迁移，
  ALTER 既有表，无 `octo_` 前缀规则）。CAS = 条件 `UPDATE ... WHERE message_id=? AND (card_seq IS NULL OR card_seq < ?)`；
  affected=0 且已存 ≥ new → `ErrBotAPICardSeqConflict`(409)。**丢弃 POC 的 `cardSeqStale`+`cardseq:` Redis 键**。
  复用 P1 的 `insertOrUpdateContentEditTx` 风格做同事务写（user 路径已是 Tx 方法）。
- 复用现有 `content_edit_hash` dedup、message_extra upsert、`SendCMD(CMDSyncMessageExtra)` fanout —— octo-im 零改。

## G. i18n / guard / docs

- `make i18n-extract && make i18n-extract-check && make i18n-lint` 绿；zh-CN 补 3 新码。
- 新 handler 文件（`api_card_action.go` + bot_api 若新增文件）加入模块
  `Test<Module>NoLegacyResponseError` guard 名单。
- `docs/card-protocol.md`（#543 已含完整 P2 契约）**只核对不漂移**，PR-B 无需新增文本。

---

## Harvest 一句话总结

| 层 | 策略 |
|---|---|
| `cardmsg/validate.go` | **改** P1 版接缝（interactiveByProfile + walker.seenIDs + Input/Submit 分支），POC 作参考 |
| `cardmsg/plain.go` | **不碰**（P1 更优） |
| `cardmsg/interactive.go`,`inputs.go` | **新增**，从 POC 搬 + 去重(IsCardRawPayload/interactiveByProfile) + 补 `SubmitAction`(带 data) |
| `errcode` | **新增** 3 码分散进模块文件 + 1 sentinel；孤儿 `ErrBotAPICardEditForbidden` 保留 |
| `robot` | 搬 `EnqueueBotTypedEvent`（同 chokepoint） |
| `message` | 搬端点+claims；**必修** ExistMemberActive + event_data.data |
| `bot_api` | 撤 RejectsCardEdit 换 type-17 分支；D9 CAS 落 message_extra（弃 Redis 键） |
