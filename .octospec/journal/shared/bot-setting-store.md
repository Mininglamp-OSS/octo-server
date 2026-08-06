---
type: Journal
title: "Journal: bot-setting-store"
description: Add a generic per-bot config store (bot_setting) with a bot → system_setting → code-default resolution chain, and put the four bot-level card switches on it.
tags: ["bot-api", "robot", "config", "wire-contract", "auth", "rate-limit", "testing"]
timestamp: 2026-08-06T16:20:00+08:00
# --- octospec extension fields ---
task: bot-setting-store
upstream: "openclaw-channel-octo 推理进度卡策略请求"
source: user
---

# Journal: bot-setting-store

## What was done

- Added `bot_setting`, a sparse `(robot_id, key_name)` override table, plus a
  registry in `modules/robot/bot_setting.go` that is simultaneously the write
  whitelist, the owner-facing catalog, and the resolution chain. A new config
  key is a registry entry, not a migration.
- Resolution: bot override → `system_setting` deployment default → code default.
  Deleting an override falls back to the layer underneath rather than latching
  false; the read endpoint returns `value` / `effective_value` / `source`
  separately so a restore-default control can tell "I chose off" from "I never
  set it".
- Owner endpoints on `/v1/robot/:robot_id/settings` (GET catalog + echo, PUT
  batch write, DELETE one override), guarded by `assertRobotOwner` and mounting
  `SharedUIDRateLimiter` per route.
- First consumer: four card switches. `bot.card_enabled` is derived from
  `cardmsg.BotEnabled()` and rejects writes; `display` / `interaction` /
  `reasoning` are owner-editable and default true.
- `GET /v1/bot/card/profile` gained one additive `config` object carrying the
  already-AND-ed values, with the ref invariants asserted in tests.
  `sendMessage` enforces the same config independently.
- Writes enqueue a valueless typed bot event so an adapter drops its cached
  profile immediately instead of waiting out a TTL.

## Decisions worth keeping

**The precedent that looked right was the wrong one.** `bot_mention_pref` is a
sparse table, so it reads as "this repo prefers sparse tables for bot config".
It is not: its dimension is `(robot_id, group_no)`, and a `robot` column cannot
express two dimensions — the table was forced, not chosen. The actual precedent
for *one-dimensional* bot config is a column on `robot` (`auto_approve`,
`inline_on`, `placeholder`, `bot_commands`). So this task replaces
"keep adding columns", and borrows only mention_pref's *surrounding* shape:
owner guard, delete-means-fall-back, post-write cache invalidation, and the
owner-writes / adapter-reads split across two modules.

**A derived key must not be storable.** `card_enabled` reflects the deployment
env chain. Had it been a normal stored key, the DB could hold `true` while the
env holds it off — the manifest would advertise a capability the send path
refuses, breaking the "manifest agrees with the send gate" invariant
`pkg/cardmsg` already established. Making it read-only and rejecting writes
removes the state rather than documenting it.

**Sub-switches default true because the master switch is already fail-closed.**
The upstream asked for a conservative default. `OCTO_CARD_MESSAGE_ENABLED`
defaults to false, so with sub-switches at true the effective outcome is still
"no cards" until an operator opts in. Defaulting them false as well would have
made operators opt in twice for no extra safety.

**Three switches, not a profile filter.** The upstream proposed clipping
`profiles` per bot (octo/v1 = display, octo/v2 = interaction). Two reasons not
to: `pkg/cardmsg/profiles.go` documents `acceptedProfiles` as the single
authority shared by the validator and the D12 manifest, and clipping breaks that
by design; and the reasoning card spans both profiles (active/error are
`octo/v2`, result is `octo/v1`), so a profile-shaped switch would leave it with
only its terminal state or only its progress states.

The raw pair and the template switch are orthogonal — display/interaction gate
the raw-card path, reasoning gates the template path — but **display and
interaction are not orthogonal to each other**, and an earlier draft of this
journal said they were. Review found the bypass that claim licences: `octo/v2`
is a strict superset of `octo/v1` (the `interactive` flag in
`cardmsg.validateCard` only ever relaxes checks, never requires an interactive
element), so with the two treated as peers a bot defeats `display_enabled=false`
by changing one string and sending byte-identical display-only cards as
`octo/v2`. Display is a **floor**: `octo/v1` needs display, `octo/v2` needs
display AND interaction. Both live on `BotCardConfig.AllowsRawDisplayCard()` /
`AllowsRawInteractiveCard()`, which every gate and the manifest call — restoring
"orthogonality" here reopens the bypass.

