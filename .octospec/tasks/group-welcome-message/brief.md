---
type: Task
title: "Task: group-welcome-message"
description: Add a per-group new-member welcome message — each group's owner/admin configures one welcome that is posted publicly into the group channel on a member's first join; mirrors the per-Space welcome infra but delivers to the group channel (not a DM) with no global fallback.
tags: ["notify", "onboarding", "group", "isolation", "auth", "acl", "i18n", "error-response", "wire-contract", "migration", "rate-limit", "idempotency", "observability", "testing", "commit", "git"]
timestamp: 2026-07-23T12:40:00+08:00
# --- octospec extension fields ---
slug: group-welcome-message
upstream: self
source: self
---

# Task: group-welcome-message

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add a **group new-member welcome message (群入群欢迎语)**: each group's owner/admin
manages **one** welcome config for **their** group, and when a human member joins
that group **for the first time**, one welcome message is **posted publicly into
the group channel** (visible to everyone), at most once per member.

Locked product decisions (design review, this task):

1. **Delivery = public group-channel post**, NOT a personal DM. The message is
   sent to `channel_id = group_no`, `channel_type = GROUP (2)` — the whole group
   sees it. (This is the key divergence from the per-Space welcome, which DMs the
   newcomer via `channel_type = PERSON`.)
2. **Config = one row per group, self-service by that group's owner/admin**
   (`IsCreatorOrManager`); **no platform-global fallback** (unlike the Space
   feature). A group with no enabled config → no welcome.
3. **Trigger = first-time join only.** `active_from` cutoff + an at-most-once
   delivery ledger keyed `(group_no, uid)` is the dedup authority, so leaving and
   rejoining does **not** re-post for anyone already welcomed. (Note: unlike
   `space_member`, `group_member.created_at` resets on rejoin — see Background;
   the ledger, not the timestamp, guarantees at-most-once.)
4. **Body may name the joining member.** The configured message supports a
   **placeholder** that renders to the new member's display name / @mention at
   send time (公开欢迎语点名新成员) — the personalization the Space version
   deliberately omitted. This also makes the per-member post non-repetitive.
5. **Platform master switch, default OFF (dark launch).** A single
   `system_setting onboarding.group_welcome_enabled` (bool) gates the whole
   feature: the event path enqueues nothing and the worker posts nothing while it
   is off. It is **enablement only** — it does NOT introduce any platform-global
   *content* fallback (decision #2 stands; a group's body always comes from its own
   row). Flipping it back to off is an instant kill switch (no per-group rows
   touched, converges across replicas within the settings snapshot TTL). This is
   the outer AND over each group's own `enabled`.
6. **Coalesce a burst of joins into ONE post.** A batch invite arrives as a single
   `GroupMemberAdd` event → many `(group_no, uid)` rows at once; delivering one
   public post per joiner would spam the channel. Instead the worker claims a
   group's due rows as a batch and posts **one** message naming everyone
   (`{member}` → the joined name list; a large batch collapses the tail to
   `… 等 N 人`). At-most-once per `(group_no, uid)` is preserved (each row still
   transitions independently); a burst larger than the per-post cap spills its tail
   into later worker wakes (a few posts, not N).

Ships with the master switch **off** and per-group configs defaulting to
`enabled=false` — double-inert on deploy: nothing happens until an operator turns
on the platform switch AND a group admin opts that group in.

