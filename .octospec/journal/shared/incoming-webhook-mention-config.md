---
type: Journal
title: "Journal: incoming-webhook-mention-config"
description: Moved incoming-webhook @mention from a caller-supplied push-body param to webhook create/update config; mention now applies across all adapter endpoints, not just native.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "webhook", "external-content", "space", "isolation"]
timestamp: 2026-06-25T00:00:00Z
# --- octospec extension fields ---
task: incoming-webhook-mention-config
upstream: self
source: self
---
# Journal: incoming-webhook-mention-config

## What was done
The incoming-webhook `@mention` is no longer a per-push body param. The push
endpoint stopped reading `mention` from the request body; the @ targets are now
**configured on the webhook** at create/update time:

- New column `incoming_webhook.mention_uids` (`VARCHAR(4096)`, JSON array of octo
  UIDs; empty = no @). `create`/`update` accept + validate `mention_uids`
  (dedup, ≤ cap, all current group members — non-member/over-cap → 400
  `reason=mention_uids`); `webhookResp` surfaces it (always an array).
- `buildMention` now sources everything from the webhook row
  (`MentionUids` + `AllowMention*` switches); the request only supplies the body
  text/msg_type. Directed render (uid → `@name` pill + UTF-16 entity) is
  default-on for configured targets. Targets are re-filtered to **current**
  members at push time, so a target who left the group is dropped automatically.
- Broadcast switches `allow_mention_all` / `allow_mention_bots` now mean "this
  webhook @所有人 / @所有 AI on every push"; the `broadcastPermitted` policy gate
  (`member_can_broadcast` || creator-is-admin) is unchanged.
- **Adapter parity flipped in the safe direction:** removed the native-only
  `pushAdapter.allowMention` gate, so `handlePush` builds mention for **every**
  adapter (native + github/wecom/gitlab/feishu/multica). This was previously
  impossible because the @ targets came from the body and platform-event bodies
  carry no octo UID semantics — config-sourced targets remove that blocker.
- Removed the now-dead caller-supplied machinery: `pushPayloadReq.Mention`,
  `mentionReq`, `decodeMention`, `decodeEntities`/`finalizeEntities`/
  `entityUIDsOf`/`rangeClaimed`/`markClaimed`/`shiftEntityOffsets` and their
  tests. Pure helpers (`composeMentionContent`, `assembleMention`,
  `dedupNonEmpty`) are untouched and still unit-tested.

## Why
Requirement change: external callers must not pass `mention`; @ targets are an
operator decision made when wiring the webhook. The bonus is that mention now
works on the adapter endpoints too (the original native-only limitation was a
direct consequence of body-sourced targets).

## Learnings / gotchas
- **Column-name mapping trap.** octo-lib `util.UnderscoreName` and dbr
  `camelCaseToSnakeCase` are byte-identical, and both turn `MentionUIDs` →
  `mention_ui_ds` (consecutive-capital split), which would silently miss the
  `mention_uids` column on read AND write. The Go field is deliberately
  `MentionUids` → `mention_uids`. (Verified by reading both mappers.)
- **TEXT default portability.** Used `VARCHAR(4096) NOT NULL DEFAULT ''` (not
  `TEXT`) to avoid the MySQL 8.0.13 expression-default requirement and the
  NULL→`string` scan problem; the 50-uid × ≤40-char cap fits with ~2× margin.