**No per-bot cache, on purpose.** Reads go to MySQL every time, so an owner's
change on one replica is visible on every other immediately. A process-local
cache would trade that for TTL drift, and the invalidation event reaches only
the bot, not sibling replicas — Redis pub/sub would be required to close it.

Cost of the no-cache choice, per path: non-card sends never enter any gate;
raw-card creation and raw-card edit each cost one indexed single-row read;
template creation costs one. Only the **template** branch of `message/edit` is
ungated, which is what lets a streaming card reach its terminal state — the raw
branch is gated, because `cardmsg.Validate` accepts any profile in the accepted
set and never compares the edit frame to the original, so leaving it open was a
privilege-escalation path (send `octo/v1`, then edit into an `octo/v2` frame
carrying `Action.Submit`, which is the frame the action endpoint trusts).
A producer streaming through raw card edits therefore pays one read per frame;
the shipped reasoning-card consumer streams through the template branch and
pays nothing per frame.

## What went wrong on the way

1. **A hot-path global mutex, found only because performance was questioned.**
   The resolver called `common.EnsureSystemSettings` per request, and that
   function takes a process-wide `sync.Mutex` on *every* call — on the card send
   path. `SystemSettings` is explicitly built so readers never take a lock
   (atomic.Pointer snapshot); calling the constructor-shaped accessor per request
   silently undid that. Fixed by resolving the singleton once at construction and
   holding it on `Robot` / `Service`.

2. **Test isolation broke under `-shuffle`, and the cause is a production
   property.** `EnsureSystemSettings` is a process-wide singleton whose in-memory
   snapshot `CleanAllTables` does not touch. A case that wrote a deployment
   default leaked it into every later case in the binary, surfacing as
   `source="global"` where `"default"` was expected. Fixed by forcing `Reload()`
   in setup. Same family as the already-documented "CleanAllTables does not clear
   Redis rate-limit buckets" — staged as a learning.

3. **The contract guard did its job.** `TestBotCardProfile_AdditiveContractFieldSet`
   freezes the profile's top-level field set, so adding `config` failed it. The
   fix is to extend the frozen set — and `config`'s own five sub-fields were
   frozen the same way `limits` already is.

## What review found that the author did not

Three rounds of review across four heads. Recording these because the pattern in
them is more useful than any individual fix: **every one was a sibling path or a
sibling consumer that was not updated when a new gate was introduced.**

1. **A new gate does not gate what it does not reach.** Round 1 found the raw
   branch of `/v1/bot/message/edit` ungated: `cardmsg.Validate` accepts any
   accepted profile and never compares the edit frame to the original, so a bot
   with interaction off could send `octo/v1` and then edit into an `octo/v2`
   frame with `Action.Submit` — the frame the action endpoint treats as
   authoritative. Round 2 then found the same shape one door further out: the
   legacy `POST /v1/robots/:robot_id/:app_key/sendMessage` ingress consulted no
   per-bot config at all, while being keyed on the *same* `robot_id` column. The
   comment at `payloadIsVail` already stated the three card ingresses must stay
   symmetric; the new layer broke an invariant the code asserts about itself.
   **When adding an authorization layer, enumerate every authenticated path that
   reaches the same resource before writing the first gate** — the search radius,
   not the gate, is what was wrong both times.

2. **Fixing a gate splits it from whatever advertises it.** The round-1 floor fix
   changed what the send path accepts; the manifest kept echoing the raw switch,
   so `/v1/bot/card/profile` advertised `interaction_enabled:true` while every
   `octo/v2` card was refused — precisely the ambiguity that endpoint exists to
   remove. "Both read the same resolver" was true and not enough: they combined
   the same fields differently. Closed by making the *predicates* the shared
   thing (`AllowsRawDisplayCard` / `AllowsRawInteractiveCard`) rather than the
   data, so a future divergence requires bypassing a named function.

