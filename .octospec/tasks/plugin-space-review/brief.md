---
type: Task
title: "Task: plugin-space-review"
description: Give octo-marketplace the Space role facts its plugin review workflow needs — space_roles on verify, a single-subject internal role lookup, and role-targeted notify delivery — without ever moving a Space roster across the tenant boundary.
tags: ["space", "notify", "internal-api", "authorization", "rate-limit", "pii", "marketplace", "card-action", "breaking-change"]
timestamp: 2026-09-01T12:00:00+08:00
# --- octospec extension fields ---
slug: plugin-space-review
source: self
---

# Task: plugin-space-review

## Goal

octo-marketplace is adding a Space-level plugin review workflow: a member submits
a plugin for org visibility, and a Space owner/admin approves it either in the
market UI or by clicking a button on an approval card delivered by 通知助手.
Marketplace owns the review state; it does not and must not own Space membership.

This task supplies the three facts marketplace needs from octo-server, and
supplies them without ever handing a Space roster across the tenant boundary:

1. **`space_roles` on token verify** — so the web UI can decide whether to render
   the 组织管理 tab, and marketplace can authorize reviewer-side endpoints for the
   Space the request is for.
2. **A single-subject internal role lookup** — so the HMAC-signed card-action
   callback can re-derive the clicking operator's role instead of trusting the
   `operator_uid` in the payload.
3. **Role-targeted notify delivery** (`target_role: "space_admin"`) — so
   marketplace can say "deliver this card to this Space's admins" without
   learning who they are.

## Background

The first revision of this branch (commit `1c830a5`) took the obvious route and
gave marketplace a roster endpoint, `GET /v1/internal/spaces/:space_id/admins`,
returning `{admins:[{uid, name, role}]}`. Commit `39b016c` deletes it. Two
reasons, both structural rather than cosmetic:

- **It leaked verified legal names cross-tenant.** `name` came from the `user`
  table (with a `user_verification.real_name` fallback). One shared token
  therefore read the real names of any Space's administrators, for every Space in
  the deployment. Marketplace never needed a name — it needed to send a card and
  to check one role.
- **Empty-vs-non-empty was a Space-existence oracle.** A roster for an unknown or
  disbanded Space is empty; a roster for a real one is not. Anyone holding the
  token could enumerate Space ids.

