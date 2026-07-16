---
type: Task
title: "Task: space-new-user-welcome-message"
description: Reliably deliver one configurable welcome DM from the system notification bot to a human user on their first join to a designated Space (minimal implementation).
tags: ["notify", "onboarding", "space", "isolation", "i18n", "system-setting", "migration", "idempotency", "observability", "wire-contract", "error-response", "testing"]
timestamp: 2026-07-16T18:20:00+08:00
# --- octospec extension fields ---
slug: space-new-user-welcome-message
upstream: self
source: self
---

# Task: space-new-user-welcome-message

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Provide an automated welcome for a single admin-designated Space: once the
feature is active, when a human user first joins that Space, deliver one DM
from the built-in system identity `notification` (display name "通知助手")
carrying the current `space_id` and a red-dot indicator.

Scope is the **minimal implementation**: guarantee only the main chain
"first-join human → exactly one delivery ledger row → member re-check before
send" as a reliable replacement for the manual workflow. Send semantics is
**at-most-once**; outcomes that cross the WuKongIM call boundary and become
unclear are recorded as `unknown`, never auto-retried, and surfaced via
metrics + logs for operator handling.

## Background

- `SpaceMemberJoin` is written by a goroutine in a separate transaction after
  the member row commits (`modules/space/api.go:1977`); a process crash between
  the two loses the event. `api_manager.addMembers` explicitly bypasses the
  event (`modules/space/api_manager.go:609`). Therefore the event cannot be
  the sole source of truth — a persistent ledger plus periodic reconciliation
  are both required.
