---
type: Journal
title: "Journal: channel-get-object-authz"
description: Added object-level authorization to the channel-detail and user-profile endpoints, and the traps found along the way — display-layer datasources are not an authz layer, membership rows outlive disbanded groups, and a not-found branch that a datasource error kept unreachable.
tags: ["space", "isolation", "auth", "acl", "thread", "wire-contract", "sql"]
timestamp: 2026-08-09T05:45:39Z
slug: channel-get-object-authz
---

# channel-get-object-authz

## What was wrong

`GET /v1/channels/:channel_id/:channel_type` (`channelGet`) had **no
object-level authorization**. `loginUID` was passed to the datasource purely as
a render parameter and never used as an authorization subject. Any authenticated
caller could swap `channel_id` and read:

- **GROUP**: name / notice / member count / `space_id` / external-group flag of
  any group they had never joined — the sibling `GET /v1/groups/:group_no`
  (`groupGet`) gated the *same* `GetGroupDetail` behind `ExistMember`, this one
  did not.
- **COMMUNITY_TOPIC**: sub-channel metadata with no parent-group membership
  check (violating the repo's own "thread must verify parent channel access").
- **PERSON**: `short_no` / device flags / last-offline / realname of any user.

Confirmed live against a test environment via a leave-group closure: after
leaving a group, `groups/{gno}` returned 403 while `channels/{gno}/2` still
returned 200 + full detail (`quit:1` — the server knew the caller had left).
Origin: a pentest finding; the report conflated it with a token-revocation
issue, but the evidence was a plain object-level (IDOR) read.

## The fix

Per-`channel_type` authorization in the handler (the datasources stay a pure
display layer):

- GROUP: `ExistMember` up front — non-member *and* missing group both map to one
  `ErrGroupViewForbidden` (no existence oracle).
- COMMUNITY_TOPIC: `ExistMemberActive` on the parent group (read from the
  display payload's `extra.group_no`, so `channel` need not import `thread`).
- PERSON: relationship-graded. Related (self / friend / same-Space / common
  group / bot / system / webhook) → full detail; unrelated → a minimal DTO. Not
  a 403: this endpoint is the sole data source for rendering arbitrary message
  senders, so a hard reject would break avatar/name rendering for external-group
  members from other Spaces.

`GET /v1/users/:uid` is the same root cause reached through a second door — same
login-only gate, same `GetUserDetail` call with no relationship check — so it is
fixed here too rather than left as a documented-but-open sibling. The decision
lives in `modules/channel/service` (a dependency-free leaf package that
`modules/user` already imports), so the two endpoints cannot drift apart; if they
had each kept their own copy, relaxing one would silently reopen the other. The
profile endpoint's minimal set deliberately **keeps** `follow` where the channel
one omits it: a profile page needs `follow == 0` to render the add-friend entry,
while a sender-rendering endpoint must not hand out a relationship verdict at
all. `modules/user` cannot import `modules/group` (group imports user), so the
common-group lookup arrives via a registration hook, mirroring the existing
`RegisterGroupMemberChecker`; an unregistered hook fails closed.

Also: mounted `SharedUIDRateLimiter`, routed all rejections through the
`httperr` envelope, and fixed a nil-panic (missing group → 500 empty body) that
was itself an existence oracle.

## Structural learnings & gotchas

- **A display-layer datasource is not an authz layer.** `BussDataSource.
  ChannelGet` is shared by push-title fallback, webhook identity, etc.; it
  renders, it must not gate. Authorization belongs in the handler, per type.
- **A membership row outlives its parent.** `disband` only flips
  `group.status`; member cleanup is an async event, so rows linger with
  `is_deleted=0`. A relationship check that queries only `group_member`
  fail-opens on disbanded groups — `ExistCommonGroup` must `JOIN group` and
  exclude the dissolved state. Do not let an async cleanup be an authorization
  precondition. Same shape as the reminder-sync leak: an association table needs
  the parent's predicate. But exclude *only* what the rationale covers — see the
  review follow-up below on why a `status = Normal` whitelist was too strict.
- **A datasource error can strand a handler's not-found branch.**
  `GetUserDetail` returned a plain error for a missing user, so the datasource
  loop hit the legacy `c.ResponseError` and the handler's `ErrUserNotFound`
  branch was dead. Fix: return a sentinel (`ErrorUserNotExist`) and have the
  datasource translate it to `ErrDatasourceNotProcess` so the chain falls
  through to the unified not-found. There was already a zombie
  `ErrorUserNotExist` (defined, never used) — reused it instead of adding a
  second near-identical sentinel.
- **A minimal response needs its own DTO.** `model.ChannelResp` has no
  `omitempty`, so copying four fields still serializes `follow:0` / `status:0` /
  `extra:null` — and `follow:0` reads to clients as "definitely not a friend".
  Use a dedicated whitelist DTO and assert at the JSON key level.
- **PERSON existence can't be unified the way GROUP can.** GROUP folds
  missing+forbidden into one response; PERSON can't, because an
  unrelated-but-existing user must still return name/logo for sender rendering.
  A high-entropy 32-hex uid makes the residual oracle a non-threat; the real
  enumeration surface (`short_no`) never reaches this endpoint.

## Review follow-ups worth keeping

- **A brief is a published artifact.** The first cut of this task's brief named
  the still-unpatched sibling endpoint with its handler and `file:line`, in a
  public repo. That is a precise pointer to a live hole. Either redact, or — as
  here — land the sibling fix so there is nothing to point at. `.octospec/`
  briefs read as internal notes but ship publicly.
- **A whitelist status check is not automatically the safe one.** The first cut
  required `status = Normal`, which also excluded admin-**disabled** groups — a
  distinct, reversible state that every other liveness check in the module
  treats as live (16 call sites test `== GroupStatusDisband`). Matching the
  module's blacklist convention (`<> Disband`) satisfies the same
  disbanded-group rationale without silently dropping mutual visibility for two
  users whose only shared group is temporarily disabled.
- **Copying a constant across a layer you already import is drift for free.**
  The channel-side `iwh_` copy was justified as avoiding a cross-module import,
  but the file already imports `modules/user` and uses two of its constants
  fourteen lines away; `user.WebhookUIDPrefix` exists for exactly this.

## Test notes

Both endpoints had zero authorization coverage before. Added an authorization matrix
(`modules/channel/channel_get_authz_test.go`): group non-member / member /
missing; person no-relation minimal / common-group full / bot viewable /
not-found / disbanded-common-group minimal; plus a JSON-key-level minimal-DTO
assertion, plus a `/v1/users/:uid` matrix (no-relation minimal keeping `follow`,
same-Space, common-group-only, bot, system account, self, not-found) and pure
decision tests in the service package (early-allow identities must not even
reach the common-group query; nil hook and query errors fail closed).

The COMMUNITY_TOPIC route test needs the thread module's datasource registered,
which means `DM_THREAD_ON` must be set **before** the first
`register.GetModules` call (the flag is read inside the module factory, and
factories run once under `once.Do`) — hence a package-level `TestMain` plus a
blank import of `modules/thread` (channel and thread do not import each other).
The positive case asserts a 200 carrying the topic name, so it also pins the
cross-module `Extra["group_no"]` contract the parent-group check depends on; a
forbidden-only test would pass even if the datasource were never registered.

Route now carries `SharedUIDRateLimiter`, so setup deletes this user's
`ratelimit:uid:{uid}` key (the bucket survives `CleanAllTables`; a
`KEYS ratelimit:uid:*` sweep would delete buckets of packages running
concurrently). Test DB needs a drop&recreate between differing module sets
(`unknown migration`).
