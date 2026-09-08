---
type: Task
title: "Task: project-p2-subsystem-integration"
description: P2 of the Project collaboration layer — create the all-hands group with the project, add the personal-assistant member kind and the pending-invitation table, add standalone ownership transfer, add the internal endpoint subsystems need in order to reclaim per-project containers, adapt the existing endpoints that already hand member data to external principals, and build the octo-server half of eager subsystem provisioning (transactional outbox, opaque container ids, no disclosure until each target narrows authorization by Project). Draft — one open question remains (Q1).
tags: ["space", "isolation", "acl", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "migration", "provisioning", "im"]
timestamp: 2026-09-07T00:00:00Z
# --- octospec extension fields ---
slug: project-p2-subsystem-integration
upstream: self
source: self
---

# Task: project-p2-subsystem-integration

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.
>
> **Status: DRAFT, decisions settled 2026-09-07.** D1–D13 are argued from code and
> from the product answers of 2026-09-07. Only **Q1** (subsystem ownership and
> scheduling) is still open, and it gates PR-5's *rollout*, not its code. Two things
> are explicitly parked rather than decided: the dedicated project assistant
> (§Parked) and roster equality for the all-hands group (D10 level 2).

## Goal

Make the 2026-09-07 prototype's five tabs expressible, and provision the two
subsystem containers a Project needs — from octo-server, eagerly, without shipping
a container that is open to the whole Space.

Three things follow from the prototypes and none is in P0 or P1:

1. **The all-hands group does not exist yet.** The create dialog promises
   「创建后自动生成全员群」 and the 群聊 tree shows it, but P1 only bound
   *existing* groups to a project. P2 creates it with the project; making its
   roster track the project's is a separate, deferred level — D10.
2. **A project member is no longer always a human.** The member list shows
   `5 位人类 · 0 个个人分身 · 1 个项目分身`. 分身 means a **platform-hosted** bot,
   one whose runtime survives its owner's machine being off. `modules/project`
   today has zero bot handling — grep for `robot` / `is_bot` in the module returns
   nothing. P2 delivers 个人分身; 项目分身 is parked as a product question.
3. **Two of the five tabs are not octo-server's data at all.** 任务 is fleet's
   issue/agent/squad model; 团队文件 is drive's shared space. The prototype puts
   任务前缀 inside 项目设置, and 任务前缀 is fleet's `workspace.issue_prefix` /
   `issue_counter` — a per-workspace column. So one Project corresponds to one
   fleet workspace and one drive shared space.

That third point is where this brief spends most of its argument. The product chose
eager, octo-server-initiated provisioning over subsystem-side lazy `ensure`, and
that choice carries one consequence shaping the whole design: **a container that
exists before anyone visits it is a container whose access control has not yet been
narrowed.** Fleet's workspace gate today admits on octo Space membership alone and
then materializes the caller as a member, so an eagerly provisioned workspace with a
guessable id would let every Space member self-admit into every project's workspace,
permanently. Hence opaque ids (D1), R1–R3, and the rule that no container id reaches
a client until its target has narrowed.

## Background

### What P0 and P1 shipped