3. **A test named after a guarantee it does not exercise.** The first atomicity
   test drove the batch through a validation failure — which returns before
   `Begin()` — so it passed identically against the pre-transaction code. Review
   caught that the name promised more than the body delivered. Split into a
   validation test named for what it covers, plus a real rollback test that
   provokes a mid-batch MySQL error (an over-long `key_name`) via an extracted
   `writeBotSettingOverrides`. The extraction is the point: the failure needed to
   test the transaction was, by design, unreachable through the endpoint.

4. **Transactionality is not free.** Wrapping the batch in one transaction holds
   row locks to commit, which made two concurrent opposite-order batches
   deadlockable in a way per-statement autocommit was not. Sorting `plans` by key
   removes the ordering variable — the repo already had the precedent under
   "#627 D4 uniform lock order".

5. **A copied header can be worse than no header.** `Vary: Authorization` was
   carried over to the owner settings routes from the bot profile endpoint. Those
   routes authenticate with `token`, so it named a header with no effect on the
   response — the appearance of cache isolation without the substance.

6. **A recurring nit is evidence.** Ownership-after-shape-validation was raised
   by three reviewers across three rounds, and twice deflected on "shape errors
   need no DB query". The framing that landed was different in kind: it made a
   malformed request against an unknown bot answer 400 where the endpoint
   documents 404/403 — a contradiction with its own stated semantics, not a
   hardening preference.

7. **Two predicates for one question is a gap by construction.** The legacy
   ingress decides "is this a card?" with `maputil.Data.Int("type")`, which
   coerces strings; `cardmsg.IsCardPayload` — the predicate `Validate` itself
   short-circuits on — deliberately does not. Nothing was wrong with either
   function; the defect lived in the gap between them, and it swallowed the URL
   allowlist, the node/depth caps, profile negotiation, `Finalize` **and** these
   new switches for any `{"type":"17"}` payload. Same family as lessons 1 and 2
   (a sibling path, then a sibling consumer) with the radius shrunk to a single
   function: **when a branch's guard and its body disagree about what reached
   them, the body wins and the guard is decoration.** Closed by routing on the
   validator's own predicate, so reachability and enforcement cannot drift.

## Closed after round 3

Three review follow-ups were fixed in this branch rather than deferred, because
each one's failure direction is "a control silently does not apply":

- **`payloadIsVail` no longer routes on a coerced `type`.** The card branch now
  guards on `cardmsg.IsCardPayload` — the same predicate `Validate` short-circuits
  on — so a string `"17"` is refused instead of entering the card branch and
  skipping every check (see lesson 7 above). This ingress was the **only** one in
  the repo dispatching on `maputil.Data.Int`; `bot_api`, `message` and `notify`
  already called `IsCardPayload` directly. The regression test asserts both halves
  of the gap still exist (`Int` coerces, `IsCardPayload` does not) so it deletes
  itself honestly if either upstream ever changes, and it uses a `javascript:` URL
  so a pass means the allowlist ran, not merely that the type was rejected.
- **`SettingBoolOK` lexes tolerantly** (trim + case-fold). Its callers layer a
  code default on top, and for all three card switches that default is `true` —
  so `False` being read as "unconfigured" left on exactly the capability the
  operator was trying to disable, silently. The *vocabulary* deliberately stays
  identical to `parseSettingBool`'s; only the lexing relaxed. **This fix was
  itself half-done and shipped a regression for one round — see below.**
- **A malformed stored override now warns** instead of falling back mutely, once
  per `(robot, key)`. The dedupe is not polish: resolution runs on every card send
  and every profile read, and a dirty row is persistent state, so an un-deduped
  warning is a log flood — which is the same outcome as no log at all.

## The round-3 fix that broke the thing it was fixing

Worth its own section because it is the sharpest lesson on this branch, and
because it was caught by review rather than by me.

`SettingBoolOK` was relaxed to trim and case-fold. The three
`BotCard*EnabledDefault()` getters — the *other* reader of the very same
`botcard` rows, feeding the admin console's `effective_value` — were left on
`getBool` → `parseSettingBool`, which still matched exactly. So after the fix:

| reader | `botcard.display_enabled = "False"` |
|---|---|
| per-bot resolver (`SettingBoolOK`) | explicitly **false** |
| admin console (`parseSettingBool`) | unparseable → fallback → **true** |

An operator would see "display cards on" in the console while every bot resolved
it off. The comment I wrote directly above the fix says one column must not come
to mean different things to two readers — and the fix made that false in exactly
the dimension it touched.