- `modules/notify` already provides the `notification` system bot,
  authoritative `payload.space_id` injection via `NewPersonalMsgSendReq`,
  server-returned `message_id/client_msg_no` (via octo-lib's send helper),
  and red-dot personal DM delivery (`modules/notify/api.go:437-468`). This
  task reuses the payload-construction primitive `NewPersonalMsgSendReq`
  but replaces the HTTP delivery step (see below).
- `MsgSendReq` has no `client_msg_no` input field (octo-lib
  `config/msg.go:797`); an end-to-end idempotency key does not exist, so
  **only** at-most-once is achievable. This task does not attempt to add one.
- `SystemSettings` uses an in-memory snapshot with a 60s background reload
  (`modules/common/system_settings.go:72`). The minimal implementation accepts
  this convergence window; an admin write triggers `Reload()` **on the replica
  handling that write**, and peer replicas converge within at most 60s.
- octo-lib's `SendMessageWithResult` neither accepts a `context.Context`
  nor sets an HTTP client timeout — the underlying sendgrid `rest.API`
  (`pkg/network/network.go`) uses default timeouts (i.e. none). Wrapping
  the call in `context.WithTimeout` at the caller cannot actually interrupt
  the request; the caller can only return early while the goroutine (and
  socket) linger, and at-most-once accounting cannot rely on the timeout.
- **The task must not modify octo-lib.** The following are already exported
  and sufficient for a self-contained context-aware sender inside
  `modules/notify`:
  - `config.Context.GetConfig() *config.Config` (`config/context.go:77`)
  - `config.Config.WuKongIM.APIURL` (`config/config.go:156`)
  - `config.Config.WuKongIMManagerTokenHeaderMap()` (`config/msg.go`,
    explicitly labeled "公共方法供外部模块使用")
  - `config.NewPersonalMsgSendReq(...)` (payload construction, unchanged)
  Response body shape (`data.message_id`, `data.client_msg_no`,
  `data.message_seq`) is stable — this task mirrors the parse in
  `config/msg.go:152-163`.
- `modules/notify/1module.go` today does **not** register a `SQLDir`. This
  task must add `//go:embed sql` and `SQLDir: register.NewSQLFS(sqlFS)`
  following the pattern used in every other module (e.g. `app_bot`,
  `backup`, `botfather`); without it, the new migration will not execute.
- BotFather's Space welcome and `u_10000`'s register/login welcome serve
  different purposes; they are left untouched and may coexist with this feature.

## Load-bearing list

- **Product trigger semantics** — Deliver only when a `space_member` row
  satisfies `space_id = target`, `status = 1`, and `created_at >= active_from`,
  and only for the **first** such membership. Pre-existing members and
  rejoiners whose original join predated `active_from` are excluded.
- **Sender identity and wire protocol** — Fixed
  `from_uid = notification` / `channel_type = PERSON` / `red_dot = 1` /
  `payload.type = Text`; `payload.space_id` is authoritatively overwritten by
  `NewPersonalMsgSendReq`. Neither admin nor caller may choose or forge the
  sender or submit arbitrary payload. The message body is plain text — no
  placeholder rendering, no HTML, no card variants.
- **Space isolation** — Config must match exactly one existing, non-dissolved
  Space; both enqueue and pre-send re-check membership by
  `(space_id, uid, status=1)`. Any cache or lookup anomaly fails closed; the
  code never delivers cross-Space or to non-members. Pre-send membership
  check must not read a stale cache; if `modules/space` does not expose a
  cache-bypassing accessor, the implementation performs a direct DB read
  (documented on the caller in `modules/notify`).
- **Human filter** — Both enqueue and pre-send exclude `user.robot = 1`,
  `pkg/space.SystemBots`, and orphan `space_member` rows whose `user` row is
  missing.
  - **`SystemBots` contains `notification`** (`pkg/space/query.go:20-25`).
    Any call to `pkg/space.IsSystemBot` / `SystemBots[uid]` in this feature
    must be scoped strictly to *"is this recipient uid ineligible"*.
    Send-path code, sender-identity resolution, and any generic
    early-return `if IsSystemBot(uid) { return }` template **must not** touch
    the sender uid, or the feature will self-exclude.
- **First-join semantics** — `space_member.created_at` is the permanent
  anchor for a user's first-ever membership in this Space and is not reset
  on re-join. Verified in code:
  - `executeJoinSpace` → `atomicReactivateMemberIfNotFull` updates only
    `status/role/updated_at` (`modules/space/db.go:562-565`), leaving
    `created_at` untouched.
  - Manager force-add uses `INSERT ... ON DUPLICATE KEY UPDATE status=1,
    updated_at=NOW()` (`modules/space/db_manager.go:381`); the update clause
    excludes `created_at`.
  Therefore `space_member.created_at >= active_from` correctly excludes
  members who first joined before the feature went live even if they left
  and rejoined afterwards. The reconciler and event handler both key off
  this column; no auxiliary "current-active-period-start" is required.
- **Runtime configuration** (registered under `system_setting`, typed getters)
  - `onboarding.space_welcome_enabled` (bool, default false)
  - `onboarding.space_welcome_space_id` (string)
  - `onboarding.space_welcome_active_from` (UTC RFC3339 string)
  - `onboarding.space_welcome_message_zh_cn` (string, trimmed non-empty,
    ≤ 2000 Unicode code points)
  - `onboarding.space_welcome_message_en_us` (same rule)
  - **Snapshot accessor** — `modules/common` must expose a single
    `SpaceWelcomeConfig()` returning a struct with all five fields read from
    the **same** `SystemSettings` snapshot in one atomic access. Callers
    (event handler, worker, reconciler, manager write path) must not read
    the five keys individually; splitting the reads risks straddling a
    background `Reload()` and using an inconsistent combination.
- **Configuration validation** — When enabling, all five keys must form a
  **valid combination**: Space exists and is active, time parses, both message
  bodies non-empty within the length limit.
  - **Manager write path uses prospective validation** — the write endpoint
    must NOT validate the current `SpaceWelcomeConfig()` snapshot; it must
    construct `prospective = merge(current snapshot, incoming items)` and
    validate that composite. Rationale: partial updates (e.g. changing only
    `space_id` while leaving message bodies unchanged) must pass if the
    resulting five-tuple is valid, and must fail if any incoming field
    breaks the composite — using the pre-write snapshot alone gets both
    directions wrong. Validate first, then commit the items in one
    transaction, then trigger `SystemSettings.Reload()` on the handling
    replica.
  - **Runtime re-validation** — Worker/reconciler re-validate
    `SpaceWelcomeConfig()` **before each cycle**; any failure fails closed
    for that cycle and increments `config_invalid_total`. Fail-closed
    applies from the next cycle after `SystemSettings` reloads the invalid
    combination — cycles still running on a valid earlier snapshot are not
    aborted mid-flight.
- **Delivery ledger table** — New migration
  `modules/notify/sql/<yyyyMMdd>-01_add_octo_space_welcome_delivery.sql`
  creating `octo_space_welcome_delivery`:
  ```
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  space_id VARCHAR(<match space.space_id>) NOT NULL,   -- length+COLLATE must match `space`
  uid      VARCHAR(<match user.uid>)      NOT NULL,   -- length+COLLATE must match `user`
  status TINYINT NOT NULL DEFAULT 0,
  -- 0=pending    : claimable by worker
  -- 1=claimed    : worker locked the row; IM call not yet started; sweep => back to pending
  -- 2=dispatching: worker has begun the IM call; sweep => unknown (transport-ambiguous)
  -- 3=sent       : terminal success
  -- 4=failed     : terminal (pre-IM retry budget exhausted)
  -- 5=skipped    : terminal (recipient ineligible at pre-send check: left / bot / orphan)
  -- 6=unknown    : terminal (transport-ambiguous; operator handles)
  attempts INT NOT NULL DEFAULT 0,
  next_retry_at DATETIME NULL COMMENT 'UTC; application-level UTC values, never NOW()',
  lang VARCHAR(16) NULL,                  -- written at send time
  message_id BIGINT NULL,
  client_msg_no VARCHAR(100) NULL,        -- match existing message table width
  claim_owner VARCHAR(128) NULL,          -- <hostname>:<pid>, K8s pod names can approach 63 chars
  claim_expire_at DATETIME NULL COMMENT 'UTC; application-level UTC values',
  error_class VARCHAR(64) NULL,
  created_at DATETIME NOT NULL COMMENT 'UTC; application-level UTC values',
  updated_at DATETIME NOT NULL COMMENT 'UTC; application-level UTC values',
  UNIQUE KEY uk_space_uid (space_id, uid),
  KEY idx_claim (space_id, status, next_retry_at)
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=<match reconciler-join partners>
    COMMENT='onboarding space welcome delivery ledger';
  ```
  - `space_id` / `uid` column type, length, and `COLLATE` must be identical to
    the columns joined by the reconciler (`space`, `space_member`, `user`);
    mismatched collations trigger MySQL error 1267 on JOIN (see prior incident
    `issue_344_display_name_fallback`).
  - `idx_claim (space_id, status, next_retry_at)` is shaped to match the
    worker's claim `WHERE` predicate exactly; leaving `space_id` out forces a
    range scan.
  - **Time discipline** — all timestamps are UTC. The application computes
    `time.Now().UTC()` and passes it as a bound parameter for every write;
    SQL must **not** use `NOW()` for this table (repository convention writes
    naked `NOW()`, but the DB session TZ is not pinned in the DSN, so `NOW()`
    would silently mix wall-clock with the UTC RFC3339 `active_from`).
    `active_from` from `system_setting` is parsed as RFC3339 UTC.
  - Retention: **no auto-archival**. The ledger is the permanent dedupe
    source-of-truth and grows monotonically; operators do not truncate it.
- **Module wiring** — `modules/notify/1module.go` must add
  `//go:embed sql` (with `import "embed"`) and register `SQLDir:
  register.NewSQLFS(sqlFS)` on the module. Follow `modules/app_bot/1module.go`
  as a reference.
- **Enqueue idempotency** — Both the event path and the reconciler use
  `INSERT ... ON DUPLICATE KEY UPDATE id = id` (never `INSERT IGNORE`, which
  silently swallows non-uniqueness errors such as charset truncation).
  `enqueue_total{source}` is incremented **only when affected rows > 0**;
  duplicate hits increment `enqueue_dedup_total{source}` instead.
- **Low-latency event path** — Extend `notify.handleSpaceMemberEvent`
  (`modules/notify/api.go:213`); when the event matches
  `SpaceWelcomeConfig()`, upsert a pending row. The listener does not send
  messages; enqueue failure must never roll back or block the completed join.
- **Reconciliation path** — Per-process background goroutine (interval 60s,
  compile-time constant), started from `modules/notify/1module.go`'s
  `Start()` hook and stopped on `context.Cancel()` for clean test shutdown.
  Each cycle: read `SpaceWelcomeConfig()` (skip if disabled/invalid) → scan
  the current target Space for `status=1 AND created_at >= active_from`
  members that lack a ledger row → cap at 500 rows per cycle → upsert
  pending rows. This path covers dropped events, restarts, and `addMembers`.
- **Send worker (in-process)** — Per-process background goroutine, same
  lifecycle hooks. **Drive loop**:
  - **Idle → sleep, active → drain.** When a cycle produces a claimed row,
    immediately try the next claim without waiting; when a cycle produces no
    claimable row, sleep 5s before the next attempt. This lets a small
    backlog drain quickly while keeping steady-state polling at 5s.
  - **Per-cycle safety cap: 20 rows.** After 20 successfully claimed rows in
    one wake, break out to the sleep branch so a single goroutine cannot
    monopolize the shared DB session or starve other work. The cap is a
    safety valve, not a rate limit — the next wake resumes immediately.
  - **Steady-state upper bound (single replica): 20 rows / (20 × per-row
    latency + 5s idle), i.e. under nominal per-row latency ≲1s roughly
    240 rows/min in burst; empty queue steady state is one poll per 5s.
    Multiple replicas scale linearly via row-lock contention.** This bound
    is descriptive, not a product SLA — see Open questions.
  1. Read `SpaceWelcomeConfig()`; skip (fall through to sleep) if
     disabled/invalid.
  2. **Sweep** (once per wake, not per row):
     - `claimed` rows past `claim_expire_at` → back to `pending`
       (`next_retry_at = NOW_UTC()`; safe because IM was not yet called;
       `attempts` **is not decremented and was not incremented at claim** —
       see below).
     - `dispatching` rows past `claim_expire_at` → `unknown` with
       `error_class='claim_expired'` (we cannot prove whether IM was called).
  3. **Claim** — one row per iteration, two-phase inside one transaction:
     ```sql
     SELECT id FROM octo_space_welcome_delivery
       WHERE space_id=? AND status=0
         AND (next_retry_at IS NULL OR next_retry_at<=?)   -- ? = NOW_UTC
       ORDER BY id LIMIT 1
       FOR UPDATE SKIP LOCKED;
     UPDATE octo_space_welcome_delivery
       SET status=1,
           claim_owner=?, claim_expire_at=?,               -- lease = NOW_UTC + 30s
           updated_at=?
       WHERE id=?;
     ```
     `claim_owner=<hostname>:<pid>` re-selects the row and is CAS predicate
     on every subsequent update. **Note**: `attempts` is deliberately NOT
     incremented at claim. `attempts` counts only **consecutive pre-IM
     failures** (see Dispatch below); claim, precheck-skip, sweep-recycle,
     `sent`, and `unknown` never consume the retry budget. Otherwise a
     process crash between claim and IM call would burn attempts without any
     failure signal, which contradicts the semantic.
     - No row returned → break out to the sleep branch (idle).
     - Row returned → run Dispatch, then continue the loop (drain).
  4. **Dispatch** — the claimed row goes through:
     - **Pre-send re-check** (bypassing stale caches, executed under a bounded
       DB context — see DB timeouts below): recipient still member of target
       Space, not a robot / system bot (`SystemBots` **for recipient only** —
       see Human filter), `user` row exists.
       - Not eligible → CAS `WHERE id=? AND status=1 AND claim_owner=?` to
         `status=5 skipped` with `error_class=member_left | human_filter |
         orphan_member`. Increments `skip_non_member_total`. No further
         cycles will touch this row — this replaces the earlier
         "keep pending forever" loop and avoids the hot-loop on users who
         never rejoin. **`attempts` is not consumed.**
     - Resolve language via `user.LanguageService` (`zh-*` → zh-CN, otherwise
       en-US); on **any** lookup failure fall back to `OCTO_DEFAULT_LANGUAGE`
       and continue — language lookup is never treated as a retryable error.
     - CAS transition to `status=2 dispatching`, extend
       `claim_expire_at = NOW_UTC + 30s` and persist `lang`.
     - Build the payload via `config.NewPersonalMsgSendReq(...)` (retained
       from octo-lib for authoritative `payload.space_id` and content
       shaping).
     - Deliver via a **notify-local context-aware HTTP sender** — do **not**
       call `SendMessageWithResult`. The sender:
       - reads `APIURL = ctx.GetConfig().WuKongIM.APIURL` and
         `authHeader = ctx.GetConfig().WuKongIMManagerTokenHeaderMap()`
         (this helper returns only `{"token": ...}`);
       - builds `http.NewRequestWithContext(ctx, POST, APIURL+"/message/send",
         body)`; **sets `Content-Type: application/json` explicitly** in
         addition to `authHeader`; posts through a shared `http.Client` with
         `Timeout: 15s` (this timeout is authoritative because the socket is
         actually closed on expiry — unlike the octo-lib helper);
       - on 200 OK parses `data.message_id` / `data.client_msg_no` /
         `data.message_seq` (mirrors octo-lib `config/msg.go:152-163`);
       - on non-200 or transport error returns a typed result the worker can
         classify below.
       This sender lives in `modules/notify` only. octo-lib is **not**
       modified.
     - **Success** → CAS to `status=3 sent`, persist
       `message_id/client_msg_no`. `attempts` unchanged.
     - **Definitively pre-IM failure** observed before the `dispatching`
       transition (bot not ready, config unreadable, precheck DB error, lang
       service DB error not caught by the local fallback) → CAS back to
       `status=0 pending` with `attempts=attempts+1` and
       `next_retry_at = NOW_UTC + backoff(new_attempts)`,
       `backoff = {5s, 30s, 120s}` for attempts 1/2/3. **`attempts` is
       incremented here — this is the only place it grows.** After the 4th
       consecutive pre-IM failure, CAS to `status=4 failed` with
       `error_class`.
     - **Any failure after the `dispatching` transition** — IM timeout,
       connection reset, empty response, non-200, or the post-success ledger
       update failing — CAS to `status=6 unknown`; never auto-retry.
       `attempts` unchanged.
  - **DB call context deadlines.** Every DB call issued by the worker
    (sweep, claim, precheck, CAS transitions, ledger persist) runs under a
    bounded context — recommended 3s per call — so the "precheck + CAS
    overhead ≲ 5s < 30s lease" budget is enforced rather than assumed. A DB
    timeout on precheck is a definitive pre-IM failure (attempts+1 + backoff);
    a DB timeout on the post-success persist is transport-ambiguous → mark
    `unknown`.
  - Every UPDATE past claim includes `AND status=<expected> AND
    claim_owner=<self>` to prevent a lease-expired sweep on another replica
    from being overwritten by this replica.
  - Multiple replicas race safely via `SELECT ... FOR UPDATE SKIP LOCKED`;
    no leader election, no distributed lock.
- **Disable / re-enable** — When disabled: new events are not enqueued, the
  reconciler skips, and the worker skips claiming and dispatch (sweep still
  runs to promote any stale rows to their terminal state). The replica
  handling the admin flip takes effect immediately via
  `SystemSettings.Reload()`; peer replicas converge within at most one
  `SystemSettings` reload cycle (currently 60s). In-flight rows in
  `dispatching` cannot be physically recalled (the HTTP request is already
  on the wire) and are allowed to complete. Re-enabling with the same
  `active_from` lets the reconciler catch up members who joined during the
  pause; advance `active_from` if that catch-up is undesired.
- **Space switch** — Worker and reconciler always operate against the
  current configured `space_id`. Leftover pending rows for the previous
  Space are filtered out by the SQL `space_id = current` clause and are not
  garbage collected; operators clean them up manually via SQL as needed.
- **Observability** (minimal metric set)
  - **counters**: `enqueue_total{source=event|reconciler}`,
    `enqueue_dedup_total{source=event|reconciler}`, `send_success_total`,
    `send_failed_total`, `send_unknown_total`, `skip_non_member_total`,
    `config_invalid_total`, `sweep_reclaimed_total{from=claimed|dispatching}`
  - **gauges**: `pending_backlog`, `unknown_backlog`,
    `oldest_pending_age_seconds` (implementation may sample; large-scale
    accuracy is not required for the minimal version)
  - **stage** (structured log field) enumeration: `enqueue`, `sweep`,
    `claim`, `precheck`, `dispatch`, `persist`
  - **error_class** enumeration: `bot_not_ready`, `member_left`,
    `human_filter`, `orphan_member`, `config_read`, `im_timeout`,
    `im_bad_response`, `sent_persist`, `claim_expired`
  - Structured log fields per event: `delivery_id`, `space_id`, `uid`,
    `stage`, `error_class`. **Never** log the full welcome body, sensitive
    user data, or raw upstream error strings.
- **Operator handling channel (minimal)** — `unknown` and `failed` rows are
  handled via Grafana alerts on the above metrics plus direct SQL access.
  This task **does not** introduce a management API or CLI.
- **Test-infra caveats** — Integration tests must reset any residual
  `ratelimit:uid:*` buckets in setup. i18n error branches must inject an
  `ErrorRenderer` in the test server. If the test DB reports an unknown
  migration, drop and recreate the DB rather than diff migrations.
- **Error responses and permissions** — No new public write endpoints. Any
  new config-validation error codes go through the `pkg/httperr` i18n
  envelope; raw `ResponseError` / `c.JSON` / `AbortWithStatusJSON` non-OK
  responses are forbidden.
- **Compatibility with existing behavior** — `/v1/internal/notify`, card
  notifications, BotFather Space welcome, and the `u_10000` register/login
  welcome remain unchanged. Target users may see these DMs alongside the new
  Space welcome — this is a pre-existing behavior of those pipelines, not a
  change introduced by this task.

## Out of scope

- Multi-Space configuration with per-Space copy; per-Space admin self-service
  UI/API.
- Retroactive bulk send to members who joined before `active_from`.
- Rich text, cards, attachments, placeholder rendering (e.g. `{nickname}`),
  scheduled send, audience targeting, workflow orchestration.
- Modifying or replacing the `u_10000` register/login welcome or BotFather
  Space welcome pipelines and templates.
- Changes to client message protocol, conversation list, or red-dot rules;
  separate triggering logic per Web/iOS/Android client.
- Modifying WuKongIM or octo-lib to add a client-supplied idempotency key or
  a default HTTP timeout; end-to-end exactly-once is a separate task.
- Auto-retrying `unknown` rows; auto-recalling possible duplicate messages;
  guessing the outcome of an ambiguous IM call.
- Bounded concurrent dispatch within one claim batch, multi-row batches, or
  a worker pool inside a single replica (single-row-per-cycle is deliberate).
- Modifying octo-lib (add `SendMessageWithContext`, add HTTP client timeout,
  etc.). A tracking follow-up may be filed against octo-lib separately, but
  this task is self-contained on the octo-server side.
- Re-attempting delivery after a `skipped` terminal state (users who leave
  before send do not get a later welcome even if they rejoin).
- General outbox / task queue / workflow platform.
- Cross-replica leader election, distributed lock, or introducing
  Kafka/Redis-backed queues.
- Management endpoints for pending listing, manual resend, or cleanup —
  handled via SQL.
- Ledger archival / TTL / purge; the table grows monotonically by design.
- Production repo assignment, image SHA, K8s manifests, and final production
  configuration values (owned by the release pipeline).

## Acceptance

- **Config validation**: with `enabled=true`, writes with a non-existent
  Space, unparseable time, empty (after trim) or > 2000 code point message
  bodies are rejected at the manager write path. Dirty configs written
  directly to the DB fail closed at the worker/reconciler and increment
  `config_invalid_total`.
- **Prospective validation**: a partial update touching only a subset of
  the five keys is accepted iff `merge(current snapshot, incoming items)`
  is a valid combination. A test where the current snapshot is valid, the
  incoming patch alone would look valid, but the merge is invalid, is
  rejected; and vice versa (invalid snapshot + valid patch that repairs
  the composite is accepted).
- **Snapshot atomicity**: worker/reconciler/event handler read
  `SpaceWelcomeConfig()` once per cycle; a mid-cycle `SystemSettings.Reload()`
  that produces an invalid combination fails closed only from the next cycle,
  never mid-cycle.
- **Disable / re-enable**: flipping to `enabled=false` immediately stops the
  admin-request replica from claiming; peer replicas stop within 60s;
  in-flight `dispatching` rows are allowed to complete; no new pending is
  created and no reconciler catch-up happens. Flipping back to `enabled=true`
  with the same `active_from` results in the next reconciler cycle enqueueing
  members who joined during the pause.
- **Unique delivery**: a human user's first join to the target Space produces
  exactly one `(space_id, uid)` row; duplicate/concurrent/replayed
  `SpaceMemberJoin` events neither insert new rows nor re-send.
  `enqueue_total` counts only actual insertions; duplicates raise
  `enqueue_dedup_total` instead.
- **Exclusion set**: non-target Spaces, pre-`active_from` members, robots,
  system bots (recipient only), orphan `space_member` rows, and users who
  left and did not rejoin as active members receive no message.
- **Entry-point coverage**: normal invite, approval, and Space-admin add flow
  through the low-latency event path; `addMembers` and any "row committed but
  event missing" scenarios are covered by the next reconciler cycle.
- **Restart recovery**: simulating a crash between member commit and event
  write and then restarting produces a delivery via the reconciler within at
  most one reconciler cycle (60s). Recovery is not sub-second; this is the
  chosen reconciler interval, not a product trade-off.
- **Migration executes**: after adding `//go:embed sql` + `SQLDir` to
  `modules/notify/1module.go`, a fresh test DB brings `octo_space_welcome_
  delivery` online without operator intervention.
- **Concurrent claim**: two replicas racing on the same pending row —
  `SELECT ... FOR UPDATE SKIP LOCKED` + `UPDATE` ensures the row is claimed
  by exactly one replica.
- **Drive loop**: idle poll interval is 5s; on any successful claim the
  worker immediately loops back to claim the next row without waiting; a
  per-wake safety cap of 20 rows forces a fall-through to the sleep branch.
  No throughput SLA is asserted; see **Open questions** for the required
  peak-rate confirmation.
- **`attempts` semantics**: `attempts` is incremented **only** on a
  definitive pre-IM failure that transitions the row back to `pending`; it
  is not touched by claim, sweep, precheck-skip, `sent`, or `unknown`. A
  crash-and-recover path never burns the retry budget.
- **State-machine transitions**:
  - Crash after `claimed`, before `dispatching` → sweep returns the row to
    `pending`; subsequent claim proceeds normally without duplicating a
    real send.
  - Crash after `dispatching`, before terminal → sweep transitions the row
    to `unknown`; no auto re-send.
  - Every UPDATE past claim uses `WHERE id=? AND status=<expected> AND
    claim_owner=<self>`; a stale replica cannot overwrite the current owner.
- **User leaves before send**: while a row is pending, if the user leaves
  the target Space, the pre-send check transitions the row to
  `status=5 skipped` with `error_class=member_left`; no message is sent and
  the row is not revisited even if the user rejoins later.
- **Language selection**: `zh-*` users receive the zh copy, other supported
  users the en copy; on any lookup failure/unset the code falls back to
  `OCTO_DEFAULT_LANGUAGE` and proceeds (no retry). The chosen language is
  persisted to `lang`.
- **Send protocol**: `from_uid=notification`, `channel_type=PERSON`,
  `red_dot=1`, `payload.type=Text`, `payload.space_id = current configured
  space_id`.
- **IM timeout enforcement**: the notify-local sender uses
  `http.NewRequestWithContext` + `http.Client{Timeout: 15s}`; timeouts
  actually close the socket and unblock the caller (unlike a
  `context.WithTimeout` wrapper around octo-lib's `SendMessageWithResult`,
  which cannot interrupt the underlying request). 15s < 30s dispatch lease
  guarantees a hung request cannot outlive the lease.
- **HTTP request headers**: the notify-local sender explicitly sets
  `Content-Type: application/json` in addition to the `token` header
  returned by `WuKongIMManagerTokenHeaderMap()`; a test asserts both are
  present on outbound requests.
- **DB call context deadlines**: every DB call inside the worker (sweep,
  claim, precheck, CAS transitions, ledger persist) is issued under a
  bounded context (≤ 3s per call); a slow-query fault-injection test
  confirms the worker fails the row through the pre-IM / unknown paths
  rather than dangling.
- **No octo-lib change**: the diff contains no modification under
  `vendor/github.com/Mininglamp-OSS/octo-lib` (or the module path
  equivalent); `go.mod` version of octo-lib is unchanged.
- **Success persistence**: on a 200-OK response from the notify-local
  sender the row records `message_id` / `client_msg_no` (parsed from the IM
  response body, same shape as octo-lib parses) and moves to `status=3`;
  no subsequent event/reconciler/restart triggers another send.
- **Bounded retry**: pre-IM definitive failures back off `{5s, 30s, 120s}`
  and after the **4th consecutive pre-IM failure** move to `failed`.
- **Transport-ambiguous**: IM timeout / connection reset / empty response /
  post-success ledger update failure all record `status=6 unknown`; neither
  the worker nor the reconciler retries.
- **Space switch**: after the config changes to a different `space_id`, the
  worker and reconciler operate only on the new Space; the previous Space's
  pending rows remain in the table (operator-cleaned via SQL) but are never
  claimed again.
- **Schema alignment**: `space_id` and `uid` columns match the length,
  character set, and collation of the columns they join against; a JOIN
  smoke test confirms no MySQL 1267 error.
- **Time discipline**: no SQL statement in this feature uses naked `NOW()`;
  all timestamps are application-supplied UTC values.
- **Backward compatibility**: existing tests in `modules/notify`,
  `modules/space`, `modules/common`, and bot provisioning continue to pass;
  no wire response changes.
- **This task's tests cover at minimum**: config combination validation and
  `SpaceWelcomeConfig()` snapshot atomicity; ledger unique constraint under
  concurrent upsert; event enqueue; reconciler catch-up (including the
  `addMembers` path); `active_from` time boundary; human filter (with
  `notification` as sender not filtered); user leaves before send → row
  becomes `skipped` and is not revisited even after rejoin; language
  fallback (including lookup failure fallback without retry); disable
  (admin replica immediate / peer convergence via reload); success
  persistence; `unknown` non-retry; sweep of `claimed` back to `pending`;
  sweep of `dispatching` to `unknown`; CAS predicate blocking cross-owner
  overwrite; concurrent `SELECT ... FOR UPDATE SKIP LOCKED` claim.
- **Command-line verification**:
  - `go test -race ./modules/notify/...`
  - `go test -race ./modules/common/...`
  - `go test -race ./modules/space/...`
  - When touching error codes / i18n: `make i18n-extract-check && make i18n-lint`
  - `go test -race ./...`
  - `git diff --check`
- **Rollout**: land migration + deploy with `enabled=false` → write target
  `space_id`, UTC `active_from`, and zh/en message bodies → verify with two
  fresh test accounts that the DM, red dot, copy, ledger row (`status=3`),
  and metrics are correct → flip the switch. On incident, flip the switch
  off first; retain the ledger and migration for audit and recovery.

## Open questions (product / operations sign-off required before enabling)

These are product/business decisions, not physical constraints or existing
system facts. Enabling the feature in production requires explicit sign-off
on each. Code can land and be deployed with `enabled=false` without
sign-off; flipping `enabled=true` in production is blocked until each is
recorded (e.g. in the octospec journal for this task).

1. **At-most-once behavior.** In transport-ambiguous cases (IM 15s timeout,
   connection reset, non-200, post-success ledger update failure), the row
   is marked `unknown` and never auto-retried; the user may or may not have
   received the message, with no side channel to reconstruct which. →
   Confirm a small ongoing `unknown_backlog` handled by SQL + Grafana is
   acceptable, and that occasional silent missed welcomes are preferred
   over occasional duplicates.
2. **`skipped` is terminal.** A user who leaves the target Space before the
   worker sends is marked `skipped` and will not receive the welcome even
   if they rejoin later. → Confirm this matches product intent, or change
   the design to keep such rows revisitable (out of the current minimal
   scope).
3. **Throughput ceiling vs peak join rate.** A single replica drains a
   fresh backlog at roughly `20 rows / (20 × per-row latency + 5s)` —
   under nominal ≲ 1s per-row latency, ~240 rows/min in burst; steady
   state polls every 5s when idle. Multiple replicas scale linearly. →
   Confirm the expected peak-join rate for the target Space fits within
   this envelope, backed by a real estimate rather than assumed.

Existing system facts, documented here for completeness but **not**
open questions:

- Target users may receive DMs from BotFather's Space welcome and/or
  `u_10000`'s register/login welcome in addition to this feature's DM.
  Those pipelines predate this task and are unchanged; this task does not
  need product sign-off to leave them alone. If the product later wants to
  sequence or suppress them, file a separate task.
- Disable takes effect immediately on the admin-request replica and within
  ≤ 60s on peers, bounded by the shared `SystemSettings` reload TTL. Lost
  events are picked up within ≤ 60s by the reconciler. These are
  properties of `system_setting` and the chosen 60s reconciler interval,
  not one-off product trade-offs. Every other `system_setting`-backed
  feature on this server operates under the same reload window.
