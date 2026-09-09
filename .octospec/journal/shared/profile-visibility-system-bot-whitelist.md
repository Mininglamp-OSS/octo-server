---
type: Journal
title: "Journal: profile-visibility-system-bot-whitelist"
description: The shared person-profile visibility decision now takes its public-bot exemption from the pkg/space.SystemBots whitelist instead of the writable user.category column, so a category=system row that is not a system bot no longer bypasses the relationship check.
tags: ["user", "channel", "auth", "acl", "space", "isolation", "trust-boundary", "security", "wire-contract", "test"]
timestamp: 2026-08-12T12:10:00+08:00
task: profile-visibility-system-bot-whitelist
upstream: internal follow-up to channel-get-object-authz (PR #722)
source: self
---

# profile-visibility-system-bot-whitelist

## What was done

- `PersonProfileInput.SystemAccount` became `SystemBot`, and the two call sites
  (`modules/user/api.go` `u.get`, `modules/channel/api.go`
  `personProfileVisible`) now populate it with `spacepkg.IsSystemBot(uid)`
  instead of `Category == CategorySystem || Category == CategoryCustomerService`.
- The field documentation states the constraint directly: the caller must use
  the whitelist and must not use `category`. The rename exists because the old
  name invited exactly the wiring that caused the defect.
- The decision itself stays in `modules/channel/service` (zero-dependency leaf
  package), so `GET /v1/users/:uid` and the PERSON branch of
  `GET /v1/channels/:id/:type` cannot drift apart.
- Two tests inherited from `channel-get-object-authz` had asserted the defective
  behavior (`category=system` implies a full profile). They were rewritten to the
  whitelist contract, and a reverse regression was added on both endpoints.

## Load-bearing decisions

- `user.category` has exactly two values, `system` and `customerService`
  (`modules/user/const.go`). The prior predicate covered both, so the defect was
  purely over-broad; an earlier claim during review that it also missed
  `service`/`service_notice` categories was wrong and is corrected in the brief.
- The account that made this reachable is the superuser seeded by
  `newManagerSeedModel`: `category=system`, `role=superAdmin`, `robot=0`, and a
  fixed guessable UID. Its build entry is intentionally unchanged, because
  `category` remains legitimate as a display and classification attribute; only
  its use as an authorization input is removed.
- No material disclosure was reproduced, and the change is recorded as a
  narrowing of the authorization input rather than an incident fix. The two
  endpoints do **not** share the same exposure surface, and conflating them was
  the main error in the first drafts of this record, so they are stated
  separately here.
- `GET /v1/users/:uid`: a stranger saw `category=system` as the only populated
  field beyond the minimal set. The reason is not that the row is empty —
  `newManagerSeedModel` writes `Name: "超级管理员"` and `ShortNo: "30000"` — it is
  that `modules/user/api.go:1431-1436` blanks `ShortNo` and `Vercode` unless
  `Follow == 1` or the caller is the target.
- `GET /v1/channels/:id/:type` (PERSON) has **no such blanking**. The response is
  built by `newChannelRespWithUserDetailResp` (`modules/user/1module.go:189-217`),
  where `extraMap["short_no"] = user.ShortNo` is unconditional and nothing in
  `modules/channel/api.go` clears it afterwards. So before this change, a caller
  with no relationship did receive the superuser's `extra.short_no = "30000"`
  along with `online` / `last_offline` / `device_flag` / `sex` / `source_desc` /
  `category`. That surface was missing from the first drafts, which cited the
  `u.get` blanking as if it covered both endpoints.
- Two further claims raised in review were checked and **disproved**, recorded so
  they are not carried forward: `username = "superAdmin"` never reached the wire,
  because `NewUserDetailResp` (`modules/user/service.go:1554-1556`) gates it on
  `m.Robot == 1` and the superuser is `robot=0`, with
  `model.ChannelResp.Username` being `omitempty`; and `extra.vercode` was never
  disclosed to a stranger, because `service.go:509-524` assigns it only when
  `friend != nil && friend.IsDeleted == 0` — and where a friend relation exists,
  the `Followed` leg already granted visibility independently of this predicate.
- The conclusion therefore stands with a corrected basis: phone, email, and zone
  are self-only on both paths via `self := loginUID == m.UID`; the extra field the
  channel endpoint did hand out is a short number hardcoded as a public seed
  value in this open-source repository (`modules/user/api_manager.go:1606`),
  carrying neither PII nor a capability.
- The assertions in the new tests use the JSON-key form (`"short_no":`) rather
  than a bare substring, so they match what the record claims they are: `short_no`
  carries no `omitempty` (`modules/user/service.go:1467`), the channel full path
  inserts the key unconditionally, and neither `minimalUserDetailResp` nor
  `MinimalChannelResp` declares it — the assertion is about response *shape*. The
  looser bare-substring form inherited from `channel-get-object-authz` is left
  untouched in that task's own cases.
- The residual value is defense in depth: the moment anyone seeds a populated
  `category=system` display account, or `customerService` starts being used, the
  old predicate would have silently become a real disclosure surface.
- `fileHelper` is whitelisted but carries `robot=0`
  (`modules/user/sql/20191106000003_user_legacy01.sql`), so it is the correct
  fixture for proving the whitelist leg rather than the robot leg. `u_10000`
  would have passed through the pre-existing `Robot` branch and proven nothing.
- `customerService` rows now fall back to the relationship check. If such an
  account must stay publicly readable, the supported route is the whitelist or
  `robot=1`, not another category shortcut.
- `SystemBots` stays a four-entry hardcoded map. Making it configuration- or
  DB-driven is deliberately deferred; the existing comment in
  `pkg/space/query.go` already reserves that path.

## Verification

- `go build ./...` and `go vet ./modules/channel/service/ ./modules/channel/
  ./modules/user/` pass, the latter compiling the integration test files and so
  confirming no stale references to the renamed field.
- Focused integration runs pass against local MySQL, Redis, and WuKongIM:
  `TestUserGet_SystemBot_Viewable`,
  `TestUserGet_SystemCategoryNotWhitelisted_Minimal`,
  `TestChannelGet_Person_SystemBot_Viewable`, and
  `TestChannelGet_Person_SystemCategoryNotWhitelisted_Minimal`.
- No regression in the authorization matrix established by the prior task:
  `go test ./modules/user/ -run TestUserGet` and `go test ./modules/channel/
  ./modules/channel/service/ -run 'TestChannelGet|TestMinimalChannelResp|TestPersonProfileVisible|TestNewMinimalChannelResp'`
  are green, covering the no-relation, common-group, bot, same-Space,
  cross-Space, and topic branches.
- All focused runs were repeated after rebasing onto `origin/main`, which had
  advanced by `#741` and `#742`; `#741` touches the same file
  (`modules/user/api.go`) in the login-audit region and the rebase was clean.
- Two environment prerequisites surfaced and are not caused by this change: the
  shared `test` schema retains migration rows written by other workspaces and
  needs a drop and recreate with an explicit `utf8mb4_general_ci` collation per
  test package, and the `modules/channel` package requires a 32-byte
  `OCTO_MASTER_KEY` when it mounts `modules/common`.
- `make i18n-extract-check` and `make i18n-lint` were not rerun. No error code,
  `httperr` call site, or locale file is touched, so no new marker or
  translation is expected; CI remains the gate for that claim.

## Rollout and rollback

- No schema change, migration, configuration flag, or environment variable. The
  change is behavioral only and takes effect on deploy.
- The only observable wire change is for callers with no relationship to a
  non-whitelisted `category=system` account: the response narrows from the full
  shape to the established minimal set. That account is not a conversation peer
  and carries no client-rendered business data, so no client impact is expected.
- Rollback is a plain revert of the commit; nothing persists that would need
  repair afterwards.
- Worth watching after deploy: any client error concentrated on profile reads of
  the superuser UID would indicate an unexpected consumer of that full shape.
