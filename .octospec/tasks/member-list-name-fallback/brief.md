# Task: member-list-name-fallback

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

Upstream: octo-server #344 — the space member list (`GET /v1/space/:space_id/members`)
returns an empty `name` for members whose `user.name` column is empty, leaving the
client to render a blank row. There is no display-name fallback.

## Goal
When `user.name` is empty, the space member list must fall back to the member's
`user_verification.real_name`. When both are empty, it must return a stable,
human-readable placeholder instead of an empty string. Members with a real
`user.name` are unaffected. The fallback chain is **privacy-gated**: it must
never expose `short_no` or `username` as a name.

## Background
- Handler: `modules/space/api.go` `listMembers` → `s.db.queryMembers(...)`.
- Data: `modules/space/db.go` `queryMembers` joins `space_member` → `user`
  (`IFNULL(u.name,'')`). `real_name` lives in `user_verification` (cache of
  Aegis identity claims, see `modules/user/db_verification.go`).
- Model: `modules/space/model.go` `MemberDetailModel` (embeds `MemberModel`,
  has `Name`, `Robot`). Response shape: `memberResp` (`uid`,`name`,`role`,
  `robot`,`created_at`). Note `uid` is already returned, so a uid-derived
  placeholder leaks nothing new — but `short_no`/`username` must not be used.
- Issue: octo-server #344.

## Load-bearing list
- **space**: query is scoped by `sm.space_id` + `sm.status=1` and a bot-ownership
  filter (`r.robot_id IS NULL OR r.creator_uid = ?`). The fallback must NOT widen
  the WHERE clause or weaken Space/owner isolation. (rules: space-isolation)
- **wire-contract**: `memberResp.name` is consumed by web + mobile; the field
  must stay a non-null string and never regress a populated name. (rules:
  error-handling — wire-contract/i18n envelope concerns)
- **acl**: bot rows are still filtered by creator ownership; fallback applies to
  the name only, not to row visibility. (rules: space-isolation)

## Out of scope
- OIDC / `user_verification` **write** paths (`modules/user/service.go`,
  `modules/oidc/`) — read-only consumption here.
- Any octo-lib cross-repo change.
- The admin member list (`queryMembersAdmin`) and member search
  (`searchMembers`) — #344 is only the user-facing `listMembers`.
- Using `short_no` / `username` as a name source (privacy-gated, forbidden).
- Avatar / other member fields.

## Acceptance
- `go test ./modules/space/...` passes.
- New tests in `modules/space/` cover:
  1. `user.name` empty + `user_verification.real_name` present → returns `real_name`.
  2. both empty → returns the stable placeholder (never `""`, never `short_no`/`username`).
  3. populated `user.name` → returned unchanged.
- `queryMembers` selects `real_name` via a `LEFT JOIN user_verification`; the
  Space + bot-ownership WHERE clause is byte-for-byte unchanged in intent.
- i18n gate (`make i18n-lint`) and lint pass; no raw error responses introduced.
