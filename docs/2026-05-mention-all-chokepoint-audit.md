# `mention.all` Chokepoint Audit — octo-server (HEAD `cf7e2c5`)

**Issue**: [#69](https://github.com/Mininglamp-OSS/octo-server/issues/69) (close via PR) · **YUJ-1045**
**Author**: yujiawei · **Date**: 2026-05-17 (R3 — post ReviewBot R2 corrections)
**Scope**: read-only audit, no business code touched. Output is this single file.
**Decision target**: pick the server-side chokepoint for **方案 X** (death-field strategy — rewrite outbound `mention.all=1` to `mention.humans=1`).

---

## TL;DR

1. **No octo-server module ever writes `mention.all`.** Every server-side `payload["mention"] = …` setter (6 sites, all `_md_notification` / bot-API system messages) writes only `mention.uids`. `mention.all=1` enters the system **exclusively from clients** via **three** HTTP entry points, each of which forwards a client-supplied `payload` verbatim to WuKongIM:
   - `POST /v1/message/send` → `Message.sendMsg` → `Message.sendMessage()` (`modules/message/api.go:442`)
   - `POST /v1/bot/sendMessage` → `BotAPI.sendMessage` (`modules/bot_api/send.go:29`) → `dispatchMsgSendReq` → `ctx.SendMessageWithResult` (`modules/bot_api/bot_api.go:54`)
   - `POST /v1/robots/:robot_id/:app_key/sendMessage` → `Robot.sendMessage` (`modules/robot/api.go:290`) → `ctx.SendMessageWithResult` (`modules/robot/api.go:353`)
2. The first chokepoint already hosts the `enrichPayloadWithSpaceID` rewrite precedent (YUJ-219-A / YUJ-644). Because there are three structurally similar handlers, the right shape is a **shared helper** (`RewriteDeadMentionAll`) invoked from all three — single source of truth, three thin call sites.
3. `ctx.SendMessage()` (octo-lib `config.Context.SendMessage`) is a strictly broader chokepoint (36 in-tree call sites) but lives in **the published octo-lib module** — wrapping it would force an octo-lib release and break the existing "octo-server owns payload semantics" boundary. The cost gap vs. the helper approach narrowed once we discovered 3 entries (instead of 1), but the cross-repo release-cadence and over-broad rewrite problems remain. Recommended **not** to intercept there.
4. `messageEdit` does NOT fan out a fresh payload (it only writes `content_edit` to `messageExtraDB` and emits a `CMDSyncMessageExtra` CMD). No new mention.all fan-out happens through it. Risk is bounded to **stale ghost** values surviving in `reply.payload` enrichment and merge-forward nested copies — neither triggers a live `isMentioned` evaluation in the adapter.
5. **Recommendation**: ship a single-PR change that adds one helper file (`modules/message/mention_rewrite.go` exporting `RewriteDeadMentionAll`), three one-line call-site insertions (message / bot_api / robot), one reminder-side teach (`api_reminders.go`), and three handler-path tests. Adapter `ignoreMentionAll` (方案 A in 0.6.3) stays as belt-and-braces; this PR is the suspenders.

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

**Finding**: zero server modules construct `mention.all=1`. Every `mention.all=1` byte on the wire originated in a client payload that was forwarded verbatim through one of the three client-controlled entries listed in §2 (POST `/v1/message/send`, POST `/v1/bot/sendMessage`, POST `/v1/robots/:robot_id/:app_key/sendMessage`).

**Consumer audit (read-side)**. A naive grep `grep -rn 'mention.all' modules/` on HEAD `cf7e2c5` only returns a single test assertion (`modules/robot/event_test.go:394`), because the production consumer reads `mention.all` via Go map indexing (`mentionMap["all"]`), which the literal pattern misses. Use a regex that matches both literal field access and map indexing:

```bash
grep -rEn 'mention[."]?all|mention(Map|Value)[^=]*\[?"?all"?\]?' --include='*.go' modules/ | grep -v _test.go
```

On HEAD `cf7e2c5` this returns the actual production consumer:

- `modules/message/api_reminders.go:288-289` — `Message.getMention` reads `mentionMap["all"]` (as `json.Number`) inside `getReminders`; if `1`, fans out a per-user `[有人@我]` reminder. **This is the only live "mention.all=1 means @everyone" consumer in octo-server.**

Two other code paths reference the `mention` sub-object but do not specifically branch on `.all`:

- `modules/robot/event.go:154` — debug log of the entire `mention` sub-object (`payloadValue.Get("mention").String()`), gated on `payloadValue.Get("mention").Exists()`. It logs the whole JSON blob of `mention`, **not** the `.all` field specifically; the surrounding code only iterates `mention.uids` to identify mentioned robots. Informational only, no behavior depends on `.all`.
- `modules/robot/event_test.go:394` — test assertion (not a production consumer).

Conclusion: `api_reminders.go:283-294` (the `Message.getMention` helper) is the **only** code path whose behavior changes when `mention.all=1` becomes `mention.humans=1`. The reminder-side teach in §5 is therefore both necessary and sufficient for in-tree consumer parity.

---

## 2. All `ctx.SendMessage()` / `ctx.SendMessageWithResult()` call sites

Grouped by module. ★ = **client-controlled** entry that can carry client-supplied payload (and therefore potential `mention.all=1`); rest are server-internal system messages with hard-coded payload.

| Module | File:line | Caller | Payload origin | Goes through `Message.sendMessage()` chokepoint? |
|--------|-----------|--------|----------------|--------------------------------------------------|
| message | `modules/message/api.go:450` | `Message.sendMessage` ★ | **client (via `sendMsg`)** | ✅ **IS the chokepoint** for `POST /v1/message/send` |
| bot_api | `modules/bot_api/bot_api.go:54` | `BotAPI.dispatchMsgSendReq` ★ (called from `BotAPI.sendMessage` at `modules/bot_api/send.go:29`) | **client (via `POST /v1/bot/sendMessage`)** — `BotSendMessageReq.Payload` forwarded verbatim | ❌ **separate chokepoint** — does NOT pass through `Message.sendMessage()` |
| robot | `modules/robot/api.go:353` | `Robot.sendMessage` ★ (handler at `modules/robot/api.go:290`, route `POST /v1/robots/:robot_id/:app_key/sendMessage`) | **client (via `MessageReq.Payload`)** — only validates `payload.type`, forwards full payload verbatim | ❌ **separate chokepoint** — does NOT pass through `Message.sendMessage()` |
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
| botfather | `modules/botfather/api.go:696` | `BotFather.SendMessageWithResult` admin reply | **dead code** — file inline comment at lines 692-695: "This handler is currently NOT routed (the active `/v1/bot/sendMessage` route is wired through `modules/bot_api/bot_api.go` to `ba.sendMessage`). It is kept here as legacy code…" | ❌ dead (no route) |
| botfather | `modules/botfather/welcome.go:116` | welcome DM (`BotFather.sendWelcomeMessage`) | hardcoded `{type: Text, content: welcomeContent}` | ❌ no mention |
| app_bot | `modules/app_bot/app_bot.go:1143` | app-bot system DM | hardcoded | ❌ no mention |
| user | `modules/user/api_friend.go:958,975` | friend req/accept DM | hardcoded | ❌ no mention |
| user | `modules/user/api.go:1420` | user system DM | hardcoded | ❌ no mention |
| user | `modules/user/event_friend.go:338` | friend event DM | hardcoded | ❌ no mention |
| channel | `modules/channel/api.go:366,374` | channel system msg | hardcoded | ❌ no mention |
| robot | `modules/robot/event.go:123,131,334,342` | robot webhook responses | from external robot webhook body (NOT user client) | ❌ no mention (robot payload schema separate; see §4.4) |
| notify | `modules/notify/api.go:269` | system notification DM | hardcoded | ❌ no mention |
| space | `modules/space/api.go:1554,1580` | Space invite system DM | hardcoded | ❌ no mention |

**Re-classification of `SendMessageWithResult` sites** (R2 correction — the prior R1 footnote that lumped all 4 as "none take client-supplied mention" was wrong):

| Site | Classification | Reason |
|------|----------------|--------|
| `modules/bot_api/bot_api.go:54` | ★ **client-controlled** | called by `BotAPI.sendMessage` which forwards `BotSendMessageReq.Payload` verbatim |
| `modules/robot/api.go:353` | ★ **client-controlled** | called by `Robot.sendMessage` which forwards `MessageReq.Payload` verbatim |
| `modules/botfather/api.go:696` | dead code | handler not registered on any route (see inline comment cited in table) |
| `modules/botfather/welcome.go:116` | server-built / hardcoded | `BotFather.sendWelcomeMessage` builds `{type: Text, content: welcomeContent}` |

`SendMessageBatch` sites (`modules/message/api_manager.go:98` and `:739`) remain hardcoded `{content, type:1}` with no `mention` field — out of scope.

### Matrix conclusion

- **Three entries** route client-supplied payload (and therefore could carry `mention.all=1`) to WuKongIM:
  1. `POST /v1/message/send` → `Message.sendMsg` → `Message.sendMessage()` → `ctx.SendMessage` (`modules/message/api.go:450`)
  2. `POST /v1/bot/sendMessage` → `BotAPI.sendMessage` (`modules/bot_api/send.go:29`) → `BotAPI.dispatchMsgSendReq` → `ctx.SendMessageWithResult` (`modules/bot_api/bot_api.go:54`)
  3. `POST /v1/robots/:robot_id/:app_key/sendMessage` → `Robot.sendMessage` (`modules/robot/api.go:290`) → `ctx.SendMessageWithResult` (`modules/robot/api.go:353`)
- The super-admin `POST /v1/manager/message/send` → `Manager.sendMsg` route also calls `m.ctx.SendMessage()` directly, but the handler hand-builds the payload as `{content, type:1, from_uid}` from `managerSendMsgReq` (no `mention` field at all) — so it cannot emit `mention.all` and needs no rewrite.
- All remaining `ctx.SendMessage` / `ctx.SendMessageWithResult` sites build payload server-side with hard-coded `mention.uids` (or no mention at all). They are not in scope for the dead-field rewrite.

---

## 3. Candidate interception points — coverage / risk evaluation

R1 originally listed candidate **A** as "rewrite inside `Message.sendMessage()` alone". R2 review correctly identified that a single-point intercept inside `Message.sendMessage()` only covers entry #1 of three (it does **not** cover `BotAPI.sendMessage` or `Robot.sendMessage`, which use their own `ctx.SendMessageWithResult` call sites and never pass through `Message.sendMessage`). Candidate A is therefore retired and superseded by **A+** below. The original B (octo-lib) verdict still holds but is re-justified with the updated 1→3 entry cost arithmetic.

| Candidate | Location | Coverage of client `mention.all` flows | Pros | Cons | Verdict |
|-----------|----------|----------------------------------------|------|------|---------|
| ~~**A. `Message.sendMessage()`** alone~~ | octo-server, message module only | ❌ catches only entry #1 of three (`/v1/message/send`). Misses `/v1/bot/sendMessage` and `/v1/robots/.../sendMessage`. | Minimal diff. | **Insufficient coverage** — two client-controlled entries leak `mention.all=1` to WuKongIM unrewritten. | 🚫 **Reject** — single-point intercept proven insufficient (R2 finding) |
| **A+. Shared helper `RewriteDeadMentionAll`** invoked from all 3 client-controlled entries | new file `modules/message/mention_rewrite.go` (exported helper) + 3 call sites: `modules/message/api.go:~450`, `modules/bot_api/send.go:~67` (just before `dispatchMsgSendReq`), `modules/robot/api.go:~352` (just before `ctx.SendMessageWithResult`) | ✅ catches all 3 client-controlled entries. ✅ single source of truth (helper) for the rewrite semantics. ✅ aligns with the established `enrichPayloadWithSpaceID` precedent — that one is *also* invoked from multiple handlers (`Message.sendMessage`, `BotAPI.enrichBotPayloadWithSpaceID`, `Robot.enrichBotPayloadWithSpaceID`) using a shared module convention, not a single chokepoint. | Single source of truth for rewrite semantics; 3 thin call sites each ~1 line. Mirrors `enrichBotPayloadWithSpaceID` pattern. Easy to unit-test the helper purely + test each handler path with a small handler-level integration test. Per-deployment feature-flag is straightforward. | 3 call-site insertions instead of 1 — risk of future new client-facing entries forgetting to call it. Mitigate with: (a) handler-level test for each path, (b) follow-up `errcheck`/lint to flag any new `SendMessage(WithResult)?` site whose payload originates from a `*Req.Payload` field. | ⭐ **Recommended primary** |
| **B. `Context.SendMessage()`** (octo-lib `config/msg.go:130`) | upstream module `github.com/Mininglamp-OSS/octo-lib` | ✅ blanket coverage of all 36 in-tree sites + every downstream octo-lib consumer (adapters, octo-deployment scripts) | True backstop. Catches even payloads built by future code we haven't audited. | ❌ Requires releasing a new octo-lib version (release lag, version-bump cascade) — this cost did not change just because client-controlled entries went from 1→3, the cross-repo cadence problem is unchanged. ❌ Crosses the architectural boundary — payload semantics belong to octo-server, not the transport layer. ❌ Over-broad: rewrites system messages that never had a mention field. ❌ Hard to feature-flag per-deployment. | 🚫 **Reject** — wrong layer, wrong release cadence (rationale unchanged from R1) |
| **C. `Message.messageEdit()`** (`modules/message/api.go:610`) | octo-server, message module | ❌ messageEdit only writes `messageExtraDB.content_edit` and emits `CMDSyncMessageExtra` — it does NOT re-fan-out a new payload through `ctx.SendMessage`. No `mention.all` ever gets re-evaluated by the adapter via this path. | n/a | Wrong target entirely — there's no fan-out to intercept. | 🚫 **Reject** — non-issue, see §4 risk #3 below |

### Re-evaluating A+ vs. B with 3 entries (honest cost restatement)

R1 wrote "Coverage of the only real fan-out vector is identical between A and B; A wins on blast radius and release cadence." R2 review forced us to revisit: with **3** client-controlled entries instead of 1, the A vs. B trade-off shifts:

| Cost axis | A+ (3 call sites + helper) | B (octo-lib wrapper) | Δ vs. R1 (when this was 1 entry) |
|-----------|----------------------------|-----------------------|----------------------------------|
| Lines of code touched in octo-server | helper ~25 + 3×1-line call sites + ~5 LOC reminder teach ≈ **~35 LOC** | ~5 LOC (one octo-lib bump + go.mod update) | **A+ cost rose** from ~25 to ~35 LOC |
| Cross-repo release cadence | none (single PR in octo-server) | octo-lib release → octo-server go.mod bump → re-deploy | unchanged |
| Risk of missing a future new entry | medium (need lint/checklist for new `*Req.Payload` → `SendMessage*` paths) | low (transport-layer catch-all) | **A+ risk rose** — was "1 chokepoint, can't be forgotten"; now is "3 call sites, must remember the 4th" |
| Over-broad rewrite of server-built payloads | none | applies to all 36 sites including hardcoded system messages | unchanged |
| Architectural boundary | clean (payload semantics in octo-server) | violates (transport layer mutates semantics) | unchanged |
| Per-deployment feature flag | trivial (octo-server env var) | requires octo-lib config plumbing | unchanged |

**Honest conclusion**: the cost gap narrowed (B's blanket-coverage advantage is now more attractive than when there was only 1 known entry), but the cross-repo release-cadence problem and architectural-boundary problem did not narrow — they were always the dominant reasons to reject B, and they still are. **A+ still wins, but by a smaller margin than R1 claimed.** We accept the residual "must remember the 4th call site" risk and mitigate with a per-handler test + a follow-up lint task in §5.

### Coverage matrix

| Flow | Caught by (A+) helper-in-3-handlers | Caught by (B) ctx.SendMessage |
|------|--------------------------------------|-------------------------------|
| Client `POST /v1/message/send` | ✅ | ✅ |
| Client `POST /v1/bot/sendMessage` | ✅ | ✅ |
| Client `POST /v1/robots/:robot_id/:app_key/sendMessage` | ✅ | ✅ |
| Super-admin `POST /v1/manager/*` | n/a — handler hardcodes `{content, type:1, from_uid}`, no `mention` field possible | n/a |
| Server-built `*MdNotification` (uids only) | n/a | ✅ (no-op rewrite) |
| Reply enrichment with stale `mention.all` snapshot | ❌ | ❌ — read-path, not write-path; see §4 #1 |
| Mergeforward nested `messages[].mention.all` | ❌ | ❌ — adapter `isMentioned` only inspects outer payload; see §4 #2 |
| messageEdit | n/a — no fan-out | n/a |

> Coverage of all real fan-out vectors is now identical between (A+) and (B). (A+) wins on architectural boundary, release cadence, and blast radius — but the margin is smaller than R1 implied.

---

## 4. Risk list (explicitly enumerated by issue)

### 4.1 Reply enrichment with stale `mention.all`
- Location: `modules/message/api.go:2483-2503` (`newSyncChannelMessageResp` → reply hydration block).
- Read-path: `payloadMap["reply"]["payload"]` is overwritten with the latest `content_edit` snapshot from `messageExtraDB`. The snapshot is a JSON blob; if a historical message carried `mention.all=1`, the snapshot still has it.
- **Live fan-out impact**: ❌ none. This runs only during `POST /v1/message/channel/sync` (history pull). The adapter does its `isMentioned` evaluation on the **realtime WuKongIM fanout**, not on REST sync responses. A stale `reply.payload.mention.all=1` in a sync response is a UI artifact (quoted preview), not a notification.
- **Action**: no chokepoint rewrite required. Optionally, in a follow-up, scrub `reply.payload.mention.all` during this read enrichment for cosmetic cleanliness, but it does NOT trigger adapter "@all" behavior.

### 4.2 Mergeforward (content type 11 / `MultipleForward`) nested `mention.all`
- Construction: 100% client-side. octo-server never builds a mergeforward bundle; it forwards them through `POST /v1/message/send` like any other payload.
- Server enrichment: `applyExternalMarkers` (`modules/message/api.go:3034`; called from `enrichExternalMarkers` at `modules/message/api.go:3017`) walks `payload["users"]` to inject `is_external` etc. It does NOT walk `payload["messages"][].mention`.
- **Live fan-out impact**: ❌ adapter `isMentioned` is asserted to inspect only the OUTER `payload.mention.{all,uids}` (reference: dmwork-adapters 0.6.3 `octo/src/inbound.ts:~1186` per cross-team verification; **pending adapter-side confirmation** in the PR review thread for this audit before merge of any 方案 X implementation PR). Under that assertion, nested `messages[].mention.all=1` inside a forwarded bundle is data-at-rest in the carrier payload and does NOT re-trigger an @all notification when the carrier is delivered.
- **Action**: NO rewrite at the chokepoint for nested fields. If we later want defense-in-depth (forensic / search hygiene), add a separate pass that walks nested `messages[].mention.all` — but treat as P3, not P1.

### 4.3 messageEdit
- Path: `messageEdit` (`modules/message/api.go:610`) writes `messageExtraDB.content_edit` then `m.ctx.SendCMD(CMDSyncMessageExtra)`. **No `m.ctx.SendMessage` call.**
- The CMD is a sync hint; clients pull `messageExtra` and reconcile. `content_edit` can technically carry a new `mention.all=1` payload if a malicious client crafts one, but:
  - The adapter is asserted to **not** evaluate `isMentioned` on `messageExtra` deltas — only on the original fan-out (reference: dmwork-adapters 0.6.3 `octo/src/inbound.ts:~1186` per cross-team verification; **pending adapter-side confirmation** in the PR review thread for this audit before merge of any 方案 X implementation PR).
  - Per-user reminders for the edit do NOT regenerate (the original `mention.all` reminder was created by `listenerMessages` at send time).
- **Live fan-out impact**: ❌ none.
- **Action**: skip. If product wants edited-payload mention scrubbing for forensic correctness, add later.

### 4.4 New / not-listed risks discovered during audit
- **Super-admin `/v1/manager/message/send`**: bypasses `Message.sendMessage()` but **needs no rewrite**. The handler (`Manager.sendMsg` at `api_manager.go:760-843`) hand-builds the outbound payload as `{content: req.Content, type: 1, from_uid: req.Sender}` — the `managerSendMsgReq` schema only exposes `Content`/`Sender`/`ReceivedChannelID`/`ReceivedChannelType`, so no `mention.*` field can ever flow through this path. No `rewriteDeadMentionAll` mirror is required.
- **Robot webhook — friend-tip / command-reply sends from `robotMessageListen`** (`modules/robot/event.go:131,342`): payload comes from external robot HTTP webhook body, not user client. We don't currently observe `mention.all` being set by webhook robots in production traffic, and no internal robot contract document mandates it; flag this path explicitly if 3rd-party robots start setting `mention.all=1` (would surface in the `api_reminders.go:288` reminder consumer or in adapter telemetry). Out of scope for 方案 X.
- **`listenerMessages` → `getReminders`**: this is the **consumer** of `mention.all` (`modules/message/api_reminders.go:283`). After 方案 X rewrites outbound payload to `mention.humans=1`, this code path stops generating "[有人@我]" group-wide reminders for newly-sent messages. **This is a behavior change requiring a parallel update**: either teach `getReminders` to also recognize `mention.humans=1` as @all-equivalent, or accept the product change (no more "@all generates a reminder for everyone"). Flag explicitly to product before merging方案 X.

---

## 5. Recommended PR split

Single repo: `Mininglamp-OSS/octo-server`. No octo-lib bump, no adapter changes (adapter `ignoreMentionAll` already shipped in 0.6.3 stays).

### PR #1 — server-side `mention.all` → `mention.humans` rewrite (this audit's target)

**Touched files** (1 new helper + 3 handler-path call sites + 1 reminder teach + 3 tests = 5 production + 3 test):

1. **NEW**: `modules/message/mention_rewrite.go` — define exported helper `RewriteDeadMentionAll(payload map[string]interface{}) map[string]interface{}` (or `common/mention.go` if cross-module placement preferred; recommend `modules/message/` since `Message.getMention` already lives in `modules/message/api_reminders.go` — keep mention semantics co-located). Helper is pure (no IO, no logger), table-testable. ~25 LOC including doc comment + sentinel handling for absent / non-map / v2-`entities` payloads.
2. `modules/message/api.go` — call `payload = RewriteDeadMentionAll(payload)` inside `Message.sendMessage()` right after `enrichPayloadWithSpaceID(...)` (around line 449). ~1 LOC.
3. `modules/bot_api/send.go` — call `payload = message.RewriteDeadMentionAll(payload)` inside `BotAPI.sendMessage` right after the `enrichBotPayloadWithSpaceID` block (around line 67, just before constructing `msgReq`). ~1 LOC + import.
4. `modules/robot/api.go` — call `payload = message.RewriteDeadMentionAll(payload)` inside `Robot.sendMessage` right after the `enrichBotPayloadWithSpaceID` block (around line 352, just before `ctx.SendMessageWithResult`). ~1 LOC + import.
5. `modules/message/api_reminders.go` — extend `Message.getMention` (line 283) to also treat `mention.humans=1` as the @all-equivalent so reminder-generation behavior is preserved post-rewrite. ~5 LOC.

**Tests** — 1 helper-level + 3 handler-path:

6. `modules/message/mention_rewrite_test.go` — table-driven tests for the pure helper: `mention.all=1` → rewritten to `mention.humans=1`; `mention.all=0` → untouched; absent → untouched; mixed `all` + `uids` → uids preserved; non-map mention → untouched; **v2 mention with `entities` — rewrite must not corrupt or drop `mention.entities`** (cross-reference `modules/message/validation_test.go:885-928`).
7. `modules/message/api_test.go` — handler-path integration test: `POST /v1/message/send` with `payload.mention.all=1` → assert outbound `MsgSendReq.Payload` has `mention.humans=1` and no `mention.all`.
8. `modules/bot_api/send_test.go` — handler-path integration test: `POST /v1/bot/sendMessage` with `payload.mention.all=1` → assert intercepted `MsgSendReq.Payload` (via `dispatchOverride` hook, already used in `bot_api.go:44`) has `mention.humans=1` and no `mention.all`.
9. `modules/robot/api_test.go` — handler-path integration test: `POST /v1/robots/:robot_id/:app_key/sendMessage` with `payload.mention.all=1` → assert intercepted dispatch has `mention.humans=1`. (Add a sibling `dispatchOverride` pattern in `Robot` if not present; this is small.)

**Explicitly NOT touched**:
- `modules/message/api_manager.go` — `Manager.sendMsg` hand-builds `{content, type:1, from_uid}` from `managerSendMsgReq`; no `mention.*` field flows through this path, so the rewrite is a no-op and is omitted.
- `modules/botfather/api.go:696` — dead code (handler not routed; file inline comment documents this).
- `modules/botfather/welcome.go:116` — server-built hardcoded `{type: Text, content}`, no mention field.

**Do NOT touch**:
- octo-lib (`Context.SendMessage`) — wrong layer (§3 candidate B).
- adapter (`ignoreMentionAll` is the belt; this is the suspenders).
- `messageEdit`, reply enrichment, mergeforward walker — see §4.

**Follow-up lint task** (track as a separate small ticket, not blocking this PR): add a `golangci-lint` custom check or a CI grep that flags any new `ctx.SendMessage(WithResult)?` call whose `Payload` derives from a `*Req.Payload` field but doesn't go through `RewriteDeadMentionAll` first. This addresses the "must remember the 4th call site" residual risk identified in §3.

### PR #2 (optional follow-up, separate ticket) — read-side scrub
- `newSyncChannelMessageResp` reply enrichment: scrub `reply.payload.mention.all` (cosmetic only; see §4.1).
- `applyExternalMarkers` (`modules/message/api.go:3034`): optionally walk nested `messages[].mention.all` for mergeforward bundles (§4.2).
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

# All ctx.SendMessage / SendMessageWithResult call sites
grep -rEn 'ctx\.SendMessage(WithResult|Batch)?\(' --include='*.go' modules/ | grep -v _test.go

# All mention.all consumers — must match BOTH literal-string access AND Go map indexing.
# The naive `grep 'mention.all'` only catches the literal form (e.g. gjson `.Get("mention.all")`)
# and MISSES Go map indexing like `mentionMap["all"]` which is how the real consumer in
# api_reminders.go reads the field. Use this regex instead:
grep -rEn 'mention[."]?all|mention(Map|Value)[^=]*\[?"?all"?\]?' --include='*.go' modules/ | grep -v _test.go

# Equivalent two-pass form (literal-string consumers + map-index consumers):
grep -rn 'mention\.all\|mention\["all"\]\|"mention\.all"' --include='*.go' modules/ | grep -v _test.go
grep -rEn 'mention(Map|Value)\s*\[\s*"all"\s*\]' --include='*.go' modules/ | grep -v _test.go

# Chokepoints and precedent
sed -n '438,464p' modules/message/api.go        # Message.sendMessage + enrichPayloadWithSpaceID
sed -n '29,80p'  modules/bot_api/send.go        # BotAPI.sendMessage entry
sed -n '40,56p'  modules/bot_api/bot_api.go     # BotAPI.dispatchMsgSendReq → ctx.SendMessageWithResult
sed -n '290,360p' modules/robot/api.go          # Robot.sendMessage + ctx.SendMessageWithResult
```

## Appendix B — Glossary

- **Chokepoint**: a single function through which all flows of interest must pass.
- **Dead field**: a field name (`mention.all`) we semantically retire by ensuring it never appears on the wire post-rewrite. Old clients still understand `mention.all`; new server emits `mention.humans` instead; adapter `ignoreMentionAll` (already shipped in 0.6.3) silently drops any residual `mention.all=1` it sees.
- **方案 X**: server-side rewrite of outbound `mention.all=1` → `mention.humans=1`.
- **方案 A**: adapter `ignoreMentionAll` flag — already shipped in dmwork-adapters 0.6.3. Defense in depth.