P0 (#841, merged) shipped exactly two tables — `octo_project` and
`octo_project_member` (`modules/project/sql/20260904000001_project_core.sql:78`
and `:118`). There is no invitation table; `octo_project_member.invite_uid` records
*who* invited an already-admitted member, not a pending invitation.

P1 (#846, OPEN — supersedes the closed #844) ships `group.project_id`, one admission entry enforcing I2, the
reverse-registered project→group cascade, and the point-query read contract on
`POST /v1/auth/verify?include=context` (`modules/user/api_project_context.go`).
`project_on` ships to clients through `GET /v1/common/appconfig`.

Three P0 columns exist with no writer, and their own comments assign them here:
`join_mode` (「自助加入是 P2，本列在 P0 无消费方」), `is_official`
(「P0 无任何写入方」), and `discoverability`'s unlisted value. Of those, P2 claims
none — they belong to the sibling brief named below. P2 follows the same
declare-without-writer pattern for one new value (D5).

### Why bots need no I1 exemption

I1 requires an active `octo_project_member.uid` to hold an active `space_member`
seat in the same Space. Bots satisfy it already: `user.robot = 1` rows get
`space_member` seats (`modules/bot_api/space_inject.go:32` describes the split
between `robot`-backed bots and platform App Bots; migrations
`modules/space/sql/20260308000002_space_legacy01.sql:8` and `...003:8` backfill
seats for every `robot` row). Bot ownership is `robot.creator_uid`, already queried
by verify's owned-bots list (`modules/user/api.go:4787-4791`).

So the assistant member kind needs **no** hole in I1, and this brief must not open
one. A source guard pins that.

### What the group module already enforces about bots

Load-bearing for D7, and it means P2 writes far less than it looks:

- **A bot may only be pulled into a group by its owner.** `#1182` forced
  `inviter == robot.creator_uid`; recorded at `modules/group/db.go:881`.
- **A bot always follows its owner out, with no role exception.** `#354` is the
  written product decision quoted at `modules/group/db.go:895-905`:
  「bot 永远跟随其主人，无角色例外」, applying even when the owner is the group
  creator whose role was just handed over.
- **That cascade already exists and is transactional.** `QueryBotsInvitedByUIDTx`
  (`modules/group/db.go:891`) selects the owner's active bot members `FOR UPDATE`
  so a leave / kick / blacklist removes them in the same transaction
  (D-2 cascade, `#1186`). `requireCommonRole` distinguishes self-service removal
  (excludes role-holding bots) from the other three paths (does not).
- Ownership checks live in `modules/group/bot_ownership.go` (`checkBotOwnership`),
  whose SQL is scoped `WHERE u.robot = 1` — `modules/group/api.go:3084` warns
  against reusing it for human uids.

### What the subsystems actually are

Verified against octo-drive at `076e289` and against octo-fleet's integration
branch (the locally checked-out fleet tree is ~6 weeks stale; findings below were
re-verified on the integration branch, where the same logic sits at different line
numbers).

| | octo-fleet | octo-drive |
|---|---|---|
| container | `workspace` (1 octo Space : N workspaces, deliberately) | `drive_space` (`type` personal/shared, bound by `octo_space_id`) |
| create route | `POST /api/workspaces`, body `name`/`slug`/`description`/`context`/`issue_prefix` | `POST /v1/drive/spaces`, body `{name}` only |
| idempotent? | **No.** No `Idempotency-Key`; `workspace.slug` is **globally** unique, never scoped to a space | **No.** Server-generated UUID; the space and member inserts are not in one transaction |
| service credential | **None.** No `X-Internal-Token` or equivalent anywhere in the repo. `bf_` bot tokens are hard-blocked from this route by the mapped-action allowlist; only a real person's `uk_` API key works | **None for provisioning.** The internal route group registers exactly one route, `POST /v1/internal/drive/sign-download` |
| knows about Project? | No. Fleet's own `project` table is a workspace-scoped issue container with a `lead` and no member table — same word, different concept | No. Zero matches for `project` in any `.go` or `.sql` |
| how it learns members | Copy-on-first-request: `UpsertMember{Role:"member"}` when the caller's verified Space list contains the workspace's `octo_space_id`. Never pruned; the roster-fetch helper that could prune it has zero callers | Its own authoritative `drive_space_member`. Never reconciled against octo-server; a user removed from an octo Space keeps drive access indefinitely |

Two findings decide this brief:

**Fleet's workspace gate is Space, not Project.** The predicate is
`containsString(octoIdentity.Spaces, workspaceSpaceID)`, and on success a human
caller is materialized with `UpsertMember{Role: "member"}`. Provision a workspace
per Project and **every Space member who opens it becomes a member of it**, with a
row nothing ever deletes. The design brief already warned: 「尤其不要把它的自动物化
机制延伸到 Project 维度，那等于批量生产无退出通道的副本」.

**Drive's main read paths never validate the octo Space.** `/files/*`,
`/folders/*`, `/browse`, `/trash/*` and `/search` are mounted without the space
gate, and no code anywhere compares `drive_space.octo_space_id` against the
request's validated `X-Space-Id`. What they do check is drive's own
`drive_space_member` row — which is exactly the row that is never revoked.

### The provisioning pattern this repo already shipped

`modules/agentmailgateway/provisioning.go:26` — `ensureProvisioned(ctx, uid,
spaceID)`: `singleflight`-collapsed, cached per process, `context.WithoutCancel` so
a client disconnect cannot abort a half-done provision, bounded timeout and bounded
response size. Its peer is octo-mail's
`POST /internal/v0/gateway-identities/ensure`.

That is the shape each target's `ensure` endpoint must have **internally**, because
Shape S delivers at least once and will call it repeatedly: get-first, create,
duplicate-key downgrade, bounded. What does not carry over is the driver — mail is
gateway-style so octo-server runs the ensure inline on the request path, while here
a background worker drives it from an outbox and the outbox's lease does
`singleflight`'s job.

## Load-bearing list

- **auth / anti-enumeration**: the new internal endpoint must not let a caller
  distinguish "no such project" from "outside your grant". One answer for both.
- **I1**: assistant members satisfy it through a real `space_member` seat, never an
  exemption.
- **I2**: an assistant admitted to a project must be admissible to that project's
  groups through the same P1 admission entry, with no new bypass.
- **I3**: unchanged — a group belongs to at most one Project.
- **`member_epoch`**: every new membership write (assistant admission and removal,
  invitation acceptance, ownership transfer, owner-cascade of an assistant)
  increments it in the same transaction. Still the only cache-invalidation
  primitive.
- **cascade**: `cleanupSpaceMemberProjects` and the P1 project→group cascade must
  both handle bot rows. An assistant whose owner is cascaded out goes with them, in
  the same transaction — the project-scope analogue of `#354` / `#1186`.
- **project-create availability**: `POST /v1/space/:space_id/projects` touches no IM
  call today. PR-0 makes it touch two (channel create, subscriber add), inside the
  transaction and fail-closed, so an IM outage turns project creation from "works"
  into "refuses". That is the intended semantics, not a side effect, and it is the
  single biggest behavioural change in this brief.
- **lock order**: PR-0 writes `group` and `group_member` inside the project-create
  transaction, at the positions P1 assigned them
  (`space_member -> space -> project -> group -> group_member ->
  octo_project_member`). Any deviation reopens the Error 1213 incident that
  `modules/project/db.go` and `modules/space/db.go:71-88` both record.
- **trust boundary (new egress)**: the provisioning call is octo-server's first
  egress to fleet or drive. One secret per target, HMAC-signed per `pkg/octosign`,
  registered in `ValidateNotifyTokenExclusions`
  (`internal/cardactiondispatch/registry.go:242`, called from `main.go:683`) so it
  cannot collide with any existing internal token.
- **container id secrecy**: until R2/R3 land, the opaque id is the only thing
  between an eagerly provisioned container and every Space member. Generated by
  octo-server, stored in the mapping table, and absent from every client response,
  log line and error detail while the target's capability switch is off.
- **outbound confinement**: the HTTP client lives in the provisioning worker
  package only. No request handler in `modules/project` or `modules/user` may call
  fleet or drive — a handler that does makes a user request depend on a subsystem
  being up.
- **wire contract**: `GET /v1/projects/:project_id/members` gains fields; the three
  verify routes gain nothing new here.
- **rate limiting**: `SharedUIDRateLimiter` after `AuthMiddleware` for the user
  routes; the internal route is not UID-scoped and needs its own bound.
- **error envelope**: `httperr.ResponseErrorL` + registered `pkg/errcode` codes;
  `make i18n-extract`, `i18n-extract-check`, `i18n-lint`, plus zh-CN entries.
- **existing external member surfaces**: the endpoints in PR-3 already return
  Space-wide member data to bots and subsystems.
- **`pkg/authtree` census**: every route added or affected must be registered with
  its Project dimension, even when the decision is "deliberately unconstrained" —
  P1 already wrote that paragraph for its two routes.

## Out of scope

- **Roster equality between a project and its all-hands group.** The group itself
  is in scope (D10, PR-0); the property "project member set *equals* all-hands
  group member set" is not. It queues behind #797 / `im-pending-outbox` for the
  reason in D10.
- **The dedicated project assistant (项目分身).** Product requirement parked
  2026-09-07 — see §Parked. P2 declares the `kind` value and writes nothing to it.
- **Read-path hardening.** `sidebar/sync`'s `ExistMembersActive` backstop, the
  avatar endpoint's default-avatar fallback, the deprecated conversations endpoint
  and `querySavedGroups`. Independently shippable, unrelated to subsystems, own
  brief.
- **The product surfaces**: `join_mode = 0` self-join, `is_official` management,
  per-user pinning, `GET /v1/projects/:project_id/groups`. Own brief.
- **The IM-unsubscribe leak.** Inherited from P1 and unmeasured at project
  granularity by P1's own admission; belongs to #797 / `im-pending-outbox`. PR-0
  adds one `IMAddSubscriber` site, which is the opposite direction and fails closed
  (D10), but D10 level 2 must wait for that work.
- **Disclosing any container id to a client.** PR-5 provisions but does not
  disclose.
- **Backfilling a container for a project created while its target was off or invalid.**
  Added 2026-09-07 after review. PR-5 enqueues only for targets that were enabled AND
  valid at the instant of creation, and there is no backfill anywhere — so at FIRST
  enablement every already-existing project gets no container, ever, and a window with a
  typo'd URL leaves projects whose missing row nothing will retry. That is not an edge
  case: since the default is empty and the runbook gates enablement on P-2 landing, "every
  project predates enablement" is the *first* state an operator is in. Consequently D1's
  invariant ("one Project maps to one fleet workspace and one drive shared space") is not
  reachable by PR-5 alone. Out of scope here, with two things in its place: a bounded
  remediation statement in the runbook (`plan.md` §3.4) so it is operable today, and PR-6
  below so it is not lost.
- **The `provisioning:` field on the project detail response.** An earlier sketch
  listed it; it is exactly the read path D12 forbids gating on. It may return later
  as display-only, never as a precondition.
- **Storing 任务前缀 / 专家 / 专家团 / 技能 / file metadata.** See D4. The 技能 tab
  in particular is fleet's own concern and octo-server has no part in it.
- **Cross-Space or external-guest projects.** Still a hard v1 constraint.

## Decisions

### D1 — One Project maps to one fleet workspace and one drive shared space, and the container id is opaque, generated by octo-server, and stored

The mapping is real: the prototype puts 任务前缀 in 项目设置 and that field is
fleet's per-workspace `issue_prefix`.

A derived id (`project:<project_id>`, `p-<project_id>`) was the first design and is
**rejected under eager provisioning** — see D2's escalation analysis. Instead:

- the container id is **random and opaque**, not a function of `project_id`;
- octo-server generates it **at enqueue time**, in the same transaction as the
  project INSERT, and sends it to the target as the id to create;
- it lives in the mapping table (D12) and is disclosed only to project members,
  and only after the target's capability switch is on;
- the project *name* never becomes a uniqueness key. Fleet's `slug` is globally
  unique, so a name-derived slug would make two Spaces' identically named projects
  collide with a 409 — a cross-tenant conflict and a name-existence oracle in one.
  The name goes to `workspace.name`, which is not unique.

Generating the id at enqueue rather than accepting one back has three consequences
and all three matter: the mapping row carries the id from t=0 so nothing is
backfilled and a failed call leaves a row incomplete only in `status`; the retry is
idempotent on a key the receiver enforces with a plain unique index (drive's
`drive_space` primary key, fleet's `slug` unique index), so at-least-once delivery
cannot manufacture duplicates; and `project_id` still travels in the body because
the receiver needs it to answer "which project is this for" when it reclaims (D9) —
but it is not the id and must not be used to derive one.

### D2 — Provisioning is a transactional outbox driven from octo-server (Shape S)

Chosen by the product over subsystem-side lazy `ensure`.

1. **Table `octo_project_provisioning`** (`octo_` prefix per repo rule): `id`,
   `project_id`, `target` (`fleet` / `drive`), `container_id`, `status`, `attempts`,
   `next_attempt_at`, `lease_owner`, `lease_until`, `last_error`, `created_at`,
   `finished_at`. Shaped after
   `modules/space/sql/20260821000001_space_member_removal_cleanup.sql` with P1's
   corrections: app-written UTC (no `NOW()` / `ON UPDATE`) and a draining purge with its
   own `(target, status, finished_at)` index. Both scan indexes lead with `target` because
   every claim/sweep/purge statement filters on it and, without that leading column, one
   target's scan locks the other's rows and `SKIP LOCKED` then starves it.

   **No lease heartbeat** — corrected 2026-09-07. An earlier revision of this item listed
   one and the migration header repeated the claim, while no code ever extended a held
   lease. The claim is withdrawn rather than implemented: a heartbeat is machinery for
   long jobs, and this job is a single bounded outbound call. What replaces it is an
   *enforceable* form of the relationship the lease constant was justified by —
   `OCTO_PROJECT_PROVISION_TIMEOUT` is rejected at config load unless it is at most a
   quarter of the lease. With neither, a timeout configured above the lease lets a second
   replica legitimately re-claim and run the same row concurrently, and lets the sweep
   write `abandoned` under a still-running executor.
2. **Enqueue in the same transaction as the project INSERT.** The only construction
   that makes "project exists ⟹ job exists" true. The existing Redis queue
   (`internal/cardactiondispatch/queue.go`, `RedisQueue`) cannot do it: not the same
   store as the MySQL write, so an enqueue can be lost or orphaned relative to the
   commit.
3. **Worker** claims with `FOR UPDATE SKIP LOCKED` under a lease, exponential
   backoff, and on attempt exhaustion sets `abandoned` and alerts. Do **not** route
   it through `modules/base/event`: a listener error sets the row to `Fail`,
   `QueryAllWait` selects only `Wait`, so `Fail` rows are never redelivered.
4. **Signing** reuses the v1 primitive, which now lives in `pkg/octosign` — HMAC-SHA256
   over `method \n path \n timestamp \n eventID \n sha256(body)` — one secret per target,
   mutually exclusive from every other internal token. It was written for and is still
   used by card action dispatch, which delegates to the same leaf; the move out of
   `internal/cardactiondispatch` happened because P1 landing closed an import cycle. A
   receiver implementing `ensure` should read `pkg/octosign`, not the delegating wrapper.
5. **No UI state in this slice** (see Out of scope).

**Target endpoint contracts** (each subsystem's work, both new):

```
POST /v1/internal/drive/spaces/ensure
  { "container_id": "...", "project_id": "...", "octo_space_id": "...", "name": "..." }
  → 200 { "container_id": "..." }

POST /api/internal/workspaces/ensure
  { "container_id": "...", "project_id": "...", "octo_space_id": "...",
    "name": "...", "issue_prefix": "..." }
  → 200 { "container_id": "...", "slug": "..." }

Headers on both:
  X-Octo-Timestamp   unix seconds
  X-Octo-Event-ID    sha256(container_id), hex  ← NOT the container id
  X-Octo-Signature   v1=<hmac-sha256 over CanonicalRequest(POST, path, ts, event-id, body)>
```

**Three things the receiver MUST do**, added 2026-09-07 because the original sketch
specified how the request is signed and not what verifying it requires — and the first
two have no enforcement on octo-server's side at all:

1. **Verify the signature** against the shared per-target secret. Refuse anything
   unsigned or wrongly signed.
2. **Reject a stale `X-Octo-Timestamp`** (a few minutes of skew). The timestamp is
   inside the signed string so it cannot be edited, but nothing stops a captured
   request from being *replayed* — and because ensure is idempotent, a replay would
   **resurrect a container the subsystem had already reclaimed**.
   `cardactiondispatch.Verify` deliberately checks only the MAC, so freshness is the
   receiver's.
3. **Treat `container_id` as the idempotency key** (get-first / create /
   duplicate-key downgrade). Delivery is at-least-once by construction.

**`X-Octo-Event-ID` carries sha256(container_id), not the id.** Bodies are almost never
logged; headers routinely are (a proxy's custom log format, an APM agent's default
header capture). Until R2/R3 land the container id is a capability, so putting it where
infrastructure nobody here controls will pick it up is a measurable widening for no
gain — the receiver reads the real id from the body. The hash keeps both properties the
slot needs: stable across replays of one job, and bound to one specific resource.

**Conformance vectors ship with the contract** (`internal/projectprovision/conformance.go`,
added 2026-09-07). Four fixed request/verdict pairs — valid, stale timestamp, tampered
body, wrong secret — with the expected signature spelled out, plus the canonical string
written byte by byte. Two reasons this is data rather than prose. The receivers are not Go —
pointing them at `pkg/octosign.CanonicalRequest` asks two separate codebases to reimplement it
from a description, and no library call expresses the freshness clause at all (the original
wording here said the primitive lived in an unimportable `internal/` package; that stopped
being true when P1 forced the leaf extraction, and the language boundary is the reason that
survives); and of the three MUST clauses, a wrong canonical string fails CLOSED and is
self-announcing, while a lenient timestamp check fails OPEN and is silent — so the clause
that actually matters is the one nothing on this side can detect. A test recomputes every
published signature so the table cannot drift from the implementation. **Passing the
vectors is a precondition for adding a target to `OCTO_PROJECT_PROVISION_TARGETS`**
(`plan.md` §3.2).

The receiver is deliberately NOT asked to dedupe requests it has already seen, and the
reason is correctness rather than leniency: octo-server retries the same row, and two
deliveries landing in the same second carry the same timestamp and therefore the same
signature, so a receiver refusing an identical repeat would refuse a legitimate retry and
burn an attempt. The timestamp window is the bound instead, and the residual risk is
stated rather than papered over — inside that window a captured request can be replayed,
recreating one specific container that octo-server itself asked for. It cannot create a
container for another project, cannot change any field (the body is inside the MAC), and
discloses nothing the captor did not already hold.

**`name` is a fixed low-information label, not the project's name** (D3, and see PR-5's
deviation note). The consequence on the receiving side is real: every container arrives
with the same name, so a receiver that displays this string shows one label for every
project. Build a display label from `project_id` — it is in every request — or read the
real name from `GET /v1/projects/:project_id`, which is what D3 already tells fleet to
do for `context`.

**What Shape S costs**, recorded so the choice stays reviewable: a table, a worker,
an egress, a secret per target; project creation gains a durable side effect in two
other systems even though it stays a single-system *transaction*; containers exist
before any membership check has run against them, which is the entire reason D1's
opaque ids and R1–R3 exist; and the worker cannot succeed against either target
until P-2 lands, so the backlog is real work sitting visible. What it buys: the
container is ready before anyone opens the tab, and octo-server holds a local record
of what was provisioned, which reclaim accounting wants (D12).

### D3 — 共同目标 and the project name are authoritative in octo-server; no subsystem holds a second writable copy

共同目标 maps to `octo_project.description` (VARCHAR(500)) — no new column. Fleet has
`workspace.context`, whose whole purpose is project background for its agents, so
without a rule the two drift. The rule: 项目设置 writes only octo-server; for a
Project-bound workspace fleet treats `context` as read-only and re-reads it from
`GET /v1/projects/:project_id` when needed.

`description` deliberately does **not** join the verify response: verify runs on
every request of every subsystem that fronts octo-server, and a several-hundred-byte
free-text field is not authorization data.

Renaming follows the same rule and touches nothing outbound. `workspace.name` is set
once at provisioning and never synced; 任务前缀 is fleet's and the prototype's own
help text agrees — 「修改前缀只影响新任务，已有编号保留」.

### D4 — octo-server stores no field belonging to a subsystem tab

项目设置 is composed from two backends: 项目信息 (name, 共同目标), 成员管理 and
邀请记录 are octo-server; 任务前缀 / 专家 / 专家团 / 技能 are fleet; 团队文件 is
drive. The client reaches each directly; octo-server does not proxy and stores none
of their fields.

### D5 — 个人分身 is `kind` + `owner_uid` on `octo_project_member`; 项目分身 is a declared value with no writer

- **人类** — today's row, unchanged.
- **个人分身** — `kind = assistant_personal`, plus `owner_uid` pointing at the human
  member who brought it in. Its lifetime hangs off that human: when the human
  leaves, is removed, or is cascaded out by a Space removal, the assistant is
  deactivated **in the same transaction**, with one `member_epoch` increment
  covering both rows. This is `#354`'s rule 「bot 永远跟随其主人」 at project scope.
- **项目分身** — the `kind` value is declared in the enum and documented as having
  no writer, exactly as P0 did for `join_mode` and `is_official`. No `removable`
  column ships until the requirement does (§Parked); adding a column no code writes
  is how the previous two ended up needing a comment explaining themselves.
- **An ordinary bot** — a self-hosted helper, an integration bot — may be a project
  member with no assistant kind. It is simply not counted or badged as a 分身.

Not new tables, because I2's predicate, the P1 cascade and the epoch bump are all
already written against `octo_project_member`; a second table means reimplementing
all three and keeping them in step forever.

Ordinary bots are admissible deliberately, and the reason is a concrete regression
rather than symmetry: I2 exempts only *system* bots by whitelist, so any other bot
inside a project group must be a project member. If ordinary bots could not be
project members, binding an existing group to a project would make the cascade evict
the integration bots already in it.

The member-list response carries `N 位人类 · M 个个人分身`, both computed from
`kind`. The third count appears when 项目分身 does.

### D6 — 分身 classification uses `agent_hosting`; admission never does

分身 means a **platform-hosted** bot. The prototype states the product meaning in
the assistant's own opening message: 「我是本项目的云端 AI…电脑关闭后也可继续服务」.

**Classification (accepted):**

```
agent_hosting != '' AND agent_hosting != 'self_hosted'   → 分身
```

No special case for `none` is needed: retraction is normalized to `""` **before
storage** (`modules/bot_api/register.go:174-177`, `if folded == AgentHostingNone
{ return "", true }`), so the column never holds `none` and `""` covers both "never
reported" and "reported then retracted". The single writer is `bot_api`'s register
(`modules/botfather/db.go` notes 「这里只读」) and it folds to lowercase first, so a
plain comparison is sound. **A test must pin that normalization** — this predicate
becomes silently wrong if `none` ever starts reaching the column.

**Admission is ownership:** `robot.creator_uid = caller`. That is what
「仅可带入自己的分身」 protects, and it is server-owned.

`agent_hosting` may decide a **label**, never a **privilege**. Three comments in the
modules that own the field say so — `modules/botfather/db.go:53-56`
(「自报值，不可用于鉴权」), `modules/botfather/model.go:173-179`
(「调用方不得据此做授权判定」), and `modules/bot_api/register.go:96-100` ("any
holder of the bot's `bf_` token can claim `octo_hosted`", and a whitelist was tried
and rejected because it validates "the value is in the set", never "you are entitled
to it"). Because D5 admits ordinary bots, a bot lying about `octo_hosted` gains only
a badge and a place in the tally, not a seat it could not otherwise have — which is
the only form in which using this field is safe.

Display rules: show the slug **verbatim** alongside `agent_reported_hosting_at`
(absence has two meanings and only the timestamp separates them), and build no
mapping table on either side — `model.go` asks callers not to, so a new host needs
no release.

### D7 — An assistant's group semantics reuse the existing bot rules unchanged

Answered 2026-09-07: 「可以和群聊拉 bot 入群的逻辑一样，个人拉个人的分身入项目群聊，
其他人不能拉取，只能在项目成员列表看到」. Every clause of that already exists:

- **only the owner may pull it in** — `#1182` forced
  `inviter == robot.creator_uid` (`modules/group/db.go:881`);
- **others cannot** — same check, from the other side;
- **it follows its owner out of a group** — `#354` / `#1186`, via
  `QueryBotsInvitedByUIDTx` `FOR UPDATE` in the same transaction
  (`modules/group/db.go:891`);
- **everyone sees it in the project member list** — that is the new part, and it is
  a response field, not a permission.

So P2 adds **no new group-side restriction**. What it adds is the same rule at
project scope: the assistant holds a project member row owned by its human (D5), and
that row closes with its owner's.

Two consequences to pin with tests rather than leave implied:

- an assistant is **admissible** to every project group (I2) but a **member** of
  none by default — I2 is a ceiling, not a floor;
- an assistant must never reach further than its owner. Group admission already
  requires the actor to be the owner, and the owner must hold group-add permission
  there, so this falls out — but it falls out of two separate modules' rules, which
  is exactly the kind of thing that breaks silently when one of them changes.

### D8 — Pending invitations need a new table `octo_project_invitation`

The prototype shows 邀请记录 with 被邀请人 / 邀请人 / 状态 / 操作, and
`join_mode = 1` (invite-only) is already the DDL default with no invitation flow
behind it.

Uniqueness must be "at most one pending invitation per (project, invitee)" while
still allowing invite → decline → invite again. MySQL 8.0 has no partial index, so
use P0's generated-column technique: `active_invitee` equals `invitee_uid` while
pending and NULL otherwise, with a unique key over `(project_id, active_invitee)` —
the same construction as `octo_project.active_name`, and for the same reason. A
plain unique key would permanently block re-invitation, which is the shape of the
`joinPresetGroups` defect P0's comment cites (`modules/space/api.go:1366`).

Accepting an invitation goes through the same `AdmitOrRestoreMemberTx` entry as
every other admission. It is not a second write path.

### D9 — P2 adds exactly one interface for subsystems: `POST /v1/internal/projects/status`

P1's read contract answers "is this token holder a member" and deliberately folds
"not a member", "no such project" and "another Space" into one `member: false`.
That is right for anti-enumeration and it has a cost: a subsystem cannot tell that a
project was **disbanded**, so it can never reclaim the container it provisioned. That
gap is the direct cause of the "replica with no exit channel" state both subsystems
are in; provisioning per-project containers without fixing it copies the defect one
level down.

- Auth: `X-Internal-Token`, one distinct token per consumer, constant-time compare,
  added to `ValidateNotifyTokenExclusions` — the only place that can see every
  per-route token at once.
- Request: `{"project_ids": [...]}`, at most 50, mirroring `maxVerifyProjectIDs`.
- Response: one answer per id, `active` / `disbanded` / `unknown`. No member data,
  no Space id, no name, no epoch.
- `unknown` covers both "does not exist" and "outside this consumer's grant",
  indistinguishably.
- Its own rate limit — `SharedUIDRateLimiter` cannot apply, there is no login uid.

It does not wait for a named consumer, unlike an epoch-polling endpoint: its volume
is estimable (consumers × reclaim period, measured in hours), the capability is
missing today rather than speculative, and it serves the one scenario that has no
user token by construction — a scheduled sweep.

### D10 — The all-hands group ships in two levels; only level 1 is in P2

**What P1 already gives us free.** The removal direction is done and thorough.
`detachMemberFromProjectGroups` (`modules/group/project_cascade.go`,
reverse-registered as `group_project_detach`) removes a closing member from **every
group of that project**, so the all-hands group is covered with no new code. It goes
through `RemoveGroupMembers` rather than a flag flip, which also unsubscribes from
IM, emits `CMDGroupMemberUpdate`, cascades to bots that member invited, cleans
`thread_member` / `thread_setting`, clears Space-scoped pinned messages and
conversation extras, and reclaims `is_external_group`.

**What P1 deliberately does not give us.** No add-direction cascade, and I2 does not
ask for one: I2 is a *subset* relation, so it constrains who may enter a group and
says nothing about whether a project member belongs to any group.
`modules/project/cascade_registry.go` has exactly two step kinds,
`MemberRemovalStep` and `DisbandStep`.

#### Level 1 — create the group with the project (in P2, PR-0)

One transaction creates `octo_project`, the creator's `octo_project_member` row, the
`group` row with `project_id` set, and the creator's `group_member` row. All four
tables are in the same MySQL.

The IM side is decided by the existing shape: `addMembersTxWithSpace`
(`modules/group/api.go:1651`) calls `IMAddSubscriber` **inside** the
transaction-scoped function and returns an error on failure (`:1940-1948`), aborting
before the commit. Adding a member to a group is therefore already fail-closed on IM
availability, and PR-0 inherits that rather than inventing a policy: **if IM is
down, project creation fails.** The alternative — commit the project, then create the
group best-effort — was rejected: it produces a project with no all-hands group,
nothing repairs it, and the UI has already told the user the group exists.

The reverse hazard is worth recording even though PR-0 does not introduce it:
subscribe succeeds, then the MySQL commit fails, leaving an IM subscriber with no
`group_member` row. `im-pending-outbox`'s probe against the pinned broker
(`wukongim v2.2.4-20260313`) measured what that state is worth — sending is gated on
`Store.ExistSubscriber` independent of anything octo-server knows, so such a
subscriber can send and receive and is **indistinguishable from a full member**. That
shape already exists at all seven `IMAddSubscriber` sites; PR-0 makes it an eighth,
inside a longer transaction, which widens the window. It does not create the defect
and must not be asked to fix it, but it is why level 2 waits.

Naming: default the group's name from the project name at create, and do **not** keep
it in sync. If the product wants them to look coupled, have the client *display* the
project name for the all-hands group rather than writing it — a server-side rename
would bypass the group's own rename permissions.

#### Level 2 — roster equality (deferred, behind #797 / `im-pending-outbox`)

The property is stronger than I2: the all-hands group's active member set *equals*
the project's active member set, both directions.

The DB half is cheap for the same reason level 1's is: one transaction, one database,
and the add goes through `admitOrRestoreMembersTx` so no new admission path appears.
The IM half is what defers it — every project member add becomes an
`IMAddSubscriber`, so the widened commit-after-subscribe window moves onto the *hot*
membership path, and nothing yet detects or repairs a leak in either direction at
project granularity. P1's journal recorded that gap and assigned it to #797 /
`im-pending-outbox`; level 2 is the first feature that would make it load-bearing.

Level 2 also owes a **reconcile scan in the new direction**: today's scan looks for
I2 violations (a non-member inside a project group); equality needs the opposite scan
(a project member missing from the all-hands group), satisfying
`TestReconcileQueriesAreBounded`.

### D11 — The all-hands group's owner-exit and disband rules

The group's `creator` is the project creator, and `RemoveGroupMembers` silently skips
`role = creator`, so P1's cascade hands the group over to the senior remaining member
who is also an active project member and **detaches it to Space-direct when there is
no such successor**. For an ordinary project group that is right. For a group named
全员群 it would leave a Space-direct group still carrying that name and belonging to
no project. Three cases, three rules:

1. **Voluntary leave — refused; transfer first (D13).** The alternatives silently
   change what other people can see: disbanding destroys the project's main
   conversation over one person's exit, and detaching leaves that misnamed
   Space-direct group reachable under Space rules.
2. **Involuntary removal (Space cascade, admin removal) — cannot be refused**, so
   fall back to P1's existing handover to the senior remaining project member.
3. **No other member exists at all** — P0's ownerless branch is already disbanding
   the project, and the all-hands group is **disbanded with it, not detached.** This
   deliberately differs from D10's general rule: an ordinary project group may have a
   life of its own worth preserving, whereas the all-hands group's entire identity is
   the project.

### D12 — The mapping table records state, and no read path may gate on it

`octo_project_provisioning` is both the outbox and the mapping; the row that drives
the retry is the row that records the outcome. Do not build a second table beside
it.

It stores `project_id`, `target`, `container_id`, `status` and retry bookkeeping.

The trap, and the reason this is written down: **`status = ready` records "we called
successfully once", not "the container exists now."** A container can be deleted,
archived or migrated on the subsystem side and nothing informs octo-server. So **no
read path may gate on this table.** If the 团队文件 tab is shown only when
`status = ready`, one inconsistency locks that tab forever with no user-triggerable
repair. Gate tab visibility on the per-subsystem capability switch; let the target's
`ensure` be what guarantees existence.

What the table buys beyond retry is **local enumeration** — "which projects were ever
provisioned into drive", answerable without asking drive, which reclaim accounting
wants. That justifies keeping rows after `ready`, subject to the caveat above.

Teardown stays pull-based (D9): disbanding a project does not push a delete. The row
moves to `disband_pending` for accounting; the subsystem learns by asking
`POST /v1/internal/projects/status`.

### D13 — Standalone ownership transfer ships in P2

Small (one endpoint, one role swap in a transaction, one epoch bump), shown on the
prototype's owner row, and without it the only ways to change owner are
disband-with-`transfer_to` (`modules/project/model.go:154`) or the Space cascade's
automatic handover. D11 case 1 depends on transfer being the user's way out.

## Preconditions on the subsystems

**P-1 — an `ensure` endpoint keyed on the supplied `container_id`.** Neither
subsystem needs a migration: drive's `drive_space.id VARCHAR(128) PRIMARY KEY` and
fleet's `workspace.slug TEXT UNIQUE NOT NULL` are each already the idempotency
constraint, and fleet's preferred addressing is by slug (`?workspace_slug=` /
`X-Workspace-Slug`, with `GetWorkspaceBySlug` already present), so no one ever needs
its UUID. What remains is the `ensure` wrapper itself — get-first, create,
duplicate-key downgrade. Owner: each subsystem.

**P-2 — a service identity.** Today drive's internal route group has exactly one
route (`sign-download`); fleet has no internal token concept at all, its `bf_` bot
tokens are blocked from the workspace-create route by the mapped-action allowlist,
and the only credential that can create a workspace is a real person's `uk_` API
key. Provisioning under a human's credential would make that human the owner member
of every project's workspace, which is not acceptable. Owner: each subsystem.
**Not met.**

**P-3 — authorization narrowed by Project before a container exists.** **Not met**,
and eager provisioning makes it exploitable rather than merely pending:

`listVisibleInSpace` (`modules/project/db.go:256-272`) filters on
`p.space_id = ? AND p.status = 1 AND (p.discoverability = 0 OR pm.uid IS NOT NULL)`,
and `discoverability` defaults to 0 (space_listed). So **any active Space member
reads `project_id` for every space_listed project in that Space**, member or not
(`my_role = -1`). With a derived container id the chain is: list projects → derive
`p-<project_id>` → call fleet with `?workspace_slug=` → `authorizeWorkspaceSpace`
checks Space membership only → passes → `UpsertMember{Role:"member"}` → the caller is
permanently a member of that project's workspace. `discoverability = unlisted` does
not help; its own column comment says 「只过滤列表/搜索，不是安全边界」.

**R1 (octo-server, ships with PR-5): the container id is not derivable.** D1's opaque
id, held only in the mapping table, disclosed only to project members. The only part
of P-3 that does not depend on another team's schedule. Its limits must be stated
rather than assumed away: this is capability-URL protection — knowing the id grants
access. The id leaks through the target's own responses, browser history, logs and
any shared link, and a member who already materialized keeps their row after leaving
the project. It also reaches Space admins by design, since `role = -1` lets them read
the detail of projects they have not joined. R1 buys time; R2/R3 are the fix.

**R2 (fleet, blocks the 任务 tab):** gate on Space ∧ Project, disable `UpsertMember`
materialization for project-bound workspaces, and resolve role from verify's
`projects[].role`. Under Shape S the provisioning call comes from a service identity,
so no human should be written as workspace owner — create no `member` row at all if
fleet permits it, a system principal if it does not.

**R3 (drive, blocks the 团队文件 tab):** route `assertMember` / `assertEditor` for
project-bound spaces through verify, and do not use `super_admin_uid` for them — it
short-circuits every check, so the project creator would remain a permanent drive
super-admin of that space after leaving the project.

**Rollout sequencing.** Provision containers before R2/R3 land, but do not disclose
their ids until the target has declared project narrowing; keep the tab greyed
meanwhile. Gate that on a **global per-subsystem capability switch**, never on the
per-project mapping row (D12). Add a gauge for containers provisioned while their
target has not yet declared narrowing, so the exposed surface is observable without
waiting on another repo.

## Implementation slices

**P1 is not merged.** It is #846 (OPEN, `CHANGES_REQUESTED`); #844 was closed
unmerged on 2026-09-07 and superseded by it. So the base each slice can build on
matters, and it is not uniform. Verified against this repo at P0 (`#841`, merged):
`group.project_id`, `admitOrRestoreMembersTx`, `cascade_registry.go` and
`removal_worker.go` **do not exist on P0** — they all arrive with P1. What P0 does
have is `createProjectOnce`, `addMembers` (`modules/project/service.go:675`) and
`deactivateMemberTx` (`modules/project/db.go:576`).

| slice | base | why |
|---|---|---|
| **PR-2** internal status endpoint | **PR-5** (corrected 2026-09-07) | its handler reads `octo_project.status` only, so the *endpoint* is P0 — but D9's per-consumer **grant** has no representation on P0, and the acceptance item "byte-identical answers for no-such-project / another-consumer's-project / outside-the-grant" is therefore unsatisfiable there: with no grant, every valid token gets every project's real status. `octo_project_provisioning` is the grant (one row per project per target), so PR-2 stacks on PR-5 |
| **PR-5** provisioning outbox | **P0** | enqueues inside `createProjectOnce`, which is P0's. Touches no group table and no admission entry |
| **PR-1** assistant kind / invitations / transfer | **P0**, with a rebase cost | the member model, `octo_project_invitation` and transfer all sit on P0's `addMembers` / `deactivateMemberTx`. P1 consolidates that path into `admitOrRestoreMembersTx`, so whichever lands second rebases onto the other. The group-interaction acceptance items need P1 |
| **PR-3** existing external member surfaces | **split** | the `project_id` member filters are P0; passing `project_id` through `GET /v1/bot/groups*` needs `group.project_id`, so that part is P1 |
| **PR-0** all-hands group | **P1, hard** | needs `group.project_id`, `admitOrRestoreMembersTx`, the reverse-registration registry, and the removal cascade that walks every project group. Its own acceptance says "through the existing P1 cascade" |
| **PR-6** provisioning backfill / reconcile | **PR-5** (added 2026-09-07) | a bounded scan enqueueing a row for every active project that has none for a currently enabled target, so first enablement and any misconfiguration window converge without a human. Needed for D1's invariant to hold at all; see §Out of scope. Deliberately NOT folded into PR-5: it is a new cross-table scan and owes the same bounds the reconcile job already carries (`LIMIT` + persisted cursor, `TestReconcileQueriesAreBounded`), which is a slice's worth of work rather than a clause |

So PR-5 does **not** depend on PR-0 — an earlier revision of this brief claimed it
did, which was wrong: both enqueue into and extend the same P0 transaction, and they
are independent of each other.

**Recommended order given P1's state:** PR-5 first (pure P0, unblocked today), then
PR-2 on top of it (see the corrected base above), PR-1 next, then PR-0 and PR-3's
group half once P1 merges.

The `capabilities` documentation bug is **not a P2 slice**. It lives in P1's
unmerged code (`modules/user/api_project_context.go`), so it belongs inside #846
while that PR is open — see §Deferred.

### PR-0 — create the all-hands group with the project (D10 level 1)

`createProjectOnce` gains the group creation inside its existing transaction and
after its existing locks, following the order P1 documented. P0 left that middle
elided (`modules/project/db.go:326-327` reads `space_member -> space -> project ->
... -> octo_project_member`); P1 chose `group` and `group_member` for those
positions, so PR-0 adds no new edge to the lock graph.

The group is created through the group module's own path so nothing is
reimplemented: `project_id` set, creator admitted through `admitOrRestoreMembersTx`,
IM channel and subscriber inside the transaction scope exactly as
`addMembersTxWithSpace` does it. `modules/project` must still not import
`modules/group` (`pkg/project/import_guard_test.go` pins that), so this is a third
reverse-registered hook alongside the two in `cascade_registry.go`.

The signature differs from those two and that is not incidental:
`MemberRemovalStep` and `DisbandStep` take `(ctx, payload)` and run in the worker
with their own transactions because they are allowed to be late. A creation step must
run **inside** the caller's transaction, so it takes the `*dbr.Tx`. That is a
different contract with a different hazard — a step holding someone else's
transaction can deadlock it — and the registry's step-contract comment must be
extended to say so rather than letting a reader assume the three are alike.

Also in this slice: D11's owner-exit rules, since case 3 is reachable the moment the
group exists.

Not here: add-direction roster sync (D10 level 2), name propagation (D3), and any
all-hands special case in the removal cascade — the cascade already covers it because
it walks every group of the project.

Observability: the create handler's rejection metric gains a reason for "group
creation failed", so an IM outage shows up as a create failure with a cause rather
than as `respondStoreFailed`.

### PR-1 — assistant member kind, invitations, ownership transfer

Migration in `modules/project/sql/`: add `kind` and `owner_uid` to
`octo_project_member`; create `octo_project_invitation` with D8's generated-column
unique key. No `removable` column (D5).

Code: extend `AdmitOrRestoreMemberTx` to take the kind; ownership validation against
`robot.creator_uid`; the owner-cascade of `assistant_personal` rows inside the
human's removal transaction; invitation create / accept / decline / revoke; standalone
ownership transfer (D13); member-list response fields for the two counts (from
`kind`) and the per-member 分身 badge.

The badge follows `agent_hosting != '' AND != 'self_hosted'` (D6) and carries the
slug **verbatim** plus `agent_reported_hosting_at`. Admission **must not** compare
against `agent_hosting` — a guard test asserts no such comparison on any admission
path.

Source guard: no new I1 exemption. The guard walks `modules/project` and fails if any
admission path reaches an insert without the `space_member` seat check —
tree-walking `filepath.WalkDir` plus allowlist-by-prefix, modelled on
`internal/msgextraseq/source_guard_test.go` (its `allowedDir` const and
`TestNoLegacyMessageExtraGenSeqOutsideAllocator`), not a fixed file list plus
`strings.Contains`.

### PR-2 — `POST /v1/internal/projects/status`

D9's endpoint, its own token per consumer, the `ValidateNotifyTokenExclusions` entry,
its own rate limit (`SharedUIDRateLimiter` cannot apply — there is no login uid),
`pkg/authtree` census registration, and a test that the three refusal cases are
byte-identical.

**Base correction, 2026-09-07.** The last item is what moves this slice behind PR-5.
"Another consumer's project" can only be answered `unknown` if a consumer HAS a
grant, and the only thing that expresses one is `octo_project_provisioning`: a
consumer's grant is the set of projects with a row for ITS target. On P0 there is no
such data, so the honest options were (a) ship with a uniform grant — every valid
token gets every project's real status — and document the acceptance item as
unsatisfied, or (b) stack on PR-5. (b) was chosen: the endpoint gains its whole
reason for existing (a subsystem reclaiming ITS containers) from that table anyway,
and (a) would have shipped a documented weakness for no schedule benefit, since PR-5
is the slice being written first regardless.

### PR-3 — Project dimension on the existing external member surfaces

These already hand Space-wide member data to external principals, so leaving them
alone means the Project write boundary is bypassable over the service-to-service
channel. All additions are **optional** parameters that do not change default
behaviour, or a defect ships with them:

- `GET /v1/bot/space/members` — optional `project_id` filter; also fix the existing
  "no `space_id` implies the first Space" hazard.
- `GET /v1/bot/space/principals/:uid` — keep the existing "resolve one principal
  without revealing why it is absent" posture.
- `GET /v1/space/:space_id/members` — optional `project_id` (both subsystems'
  people-pickers point here).
- `GET /v1/bot/groups` and `GET /v1/bot/groups/:group_no/members` — pass `project_id`
  through read-only, and add pagination, which neither has today.
- `resolve/targets` — close the existing missing-Space-filter hazard.
- `pkg/authtree` census entries for each, including explicit "deliberately
  unconstrained" records.

### PR-4 — withdrawn from P2

The `capabilities` contract bug it carried belongs inside #846, not here. Recorded
in §Deferred so it is not lost if that PR merges without it.

### PR-5 — the provisioning outbox (Shape S, octo-server half only) — IMPLEMENTED 2026-09-07

Ships the whole octo-server side and nothing that discloses a container id.

> **Status: implemented** on branch `feat/project-p2-provisioning-outbox`, based on
> `main` (P0 #841 merged). The deliberate deviations from the sketch above are each
> recorded in
> [context.yaml](./context.yaml) with its reasoning; the two that change externally
> visible behaviour are repeated here so a reader of the brief alone is not misled:
>
> 1. **Enablement is a per-target LIST (`OCTO_PROJECT_PROVISION_TARGETS`), empty by
>    default, and nothing is enqueued for a target that is not on it.** The sketch
>    read as if rows should accumulate before P-2 lands ("the backlog is real work
>    sitting visible"). Narrowed to: a backlog is informative once an operator has
>    ASKED for a target, and is noise before that. Enqueueing unconditionally would
>    put two guaranteed-dead rows per project on every deployment and drive them to
>    `abandoned` — this module's alert state — before anyone requested the feature.
>    The two subsystems are also on different teams' schedules (Q1 recommends fleet
>    first), which a boolean could not express.
> 2. **The `name` sent to a target is a fixed low-information label, not the project
>    name.** D3 makes the name authoritative here and never synced outbound, so a copy
>    on the other side can only drift; and a project name is user-supplied free text,
>    so sending it would make provisioning a content-egress path with its own
>    escaping and disclosure questions. A target that needs the real name reads it
>    from `GET /v1/projects/:project_id`, which is what D3 already tells fleet to do
>    for `context`. `issue_prefix` is likewise sent empty so fleet applies its own
>    default.

Migration: `octo_project_provisioning` per D2. Code: enqueue one row per target
inside the project-create transaction with the opaque `container_id` generated there;
a worker claiming with `FOR UPDATE SKIP LOCKED` under a lease with exponential
backoff and an `abandoned` terminal state that alerts; outbound signing per D2 item
4 with one secret per target registered in `ValidateNotifyTokenExclusions`.

Deliberately not in this slice: any container id in a client response; the
`provisioning:` field; any teardown push (disband moves rows to `disband_pending` and
the subsystem asks PR-2's endpoint).

Observability: a gauge for rows whose target has not yet declared narrowing — the
size of the exposed surface, answerable without another repo.

Depends on P-2 before the worker can succeed against anything. Until then the worker
retries and the gauge reads the full backlog, which is the intended visible state
rather than a silent one.

## Acceptance

**Provisioning**

- No client-facing response contains a subsystem container id while that subsystem's
  capability switch is off. A guard test covers the project detail, list, appconfig
  and verify responses; a log-field guard covers log lines and error details.
- The container id is not a function of `project_id`: a test asserts two projects
  created with known ids do not yield predictable container ids, and that nothing in
  the codebase constructs one from `project_id`.
- A project-create rollback leaves no `octo_project_provisioning` row; a committed
  create leaves exactly one row per target, each with its `container_id` already set.
- The worker is idempotent: replaying a claimed row against a target that already has
  the container converges to `ready` without creating a second container.
- No read path gates on `octo_project_provisioning`. A guard test asserts the table
  is not queried from any handler outside the worker and the display-only accounting
  surface.
- The provisioning secrets are registered in `ValidateNotifyTokenExclusions`; a test
  fails if either equals any other internal token.
- Disbanding a project moves its rows to `disband_pending` and sends nothing
  outbound.
- The outbound fleet/drive HTTP client exists only in the provisioning worker
  package; a guard test asserts no request handler under `modules/project` or
  `modules/user` reaches either subsystem.

**All-hands group**

- Creating a project yields exactly one group with `project_id` set to it, with the
  creator as its only member, in one transaction. A test asserts that a failing
  `IMAddSubscriber` leaves **no** `octo_project`, `octo_project_member`, `group` or
  `group_member` row behind.
- Removing a project member removes them from the all-hands group through the
  existing P1 cascade, with no all-hands-specific code path.
- No test asserts that a project member added *after* creation appears in the
  all-hands group — that is D10 level 2 and deliberately absent. A test pins the
  current behaviour so the gap is recorded rather than assumed.
- D11's three cases each have a test: voluntary leave by the group's owner is
  refused; involuntary removal hands over; a project disband with no remaining member
  disbands the all-hands group rather than detaching it.

**Members**

- A bot admitted as `assistant_personal` holds a real active `space_member` seat; a
  test asserts admission fails when it does not, with no exemption branch.
- Admission is gated on `robot.creator_uid`, never on `agent_hosting`. A guard test
  asserts no admission path compares against `agent_hosting`.
- The 分身 badge follows `agent_hosting != '' AND != 'self_hosted'`, and a test pins
  the upstream normalization it depends on: reporting `none` stores `""`, so a
  retracted bot is not badged.
- The member-list counts derive from `kind`; a test changes a bot's reported
  `agent_hosting` and asserts the counts do not move.
- Removing a human deactivates their `assistant_personal` rows in the same
  transaction, with `member_epoch` incremented exactly once.
- `cleanupSpaceMemberProjects` handles bot rows; a Space removal leaves no orphaned
  assistant.
- An assistant is admissible to a project group only when its owner performs the add;
  a test asserts a non-owner project member cannot add someone else's assistant, and
  another asserts an assistant is a member of no group by default.
- An ordinary bot can be a project member; a test binds a group containing an
  integration bot to a project and asserts the cascade does not evict it.
- `kind = assistant_project` has no writer: a source guard fails if any code path
  writes it.

**Invitations and transfer**

- Invite → decline → invite again succeeds. Two concurrent invites to the same uid
  produce one pending row.
- Accepting an invitation goes through `AdmitOrRestoreMemberTx`; a source guard fails
  if a second admission path appears.
- Ownership transfer moves the owner role in one transaction with one epoch bump, and
  refuses a target who is not an active project member.

**Cross-cutting**

- `POST /v1/internal/projects/status` returns byte-identical responses for "no such
  project", "another consumer's project" and "outside the grant"; a missing or wrong
  `X-Internal-Token` is refused, never allowed through.
- Every route added or touched appears in the `pkg/authtree` census, including the
  deliberately-unconstrained records.
- `make i18n-extract-check` and `make i18n-lint` pass; every new code has a zh-CN
  entry in `pkg/i18n/locales/active.zh-CN.toml`.

## Parked

**The dedicated project assistant (项目分身).** The prototype shows one per project
(`项目分身 · 云端运行 · 不可移除`, posting the opening message in 全员群), but whether
every new project gets one is an undecided product requirement (2026-09-07). P2
therefore declares the `kind` value with no writer and ships no `removable` column.

When it is decided, the questions waiting are: who creates the `robot` row —
octo-server at project-create (one transaction covers identity, `space_member` seat
and project membership, but octo-server then owns a bot identity whose runtime it
does not host) or the subsystem reporting back (needs a new inbound write surface and
leaves a window where the member list shows an assistant-less project); and where its
name and avatar default from. The recommendation on record is octo-server, in the
project-create transaction, with the name defaulted from the project and the avatar
from `pkg/avatarrender` — owning an identity without hosting its runtime is how every
`bf_` bot already works.

## Deferred to sibling briefs

- **The `capabilities` contract bug — belongs in #846, not P2.** The integration
  guide states `capabilities` is an object of six booleans, while #846 ships a string
  array (`["project.read", "project.member.manage", "project.update",
  "project.disband"]` from `projectCapabilitiesForRole`). The six booleans are
  correct for the *project list* endpoint (`modules/project/model.go`) — the error is
  that the verify section points readers at them. Two endpoints, one field name,
  different types: the same trap the guide already warns about for `role` (string at
  top level, int inside `projects[]`), except this one is unwarned and actively
  misdirects. Code wins (a namespaced capability list is right for an authorization
  decision), so the guide is what changes, plus a collision warning. The guide's
  「现在能用什么」 table also predates P1 and still lists the verify `projects` field
  as pending. If #846 merges without this, it becomes a standalone follow-up.
- `project-p2-read-path-hardening` — the `sidebar/sync` backstop and friends. Note
  for whoever writes it: `modules/message/api_sidebar.go:560-578` currently documents
  "IMSyncUserConversation does not return rows for users who aren't current members"
  as a security invariant and explicitly declines a guard as "redundant and would
  cost a per-group channel-access round-trip". The cost claim does not survive
  contact with `ExistMembersActive`, a single batch query already used fail-closed by
  `modules/messages_search/search_global.go:676`. That comment must be rewritten in
  the same PR, or the next reader will remove the guard by following it.
- `project-p2-product-surfaces` — `join_mode = 0` self-join, `is_official`
  management, per-user pinning, `GET /v1/projects/:project_id/groups`.

## Open questions

- **Q1 — Who owns P-2 and P-3 on each subsystem, and by when?** P-1 needs no
  migration, so the blocking pair is the service identity (P-2) and the
  Project-narrowed gate (P-3, i.e. R2 and R3). PR-5 can ship without them, but its
  worker cannot succeed and no tab can be un-greyed until P-2 and the matching R land
  per target.

  Recommendation on sequencing only: **fleet first.** R2 is one predicate plus
  disabling one upsert; R3 changes drive's entire file-authorization model. Ship 任务
  first and leave 团队文件 greyed longer. And file drive's missing `/files/*` Space
  validation as its own defect — it predates Project, and letting Project depend on it
  makes this work hostage to a larger remediation.
