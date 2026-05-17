# `mention.all` Chokepoint Audit — octo-server (HEAD `f1f2f23`)

**Issue**: [#69](https://github.com/Mininglamp-OSS/octo-server/issues/69) (close via PR) · **YUJ-1045**
**Author**: yujiawei · **Date**: 2026-05-17
**Scope**: read-only audit, no business code touched. Output is this single file.
**Decision target**: pick the server-side chokepoint for **方案 X** (death-field strategy — rewrite outbound `mention.all=1` to `mention.humans=1`).

---

## TL;DR

1. **No octo-server module ever writes `mention.all`.** Every server-side `payload["mention"] = …` setter (6 sites, all `_md_notification` / bot-API system messages) writes only `mention.uids`. `mention.all=1` enters the system **exclusively from clients** via HTTP `POST /v1/message/send` (handler `Message.sendMsg`), which dispatches through one chokepoint: `modules/message/api.go:442 Message.sendMessage()`.
2. The same chokepoint already hosts the `enrichPayloadWithSpaceID` rewrite precedent (YUJ-219-A / YUJ-644). This is the cleanest place to add a `rewriteDeadMentionAll` step.
3. `ctx.SendMessage()` (octo-lib `config.Context.SendMessage`) is a strictly broader chokepoint (36 in-tree call sites) but lives in **the published octo-lib module** — wrapping it would force an octo-lib release and break the existing "octo-server owns payload semantics" boundary. Recommended **not** to intercept there.
4. `messageEdit` does NOT fan out a fresh payload (it only writes `content_edit` to `messageExtraDB` and emits a `CMDSyncMessageExtra` CMD). No new mention.all fan-out happens through it. Risk is bounded to **stale ghost** values surviving in `reply.payload` enrichment and merge-forward nested copies — neither triggers a live `isMentioned` evaluation in the adapter.
5. **Recommendation**: ship a single 1-file PR adding `rewriteDeadMentionAll` inside `sendMessage()`, immediately after `enrichPayloadWithSpaceID`. ~20 lines of code + tests. Adapter `ignoreMentionAll` (方案 A in 0.6.3) stays as belt-and-braces; this PR is the suspenders.

---

## 1. All `payload["mention"] = …` write sites

| # | File:line | Function | What it writes | Channel type | Originates `mention.all`? |
|---|-----------|----------|----------------|--------------|----------------------------|
| 1 | `modules/bot_api/threads.go:330` | `BotAPI.sendThreadMdNotification` | `{"uids": botUIDs}` | CommunityTopic | ❌ uids only |
| 2 | `modules/bot_api/groups.go:663` | `BotAPI.sendGroupMdNotification` | `{"uids": botUIDs}` | Group | ❌ uids only |
| 3 | `modules/thread/api.go:743` | `Thread.sendThreadMdNotification` | `{"uids": botUIDs}` | CommunityTopic | ❌ uids only |
| 4 | `modules/group/api.go:3695` | `Group.sendGroupMdNotification` | `{"uids": botUIDs}` | Group | ❌ uids only |
| 5 | `modules/botfather/api_bot.go:694` | `BotFather.sendGroupMdNotification` | `{"uids": botUIDs}` | Group | ❌ uids only |
| 6 | `modules/botfather/api_bot_thread.go:339` | `BotFather.sendThreadMdNotification` | `{"uids": botUIDs}` | CommunityTopic | ❌ uids only |

**Finding**: zero server modules construct `mention.all=1`. Every `mention.all=1` byte on the wire originated in a client payload that was forwarded verbatim through `POST /v1/message/send` → `Message.sendMsg` → `Message.sendMessage`.

`grep -rn 'mention.all' modules/` confirms two non-test consumers: log line at `robot/event.go:154` (debug-only field logging) + reminder check at `api_reminders.go:283-294` (`getReminders` consumes `mention.all=1` post-fanout to create per-user reminders). Neither produces the field.

---

## 2. All `ctx.SendMessage()` call sites (34)

Grouped by module. ★ = entry that can carry **client-supplied** payload (and therefore potential `mention.all=1`); rest are server-internal system messages with hard-coded payload.

| Module | File:line | Caller | Payload origin | Goes through `Message.sendMessage()` chokepoint? |
|--------|-----------|--------|----------------|--------------------------------------------------|
| message | `modules/message/api.go:450` | `Message.sendMessage` ★ | **client (via `sendMsg`)** | ✅ **IS the chokepoint** |
| message | `modules/message/api_manager.go:843` | `Manager.sendMsg` (super-admin) | hardcoded `{content, type:1, from_uid}`, no mention pass-through | ❌ separate `/v1/manager` route, **no SpaceMiddleware**; server hand-builds the payload from `managerSendMsgReq` (which only carries `Content`/`Sender`/etc., never `mention.*`) — cannot emit `mention.all` |
| message | `modules/message/api_pinned.go:282` | pinned-message Tip | hardcoded | ❌ no mention |
| message | `modules/message/event.go:307` | QR-scan join Tip | hardcoded | ❌ no mention |
| bot_api | `modules/bot_api/threads.go:336` | `sendThreadMdNotification` | server-built `{uids}` | ❌ no `.all` |
| bot_api | `modules/bot_api/groups.go:668` | `sendGroupMdNotification` | server-built `{uids}` | ❌ no `.all` |
| thread | `modules/thread/api.go:749` | `sendThreadMdNotification` | server-built `{uids}` | ❌ no `.all` |
| thread | `modules/thread/service.go:311,329` | thread create system msg | hardcoded | ❌ no mention |
| group | `modules/group/api.go:3700` | `sendGroupMdNotification` | server-built `{uids}` | ❌ no `.all` |
| group | `modules/group/event.go:30,360,590` | group disband / member-add / batch-add Tips | hardcoded | ❌ no mention |
| group | `modules/group/bot_cascade.go:92` | bot cascade system msg | hardcoded | ❌ no mention |
| botfather | `modules/botfather/api_bot.go:699` | `sendGroupMdNotification` | server-built `{uids}` | ❌ no `.all` |
| botfather | `modules/botfather/api_bot_thread.go:345` | `sendThreadMdNotification` | server-built `{uids}` | ❌ no `.all` |
| botfather | `modules/botfather/command.go:953` | bot command reply DM | hardcoded `NewPersonalMsgSendReq` | ❌ no mention |
| botfather | `modules/botfather/api_apply.go:423,455,478` | bot-apply DM notifications | hardcoded | ❌ no mention |
| botfather | `modules/botfather/friend_approve.go:243` | friend-approve DM | hardcoded | ❌ no mention |
| botfather | `modules/botfather/api.go:696` | `SendMessageWithResult` admin reply | hardcoded | ❌ no mention |
| botfather | `modules/botfather/welcome.go:116` | welcome DM | hardcoded | ❌ no mention |
| app_bot | `modules/app_bot/app_bot.go:1143` | app-bot system DM | hardcoded | ❌ no mention |
| user | `modules/user/api_friend.go:958,975` | friend req/accept DM | hardcoded | ❌ no mention |
| user | `modules/user/api.go:1420` | user system DM | hardcoded | ❌ no mention |
| user | `modules/user/event_friend.go:338` | friend event DM | hardcoded | ❌ no mention |
| channel | `modules/channel/api.go:366,374` | channel system msg | hardcoded | ❌ no mention |
| robot | `modules/robot/event.go:123,131,334,342` | robot webhook responses | from external robot webhook body (NOT client) | ❌ no mention (robot payload schema separate) |
| notify | `modules/notify/api.go:269` | system notification DM | hardcoded | ❌ no mention |
| space | `modules/space/api.go:1554,1580` | Space invite system DM | hardcoded | ❌ no mention |

**Plus 4 `SendMessageWithResult` and 2 `SendMessageBatch` sites** — none take client-supplied mention. (The two `SendMessageBatch` sites are `Manager.sendMessageToFriends` at `api_manager.go:98` and `Manager.sendMessageToAll` at `api_manager.go:739` — both hardcoded `{content, type:1}`, no mention field.)

### Matrix conclusion

- Only **1 entry** routes client-supplied payload (and therefore could carry `mention.all`) to WuKongIM:
  - `POST /v1/message/send` → `Message.sendMsg` → `Message.sendMessage()` ★ — **the chokepoint already exists**.
- The super-admin `POST /v1/manager/message/send` → `Manager.sendMsg` route also calls `m.ctx.SendMessage()` directly, but the handler hand-builds the payload as `{content, type:1, from_uid}` from `managerSendMsgReq` (no `mention` field at all) — so it cannot emit `mention.all` and needs no rewrite.
- All 33 remaining `ctx.SendMessage` sites build payload server-side with hard-coded `mention.uids` (or no mention at all). They are not in scope for the dead-field rewrite.

---

## 3. Candidate interception points — coverage / risk evaluation

| Candidate | Location | Coverage of client `mention.all` flows | Pros | Cons | Verdict |
|-----------|----------|----------------------------------------|------|------|---------|
| **A. `Message.sendMessage()`** (`modules/message/api.go:442`) | octo-server, in message module | ✅ catches `POST /v1/message/send` (the only client-facing entry that actually carries `mention.all`); ✅ same kind of payload-rewrite precedent already lives one line above (`enrichPayloadWithSpaceID`); ❌ does not run for robot webhook (separate schema, no `mention.all`) | Minimal blast radius. Single-file diff. Easy to test. Matches established hardening pattern. | — (super-admin `Manager.sendMsg` doesn't need a mirror: its handler hand-builds `{content, type:1, from_uid}` with no `mention` field) | ⭐ **Recommended primary** |
| **B. `Context.SendMessage()`** (octo-lib `config/msg.go:130`) | upstream module `github.com/Mininglamp-OSS/octo-lib` | ✅ blanket coverage of all 36 in-tree sites + every downstream octo-lib consumer (adapters, octo-deployment scripts) | True backstop. Catches even payloads built by future code we haven't audited. | ❌ Requires releasing a new octo-lib version (release lag, version-bump cascade). ❌ Crosses the architectural boundary — payload semantics belong to octo-server, not the transport layer. ❌ Over-broad: rewrites system messages that never had a mention field. ❌ Hard to feature-flag per-deployment. | 🚫 **Reject** — wrong layer, wrong release cadence |
| **C. `Message.messageEdit()`** (`modules/message/api.go:610`) | octo-server, message module | ❌ messageEdit only writes `messageExtraDB.content_edit` and emits `CMDSyncMessageExtra` — it does NOT re-fan-out a new payload through `ctx.SendMessage`. No `mention.all` ever gets re-evaluated by the adapter via this path. | n/a | Wrong target entirely — there's no fan-out to intercept. | 🚫 **Reject** — non-issue, see §4 risk #3 below |

### Coverage matrix

| Flow | Caught by (A) sendMessage | Caught by (B) ctx.SendMessage | Caught by (C) messageEdit |
|------|---------------------------|-------------------------------|---------------------------|
| Client `POST /v1/message/send` | ✅ | ✅ | n/a |
| Super-admin `POST /v1/manager/*` | n/a — handler hardcodes `{content, type:1, from_uid}`, no `mention` field possible | n/a | n/a |
| Server-built `*MdNotification` (uids only) | n/a | ✅ (no-op rewrite) | n/a |
| Reply enrichment with stale `mention.all` snapshot | ❌ | ❌ | ❌ — read-path, not write-path; see §4 #1 |
| Mergeforward nested `messages[].mention.all` | ❌ | ❌ | ❌ — adapter `isMentioned` only inspects outer payload; see §4 #2 |
| messageEdit | n/a — no fan-out | n/a | n/a |

> Coverage of the **only** real fan-out vector is identical between (A) and (B); (A) wins on blast radius and release cadence.

---

## 4. Risk list (explicitly enumerated by issue)

### 4.1 Reply enrichment with stale `mention.all`
- Location: `modules/message/api.go:2483-2503` (`newSyncChannelMessageResp` → reply hydration block).
- Read-path: `payloadMap["reply"]["payload"]` is overwritten with the latest `content_edit` snapshot from `messageExtraDB`. The snapshot is a JSON blob; if a historical message carried `mention.all=1`, the snapshot still has it.
- **Live fan-out impact**: ❌ none. This runs only during `POST /v1/message/channel/sync` (history pull). The adapter does its `isMentioned` evaluation on the **realtime WuKongIM fanout**, not on REST sync responses. A stale `reply.payload.mention.all=1` in a sync response is a UI artifact (quoted preview), not a notification.
- **Action**: no chokepoint rewrite required. Optionally, in a follow-up, scrub `reply.payload.mention.all` during this read enrichment for cosmetic cleanliness, but it does NOT trigger adapter "@all" behavior.

### 4.2 Mergeforward (content type 11 / `MultipleForward`) nested `mention.all`
- Construction: 100% client-side. octo-server never builds a mergeforward bundle; it forwards them through `POST /v1/message/send` like any other payload.
- Server enrichment: `applyExternalMarkers` (`modules/message/api.go:3017`) walks `payload["users"]` to inject `is_external` etc. It does NOT walk `payload["messages"][].mention`.
- **Live fan-out impact**: ❌ adapter `isMentioned` only inspects the OUTER `payload.mention.all`. Nested `messages[].mention.all=1` inside a forwarded bundle is data-at-rest in the carrier payload; it does NOT re-trigger an @all notification when the carrier is delivered.
- **Action**: NO rewrite at the chokepoint for nested fields. If we later want defense-in-depth (forensic / search hygiene), add a separate pass that walks nested `messages[].mention.all` — but treat as P3, not P1.

### 4.3 messageEdit
- Path: `messageEdit` (`modules/message/api.go:610`) writes `messageExtraDB.content_edit` then `m.ctx.SendCMD(CMDSyncMessageExtra)`. **No `m.ctx.SendMessage` call.**
- The CMD is a sync hint; clients pull `messageExtra` and reconcile. `content_edit` can technically carry a new `mention.all=1` payload if a malicious client crafts one, but:
  - The adapter does not evaluate `isMentioned` on `messageExtra` deltas — only on the original fan-out.
  - Per-user reminders for the edit do NOT regenerate (the original `mention.all` reminder was created by `listenerMessages` at send time).
- **Live fan-out impact**: ❌ none.
- **Action**: skip. If product wants edited-payload mention scrubbing for forensic correctness, add later.

### 4.4 New / not-listed risks discovered during audit
- **Super-admin `/v1/manager/message/send`**: bypasses `Message.sendMessage()` but **needs no rewrite**. The handler (`Manager.sendMsg` at `api_manager.go:760-843`) hand-builds the outbound payload as `{content: req.Content, type: 1, from_uid: req.Sender}` — the `managerSendMsgReq` schema only exposes `Content`/`Sender`/`ReceivedChannelID`/`ReceivedChannelType`, so no `mention.*` field can ever flow through this path. No `rewriteDeadMentionAll` mirror is required.
- **Robot webhook — friend-tip / command-reply sends from `robotMessageListen`** (`modules/robot/event.go:131,342`): payload comes from external robot HTTP webhook body, not user client. Schema does not include `mention.all` in our current robot contract. Out of scope; flag as a follow-up if 3rd-party robots are later allowed to set arbitrary mention.
- **`listenerMessages` → `getReminders`**: this is the **consumer** of `mention.all` (`modules/message/api_reminders.go:283`). After 方案 X rewrites outbound payload to `mention.humans=1`, this code path stops generating "[有人@我]" group-wide reminders for newly-sent messages. **This is a behavior change requiring a parallel update**: either teach `getReminders` to also recognize `mention.humans=1` as @all-equivalent, or accept the product change (no more "@all generates a reminder for everyone"). Flag explicitly to product before merging方案 X.

---

## 5. Recommended PR split

Single repo: `Mininglamp-OSS/octo-server`. No octo-lib bump, no adapter changes (adapter `ignoreMentionAll` already shipped in 0.6.3 stays).

### PR #1 — server-side `mention.all` → `mention.humans` rewrite (this audit's target)
**Touched files** (2 production + 1 test):
1. `modules/message/api.go` — add `rewriteDeadMentionAll(payload)` helper and call it inside `sendMessage()` right after `enrichPayloadWithSpaceID(...)` (line ~449). ~20 LOC including doc comment.
2. `modules/message/api_reminders.go` — extend `getMention` to also treat `mention.humans=1` as the @all-equivalent so reminder-generation behavior is preserved post-rewrite. ~5 LOC.
3. `modules/message/api_test.go` (or a new `rewrite_dead_mention_all_test.go`) — table-driven tests for: `mention.all=1` → rewritten; `mention.all=0` → untouched; absent → untouched; mixed `all` + `uids` → uids preserved; non-map mention → untouched; **v2 mention with `entities` — rewrite must not corrupt or drop `mention.entities`** (cross-reference `modules/message/validation_test.go:885-928`).

**Explicitly NOT touched**:
- `modules/message/api_manager.go` — `Manager.sendMsg` hand-builds `{content, type:1, from_uid}` from `managerSendMsgReq`; no `mention.*` field flows through this path, so the rewrite is a no-op and is omitted.

**Do NOT touch**:
- octo-lib (`Context.SendMessage`) — wrong layer.
- adapter (`ignoreMentionAll` is the belt; this is the suspenders).
- `messageEdit`, reply enrichment, mergeforward walker — see §4.

### PR #2 (optional follow-up, separate ticket) — read-side scrub
- `newSyncChannelMessageResp` reply enrichment: scrub `reply.payload.mention.all` (cosmetic only; see §4.1).
- `applyExternalMarkers`: optionally walk nested `messages[].mention.all` for mergeforward bundles (§4.2).
- Defer until UI confirms quoted-preview "@all" pill is actually leaking through.

### PR #3 (optional follow-up) — adapter cleanup
- Once PR #1 has been live for N weeks and metrics show zero `mention.all=1` on the wire, retire the adapter `ignoreMentionAll` shim from 0.6.3. Separate repo, separate cadence.

---

## 6. Downstream consumer prerequisite (BLOCKING)

方案 X 的 server-side rewrite (`mention.all=1` → `mention.humans=1`) 隐含假设所有下游 consumer 已经识别 `mention.humans=1`。审计本 PR 时只确认了 octo-server 内部 reminder consumer（§5 step 2 已含），但**未验证以下下游**：

| Consumer | 当前状态 | 影响 |
|---|---|---|
| octo-server reminder (`api_reminders.go:283`) | ❌ 不认 | §5 step 2 解决 ✅ |
| dmwork-adapters | ✅ `ignoreMentionAll` (0.6.3) 只 drop all 不 interpret humans → bot 不被触发，符合预期 | bot 静默是想要的，无问题 |
| dmwork-web / android / ios 渲染 | ❌ **未审计** | 收到 `mention.humans=1` 可能渲染为普通文本，丢失 @所有人 蓝色 pill |
| 离线推送 (FCM/APNs/HMS push) | ❌ **未审计** | 可能丢失"@你"重要推送音 |
| 第三方 bot/集成 | ❌ **未审计** | 接口契约变更 |

**结论**：在三端客户端读侧支持 `mention.humans=1` 之前，server-side rewrite 会导致新发送的 @所有人 消息出现 UX 退化（pill 消失）。

### 推荐执行顺序

**Phase 1（前置）**：三端客户端读侧支持 `mention.humans=1` 渲染为「@所有人」蓝色 pill（**不改发送侧**）。发版覆盖率达 80%+ 后才进 Phase 2。

**Phase 2（本 PR follow-up）**：octo-server 上 `rewriteDeadMentionAll` + reminder 教学。

**Phase 3（可选）**：三端发送侧切换 humans（有 server rewrite 后非必须）。

---

## Appendix A — Reproduction commands

```bash
# All payload["mention"] writers
grep -rn 'payload\["mention"\]' --include='*.go' modules/ | grep -v _test.go

# All ctx.SendMessage call sites
grep -rn 'SendMessage(' --include='*.go' modules/ | grep -v _test.go

# All mention.all consumers
grep -rn 'mention.all\|mention\["all"\]' --include='*.go' modules/

# Chokepoint and precedent
sed -n '438,464p' modules/message/api.go
```

## Appendix B — Glossary

- **Chokepoint**: a single function through which all flows of interest must pass.
- **Dead field**: a field name (`mention.all`) we semantically retire by ensuring it never appears on the wire post-rewrite. Old clients still understand `mention.all`; new server emits `mention.humans` instead; adapter `ignoreMentionAll` (already shipped in 0.6.3) silently drops any residual `mention.all=1` it sees.
- **方案 X**: server-side rewrite of outbound `mention.all=1` → `mention.humans=1`.
- **方案 A**: adapter `ignoreMentionAll` flag — already shipped in dmwork-adapters 0.6.3. Defense in depth.