The replacement inverts the direction of the roster: octo-server already owns
`space_member` and already delivers the card, so the producer asks for a
*capability* ("deliver to this Space's admins") rather than for *data* ("tell me
who they are"). It learns afterwards only which uids were actually delivered to,
which it needs anyway and which is strictly less than a roster.

Consumer-side design record: octo-marketplace
`.octospec/tasks/plugin-space-review/brief.md`, divergence item 11.
Wire contract: `docs/space-internal-role-api.md` in this repo.

## Load-bearing list

### `space_roles` on `POST /v1/auth/verify?include=context`

`queryUserSpaceContext` already read `space_member`; it now also selects
`sm.role` and returns a third value. Wire form:
`space_roles: { "<space_id>": <0|1|2> }` — 0 member, 1 admin, 2 owner, the native
`space_member.role` encoding, NOT octo-web's inverse display encoding.

`spaces` and `space_roles` describe the **same membership set**: identical keys,
derived from one row set. The row truncation happens *before* both are built, so
the two can never disagree about which Spaces the caller belongs to — a consumer
that treats `space_roles` as the membership set gets the same answer as one that
reads `spaces`. Omitted (`omitempty`) rather than sent empty when there are no
memberships.

### `GET /v1/internal/spaces/:space_id/members/:uid/role` (NEW)

- Response `{"data":{"role":0|1|2|null}}`. `role` is a **nullable int**, because
  `0` is a real role (member) and would otherwise be indistinguishable from
  absent.
- **The absent answer is byte-identical** for non-member, removed member, unknown
  Space and disbanded Space: all four return `200 {"data":{"role":null}}`. A
  404-vs-200 split, or any distinguishable body, would rebuild the
  Space-existence oracle the roster endpoint was deleted for.
- Lives in `modules/space` (the `space_member` isolation predicates belong here),
  not `modules/internal_resolve` — that package carries route-shape source
  assertions pinning its own routes.
- **Middleware order is load-bearing:** `internal := r.Group("/v1/internal")` is
  built with **no** group middleware, and the concrete GET mounts
  `memberRoleIPLimit → marketplaceInternalTokenMiddleware() → getSpaceMemberRole`
  in that order. Gin combines group handlers ahead of route handlers, so
  attaching auth to the group would abort a tokenless request *before* the
  strict-IP bucket is consumed, leaving the endpoint open to token probing
  throttled only by the global bucket. `route_wiring_test.go` pins this at the
  source level.
- Credential is `X-Internal-Token` (constant-time compare) bound to
  **`OCTO_MARKETPLACE_INTERNAL_TOKEN`**, with a dedicated strict-IP rate-limit
  tag and env knobs sanitized before use (`wkhttp.ParseRPSFromEnv` admits
  NaN/+Inf, which silently *disables* the limiter inside the Redis Lua script).

### `target_role` on `POST /v1/internal/notify`

- `TargetRoleSpaceAdmin = "space_admin"` is the **only** accepted value; an
  unknown value is a 400, never a silent fallback, so a typo cannot widen or
  narrow the audience unnoticed.
- `target_role` and `targets` are mutually exclusive.
- **Only the action capability may use `target_role`.** The endpoint admits three
  credential classes — legacy `NOTIFY_INTERNAL_TOKEN`, docs
  `OCTO_DOCS_NOTIFY_TOKEN`, and the per-route action tokens from
  `OCTO_CARD_ACTION_ROUTES`. `delivered[]` on a role-targeted request *is* the
  Space's admin roster, so admitting the two fixed tokens would rebuild the
  deleted cross-tenant roster capability on a different endpoint: one shared
  credential, up to 200 admin uids per call, any `space_id`, no membership
  relationship required. The gate lives in `notifyCapabilityAllows`
  (`modules/notify/api.go`) beside the existing `Card` / `DocsCard` /
  `ApprovalCard` rules, and the action capability is further narrowed to its
  registered action types by `CanNotify`.
- Recipients resolve through `space.ActiveAdminUIDs` (`space_member status=1 AND
  role>=1`, INNER JOIN an active `space`, **robots excluded**).
- The caller learns the audience only from `delivered[]` in the response. An
  empty `delivered` means the Space has no active human admin — a legitimate
  state, warned not errored.
- **Validation never depends on tenant state.** Payload/card validation runs
  before recipient resolution, so a malformed request is a 400 whether or not the
  named Space happens to have an admin. `prepareApprovalCard` is the single
  source of truth, called both by the pre-resolution gate in `sendNotify` and by
  `deliverApprovalCardNotification`.
- The 200-recipient cap is **truncated, not rejected**, on this path: a producer
  does not choose how many admins an org has, so failing a legitimate
  notification because a Space has 201 admins would be an unfixable denial of
  service. Truncation is logged. `ActorUID` is excluded **before** the cap is
  applied, so the over-fetched row absorbs the actor instead of a slot being
  spent on a uid the delivery path then discards.
- `/notify/batch` does **not** accept `target_role`; a batch carrying one is
  rejected whole, on the **raw** (untrimmed) value so a whitespace-only
  `"   "` cannot slip through as "unset". That is the *only* new rejection rule
  on the batch endpoint: an entry whose `targets` is missing, `null` or an
  explicit `[]` stays a per-item error inside a `207`, byte-identical to before.
- `delivered: []` remains a Space-existence signal for whoever may use
  `target_role`. Narrowed to the action capability, not closed — a deliver
  endpoint cannot answer "who are the admins" without revealing "there are
  none". Recorded in `docs/space-internal-role-api.md` §4.2.1.

### Module direction

`modules/notify` imports `modules/space` for `ActiveAdminUIDs` rather than
restating the `space_member` predicates — those predicates drift the moment they
exist twice, and each drift is a silent authorization bug. The arrow must keep
pointing one way; `TestSpaceDoesNotImportNotifyOrUser` fails with an explanation
before the compiler's "import cycle not allowed" would.

### Token exclusion

`OCTO_MARKETPLACE_INTERNAL_TOKEN` is the one new fixed internal-token env this
task introduces. It is renamed from `OCTO_MARKETPLACE_ADMIN_LIST_TOKEN`, since
the credential no longer authorizes listing anything. Minimum length 32 bytes,
checked before the collision test. One leaked value must never grant two
capabilities.

Two checks, with **different failure strength** — the difference is deliberate
and documented in `docs/space-internal-role-api.md` §5.1 as it actually behaves:

1. **Against the other four *fixed* internal-token envs — non-fatal.**
   `resolveMarketplaceInternalToken` (`modules/space`) refuses to enable the role
   lookup when the value equals `NOTIFY_INTERNAL_TOKEN`,
   `OCTO_DOCS_NOTIFY_TOKEN`, `OCTO_DOCS_BOT_MENTION_TOKEN` or
   `OCTO_DRIVE_INTERNAL_TOKEN`. The token resolves to `""`, the middleware
   401s every request, and the reason is logged as an ERROR naming both env
   names. The process still boots. `modules/notify`, `modules/bot_mention` and
   `modules/internal_resolve` each add the mirror-image branch against
   `OCTO_MARKETPLACE_INTERNAL_TOKEN`, so a shared value fails **both** colliding
   capabilities closed rather than picking an arbitrary winner.

   Scope boundary: only the pairs involving the new env are added. The
   pre-existing pairs among the other four are left exactly as they were —
   this task does not get to turn a currently-serving deployment's notify or
   bot-mention path off as a side effect, and no central "any two fixed tokens
   colliding is fatal" check is introduced.

   Comparison is byte-exact on raw env values in all four modules. No
   normalization is added anywhere: the notify tokens are what
   `internalAuthMiddleware` compares against the request header, and trimming at
   load time would silently redefine what two pre-existing production
   credentials accept. The deployment consequence — a secret mounted from a file
   with a trailing newline does not compare equal to the same secret without one
   — is called out in §5.1.

2. **Against the *dynamic* per-route notify tokens and callback secrets from
   `OCTO_CARD_ACTION_ROUTES` — fatal.**
   `cardactiondispatch.Registry.ValidateNotifyTokenExclusions` (`main.go`) is
   the only place both sets are visible; its error panics
   `installCardActionDispatch`. The marketplace token is passed by qualified
   constant, and `main_wiring_test.go` pins that argument.

## Out of scope

- The plugin review state machine, its endpoints, and the card payload — all
  octo-marketplace.
- A custom terminal-card visual for `owner=marketplace`; the existing
  `StandardActionFinalizer` copy is used as-is.
- Any `target_role` value beyond `space_admin`, and any role-targeting on
  `/notify/batch`.
- Reinstating a roster endpoint in any form.
- The pre-existing `modules/user` scan-login / session-rollout tests, which
  require a live WuKongIM and fail identically on `upstream/main`.

## Acceptance

- `go build ./...` and `go vet ./...` clean; `gofmt` clean on changed files.
- `go test ./modules/space/... ./modules/notify/... -count=1` green, plus the
  repo-root package (`go test .`), `modules/internal_resolve` and
  `modules/bot_mention`. CI recreates the `test` database per package; these
  tests need that fixture DB (`root:demo@tcp(127.0.0.1)/test`).
- Role-lookup tests: auth failures; malformed params; active roles round-trip for
  0/1/2; **absence is indistinguishable** across non-member / removed / unknown
  Space / disbanded Space; the response carries no PII.
- `route_wiring_test.go` pins limiter → auth → handler order, the middleware-free
  `/v1/internal` group, and that the deleted roster endpoint
  (`/spaces/:space_id/admins`, `getSpaceAdmins`,
  `marketplaceAdminListTokenMiddleware`) stays deleted.
- Targeting tests: `target_role` resolves admins and reports them in `delivered`;
  zero admins succeeds with an empty `delivered`; truncation at 200; an invalid
  `target_role` is rejected **without** delivering anything; a batch carrying
  `target_role` fails whole (including a whitespace-only value); a batch entry
  with missing / `null` / explicit-`[]` `targets` stays a per-item error inside a
  `207` with the batch's other entries still delivered; the explicit-`targets`
  path is byte-identical to before.
- Capability tests: `target_role` under the legacy token (plain payload *and*
  borrowed approval card) and under the docs token is rejected with
  `err.server.notify.card_not_allowed`, no roster query, no transport; the same
  body under the action token is delivered.
- Validation-order tests: against a Space with **zero** admins, a missing /
  whitespace-only card title, an untrimmed / blank `space_id`, and an over-long
  custom-actions list are each a 400 with no roster query — paired with a control
  asserting the well-formed body against the same zero-admin Space is a 200.
- Cap-ordering test: 201 admins with the actor inside the pre-cap window
  delivers to all 200 others (199 would mean truncation ran first).
- Boot-config tests: the marketplace token is rejected when it collides with a
  route notify token or a route callback secret, accepted when unique, rejected
  when unset or under length; `main_wiring_test.go` asserts `main.go` still
  passes it into the exclusion gate (the collision tests build their own argument
  list and would otherwise pass after the production argument was deleted).
- Mirror-image exclusion tests: `modules/notify`, `modules/bot_mention` and
  `modules/internal_resolve` each disable their own capability when their token
  equals `OCTO_MARKETPLACE_INTERNAL_TOKEN`, the error names the colliding env and
  never a token value, and the duplicated env-name literal is pinned against
  `space.MarketplaceInternalTokenEnv`. Paired with negative controls asserting
  the **pre-existing** pairs (e.g. notify vs drive, bot-mention vs drive) still
  behave as they did — this task adds no new failure mode to credentials it does
  not introduce.
- Raw-comparison test (`modules/notify`): the value handed to the auth
  middleware is the **raw** env value, so the two pre-existing notify credentials
  keep comparing byte-exactly, and nothing in the resolver normalizes.
- Rate-limit tag uniqueness holds repo-wide (`ratelimit_tags_test.go`).

## Review round 2: divergences from the original design

Recorded because each reverses or narrows something the first round shipped.

1. **`target_role` is capability-scoped** (was: any internal notify credential).
   Both reviewers raised it as a P1. The first round left the roster reachable by
   the legacy and docs tokens, which re-created the very capability the PR
   deleted. Gating on the action capability rather than on a new
   `OCTO_MARKETPLACE_INTERNAL_TOKEN` check is deliberate: that token is not a
   credential this endpoint accepts at all — `modules/notify` resolves only
   `NOTIFY_INTERNAL_TOKEN` and `OCTO_DOCS_NOTIFY_TOKEN`, and treats the
   marketplace token as a *foreign* value to exclude. Requiring it here would
   have broken the marketplace path outright. The marketplace consumer
   authenticates with its per-route action token, so the action capability is the
   accurate boundary.
2. **Targeting-shape validation now runs ahead of the capability gate.** Needed
   to keep the pre-existing `err.shared.param.invalid` contract for "both set" /
   "unknown role" — with the capability gate first, a typo would come back as
   "not allowed", which is both less useful and misleading.
3. **The ApprovalCard document is rendered before `memberCache.verify`**, not
   after. This is the C1 policy the summary and docs card paths already follow
   (`docs/platform-card-base.md` §10); the approval path was the outlier. It also
   makes card-schema rejection independent of whether the recipients happen to be
   members.
4. **Actor exclusion moved before the 200 cap** (🟡 N2). Previously a Space with
   201 admins whose notification was triggered by an admin inside the first 200
   delivered to only 199, while an eligible 201st sat past the cut.
5. **The docs now describe the token-collision behaviour that exists, instead
   of one that does not.** The first round's `docs/space-internal-role-api.md`
   §5.1 was titled "hard failure at startup" and the brief repeated the claim.
   It was false: the per-module resolvers log an ERROR and disable *themselves*,
   and the process boots. A later revision tried to make the claim true by adding
   a central `validateFixedInternalTokenExclusions` in `main.go` that panicked
   on any of the ten pairs drawn from the five fixed internal-token envs. That
   went the wrong way — it gave two credentials this task does not introduce
   (`NOTIFY_INTERNAL_TOKEN` / `OCTO_DOCS_NOTIFY_TOKEN`, whose collision
   `modules/notify` had *deliberately* kept non-fatal so an already-misconfigured
   deployment would not lose the legacy text path) a brand-new way to stop a
   running deployment from booting. It has been removed. §5.1 now states plainly
   that fixed-vs-fixed collision is non-fatal, that the symptom is a permanent
   401 with one ERROR line at startup, and that only fixed-vs-dynamic-route
   collision panics.
6. **The unrelated commit `e3d7ca2`** (`feat(manager): grant expert-market admin
   capabilities to superAdmin`) was rebased off the branch — expert-market
   authorization work that had been pulled on accidentally.

## Review round 3: divergences

Recorded for the same reason as round 2 — each of these reverses something an
earlier revision of this branch shipped, and one of them corrects a claim the
code itself was making.

1. **`/notify/batch` no longer rejects a whole batch over `targets`.** An
   earlier revision preflighted `validateTargeting` per entry and answered 400
   for the whole batch when any entry had no usable `targets`, with a comment
   claiming this "RESTORES the whole-batch 400 that `NotifyReq.Targets` lost
   when its `binding:\"required\"` tag was dropped" and "keeps them
   byte-compatible". Measured against this repo's validator (v10.14.0,
   `TagName("binding")`), the claim was wrong twice:

   - `required` on a slice rejects only `nil`. An explicit `"targets":[]`
     passed binding at every version of this handler.
   - `BatchNotifyReq.Notifications` carries no `dive` tag, and
     go-playground/validator does not descend into slice **elements** without
     one — so `NotifyReq`'s own binding tags were never applied to batch
     entries at all. An entry with no `targets` key also bound cleanly.

   Both shapes therefore reached `deliverNotification` and came back as a
   per-item error inside a `207`, with the batch's other entries delivered.
   That is the endpoint's pre-existing wire contract and it is now preserved
   exactly; `target_role` remains the only new whole-batch rejection, and it is
   compared on the raw value so `"   "` cannot pass as "unset". The
   `err.shared.param.invalid` detail for that rejection now names
   `target_role` rather than `targets`.