- **Shared-DB cross-package test collision (env, not code).** Running
  `incomingwebhook` then `bot_api` against the same `test` DB panics in
  `NewTestServer` with `unknown migration in database` — different test packages
  import different module sets, so `gorp_migrations` from the first run is a
  superset the second doesn't recognize. Reset the DB (`DROP/CREATE DATABASE
  test`) between packages; both pass on a fresh DB.

## Verification
- `go test ./modules/incomingwebhook/...` — ok (30.5s), incl.
  `TestIncomingWebhookNoLegacyResponseError` and the new config-driven +
  adapter-parity + body-ignored e2e tests.
- `go test ./modules/bot_api/ -run 'IncomingWebhook|Webhook'` — ok (fresh DB).
- `make i18n-extract-check`, `make i18n-lint`, `golangci-lint run
  ./modules/incomingwebhook/...` — all clean. `go build ./...` — ok.

## Code review (max-effort) — follow-up fixes

A 10-angle adversarial review surfaced three issues worth fixing before merge
(rest were nits / accepted design):

1. **Broadcast switch semantic change was silently retroactive.** `allow_mention_*`
   meant "may broadcast when the caller asks" (#445/#448); this change makes them
   "broadcast on every push". Existing rows with the switch set would have started
   spamming the whole group on upgrade. Fix: the migration now **zeroes existing
   `allow_mention_all`/`allow_mention_bots` values** and **updates the column
   COMMENTs** (the old comment "是否允许推送" now contradicted runtime). Operators
   re-opt-in under the new, clearly-documented semantics.
2. **Test push ignored configured mention.** `testPush` built the payload via
   `buildPayload` only, so a configured `@` never appeared in the test message —
   admins would think the config was broken. Fix: `testPush` now runs
   `assemblePushPayload` with **broadcast suppressed** (`broadcastPermitted=false`)
   — it renders/pings the directed `mention_uids` (the part most likely to be
   misconfigured, and the thing worth eyeballing in a test) but does NOT fire
   `@所有人`/`@所有 AI`, so clicking "test" repeatedly can't all-hands-spam the
   group (broadcast is a boolean switch — no need to test it). Smoke test
   `TestTestPush_AppliesConfiguredMention` added.
3. **Field-name footgun guard.** Added an explicit `db:"mention_uids"` tag to the
   `MentionUids` model field so a future "consistency" rename to `MentionUIDs`
   can't silently break the read path (it would map to `mention_ui_ds`).

Accepted-as-is (noted, not changed): per-push member-gate / `@所有 AI` fan-out on
configured webhooks (behind the rate limiter); render-always-on for configured
targets (matches the PRD intent); `VARCHAR(4096)` is safe at the default 50-uid
cap (raising `OCTO_INCOMINGWEBHOOK_MAX_MENTION_UIDS` far past the default is the
only way to overflow it — documented bound). richtext + broadcast switch fires a
group red-dot with no visible literal (pre-existing, now config-driven).

## Review integration (PR #465 — 4 approvals, no P0/P1)

Four independent reviews on PR #465 all returned APPROVE (OctoBoooot byte-verified
all 11 PR claims; Octo-Q full data-flow trace; yujiawei spec+quality; Jerry-Xin
project-scope). No P0/P1. Deduped the findings and acted:

**Applied (this follow-up commit):**
- **Column-width guard** (consensus — Octo-Q P2 + Jerry-Xin warning): `mention_uids`
  is `VARCHAR(4096)` but `maxMentionUIDs()` is env-tunable; raising the env cap far
  enough could let `validateMentionUIDs` accept a list whose JSON overflows the
  column, failing at DB-write instead of cleanly. Added `mentionUIDsColumnChars`
  (= 4096) + a post-marshal length check → clean `400 reason=mention_uids`. This
  supersedes the previously "accepted-as-is" VARCHAR bound.
- **Stale doc comment** (yujiawei nit): `db.go` `filterGroupMembers` comment still
  named the removed `finalizeEntities` / `mention.render`; reworded to current state.
- **Two tests** (Octo-Q + Jerry-Xin suggestions): `TestMentionPush_BodyMentionIgnoredWhenConfigured`
  (body broadcast ignored even when the webhook is configured — proven via a denied
  policy, so a wrongly-honored body broadcast would surface as `mention_ignored`);
  `TestMention_UpdateRejectsNonMemberUID` (update path rejects non-member targets,
  matching the create-path guard).

**Reviewed, not changed (out of scope / deliberate):**
- Render-always-on for configured targets — deliberate per PRD (no caller `render`
  opt-in remains, by design).
- Two **pre-existing** P2s yujiawei confirmed are on `main`, not introduced here:
  `assemblePushPayload` expands `mention.ais` with the parent `GroupNo` for
  thread-bound webhooks (from #445; bot UIDs stay parent-group members, no Space
  crossing); the Feishu adapter can render attacker-supplied `at.user_name` as
  literal `@所有人` text (sets no `mention.humans`, so cannot forge a real broadcast).
  Tracked as follow-ups, not blockers for this change.

Re-verified after the follow-up: `go test ./modules/incomingwebhook/...` ok (25.7s),
`golangci-lint` 0 issues, `make i18n-extract-check` + `make i18n-lint` clean.
