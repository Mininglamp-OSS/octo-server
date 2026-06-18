# Journal: member-list-name-fallback (octo-server #344)

## What was done
Fixed the space member list (`GET /v1/space/:space_id/members`) returning an
empty `name` for members whose `user.name` is blank.

- `modules/space/db.go` `queryMembers`: added a read-only `LEFT JOIN
  user_verification uv ON uv.user_id=sm.uid` and selected
  `IFNULL(uv.real_name,'') as real_name`. The Space + bot-ownership WHERE clause
  (`sm.space_id=? AND sm.status=1 AND (r.robot_id IS NULL OR r.creator_uid=?)`)
  is unchanged — no isolation boundary widened.
- `modules/space/model.go`: added `RealName` to `MemberDetailModel` and a
  `DisplayName()` helper implementing the privacy-gated fallback chain:
  `user.name` → `user_verification.real_name` → stable placeholder
  `"User <uid>"`. Never returns `""`; never uses `short_no`/`username`.
- `modules/space/api.go` `listMembers`: maps `m.DisplayName()` into
  `memberResp.name`.
- Tests: `modules/space/db_member_name_fallback_test.go` covers the three
  branches (real_name hit / placeholder / unchanged name) at the DB layer plus a
  pure-function check. `api_test.go` TestMain now creates the `user_verification`
  dep table for the space test binary.

## octospec rules injected (see context.yaml)
- **space-isolation** (load-bearing): verified the fallback only adds a read-only
  join; WHERE/ownership filter untouched.
- **error-handling** (load-bearing): `name` stays a non-null wire-contract
  string; no raw error response introduced; ran `make i18n-lint` +
  `i18n-extract-check` to prove it.
- **testing**: risk-proportional tests via testutil + CleanAllTables.

## Verification
- `go test ./modules/space/...` → PASS
- `go vet ./modules/space/...` → clean
- `make i18n-lint` → OK (no new direct error responses, no unregistered codes)
- `make i18n-extract-check` → exit 0

## Lessons
- The shared `test` MySQL DB carries a `gorp_migrations` ledger from the
  full-server test binary; the space-only test binary panics on "unknown
  migration in database". Resetting the `test` schema fixes it — but a cleaner
  long-term fix would be per-package isolated test schemas.
- `queryMembers` adds a dep table (`user_verification`) the space test TestMain
  must pre-create, mirroring how `user`/`robot`/`group` are seeded. octospec's
  load-bearing "space" tag correctly flagged this as the isolation-sensitive
  surface to guard.
- The placeholder uses `uid` (already in the response payload) deliberately, so
  it leaks no new identifier — the explicit ban is on `short_no`/`username`,
  which are privacy-gated and never read by the fallback chain.
