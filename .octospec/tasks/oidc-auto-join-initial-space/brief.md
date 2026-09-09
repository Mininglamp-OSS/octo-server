---
type: Task
title: "Task: oidc-auto-join-initial-space"
description: Admin configures one "initial Space"; every account created through OIDC (browser callback, /bind/create, and both token-exchange endpoints) becomes a member of that Space right after the account is created.
tags: ["oidc", "space", "isolation", "system-setting", "idempotency", "observability", "error-response", "testing"]
timestamp: 2026-09-02T08:56:45+00:00
# --- octospec extension fields ---
slug: oidc-auto-join-initial-space
upstream: self
source: self
---

# Task: oidc-auto-join-initial-space

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add **one** admin-tunable system setting, `space.oidc_initial_space_id`
(string, empty = feature off). When it is set, every user account **created**
through the OIDC module is added as an ordinary member (`role=0`) of that
Space immediately after the account and its `user_oidc_identity` row are
persisted. Membership is added with the same side effects as a normal join
(preset groups, default category, `SpaceMemberJoin` event, member-cache
invalidation). Any failure to join is logged and counted but never fails the
login.

Why: users created via SSO belong to no Space today. `POST
/v1/integrations/oidc/exchange` requires an active Space **and** membership
(`modules/integration/api.go:324`), so those users log in successfully and
then fail exchange forever. They have no email/phone and a domain-account
name, so the existing email/phone invite flows cannot reach them; the only
remedy is a manual admin add. This is a `main` defect (browser callback
creates the same stranded accounts); it does not depend on #829.

## Background

- OIDC created accounts at exactly two sites when this brief was written, both
  through
  `user.externalLoginCreate` (`modules/user/external_login.go:154`):
  1. browser callback — `modules/oidc/api.go:652` (`CreateUser: res.IsNew`),
     identity row written at `api.go:~690`, with `recoverFromIdentityRace`
     (`api.go:1094`) re-issuing the loser's session onto the winner uid and
     leaving a **ghost user**;
  2. self-service create — `modules/oidc/bind_service.go:566`, identity row
     written right after `IssueSession`.
  `sync_worker` never creates users.
  **Updated after the merge with #829:** that PR added `POST …/exchange` and
  `POST …/exchange-jwt`, which create accounts through
  `modules/oidc/exchange_complete.go` (`CreateUser: res.IsNew`). They are a
  third creation site and are hooked identically; the acceptance items below
  that name "two entry points" should be read as "every creation site".
- Account creation (`modules/user/api.go:4135 createUserWithRespAndTx`) adds
  system-account friends and fires `EventUserRegister`; no listener writes
  `space_member`.
- The canonical join path is `space.executeJoinSpace` +
  `space.afterJoinSpace` (`modules/space/api.go:1284`, `:1310`): capacity
  check via `atomicAddMemberIfNotFull` (returns `ErrSpaceFull`), then preset
  groups, `ensureDefaultCategoryProvisioned`, `SpaceMemberJoin` event
  (consumed by botfather welcome and notify space-welcome), and
  `event.SpaceMemberCacheInvalidator`. It does **not** check Space status
  and it **reactivates** `status=0` rows. The manager `addMembers` path
  (`api_manager.go:619`) bypasses the event and is not a good template.
- Import graph: `modules/space` imports only `modules/base/*`;
  `modules/user` imports `space`; `modules/oidc` imports `user`. So
  `oidc → space` is cycle-free.
- Setting precedent: `onboarding.space_welcome_space_id`
  (`modules/common/system_setting_schema.go:387`) is a string setting whose
  write is rejected unless `spacepkg.GetSpaceName` returns non-empty, i.e.
  the Space exists and `status=1` (`api_manager_system_setting.go:389`).
  Reads go through the `EnsureSystemSettings` snapshot; peers converge within
  60s. Admin write endpoint: `POST /v1/manager/common/system_setting`
  with `{"items":[{"category":"space","key":"oidc_initial_space_id","value":"<space_id>"}]}`.
- `GET /v1/integrations/oidc/spaces` (`modules/integration/db.go:113`)
  already filters `sm.status=1 AND s.status=1`, so the new member row is
  visible there with no change.
- `oidc` already exposes Prometheus counters with a `result` label
  (`modules/oidc/metrics.go`).

## Design (confirmed with owner)

- **Config**: single key `space.oidc_initial_space_id`, `settingTypeString`.
  Empty string = off. No separate enable flag. Value is a `space_id`, not a
  name (names are not unique; `space_id` is).
- **Write validation**: reject when the Space does not exist or `status!=1`,
  reusing the `GetSpaceName` check; return a registered 4xx code with
  `details.field = "oidc_initial_space_id"`. Empty value always accepted.
- **Hook location**: `modules/oidc`, **after** the identity row is persisted:
  - callback: after the `res.IsNew` identity-insert block succeeds and only
    when the session was **not** race-recovered (ghost users must not join);
  - bind `Create`: after `identity.Insert` succeeds.
  Not in `user.externalLoginCreate` (would join ghosts and run join side
  effects inside/around the create transaction).