**Architecture: mirror, don't refactor.** The per-Space welcome delivery engine
(`modules/notify/space_welcome*.go` + `octo_space_welcome_delivery` ledger) is
freshly shipped (PR #646) and delicate (at-most-once). Rather than generalize it
into a shared "container" abstraction — a large refactor that re-opens that
reviewed surface — this task stands up a **parallel, group-scoped** copy of the
same proven shape (config store + delivery ledger + reconciler + send worker +
rotating cursor + CAS/at-most-once), swapping only (a) the container (`space_*` →
`group_*`), (b) the sender (personal DM → group-channel post), and (c) dropping
the global-fallback branch. Trivial pure helpers may be shared; the space code is
otherwise untouched. (Generalizing both onto one engine is a possible future
refactor — explicitly out of scope here.)

## Background

- **Prior art (the pattern this copies).** The per-Space new-member welcome:
  - Delivery machinery: `octo_space_welcome_delivery` ledger
    (`UNIQUE(space_id,uid)`), reconciler, send worker, notify-local HTTP sender,
    rotating per-replica cursor, `FOR UPDATE SKIP LOCKED` claim + CAS-on-
    `claim_owner`, at-most-once (`modules/notify/space_welcome*.go`).
  - Per-container config: `octo_space_welcome_config` +
    `common.SpaceWelcomeConfigStore` (`UpsertMerged` insert-then-lock),
    `GET/PUT/DELETE /v1/space/:space_id/welcome`
    (`modules/space/api_welcome.go`). Briefs:
    `.octospec/tasks/space-welcome-per-space-admin-crud/brief.md` +
    `space-new-user-welcome-message`.
  - Key difference to internalize: Space **DMs** the newcomer (private, per
    recipient); Group **posts into the channel** (public, seen by all). The
    ledger `(container, uid)` dedup shape is identical (one welcome per member's
    first join); the *send target* is the only delivery change.

- **Group model anchors (verified by recon).**
  - **Tables:** `group` (`group_no VARCHAR(40)` = the channel id, `status smallint`,
    `creator`) and `group_member` (`group_no`, `uid`, `role smallint`, `status`,
    `is_deleted`, `robot`, `created_at`, UNIQUE `(group_no, uid)`) —
    `modules/group/sql/20191106000002_group_legacy01.sql`; models
    `modules/group/db.go:648-717`.
  - **Roles** (`modules/group/const.go:19-26`): `MemberRoleCommon=0`,
    `MemberRoleCreator=1` (owner/creator), `MemberRoleManager=2`. **Admin = role ∈
    {1,2}.** (Note: different numbering from space's `0=member/1=admin/2=owner`.)
  - **Status** (`modules/group/const.go:3-11`): `GroupStatusDisabled=0`,
    `GroupStatusNormal=1`, `GroupStatusDisband=2`. **Active = Normal(1).**
  - **Admin gate (canonical):** `db.QueryIsGroupManagerOrCreator(groupNo, uid)`
    (`modules/group/db.go:107-111`) = `is_deleted=0 AND is_external=0 AND
    status=Normal AND role ∈ {creator,manager}`; service wrapper
    `IsCreatorOrManager` (`service.go:571`). **There is NO HTTP-layer admin/active
    helper for groups** (unlike space's `authorizeSpaceAdmin`); group handlers
    inline the check + 403 (e.g. `api.go:1172,3540,3998`). → this task **builds a
    `requireGroupWelcomeAdmin` + group-active wrapper** over these primitives.
  - **Join event already published & multi-listener:** `event.GroupMemberAdd =
    "group.memberadd"` (`modules/base/event/api.go:22-23`), published transactionally
    at `modules/group/api.go:1852-1861` with payload
    `config.MsgGroupMemberAddReq{Operator, GroupNo, Members []*UserBaseVo}`. The
    builtin `handlerMap` leaves it uncommented→fanned-out to all
    `AddEventListener` regs (`base/event/handler.go:26,41`); the message module
    already subscribes (`modules/message/api.go:335`). A notify listener
    subscribes identically — mirror how notify subscribes `event.SpaceMemberJoin`
    (`modules/notify/api.go:146` → `handleMemberJoin`). **Coverage gap (parity with
    space):** org/dept auto-add + scan-join partly bypass `GroupMemberAdd`
    (`group/event.go:341,511`; `event.GroupMemberScanJoin` `api.go:2595`) — the
    reconciler over `group_member` is what catches those (optionally also listen to
    `GroupMemberScanJoin`).
  - **Group-channel send (verified):** `ctx.SendMessage(&config.MsgSendReq{
    ChannelID: group_no, ChannelType: common.ChannelTypeGroup.Uint8()/*=2*/,
    FromUID: …, Payload: {type:1, content}})` — canonical at
    `modules/group/event.go:381-395`. The notify sender's HTTP layer
    (`space_welcome_sender.go send`, POST `/message/send`) is **container-agnostic
    and reusable verbatim**; only the *request construction* changes from
    `NewPersonalMsgSendReq` (channel_type=PERSON) to this group literal. `MsgSendReq`
    also has a `Subscribers` field to scope visibility — **not used** here (delivery
    is public per the locked decision).
  - **⚠ First-join timestamp differs from space.** Space keys first-join on
    `space_member.created_at`, which is **never reset on rejoin**. `group_member.
    created_at` **IS reset on rejoin** (`recoverMemberTx` sets `created_at: Now()`,
    `modules/group/db.go:246`). Consequence: the **ledger `UNIQUE(group_no, uid)`
    remains the at-most-once authority** — anyone already welcomed keeps their
    (SENT) ledger row, so leave→rejoin does NOT re-post (satisfies the "退群再进不
    重复" decision). The only edge: a member who joined *before* `active_from` (never
    enqueued, no ledger row), left, and rejoined *after* `active_from` would newly
    qualify (fresh `created_at`) and get welcomed on that second join. Acceptable
    under first-join-by-current-membership semantics; see Open questions.

- **Sender identity for a public group post.** Path confirmed: build a
  `config.MsgSendReq` and `ctx.SendMessage` it (the notify sender's HTTP layer is
  reused). Sender is server-chosen and fixed (not admin-forgeable). Proposed
  `from_uid = notification` (the system bot, as the Space welcome uses); existing
  group tips instead post as the *operator* (a member) — `group/event.go:392`. The
  **one remaining unknown** (implement-time verification, incl. real-wire): whether
  the `notification` bot may post into a group channel it is not a member of, or
  whether the post must use a member identity / a system-message path. This picks
  the concrete `from_uid`; it does not change the rest of the design.

## Load-bearing list

- **Group isolation & authorization (tags: group, isolation, auth, acl).**
  - A caller may read/write the welcome config for **only** a group where they are
    creator/manager. Enforcement mirrors space: gate every CRUD handler on
    `IsCreatorOrManager(:group_no, loginUID)` after a group-active check; reject
    non-managers/non-members with a **generic** permission-denied code (anti-
    enumeration: same 403 for non-member and under-privileged). The path
    `:group_no` is the **only** group a request may affect. A disbanded/frozen
    group → same reject path.
  - Delivery isolation: a claimed ledger row is dispatched using **that row's**
    `group_no` config and posts into **that** group's channel only; one group's
    config or backlog can never post into another group. The trigger member must
    be a **human** (system bots / robots excluded as joiners) and a real
    first-join; fail-closed never posts on any anomaly.
  - **Public-post consequence:** because delivery is a public channel post (not a
    private DM), there is no per-recipient DM privacy check — but the *eligibility*
    gate (group active, config enabled, member is a first-join human) still fully
    governs whether the single post happens. A member who joined then left before
    the post drains: **[open question]** still welcome them (dedup already keyed
    (group,uid)) or skip — default: post is keyed to the first-join event, so it
    fires once regardless of a later leave (matches "first join" semantics).

- **Config storage migration (tags: migration).**
  - New table `octo_group_welcome_config`, one row per group:
    `group_no` (UNIQUE, length/charset/**COLLATE aligned to the group table's
    group_no** to avoid MySQL 1267 on reconciler JOINs), `enabled TINYINT`,
    `active_from VARCHAR(40)` (RFC3339 UTC string, `""` = unset — reuse the
    Space store's string-typed approach so `ParsedActiveFrom` validators apply),
    `message VARCHAR(2000)`, `updated_by VARCHAR(40)` (audit),
    `created_at`/`updated_at DATETIME NOT NULL` (UTC, **app-written, never
    `NOW()`**), `idx_enabled`. Migration under a module that embeds `sql/`.
  - New ledger table `octo_group_welcome_delivery`, `UNIQUE(group_no, uid)`, same
    state machine columns as `octo_space_welcome_delivery` (status, attempts,
    claim_owner, claim_expire_at, next_retry_at, error_class, timestamps), plus a
    sweep index `(status, claim_expire_at)` and a claim index leading with
    `(status, next_retry_at)` for the cross-group claim.
  - **Precedence (simpler than Space — no global fallback):** effective config for
    a group = the per-group row **iff present AND enabled**; else off. Deterministic,
    exactly one effective config per group.
  - **Time discipline** identical to the Space ledger: all timestamps app-supplied
    UTC bound params; no naked `NOW()`; `active_from` parsed RFC3339 UTC.

- **Config write path + validation (tags: error-response, i18n, wire-contract).**
  - Concurrent admin PUTs serialized by an insert-then-lock upsert (copy the
    Space `UpsertMerged` idiom: idempotent INSERT → `SELECT … FOR UPDATE` → merge
    → UPDATE, bounded 1213/1205 retry) so two managers editing the same group
    can't lose fields and a first-create can't gap-lock-deadlock.
  - Validation: `enabled=true` requires a parseable RFC3339 `active_from` and a
    trimmed-non-empty `message` ≤ 2000 code points (reuse the Space combination
    validator or a group analog); message + `active_from` column-width guards run
    regardless of `enabled` (clean 400, not a driver 500 / silent truncation);
    newlines preserved, plain text (no markdown). Partial `PUT` validates the
    merged prospective config, not the patch alone.
  - `PUT` upserts (增/改); `DELETE` hard-deletes → group reverts to off (no
    fallback); `updated_by` records the acting admin.

- **Error responses & i18n (tags: error-response, i18n, wire-contract).**
  - All CRUD responses via the `pkg/httperr` i18n envelope with registered
    `pkg/errcode` codes — reuse existing group permission/param codes where they
    exist; add new codes only if none fit (each new code gets a zh-CN block in
    `active.zh-CN.toml`). No raw `c.ResponseError`/`c.JSON` non-OK/`AbortWithStatusJSON`.
  - `make i18n-extract-check` + `make i18n-lint` pass; new handler files added to
    the owning module's `Test<Module>NoLegacyResponseError` source guard.

- **Rate limiting (tags: rate-limit).**
  - The group-admin CRUD route group mounts `SharedUIDRateLimiter` **after**
    `AuthMiddleware`; no hand-rolled Redis counter. Route-hitting tests reset
    `ratelimit:uid:*` in setup.

- **Delivery drive loop — all-enabled-groups (tags: notify, onboarding, idempotency).**
  - Event path: a notify listener on `event.GroupMemberAdd` resolves the group's
    effective config and enqueues a pending `(group_no, uid)` row iff enabled +
    the joiner is an eligible first-join human. Enqueue failure never blocks/rolls
    back the completed join (fail-open on the join, fail-closed on the welcome).
  - Reconciler iterates **every group with an enabled config**, scans each for
    first-join human members past `active_from` lacking a ledger row, under a
    **global per-cycle cap** + per-group sub-cap + a **rotating start cursor**
    (fairness — no group starves another; copy the Space fix).
  - Worker claims **across groups** (`status=pending AND next_retry_at<=?`,
    `FOR UPDATE SKIP LOCKED`, index-backed), re-resolves the claimed row's group
    config for the body, re-checks eligibility, then **posts to the group
    channel**; cross-group sweep (`sweepClaimedAll`/`sweepDispatchingAll` + sweep
    index) reclaims lease-expired rows. At-most-once, CAS-on-`claim_owner`, lease,
    backoff, `unknown`/`failed` terminal handling — same contract as Space.
  - **Enqueue idempotency:** `INSERT … ON DUPLICATE KEY UPDATE id=id` on
    `(group_no, uid)`.
  - **Platform master switch (tags: notify, observability).** A single bool
    `system_setting onboarding.group_welcome_enabled` (registered in the schema,
    read via `SystemSettings.GroupWelcomeEnabled()`, default **false**). Checked at
    the event enqueue, the reconciler, and the worker (post-sweep) — off ⇒ no
    enqueue, no post; the cross-group sweep still runs so in-flight rows reach a
    terminal state. Enablement only, **no content fallback** (a group's body is
    always its own row). Hot-reloaded with the snapshot; flip-off is an instant,
    reversible kill that touches no per-group rows.
  - **Burst coalescing (tags: notify, wire-contract).** A freshly-enqueued row is
    held back from the worker until `now + coalesceWindow` (via `next_retry_at`) so
    co-arriving joins collect; the worker `claimBatch`es a group's due rows (≤
    `groupWelcomeCoalesceMax`) and `dispatchBatch` posts **one** message whose
    `{member}` renders to the joined name list. One coalesced post per group per
    wake also naturally rate-limits a single group's welcomes; a burst beyond the
    cap spills into later wakes. Each row keeps its own CAS/at-most-once transition
    and shares the one message's `message_id`.

- **Group-channel send contract (tags: notify, wire-contract).**
  - The send builds a `config.MsgSendReq` to `channel_id=group_no`,
    `channel_type=ChannelTypeGroup`, `payload.type=Text`, authoritative
    `payload.group_no`, sender fixed and server-chosen (proposed `notification`;
    admins cannot forge sender/payload). No rich text/cards/attachments/scheduled
    send. `Subscribers` not set (public to the whole group).
  - **Placeholder rendering (member name/@mention).** The stored body may contain
    a single, documented placeholder token for the joining member (e.g.
    `{member}`); at dispatch it renders to that member's display name — resolved
    from the `user`/`group_member` nickname — for **that row's `uid`**. Rendering
    happens **server-side at send time** (the stored config keeps the raw token;
    the DB column bound is on the raw body). Unknown/other tokens are left
    verbatim (no execution, no injection surface — the payload is plain text).
    Length: the rendered body still fits the wire contract; guard/truncate the
    rendered result if a long nickname could overflow.
  - **Plain-name vs clickable @mention [implement-time detail].** v1 renders the
    member's **display name as plain text**. A real clickable @mention (client
    highlights + notifies) needs the WuKongIM mention payload encoding — see
    `pkg/mentionrewrite` and existing mention payloads; adopt it if the encoding is
    cheap, else ship plain-name naming and add clickable @mention as a fast-follow.
    Either way the token and the "names the joiner" behaviour are delivered.

- **Config cache & convergence (tags: notify).** Read-through by PK (like the
  Space store — no snapshot TTL), so an admin write is visible on the next
  delivery read across replicas. Reads atomic per group.

- **Observability (tags: observability).** Mirror the Space counters/stages
  (`enqueue_total`/`enqueue_dedup_total`/`config_invalid_total`/sweep counts),
  per-group dimension `group_no` as a structured log field. Never log the welcome
  body or raw upstream error strings.

- **Incidental (附带) polish carried from the merged Space welcome (#646)** — small,
  reviewer-requested, bundled here (tags: notify, space, testing):
  1. Doc-caveat on exported `common.(*SpaceWelcomeConfigStore).Upsert` — production
     must use `UpsertMerged` (concurrency); `Upsert` stays for single-writer/test.
  2. `modules/notify/space_welcome.go enabledEffectiveConfigs`: read `r.Enabled`
     instead of hardcoded `Enabled: true` (self-documenting; behaviour identical
     since `ListEnabled` filters `enabled=1`).
  3. Space CRUD handlers derive the DB context from `c.Request.Context()` (honour
     client cancellation) instead of `context.Background()`.
  4. Add an HTTP-level test asserting Space `DELETE` reverts to an **enabled**
     global fallback (locks the response contract).

- **Commit style (tags: commit, git).** Conventional Commits, English.

## Out of scope

- **Generalizing Space + Group welcome onto one shared delivery engine** — this
  task mirrors the shape into a parallel group-scoped implementation; a unifying
  refactor of the reviewed Space code is a separate future task.
- **Any change to the Space welcome delivery/semantics** beyond the 4 incidental
  polish items above — the Space ledger, state machine, sender, and CRUD are
  otherwise untouched.
- **Multiple welcome messages per group / audience targeting / scheduling** — one
  config per group, single plain-text body, no `welcome_id`.
- **Global / platform-wide group welcome *content* fallback** — deliberately none
  (per the locked decision): a group's body always comes from its own row. The
  **one** `system_setting` key added (`onboarding.group_welcome_enabled`) is an
  enablement master switch (decision #5), NOT a content fallback — it never
  supplies a body, target, or default for any group.
- **Delivery reliability semantics** — state machine, at-most-once, sweep,
  backoff, CAS, lease, `SELECT … FOR UPDATE SKIP LOCKED`, no leader election —
  copied unchanged from the Space engine, not re-designed.
- **Rich message features** — no cards/attachments/rich text. The single
  member-name placeholder (§ Group-channel send contract) **is in scope**; a
  general templating engine / multiple placeholders / arbitrary token language is
  not. A clickable @mention (vs plain-name render) may be deferred to a fast-follow.
- **Retroactive bulk post** to members who joined before a group's `active_from`.
- **Client / admin UI** — server-side API + validation + delivery only.
- **octo-lib changes** — the notify-local sender stays self-contained; if a
  group-channel send helper is missing in octo-lib it is built server-side here.
- **Group membership/permission model changes** — reuse `IsCreatorOrManager` +
  the existing group status; no new roles or status.

## Acceptance

- **Group-admin CRUD authz.** `GET/PUT/DELETE /v1/group/:group_no/welcome` succeed
  for a creator/manager of that group; a plain member, a non-member, and a caller
  against a disbanded group all get the generic permission-denied path (no
  privilege-reason leak). A request affects only the path `:group_no`.
- **CRUD semantics.** `PUT` with no config inserts (增); `PUT` on an existing
  config updates (改); `DELETE` removes (删) → group reverts to **off** (no
  fallback). `updated_by` records the acting admin. Concurrent PUTs (edit +
  first-create) lose no field (insert-then-lock regression tests, both cases).
- **Write-path validation (prospective).** With `enabled=true`, an unparseable
  `active_from`, empty-after-trim, or >2000-code-point `message` is rejected via
  the i18n envelope; `active_from` over its column width is a clean field-too-long
  400. A partial `PUT` is accepted iff the merged prospective config is valid.
- **First-join, at-most-once, public delivery.** A human member's **first** join
  to an enabled group results in **exactly one** `(group_no, uid)` ledger row and
  **exactly one** message posted to that group's channel (`channel_type=GROUP`,
  `channel_id=group_no`), with the group's configured body. Leaving and rejoining
  does not re-post. A member who joined before `active_from` gets nothing.
- **Isolation / eligibility (regression).** System bots/robots as joiners get no
  welcome; a disabled or absent config → no post; one group's backlog never posts
  into another group; fail-closed on any anomaly.
- **Platform master switch.** With `onboarding.group_welcome_enabled=false` (the
  default), an otherwise-deliverable enabled group produces no enqueue and no post;
  turning it on lets delivery proceed; turning it back off stops new posts (a
  regression test asserts both directions). The switch adds no content fallback.
- **Burst coalescing.** N members joining a group at once are delivered as **one**
  public post naming all of them (`{member}` → joined list; overflow → `… 等 N 人`),
  not N posts; every joiner in the batch is marked SENT (at-most-once each). A
  group whose burst exceeds the per-post cap posts one coalesced batch per wake and
  spills the remainder to later wakes without starving other groups.
- **Multi-group + fairness.** Two enabled groups each welcome their own first-join
  members independently; disabling group A does not affect B; the reconciler
  catches up all enabled groups within a cycle under a global cap; a group kept
  over-cap does not starve others (rotating-cursor regression test).
- **Cross-group claim.** The worker claims from any enabled group; the claim
  predicate is index-backed (no full scan) — verified by a `(status, next_retry_at)`
  -leading index. A claimed row posts with its own group's body.
- **Sender contract + placeholder.** The posted message's sender is the fixed
  server-chosen identity (not admin-forgeable); `payload.type=Text`, authoritative
  `payload.group_no`. The rendered body equals the configured message with the
  `{member}` placeholder replaced by the joining member's display name (non-token
  text + newlines byte-preserved; unknown tokens left verbatim; two different
  joiners get their own name rendered).
- **Rate limiting.** The CRUD group carries `SharedUIDRateLimiter` after
  `AuthMiddleware`; a burst test observes the `uid`-scope headers / `rate.limited`
  envelope; setup resets `ratelimit:uid:*`.
- **i18n / error contract.** `make i18n-extract-check` + `make i18n-lint` pass;
  any new codes have zh-CN blocks; new handler files are in the module's
  `Test<Module>NoLegacyResponseError` guard; no raw error responses.
- **Time discipline & schema alignment.** No feature SQL uses naked `NOW()`;
  `octo_group_welcome_config.group_no` / `octo_group_welcome_delivery.group_no`
  match the group table's `group_no` length/charset/COLLATE; a JOIN smoke test
  confirms no MySQL 1267.
- **Incidental Space polish.** The 4 items land: `Upsert` doc caveat present;
  `enabledEffectiveConfigs` reads `r.Enabled`; Space CRUD uses request context;
  the Space DELETE→enabled-global-fallback HTTP test exists and passes.
- **Backward compatibility.** No group has a welcome by default (`enabled=false`),
  so deploy is inert until an admin opts in; existing `modules/group`,
  `modules/notify`, `modules/common`, `modules/space` tests still pass; no change
  to existing wire responses.
- **Command-line verification.**
  - `go test -race ./modules/notify/...`
  - `go test -race ./modules/group/...`
  - `go test -race ./modules/common/...`
  - `go test -race ./modules/space/...`  (incidental polish regression)
  - `make i18n-extract-check && make i18n-lint`
  - `go test -race ./...`
  - `git diff --check`
- **Tests cover at minimum:** CRUD authz matrix (creator/manager/member/non-member/
  disbanded); PUT insert-then-update + concurrent lost-update + concurrent
  first-create; DELETE→off; prospective validation both directions; first-join
  at-most-once public post to the correct group channel; `{member}` placeholder
  renders each joiner's own name (unknown tokens left verbatim); leave/rejoin no
  re-post; pre-`active_from` skip; human/system-bot exclusion; multi-group independent
  delivery + per-group disable; cross-group claim single winner
  (`FOR UPDATE SKIP LOCKED`); worker fair-rotation; migration executes on a fresh
  DB; UID rate-limit headers.

## Open questions (confirm before / during implement)

- **Sender identity for a public group post** — can the `notification` system
  identity post into a group channel it is not a member of, or must the post use a
  member identity / system-message path? Resolved at implement time (source +
  real-wire); picks the concrete `from_uid`, does not change the design.
- **Placeholder token + render style** — token name (proposed `{member}`), and
  plain-name render (v1) vs. a clickable WuKongIM @mention (fast-follow if the
  mention encoding is non-trivial). Confirm the token; @mention richness may defer.
- **Module placement (confirm)** — CRUD handler in `modules/group` (mirrors
  `modules/space/api_welcome.go`, plus a new `requireGroupWelcomeAdmin` gate),
  delivery in `modules/notify` (mirrors `space_welcome*.go`), config store +
  ledger split as with Space (config store in `modules/common`, ledger migration
  under `modules/notify/sql/`).
- **Post-join-leave** — the post is keyed to the first-join enqueue and fires once
  even if the member leaves before the worker drains (default), vs. skip if no
  longer a member at send. (Space re-checks membership at send; for a *public*
  post that names the joiner, either is defensible.)

RESOLVED in this brief: delivery = public group-channel post; per-group admin
config, no global fallback; first-join at-most-once via the ledger (not the
rejoin-resettable `created_at`); body names the joiner via a placeholder.