The test missed it for a reason worth naming: it pinned the **vocabulary** axis
(`on`/`off`/`yes`/`no` rejected by both readers) because that was the axis I had
been thinking about while writing the fix. The axis I *changed* — lexing — went
unpinned. **A test written from the same mental model as the fix inherits the
fix's blind spot.** The replacement asserts the two readers agree literal-by-
literal, including whitespace and case forms, rather than restating the intent.

The structural fix is delegation, not duplication: the getters now call
`SettingBoolOK`, so there is one lexer for one column and the next change cannot
land on only one side.

**And the commit that said so was still wrong.** Review found a second pair of
lexers inside `bot_setting.go` itself — the file that commit edited, under a
title claiming the class was closed. `normalizeBotSettingBool` (write) trimmed
and accepted `True`; `parseBotSettingBool` (read) did neither. A hand-written
`value='True'` was therefore read as "malformed, ignored, fell back" even though
the write endpoint would have accepted that exact literal. Unreachable through
the API — writes always normalise to `"1"`/`"0"` — but the same class, one layer
down, found only because a reviewer read the file the fix touched rather than the
fix's diff.

Both now share one lexer by delegation, and the rule is the same one
`SettingBoolOK` uses: trim, case-fold, vocabulary `{1,0,true,false}`. The
enumerate-the-variants style (`"true", "TRUE", "True"`) was itself the cause —
omitting a variant produces no error, only a literal that silently stops working
— so all three sites now fold instead of listing.

The invariant that would have caught it — the write-accepted set and the
read-accepted set are the same set — had no test at all until now. Three rounds
of tests asserted what each function does; none asserted that the two agree.

Two smaller ones fixed alongside, both also mine from round 3 or earlier:

- The new malformed-override warning fired on `''`, which
  `sql/20260806000001_bot_setting.sql` itself defines as "not configured" — so a
  perfectly legal empty row was logged as corrupt. Empty values are now dropped
  in `queryBotSettingOverrides`, matching how `system_settings.lookup` already
  treats `""`. Fallback behaviour is identical either way; the difference is
  whether the log tells the truth, which is the entire point of that line.
- Two code comments (`bot_setting.go`, `system_setting_schema.go`) still said the
  three switches were "三者正交" — the claim round 1 overturned, and the one this
  journal explicitly warns reopens the display bypass if acted on. The brief was
  amended at the time; the comments were not. Both now state display-as-floor and
  point at the shared predicates.

**A fourth inert test, caught by the revert discipline rather than by review.**
The first `from_uid` regression test passed with the pin deleted. The handler
pinned `req.FromUID = robotID` and then called `allowSendToChannel(robotID, …)`
— checking a *parallel variable* rather than the field it had just assigned — so
the stub recorded the right uid whether or not the pin existed. The test observed
something that could not vary. Fixed on both sides: the handler now checks
`req.FromUID` after pinning it, so one variable carries identity end to end, and
the test discriminates. The general form is worth naming because it is not the
same as the earlier three: **an assertion on a value that is correct for a reason
other than the one under test is inert**, and the only reliable detector is
deleting the fix and watching the test go red.

**The generalisable rule, after three instances of it.** Round 1 was a sibling
path (the raw edit branch), round 2 a sibling consumer (the manifest), rounds 3
and 6 a sibling *function* reading the same column. Every one was found by
looking at the neighbourhood rather than the change. **Fixing a shared rule means
enumerating everyone who implements it, not everyone the diff touches** — and the
test that proves it must assert the parties *agree*, not that each behaves as
intended, because a per-party test passes fine while they disagree.

## Known gaps at merge

- `SettingBoolOK` remains a **best-effort** tier, as its doc comment says: a
  replica whose startup `Load()` blipped has an empty snapshot, which is
  indistinguishable from "not configured", so it reports `configured=false` for up
  to one reload TTL. Tolerant lexing does not touch this; an operator who needs a
  capability genuinely off should set it at the tier that enforces.
- **`AdvertisedRef` fails closed loudly on the manifest and silently everywhere
  else.** Advertising two versions of one template id — which
  `newBotCardTemplateCatalog` permits, rejecting only exact duplicates — makes it
  return `(zero, false)`, which degrades one manifest field but rejects *every*
  template send. Reasoning cards would be completely dead with only a `Warn` line
  as the signal. The loop count (round-3 P2-4) was never the issue; the missing
  invariant is. The fix is to reject such a policy at catalog construction so a
  silent production outage becomes a startup error — deliberately deferred, as it
  changes a constructor's contract and belongs with the other template-catalog
  follow-ups rather than in a round-4 hotfix.
