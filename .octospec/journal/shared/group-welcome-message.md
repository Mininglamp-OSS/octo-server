---
type: Journal
title: "group-welcome-message: group new-member welcome (群入群欢迎语)"
description: Per-group admin-configured welcome posted publicly into the group channel on a member's first join — mirrors the per-Space welcome engine (config store + delivery ledger + reconciler + worker), adds a platform master switch (default off) and burst coalescing (one post per batch of joiners), with a {member} placeholder.
tags: [notify, group, onboarding, isolation, auth, acl, i18n, error-response, system-setting, migration, rate-limit, idempotency, observability, wire-contract, testing]
timestamp: 2026-07-23T14:40:00Z
---

# group-welcome-message

Branch `claude/group-welcome-message`. New feature: each group's owner/manager
(role ∈ {creator, manager}) configures **one** welcome for **their** group, and
on a human member's **first** join it is **posted publicly into the group
channel** (`channel_type=GROUP`), at most once per `(group_no, uid)`. The body
supports a `{member}` placeholder rendered to the joiner's display name.

Diverges from the per-Space welcome on purpose: Space **DMs** the newcomer
(private, per recipient); Group **posts into the channel** (public, seen by all),
and there is **no platform-global content fallback** — a group with no enabled row
gets no welcome.

Ships **double-inert**: the platform master switch defaults off AND per-group
configs default `enabled=false` — nothing happens until ops flips the switch and a
group admin opts that group in.

## What was done

