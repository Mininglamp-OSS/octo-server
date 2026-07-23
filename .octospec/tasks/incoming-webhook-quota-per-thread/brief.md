---
type: Task
title: "Task: incoming-webhook-quota-per-thread"
description: Move the incoming-webhook creation quota from per-parent-group to per-delivery-scope so each 子区 (thread) gets its own independent webhook budget, plus two precise-control knobs — max_per_thread (per-thread cap decoupled from the group cap) and max_total_per_group (group-wide aggregate ceiling).
tags: ["incomingwebhook", "thread", "webhook", "quota", "space", "isolation", "bot-api", "error-response", "i18n"]
timestamp: 2026-07-22T10:44:01Z
# --- octospec extension fields ---
slug: incoming-webhook-quota-per-thread
upstream: Mininglamp-OSS/octo-server#640
source: self
---

# Task: incoming-webhook-quota-per-thread

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Change the Incoming-Webhook **creation quota granularity** from *per parent
group* to *per delivery scope* — the group itself is one bucket and **each thread
(子区) is its own independent bucket**. A group with many threads (and especially
Octo Loop, which provisions a webhook per thread) is no longer blocked by the
single shared per-group cap.

Concretely, both quota layers in `insertWithQuota`
(`modules/incomingwebhook/db.go:50`) re-scope their `COUNT(*)` by
`thread_short_id` in addition to `group_no`:

- **Group-level cap** (`max_per_group`, default 10): today
  `WHERE group_no=? AND status != ?` (`db.go:66-72`) → group-self **plus every
  thread** share one 10-bucket. After: `AND thread_short_id=?` → each distinct
  `thread_short_id` value (including the group-self `''`) gets its own 10-bucket.
- **Per-creator cap** (`max_per_creator`, default 5, normal members/bots only):
  today `WHERE group_no=? AND creator_uid=? AND status != ?` (`db.go:87-88`) →
  after, additionally `AND thread_short_id=?`, so a member's personal budget is
  also per delivery scope.

The `FOR UPDATE` lock stays on the **parent group row** (`db.go:58-63`) — only the
count dimension narrows. No schema change, no data migration.

**Follow-on (added per user request during implementation): precise control.**
Two system-settings knobs let operators tune the counts instead of one shared
value (revises decision A below):

- **`max_per_thread`** — a per-thread scope cap **decoupled** from the group-self
  `max_per_group` (so e.g. group 10 / each thread 3). Unset → falls back to
  `max_per_group`, so behavior is unchanged until configured.
- **`max_total_per_group`** — a group-wide **aggregate ceiling** across the group
  and all its threads; `0` = disabled (default). Bounds the
  `max_per_scope × (threads+1)` growth that per-scope quotas alone allow, since
  thread creation itself is uncapped (raised in review by yujiawei).

`insertWithQuota` becomes `insertWithQuota(m, quotaLimits{scope, perCreator,
total})` and evaluates all three layers inside the **same** parent-group
`FOR UPDATE` critical section, so the added aggregate dimension is race-exact
(no new lock, no lock-scope change).

## Background

- **Current quota is group-wide.** `insertWithQuota` locks the parent `group`
  row `FOR UPDATE` (serialize concurrent creates in the group; deliberately NOT a
  webhook-row lock, which regresses to an InnoDB gap-lock deadlock on the
  empty-scope first insert — see `db.go:42-46`), then counts webhooks by
  `group_no` only. Cap values come from `SystemSettings`
  (`modules/common/system_settings.go`): `IncomingWebhookMaxPerGroup()` (default
  10, `:587/:644`) and `IncomingWebhookMaxPerCreator()` (default 5, `:588/:700`),
  read DB → env → default. Exceeding either returns **409** via
  `mgmtQuotaExceeded` / `mgmtCreatorQuotaExceeded`
  (`modules/incomingwebhook/api_i18n.go:123-132`).
- **This reverses a previously locked decision.** The `incoming-webhook-thread`
  task explicitly locked: *"thread webhooks share the parent group's
  `max_per_group` … no per-thread quota and no new system_setting"*
  (`.octospec/tasks/incoming-webhook-thread/brief.md:74-77, 132-133`). Per user
  request — «按照最小单位子区进行数量限制…避免一个群聊创建很多子区的情况；尤其是上了
  octo-loop 功能后» — that decision is deliberately superseded here.