2. **No token normalization at all; every module compares raw.** An earlier
   revision had `resolveInternalTokens` trim its own tokens while
   `collidesWithForeignFixedToken` read the foreign env raw — so two envs
   holding one secret with a trailing newline looked like a collision to
   `modules/space` (which disabled the role endpoint) and like two distinct
   values to `modules/notify` (which kept serving): the exact "arbitrary winner"
   asymmetry the mirror-image branches exist to remove. Trimming at load time
   also redefines what two **pre-existing** production credentials accept at
   request time, since the same value is handed to `internalAuthMiddleware` — a
   deployment configured with surrounding whitespace whose client sends the
   byte-exact value would have started getting 401.

   Both halves are resolved the same way: nothing normalizes. `modules/space`,
   `modules/notify`, `modules/bot_mention` and `modules/internal_resolve` all
   compare raw env values on both sides, so they cannot disagree, and the notify
   credentials authenticate byte-for-byte exactly as they did before. The
   residual deployment hazard (a file-mounted secret's trailing newline defeats
   the collision check) is documented in §5.1 rather than papered over with a
   one-sided trim.

3. **The Space-existence signal on the notify path is accepted, not closed.**
   `delivered: []` covers "no such Space", "disbanded" and "no active human
   admin" alike, and every real Space has a `role=2` creator. Scoping
   `target_role` to the action capability narrows who can ask; nothing can
   remove the signal from a *delivery* endpoint without removing the feature.
   Documented in `docs/space-internal-role-api.md` §4.2.1 together with the
   fact that `/v1/internal/notify` carries no strict-IP limiter (pre-existing;
   adding one is a deployment decision left to a human).

4. **Two stale in-code references fixed**: `modules/space/api_internal.go`
   pointed the tag-uniqueness invariant at `main_test.go` (it is in
   `ratelimit_tags_test.go`), and `modules/notify/config.go` described the
   collision set as "four pairs / five" when C(5,2) is ten.