1. **Mirror, don't refactor.** The freshly-shipped, at-most-once per-Space engine
   (PR #646) was copied into a parallel, group-scoped implementation rather than
   generalized into a shared "container" abstraction (which would re-open that
   reviewed surface). Only three things change: the container (`space_*` →
   `group_*`), the sender (personal DM → group-channel post), and dropping the
   global-content-fallback branch. Trivial helpers (sw* constants, the HTTP
   sender, metrics, `isRetryableTxErr`) are shared verbatim; the space code is
   otherwise untouched (bar 4 incidental polish items).
2. **Config storage (`modules/common`)** — `octo_group_welcome_config` (one row
   per group, `group_no` UNIQUE, COLLATE aligned to `group.group_no`) +
   `GroupWelcomeConfigStore` with insert-then-lock `UpsertMerged` (idempotent
   materialize → `SELECT … FOR UPDATE` → merge → UPDATE, bounded 1213/1205 retry)
   so concurrent first-create and concurrent edits both keep all fields.
   `active_from` stored as the RFC3339 string so `ParsedActiveFrom` /
   `ValidateGroupWelcomeCombination` apply as pure validators.
3. **CRUD API (`modules/group/api_welcome.go`)** — `GET/PUT/DELETE
   /v1/groups/:group_no/welcome`, gated by a new `authorizeGroupWelcomeAdmin`
   (group-active + `QueryIsGroupManagerOrCreator`, reusing existing group error
   codes — no new codes). `PUT` is a prospective merge; `DELETE` hard-deletes →
   group reverts to off. Mounted on auth + `SharedUIDRateLimiter`; all responses
   via the `httperr` i18n envelope; handler file added to the group
   `NoLegacyResponseError` guard.
4. **Delivery (`modules/notify`)** — `octo_group_welcome_delivery` ledger
   (`UNIQUE(group_no, uid)` as the at-most-once authority), event listener on
   `event.GroupMemberAdd`, reconciler (catch-up over `group_member`), and a send
   worker: cross-group `FOR UPDATE SKIP LOCKED` claim, CAS-on-`claim_owner`, 30s
   lease, rotating cursor for fairness, cross-group sweep. Posts a `MsgSendReq` to
   `channel_id=group_no`, `channel_type=GROUP`, from the fixed `notification`
   identity; `{member}` renders at send time.
5. **Platform master switch (design review add).** `system_setting
   onboarding.group_welcome_enabled` (bool, default **off**), read via
   `SystemSettings.GroupWelcomeEnabled()`, checked at enqueue + reconcile + worker
   (the cross-group sweep still runs so in-flight rows terminate). It is
   **enablement only** — no content fallback — the outer AND over each group's own
   `enabled`. Hot-reloaded; flip-off is an instant, reversible kill that touches
   no per-group rows.
6. **Burst coalescing (design review add).** A batch invite arrives as one
   `GroupMemberAdd` event → many rows; delivering one post each would spam the
   channel. Instead a fresh row is held back until `now + coalesceWindow` (via
   `next_retry_at`); the worker `claimBatch`es a group's due rows and
   `dispatchBatch` posts **one** message whose `{member}` renders to the joined
   name list (`张三、李四` / overflow → `…、nameK 等 N 人`). One coalesced post per
   group per wake also rate-limits a single group's welcomes; per-row at-most-once
   is preserved (each row keeps its own CAS transition, shares the one
   `message_id`).

## Structural learnings / gotchas

- **A master *switch* is not a global *fallback*.** The locked "no global
  fallback" decision was about **content** (body/target/default). A platform
  enablement switch is orthogonal and was the right answer to "ship it dark / kill
  it fast" — added without reintroducing any global content. Worth stating the
  distinction explicitly in the brief so the two don't get conflated.
- **`DATETIME(0)` rounds fractional seconds → a "due now" row can be ~0.5s in the
  future.** Setting `next_retry_at = now` (coalesce window 0) and immediately
  claiming with `WHERE next_retry_at <= now` returned **nothing**: MySQL rounds
  the inserted fractional value up to the next whole second, so the stored due
  time briefly exceeds the query `now`. Harmless in production (3s window + 5s
  worker cadence absorb it) but it breaks a same-instant test. Fix: drive an
  explicit clock and advance it between enqueue and claim (do not rely on wall
  time within one sub-second). Promoted to learnings/pending.
- **Coalescing lives in the worker, not the event handler.** Batching co-arriving
  joins at the ledger/claim layer (`claimBatch` → one `dispatchBatch`) keeps a
  single posting path for BOTH event-driven and reconciler-driven joins, and
  keeps per-row at-most-once intact. The event handler stays a dumb per-uid
  enqueue. `swWorkerWakeCap` is reused as a **posts**-per-wake budget for group
  (it is a **rows**-per-wake budget for space) — same constant, documented
  different unit.
- **`group_member.created_at` resets on rejoin (unlike `space_member`).** So the
  timestamp cannot be the first-join authority; the ledger `UNIQUE(group_no, uid)`
  is. Leave→rejoin keeps the SENT row → no re-post. (One acceptable edge: someone
  who joined before `active_from`, left, and rejoined after it newly qualifies.)
- **`modules/common` is the DI hub** (both `group`-write and `notify`-read import
  it; it imports neither) — the config store lives there to avoid a
  `group`↔`notify` cycle. Same pattern as the Space store.

## Verification

- Gates: `go build ./...`, `go vet`, `golangci-lint` (0 issues), `gofmt`,
  `make i18n-extract-check` + `make i18n-lint`, `git diff --check` — all clean.
- `go test -race` green for `modules/common`, `modules/notify`, `modules/group`,
  `modules/space` (each on a fresh DB; cross-package migration sets differ so the
  shared `test` DB is dropped+recreated between packages).
- New tests: config-store CRUD + `UpsertMerged` no-lost-update + concurrent-create
  + validator; CRUD authz matrix (creator/manager/member/non-member/disbanded) +
  prospective validation + partial-merge + delete; first-join at-most-once public
  post to the correct channel; `{member}` render; leave/rejoin no re-post;
  pre-`active_from` + robot/system-bot exclusion; multi-group no-crossing;
  worker coalesce + fairness; cross-group sweep; **master switch off suppresses**
  enqueue + post; **coalesced burst → one post**; name-list overflow render.
- **Live e2e** (committed, `group_welcome_e2e_test.go`, skips without a live
  WuKongIM): full pipeline against real WuKongIM — config + 2 first-join members →
  enqueue → coalesce window → batch claim → ONE public post → ledger SENT.
  Confirms the brief's open question end to end — the `notification` identity
  **posts into a group channel it is not a member of** (real `message_id`
  returned) — and proves coalescing by asserting both joiners share one
  `message_id`.

## Incidental polish (carried from #646)

`Upsert` doc caveat (prod must use `UpsertMerged`); `notify` `enabledEffectiveConfigs`
reads `r.Enabled`; Space CRUD derives DB context from the request; a Space
`DELETE`→enabled-global-fallback HTTP test.

## Review round 2 (PR #655)

Three reviewers cleared the design/security; the actionable fixes:

- **e2e skip guard was ineffective** — `config.New()` defaults `WuKongIM.APIURL`
  to `127.0.0.1:5001`, so the `APIURL==""` check never skipped and the package
  failed hard on a box without a live IM. Now probes `<APIURL>/health` (verified:
  SKIP when down, PASS when up).
- **Event fan-out** offloaded to `EventPool.Work` + config read once per batch
  (`handleMemberJoinBatch`), so a bulk invite no longer runs N sequential DB
  round-trips on the shared event-dispatch goroutine before `commit`.
- **Shutdown-safe post-send** — `casToSent`/`toUnknown` now use
  `context.WithoutCancel`, so a `Stop()` after a successful send cannot strand a
  delivered row in `dispatching`.
- **CRUD gate** tightened to require `GroupStatusNormal` (parity with the space
  welcome's `checkSpaceActive`).
- **Batch coalesce timestamp** — all members of one `GroupMemberAdd` event share
  one due time, so a slow insert can't split a sub-cap batch into two posts.
- **Master switch re-checked** before each group's claim (tighter mid-wake kill).
- A large batch of failure-branch / boundary / fairness / listener-entry tests.

**Reverted a mis-step (P1):** an earlier fix classified a non-2xx as
"definitely-not-delivered" and *retried* the coalesced post on it, to avoid a
transient IM failure suppressing a whole batch. A reviewer correctly flagged that
without a message-level idempotency key (and given the send may traverse a proxy
that can 5xx *after* WuKongIM persisted), retrying a PUBLIC post can double-post a
visible, unrecallable message. Reverted to the space engine's conservative policy:
any post-send failure → terminal `unknown`, never auto-retried. For a public
message, a missed welcome (invisible) is a safer failure than a duplicate one.

## Open items

- **Batch blast radius (known limitation, fix-before-enable).** One send failure
  marks the whole coalesced batch (≤50 winners) terminal-`unknown`; the reconciler
  only re-enqueues rows with no ledger row, so those are not auto-recovered. This
  is safe (no double-post) but suppresses the burst's welcome until an operator
  acts. Proper fix before the master switch is enabled: attach a stable
  idempotency key so WuKongIM de-dupes a retried post (needs verifying WuKongIM's
  `client_msg_no` de-dup semantics), or a bounded operator-driven recovery.
- Very large bursts (> per-post cap) spill their tail into later worker wakes (a
  few posts, not one) — natural rate-limit, `log()`-visible, acceptable.
- `{member}` renders a plain-text name; a clickable WuKongIM @mention is a
  possible fast-follow (needs the mention payload encoding).
- `swWorkerWakeCap` unit differs between space (rows/wake) and group (posts/wake);
  documented in-code. A group-specific alias is deferred as churn-for-no-gain.
