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

## Known gaps at merge

- **`payloadIsVail` routes on a coerced `type`.** `maputil.Data.Int` runs
  `strconv.Atoi` on strings while `cardmsg.IsCardPayload` deliberately rejects a
  string `"17"`, and `Validate` no-ops when `IsCardPayload` is false. A
  `{"type":"17"}` payload therefore enters the card branch on the legacy ingress
  and skips the URL allowlist, the node/depth caps, profile negotiation,
  `Finalize`, and these switches. **Pre-existing and outside this change** — the
  new gate's reachability predicate is `IsCardPayload`, the same predicate the
  repo's own validator uses, so the gate is exactly as reachable as validation
  is. Severity depends on whether any client renders a string-typed `type`;
  tracked separately rather than grown into this task.
- `SettingBoolOK` matches bool literals exactly, so an operator writing `False`
  or `off` into a `botcard` global default falls through to the code default —
  which for all three switches is `true`, i.e. the capability the operator meant
  to disable stays on. Consistent with every other `system_setting` bool, but the
  failure direction is worse here; a trimmed, case-insensitive parse is the fix.
- A malformed stored per-bot override falls through silently with no log line.

## Verification

All 18 CI checks green at the merge head, including the DB-backed `Test` job.
Locally: `go build ./...`, `go vet`, `make i18n-extract-check`, `make i18n-lint`,
both modules' `NoLegacyResponseError` source guards, and `-race -shuffle=on` on
`modules/robot` and `modules/bot_api` against MySQL 8 + Redis + WuKongIM
v2.2.4, run per-package with the CI drop/create + FLUSHALL discipline.