- **The plumbing already exists; only the count口径 is group-wide.** The thread
  binding work is done: `channel_type` + `thread_short_id` columns and the
  composite index `idx_incoming_webhook_thread(group_no, thread_short_id,
  status)` (migration `sql/20260624000001_incomingwebhook_thread.sql`); the
  thread-scoped create/list routes `/v1/groups/:group_no/threads/:short_id/
  incoming-webhooks` (`api.go:221`) and the bot variant
  (`bot_api/incoming_webhook.go:35`); and scope-isolated list queries
  `queryByGroupNo` (`thread_short_id=''`) vs `queryByThreadScope`
  (`db.go:119/133`). The create handler already stamps `m.ThreadShortID`
  (`api.go:788`). The scoped `COUNT(*)` lands squarely on the existing index.
- **Both faces already funnel through one gate.** User face (`api.go:212-223`)
  and bot face (`bot_api/incoming_webhook.go:27-35`) both mount the same
  `MountManagementRoutes` → `create` → `insertWithQuota`, so a single db-layer
  change covers user- and bot-created webhooks with no new mount.
- **Proposed decisions (confirm in review):**
  - **(A) Reuse `max_per_group` / `max_per_creator` values, applied per-scope; no
    new system_setting.** Group-self and each thread each get up to the existing
    10 / 5. Simplest, zero new config surface; the setting *keys* stay but their
    documented semantics shift from "per group" to "per delivery scope". A
    dedicated `max_per_thread` can be added later if per-thread tuning is ever
    needed. (Alternative considered: separate `max_per_thread` setting — more
    config surface + schema/i18n for marginal v1 benefit.)
    **Revised during implementation** (user request «群聊和子区的 WebHook 数量
    怎么精确控制»): the `max_per_thread` follow-on WAS added so group-self and each
    thread can carry different caps, plus a new `max_total_per_group` aggregate
    ceiling. `max_per_group` keeps its key but now means the *group-self scope*
    cap; `max_per_creator` still reuses one value per scope (no per-thread-creator
    knob). See Goal › Follow-on.
  - **(B) Re-scope the per-creator cap too.** Keeping it group-wide while the
    group cap goes per-scope makes the *personal* budget the new bottleneck
    (a member maxes out at 5 across all threads) — which directly re-creates the
    Octo Loop pain, so per-creator is re-scoped for口径 consistency.

## Load-bearing list

- **space / isolation / thread** — quota keying must stay anchored on the parent
  `group_no` for the `FOR UPDATE` serialization; the *count* additionally narrows
  by `thread_short_id`. A thread's membership/permissions still derive entirely
  from the parent group; nothing about auth, membership, or Space isolation
  changes. `thread_short_id` used in the count comes from the row being inserted
  (`m.ThreadShortID`), which was validated at bind time against the path
  `group_no` (`api.go:727-760`) — never free request input, so no cross-group /
  cross-Space budget leakage.
- **concurrency / correctness** — the lock MUST remain on the parent `group` row
  (`SELECT id FROM group … FOR UPDATE`, `db.go:58-63`), NOT on webhook rows.
  Locking the (possibly empty) `(group_no, thread_short_id)` webhook range would
  reintroduce the gap-lock → insert-intention deadlock the current design
  explicitly avoids (`db.go:42-46`). The口径 change is orthogonal to the lock:
  coarse lock (whole group serialized) + fine count (per scope). Soft-deleted
  rows (`statusDeleted`) still excluded so a delete frees a slot (`db.go:68`);
  disabled rows still occupy the scope's budget (anti-hoarding, `db.go:80-86`).
- **wire-contract / error-response / i18n** — the 409 quota-exceeded envelope
  stays `httperr.ResponseErrorLWithStatus` + registered codes
  `ErrIncomingWebhookQuotaExceeded` / `ErrIncomingWebhookCreatorQuotaExceeded`
  with the `{max}` param (`api_i18n.go:123-132`). Messaging should read correctly
  for a thread scope (currently the doc/comment says "per-group cap"); confirm
  whether the same code + message is reused for both group and thread scope or
  the wording is generalized. No new *handler* file is added, so
  `TestIncomingWebhookNoLegacyResponseError` needs no new entry — but any new
  errcode/message must still pass `make i18n-extract-check` + `make i18n-lint` and
  ship a zh-CN block in `pkg/i18n/locales/active.zh-CN.toml`.
- **bot-api** — the bot-token management face creates through the identical
  `insertWithQuota`; the per-scope cap must apply equally to bot-created
  webhooks. No permission-matrix change (bot must be a parent-group member; admin
  bot ⇒ admin ⇒ per-creator cap waived, same as today `api.go:809`).
- **settings semantics** — `max_per_group` / `max_per_creator` are reinterpreted
  from "per group" to "per delivery scope". Update the schema `Description`
  strings (`modules/common/system_setting_schema.go:119-122`) and the method doc
  comments (`system_settings.go:641-644, 697-700`) so ops understand the new
  granularity. The read-side `≤0 → default` clamp and DB→env→default precedence
  are unchanged.