- **Join entry**: new exported function in `modules/space`, e.g.
  `AutoJoinInitialSpace(ctx *config.Context, uid, spaceID string) error`,
  that (1) loads the Space and returns a typed "inactive" error unless
  `Status == SpaceStatusNormal`, (2) calls `executeJoinSpace` +
  `afterJoinSpace` semantics with `role=0`, (3) treats `ErrAlreadyMember` as
  success. Approval mode (`JoinMode=1`) is bypassed: an admin-configured
  initial Space is equivalent to an admin force-add.
- **Failure handling**: never affects the login response or the session
  already issued. Log at Warn with `uid`, `space_id`, reason; increment
  `oidc_initial_space_join_total{result=ok|already_member|space_full|space_inactive|error}`.

## Load-bearing list

- `touches: space, isolation` — writes `space_member` rows outside any
  user-driven request; capacity limit (`MaxUsers`) and Space status must be
  honored; must never insert into a disbanded/banned Space; must not
  reactivate removed members.
- `touches: auth` — OIDC login path (`callback`, `/bind/create`) is modified
  after session issue; login success/failure semantics must be unchanged.
  Ghost-user race recovery (`recoverFromIdentityRace`) must not gain a
  member row.
- `touches: error-response, i18n` — new setting-validation error code
  registered in `pkg/errcode`, zh-CN translation, `make i18n-extract-check`
  and `make i18n-lint` pass.
- `system-setting` — new row in `systemSettingSchema` + typed getter; write
  path validation in `updateSystemSettings`; effective value surfaces in
  `GET /v1/manager/common/system_setting`.
- `idempotency` — join is idempotent for the same `(space_id, uid)`; the
  trigger is creation-only, so a second login must not reach the join code.
- `observability` — new Prometheus counter and structured warn logs per the
  existing `modules/oidc/metrics.go` conventions.
- `SpaceMemberJoin` event contract — downstream listeners (botfather, notify
  space-welcome, group cache) receive the same event shape as a normal join.
- `touches: testing` — integration tests need MySQL/Redis/WuKongIM per repo
  convention; OIDC callback tests use `mock_provider.go`.

## Out of scope

- Backfilling **existing** SSO-created users into the Space. They stay as
  they are; admins use `POST /v1/manager/spaces/:space_id/members`.
- Any change to the exchange/spaces endpoints in `modules/integration`, or
  to #829.
- Accounts created by local phone/username/email registration or by
  GitHub/Gitee OAuth — they do not go through the OIDC create sites and are
  not joined.
- "Ensure membership on every login" semantics. A user removed by an admin
  is **not** re-added on later logins; this brief only joins at creation.
- Multiple initial Spaces, per-issuer mapping, role other than `0`, or a
  separate enable/disable flag.
- Migrating manager `addMembers` onto the event-emitting join path.
- Merging/cleaning ghost users produced by the identity race.

## Acceptance

Feature on (`space.oidc_initial_space_id` = active Space S):

1. Browser OIDC callback creates user U → `space_member(S,U)` exists with
   `role=0,status=1`; `GET /v1/integrations/oidc/spaces` returns S;
   `POST /v1/integrations/oidc/exchange {space_id:S}` returns a `uk_` key.
2. `/bind/create` creates user U2 → same membership outcome as (1).
3. Second OIDC login of U (`IsNew=false`) → the join function is not
   invoked (assert via counter unchanged), no new/updated member row, login
   succeeds.
4. Join function called twice for `(S,U)` → second call returns nil, exactly
   one member row (idempotency unit test).
5. Admin removes U from S (`status=0`), U logs in again → row stays
   `status=0`; not re-added.
6. Local phone/username/email registration and GitHub/Gitee OAuth create
   users → no `space_member` row in S.
7. Identity race (two concurrent first-callbacks for the same
   `(issuer,sub)`): only the winner uid gets a member row; the ghost uid has
   none.
8. S has `preset_group_ids` configured → U is also inserted into those
   groups; `SpaceMemberJoin` event row is written for `(S,U)`.
9. S has `join_mode=1` (approval) → U is added directly; no
   `space_join_apply` row is created.
10. S is at `max_users` → login succeeds, no member row, warn log names S,
    counter `result=space_full` +1.
11. S disbanded/banned after configuration → login succeeds, no member row,
    warn log names S, counter `result=space_inactive` +1.

Configuration:

12. Writing a non-existent or disbanded `space_id` →
    `POST /v1/manager/common/system_setting` rejected with the new i18n
    error code, `details.field="oidc_initial_space_id"`; nothing persisted.
13. Writing an active `space_id` → accepted; `GET` shows it as
    `effective_value`; writing `""` → accepted and turns the feature off.

Feature off (empty/unset):

14. All OIDC creation paths behave exactly as today: no `space_member`
    writes, no join-related log lines, counter unchanged. Existing
    `modules/oidc`, `modules/space`, `modules/integration`, `modules/common`
    test suites pass unchanged.

Tooling:

15. `go build ./... && go vet ./...`, `golangci-lint run` on touched
    packages, `make i18n-extract-check`, `make i18n-lint` all pass; new
    handler files (if any) are added to the module's
    `Test<Module>NoLegacyResponseError` guard list.
