---
type: Journal
title: "Journal: incoming-webhook-quota-per-thread"
description: Re-scoped the incoming-webhook creation quota from per-parent-group to per-delivery-scope (group_no, thread_short_id) so the group itself and each thread (子区) get independent webhook budgets; the FOR UPDATE serialization lock deliberately stays on the parent group row while only the COUNT(*) predicate narrows. Reuses max_per_group/max_per_creator per-scope with no new setting and no schema migration.
tags: ["incomingwebhook", "thread", "webhook", "quota", "space", "isolation", "bot-api", "error-response", "i18n", "testing"]
timestamp: 2026-07-22T10:44:01Z
# --- octospec extension fields ---
task: incoming-webhook-quota-per-thread
upstream: Mininglamp-OSS/octo-server#640
source: self
---

# Journal: incoming-webhook-quota-per-thread

## What was done

One commit re-scoping the Incoming Webhook creation quota from *per parent
group* to *per delivery scope* `(group_no, thread_short_id)`. The group itself
(`thread_short_id=''`) and every thread each get an independent webhook budget;
a group with many threads — and Octo Loop, which provisions a webhook per
thread — is no longer blocked by the single shared per-group cap.

**feat(incomingwebhook): scope webhook quota per delivery target** (`afc788e`)

- `modules/incomingwebhook/db.go` — `insertWithQuota`: both the group-level and
  the per-creator `COUNT(*)` gain `AND thread_short_id=?`
  (`m.ThreadShortID` / with `creator_uid`). The group-self scope counts
  `thread_short_id=''`, each thread its own value; hits the existing
  `idx_incoming_webhook_thread(group_no, thread_short_id, status)`. **The
  `SELECT id FROM `group` ... FOR UPDATE` lock is unchanged — still the parent
  group row.**
- `pkg/errcode/incomingwebhook.go` + `pkg/i18n/locales/active.zh-CN.toml` +
  `tools/i18nmarkers/server/active.en-US.toml` — the two 409 quota messages
  generalized from "this group" to "this group or thread"; en-US markers
  regenerated via `make i18n-extract`.
- `modules/common/system_settings.go` + `system_setting_schema.go` —
  `max_per_group` / `max_per_creator` keep their keys and default values (10 /
  5) but their documented semantics shift to "per delivery scope"; method doc
  comments + admin-facing schema `Description` strings updated.
- `modules/incomingwebhook/api.go` + `api_i18n.go` + `README.md` — comments and
  the module README's quota section aligned to per-scope.
- `modules/incomingwebhook/db_test.go` — the old
  `TestThreadWebhookSharesParentGroupQuota` (which asserted the shared-bucket
  behavior) rewritten into `TestThreadWebhookQuotaIsPerScope` (group↔thread and
  thread↔thread independence) + new `TestCreatorQuotaIsPerScope`.

No schema/data migration: the `channel_type` / `thread_short_id` columns and the
composite index already exist (from `incoming-webhook-thread`); existing rows
carry `thread_short_id=''` and fall into the group-self bucket, byte-for-byte
backward compatible.

This **supersedes a previously locked decision**. The `incoming-webhook-thread`
task explicitly locked "thread webhooks share the parent group's
`max_per_group` … no per-thread quota and no new system_setting"
(`.octospec/tasks/incoming-webhook-thread/brief.md`). Product reversed it: Octo
Loop makes per-thread webhooks routine, so the shared cap became a usability
blocker.

## Structural learnings worth remembering

### A quota's serialization lock and its counting predicate are separable

The instinct when narrowing a quota from per-group to per-scope is to also
narrow the lock to the scope: `SELECT ... FROM incoming_webhook WHERE group_no=?
AND thread_short_id=? ... FOR UPDATE`. That reintroduces exactly the InnoDB
gap-lock deadlock the original design was written to avoid — an empty
`(group_no, thread_short_id)` range matches 0 rows, so `FOR UPDATE` takes a pure
**gap** lock; gap-X locks are mutually compatible, so concurrent creators all
pass the count check and then deadlock (1213) contending for the
insert-intention lock, with no retry.

The fix is to keep the two concerns independent: **lock coarse (the always-
present parent `group` record row), count fine (the scope predicate).** The lock
only needs to *serialize* concurrent creates in the same group; it does not need
to match the counting granularity. Over-serializing (whole group instead of
per-thread) is a negligible cost here (`incoming_webhook` is a small table) and
is strictly safe. Promoted to `.octospec/learnings/pending/`.

## Gotchas worth remembering

- **`max_per_group` is now a misnomer but the key was kept on purpose.** Renaming
  the system_setting key would strand existing DB-configured values
  (`incomingwebhook.max_per_group`) and env (`DM_INCOMINGWEBHOOK_MAX_PER_GROUP`).
  The key stays; only its human-facing description + Go doc comments say "per
  delivery scope." A future `max_per_thread` split (separate value for threads)
  is possible but was deliberately out of scope — reuse the one value per-scope.
- **The per-creator cap had to move in lockstep.** Leaving `max_per_creator`
  group-wide while `max_per_group` went per-scope would make the *personal*
  budget the new bottleneck (a member maxes at 5 across all threads), re-creating
  the Octo Loop pain one layer down. Both layers re-scope or neither.
- **Existing HTTP-level quota tests needed no change.** `TestCreate_QuotaEnforced`
  / `TestCreate_QuotaConcurrent` only create group-scope webhooks
  (`thread_short_id=''`), so `WHERE group_no=?` and `WHERE group_no=? AND
  thread_short_id=''` count identically — they still pass. The behavior change is
  isolated to the DB count predicate and fully asserted at the DB layer, which is
  the single code path both the user face and the bot face funnel through.
- **Integration tests could not run in the authoring sandbox** (no
  MySQL/Redis/WuKongIM, no docker daemon). Static gates (build, vet,
  golangci-lint, i18n-extract-check, i18n-lint) all pass; the new DB tests
  compile under `go vet` but their assertions run only in CI.

## What is deliberately left for later

- A dedicated `max_per_thread` system_setting (separate cap value for threads vs
  the group itself) — reuse the single value per-scope for now.
- Retroactive reclamation of over-budget rows when a per-scope cap is lowered —
  quota stays a create-gate, not a continuous constraint (unchanged semantics).