- **backward compatibility** — existing rows carry `thread_short_id=''` and thus
  fall into the group-self bucket; their observable behavior is unchanged. No
  migration, no backfill; the existing index already serves the scoped count.
  Quota remains a *create-gate*, not a continuous constraint: lowering a cap does
  not reclaim over-budget existing rows in any scope (unchanged semantics,
  `db.go:82-86`).

## Out of scope

- **A per-creator-per-thread *separate* setting** — the per-creator cap is
  re-scoped per delivery scope (decision B) but keeps reusing `max_per_creator`;
  no distinct per-thread-creator knob. (`max_per_thread` and `max_total_per_group`
  themselves ARE delivered — see Goal › Follow-on.)
- **Per-group / per-thread quota *overrides*** (a specific group X gets cap Y) —
  all knobs stay global system-settings; no per-resource config table.
- **A cap on the *number of threads* per group** — bounding the `(threads+1)`
  factor at its source lives in the `thread` module, not here; `max_total_per_group`
  bounds the webhook total instead.
- **Changing the `FOR UPDATE` lock granularity** — it stays on the parent group
  row; only the `COUNT(*)` predicates change (the added aggregate count reuses the
  same lock).
- **Any schema / index migration or data backfill** — `channel_type`,
  `thread_short_id`, and `idx_incoming_webhook_thread` already exist; column
  defaults already classify existing rows into the group-self bucket.
- **Push hot path & delivery** — target resolution (`targetChannel`), adapters,
  rate-limiting, mention/@AI semantics, audit, and the display datasource are
  untouched; this is a management-side create gate only.
- **Lifecycle cascades** — group-disband (`disableByGroupNo`), creator-left
  lazy-disable, and group-not-Normal gates are unchanged (all still keyed on the
  parent `group_no`).
- **Retroactive quota enforcement** — no reclaiming/disabling of rows that exceed
  a newly-lowered per-scope cap (consistent with today's create-gate semantics).

## Acceptance

- **Independent buckets, group vs thread.** With `max_per_group = N`: filling the
  group-self scope to N does NOT block creating a webhook in a thread under the
  same `group_no`, and filling a thread to N does NOT block creating one on the
  group itself. Asserted with a captured 409 vs 200.
- **Independent buckets, thread vs thread.** Filling thread A's scope to N does
  NOT block creating in thread B under the same `group_no`.
- **Per-creator cap re-scoped (decision B).** A normal member who reached
  `max_per_creator` in the group-self scope can still create up to
  `max_per_creator` in a thread scope; exceeding a *scope's* per-creator cap
  returns 409 `ErrIncomingWebhookCreatorQuotaExceeded`. Admins remain exempt from
  the per-creator cap in every scope.
- **Correct scope isolation in the count.** The group-self quota count excludes
  thread rows and vice versa (a thread webhook must not consume the group's
  budget); soft-deleted rows free a slot in their own scope; disabled rows still
  occupy their scope's budget.
- **409 envelope + i18n.** Exceeding a cap returns the registered i18n
  quota-exceeded code with the `{max}` param, wording appropriate to the scope;
  `make i18n-extract-check` + `make i18n-lint` pass and any new message has its
  zh-CN counterpart.
- **Both faces enforce identically.** The user face and the bot face
  (`/v1/bot/groups/:group_no/threads/:short_id/incoming-webhooks`) both enforce
  the per-scope caps.
- **Per-thread cap decoupled (`max_per_thread`).** With `max_per_group=2`,
  `max_per_thread=1`: the group face allows 2, a thread face allows 1; unset
  `max_per_thread` falls back to `max_per_group`. Asserted at the settings and
  HTTP layers.
- **Aggregate ceiling (`max_total_per_group`).** With the ceiling set to K and
  scope caps high, creating across the group + threads stops at K total even when
  a scope still has room; a fresh empty thread scope is blocked once the group
  total hits K; `0` disables it; soft-delete frees an aggregate slot. Holds under
  concurrency (12 goroutines across group + 2 threads → exactly K succeed, group
  total == K, green under `-race`).
- **Backward compatible.** Existing group webhooks (`thread_short_id=''`) keep
  working; no schema migration is added; the `FOR UPDATE` lock is still on the
  parent group row; `max_per_thread`/`max_total_per_group` default to "same as
  group" / "disabled", so nothing changes until an operator opts in.
- **Green checks.** `go test ./modules/incomingwebhook/...` and
  `./modules/common/...` pass — **run as real integration tests against MySQL 8.0
  + Redis + WuKongIM, green under `-race`** (incl. the concurrent per-scope and
  aggregate-ceiling tests); `golangci-lint run` clean; `make i18n-extract-check` +
  `make i18n-lint` pass.