- **The owner catalog's `effective_value` is not master-switch-dominated.**
  `listBotSettings` returns the raw resolution while `botCardConfigFrom` applies
  `applyCardMasterSwitch`, so with cards off at the deployment level the Bot
  management page shows `display_enabled: true` while the profile says false and
  every send 400s. The same response carries `bot.card_enabled` with
  `source:"env"`, so a client *can* compose the truth — but nothing in the
  contract obliges it to, which makes this the "manifest disagrees with the gate"
  contradiction relocated to the owner surface. Needs an explicit decision
  (dominate `effective_value`, or document this as the pre-master-switch tier),
  not another round of implicit.
- **`stream/start` — card path closed, two pre-existing holes still open.**
  Review found a fourth authenticated path: `streamStart`
  (`modules/robot/api.go:482`) hands a raw `[]byte` `Payload` straight to
  `IMStreamStart` with no shape validation and none of this task's gates, so a
  bot could still emit `type:17` after its owner disabled cards. Two reviewers
  agreed on every fact and split on scope — one blocked (the PR ships a
  guarantee it does not deliver), one verified the handler is byte-identical to
  the merge-base and argued it belongs in its own security issue. **Resolved by
  taking the narrow half**: the handler now refuses card payloads outright
  (`cardmsg.IsCardRawPayload`), which makes the guarantee true without rebuilding
  the whole `sendMessage` pipeline on a path whose purpose is incremental text.
  Refusing rather than gating is the point — a second full validation pipeline is
  a second thing to drift.
  **The identity half turned out to be the real blocker, and is now also fixed.**
  The card refusal alone did not satisfy the blocking review: it separated the
  two and called the forgeable `from_uid` "a real authorization-symmetry gap
  independent of card rendering", with "documenting the gap does not close" it.
  Correct — `sendMessage` and `typing` in the same `robotAuth` group both pin
  `FromUID` to `c.Param("robot_id")` and both call `allowSendToChannel`;
  `streamStart` was the lone exception, so auth answered "who are you" while the
  body got to overwrite "who you say you are". Both are now aligned with the
  siblings.
  Remaining on that handler: no `payloadIsVail`/`Validate` for non-card payloads,
  i.e. it is still the one ingress without shape validation. Deliberate — see the
  brief for why refusing cards beats rebuilding the pipeline here.
  **Order is part of the fix, not cosmetics.** Identity → channel → content, the
  same rule the settings endpoints landed on after three rounds ("answering about
  content before establishing authorization contradicts the endpoint's own 403").
  The first draft had the card gate first, which meant a non-member sending a card
  got `content_invalid` instead of `channel_send_forbidden`. Caught in self-review
  by diffing against `sendMessage`, where `allowSendToChannel` precedes
  `payloadIsVail`. Pinned by `TestStreamStart_ChannelCheckPrecedesTheCardGate` —
  which is the only case that fails when the two are swapped, because every other
  case trips exactly one gate and stays green either way.
- **Adding the channel gate exposed that `allowSendToChannel` never handled
  subareas — in three handlers, not one.** Review caught that the new gate turned
  community-topic (`ChannelType` 5) streaming into a 403, because
  `allowSendToChannel` recognised only Person and Group and fell through to
  `return false`. Real regression, and it was unpinned: the stream tests covered
  only types 1 and 2.

  The supporting argument was that `streamStart`'s community-topic disband guard
  had become unreachable — a guard written for a type the function now refuses.
  True, but checking the other call sites changed the conclusion: `sendMessage`
  (check at :746, dead subarea branch at :761) and `typing` (:679 / :694) carry
  the *identical* unreachable branch on `main` today. So subareas were already
  refused on both of those, and `streamStart` was not the victim of a new
  narrowing — it was the one path where the missing check had accidentally left
  subareas working. Three guards that resolve a parent group, all dead, is not
  three mistakes; it is one missing case in the shared predicate.

  Fixed there rather than in `streamStart`: `allowSendToChannel` now resolves the
  parent group for type 5 and checks membership on it, which is where subarea
  membership actually lives. Deliberately **not** a local branch in `streamStart`
  — that would have been a fourth private copy of the channel rule, the exact
  drift this task spent three rounds eliminating elsewhere. The change is strictly
  permissive (type 5: always-deny → membership-checked), so nothing that works
  today stops working; only previously-403'd subarea requests are affected. It
  does mean `sendMessage` and `typing` gain subarea support as a side effect,
  which is a behaviour change on two endpoints this task does not otherwise own —
  recorded here rather than left to be discovered.

  **And the first version of that grant used the wrong membership predicate —
  the fourth instance of this branch's signature mistake.** It called
  `ExistMember`, which filters `is_deleted=0` only and therefore returns true for
  a blacklisted member. The strict variant `ExistMemberActive` also requires
  `status=Normal`, and its doc comment in `group/service.go` names this exact
  situation: *"子区(CommunityTopic)读/发门禁用它替代 ExistMember，避免被拉黑用户越权
  读/发（YUJ-4185 CR 整改）"*. Every other subarea gate in the tree — `thread`,
  `message`, `messages_search`, `bot_api/threads.go` — uses the strict one. The
  new line was the only subarea gate in the repository on the loose predicate,
  and it mattered more here than elsewhere: robot ingresses send server-side and
  bypass the IM datasource, so they never receive `thread`'s subarea blacklist
  inheritance and this local check is the only defence.

  The lesson is the one already written three sections above, applied to the
  wrong noun. "Enumerate everyone who implements the shared rule" was followed
  for `allowSendToChannel` — its three *callers* were all found. It was not
  followed for "how does this repo decide subarea membership", whose
  *implementers* were never enumerated. **The unit of enumeration is the
  decision, not the function**: introducing a new authorization grant means
  finding every existing grant of the same kind and matching its predicate,
  before writing the call.

  The test that pins it holds `member=true, activeMember=false` — precisely a
  blacklisted row — so it fails if the predicate is swapped back, and asserts on
  *which method was called* rather than only on the verdict, because a stub with
  one shared result would answer both identically and stay green.
- **`streamEnd` has no caller binding, and one review's description of it is
  wrong.** An automated review suggested a stream's final content could smuggle a
  card via `streamEnd`. It cannot: `config.MessageStreamEndReq` is
  `{StreamNo, ChannelID, ChannelType}` with no payload field, so there is nothing
  there to validate. Recorded because inheriting that claim would send the next
  person hunting for a gate with nothing to gate. The *real* gap on that handler
  is different and untouched: `StreamNo` is not bound to the caller, so any
  authenticated bot can end any stream whose number it can name. Pre-existing,
  byte-identical to the merge-base, out of scope here — and since no issue was
  opened by instruction, this paragraph is its only record.
- **`TestBotMessageEdit_StillReachesTerminalStateWhenReasoningOff` is inert.**
  It injects a `stubCardConfig` with every sub-switch off, but
  `botMessageEditViaRegistry` never calls `resolveBotCardConfig`, so the stub
  changes nothing and the test passes identically with all switches on. It pins
  none of the condition in its own name. Second instance of this shape on the
  branch (the first was the atomicity test in round 1), and the tell is the same
  both times: **the test never fails when the thing it names is broken, because
  the thing it names is not on the path it drives.**
- `TestListGroups_CarriesGroupAllowNoMention` (`mention_pref_test.go`, unrelated
  to this task) panics in the local sandbox because the endpoint returns an empty
  list. It reproduces identically on the pre-change HEAD and is green in CI, so it
  is an environment difference, not a regression from this branch — recorded so
  the next person running these packages locally does not chase it.

## Verification

All 18 CI checks green at the review head, including the DB-backed `Test` job.
Locally: `go build ./...`, `go vet ./...`, `make i18n-extract-check`,
`make i18n-lint`, both modules' `NoLegacyResponseError` source guards, and
`-race -shuffle=on` on `modules/common`, `modules/robot` and `modules/bot_api`
against MySQL 8 + Redis + WuKongIM v2.2.4, run per-package with the CI
drop/create + FLUSHALL discipline.

Each round-3 fix was verified by **reverting only that fix and confirming its
test fails** — the discipline round 1 taught the hard way, when an atomicity test
turned out to pass against the code it was written to catch. `SettingBoolOK`'s
test fails on `"\tfalse\n"`; the ingress test fails with the `javascript:` card
accepted.
