---
type: Task
title: "Task: login-audit-ip-spoofing"
description: Make login and OIDC audit IP attribution use the same proxy-aware source as shared rate limiting
tags: [user, oidc, auth, login-audit, rate-limit, trust-boundary, security, dependency]
timestamp: 2026-08-11T17:22:57+08:00
slug: login-audit-ip-spoofing
source: self
upstream: Mininglamp-OSS/octo-lib#119
---

# Task: login-audit-ip-spoofing

## Goal

Prevent a client-supplied leftmost `X-Forwarded-For` value from becoming the
recorded source IP for account creation, login, logout, or OIDC bind events, and
from splitting the OIDC callback failure counter across attacker-selected keys.

Use one proxy-aware client-IP implementation from octo-lib `pkg/wkhttp` for both
shared rate limiting and security-sensitive audit paths. Preserve the current
login response, audit schema, welcome-message ordering, and rate-limit quota
semantics.

## Background

Before this task, octo-server pinned octo-lib at
`v0.0.0-20260629040702-79f78844bfab`. In that version:

- `util.GetClientPublicIP` returns the first non-empty value in
  `X-Forwarded-For`, then `X-Real-Ip`, then `RemoteAddr`.
- wkhttp's private `getClientIP` returns `X-Real-Ip`, otherwise the rightmost
  `X-Forwarded-For` value, otherwise `RemoteAddr`.

The octo-lib export and selector hardening are tracked by
[`Mininglamp-OSS/octo-lib#119`](https://github.com/Mininglamp-OSS/octo-lib/pull/119).
It was squash-merged as `233dd6fcdda60ccf347811d1e7a069f1e9703bf8` after stacked
development against PR head `d1184f63703cb88b9a863e77adcb176c69f19b87`.
octo-server now pins the final merge revision through pseudo-version
`v0.0.0-20260811160929-233dd6fcdda6`, and the focused tests have been rerun
against that revision.

The exported selector deliberately tightens the former private helper. The
rightmost XFF entry is authoritative even when the proxy appends a separate
header field line; `X-Real-Ip` is a fallback only, ambiguous duplicates fail
closed, and every candidate is parsed and canonicalized before it can become a
rate-limit key or audit value. Invalid candidates enter the existing shared
unknown-IP bucket rather than creating attacker-selected Redis keys.

With the current single-proxy CLB model, an attacker can send a forged XFF value
and the proxy can append the real source on the right:

```text
X-Forwarded-For: 203.0.113.10, 198.51.100.24
                 attacker value  actual source appended by CLB
```

`GetClientPublicIP` records `203.0.113.10`; wkhttp's rate-limit helper selects
`198.51.100.24`. `normalizeLoginIP` only parses and canonicalizes an IP string,
so it rejects malformed header text but correctly cannot distinguish a valid
forged IP from a valid trusted value.

Gin `Context.ClientIP()` is not an acceptable replacement in the current
deployment. `wkhttp.New()` creates a Gin engine with the default trust-all proxy
configuration, and neither octo-lib nor octo-server calls
`SetTrustedProxies`. With all proxy addresses trusted, Gin can select the
attacker-controlled leftmost XFF entry.

The user module contains 15 direct security-sensitive
`util.GetClientPublicIP(c.Request)` calls, not 14:

- `modules/user/api.go`: 6
- `modules/user/api_emaillogin.go`: 2
- `modules/user/api_gitee.go`: 2
- `modules/user/api_github.go`: 2
- `modules/user/api_manager.go`: 1
- `modules/user/api_usernamelogin.go`: 2

OIDC contains four more direct calls. They cover the IP stored in `StateData`,
the callback failure guard key, logout audit, and the shared bind-audit helper.
`StateData.IP` also flows through `IssueSessionReq.PublicIP` and
`user.ExternalLoginReq.PublicIP` into the same user `login_log`, so replacing
only the 15 user call sites would leave OIDC login audit spoofable.

## Load-bearing list

- `trust-boundary` — forwarded headers are untrusted unless the direct peer is
  the expected proxy and that proxy owns the header contract. For the immediate
  single-CLB fix, the actual source must be the final XFF entry, whether the CLB
  comma-appends or adds a final field line. `X-Real-Ip` is fallback-only and
  must be overwritten or stripped. Pods must not be reachable through a public
  path that bypasses the proxy.
- `single-client-ip-source` — octo-lib `pkg/wkhttp` exports `ClientIP` from the
  shared rate-limit implementation, validates and canonicalizes its output, and
  the shared rate-limit middleware and audit callers use that same function.
  octo-server must not add another local copy of the header parsing algorithm.
- `user-login-audit` — all 15 direct user-module sources feeding successful or
  failed login audit, account creation audit, scan login, phone verification,
  manager login, and GitHub/Gitee login must use `wkhttp.ClientIP`.
- `oidc-audit` — authorize/callback, logout, and bind audit must use the same IP
  source. The IP propagated from OIDC into user session issuance must therefore
  match the OIDC audit IP for the same request stage.
- `oidc-callback-guard` — the existing failure-only callback guard keeps its
  thresholds, reset behavior, Redis key prefix, and error contract; source-IP
  key derivation changes from spoofable leftmost XFF to the validated,
  canonical value returned by `wkhttp.ClientIP`. A malformed or ambiguous
  header returns an empty value from that selector and must use the guard's
  stable unknown-IP bucket, never bypass the failure counter.
- `audit-data-contract` — `login_log.login_ip` and `oidc_audit_log.ip` schemas
  do not change. `normalizeLoginIP` remains at the user audit insertion boundary
  to enforce valid, canonical IP text and protect the `VARCHAR(40)` field.
- `welcome-message-ordering` — `finishSuccessfulLogin` must continue reading the
  previous successful login before inserting the current row, and the same IP
  selected for the current audit event continues to feed the welcome-message
  path.
- `rate-limit` — the global and strict shared IP middleware must retain their
  quota thresholds, key prefixes, fail-closed unknown-IP bucket, and fail-open
  Redis-error behavior. Valid IPs become canonical bucket keys; malformed,
  ambiguous, or oversized candidates use the existing unknown-IP bucket. This
  task does not introduce a new generic limiter.
- `dependency-rollout` — stacked development used octo-lib PR #119 head
  `d1184f63703cb88b9a863e77adcb176c69f19b87`; the final server dependency must
  resolve to squash-merge commit `233dd6fcdda60ccf347811d1e7a069f1e9703bf8`,
  not the superseded PR-head commit.
- `wire-contract` — no HTTP route, response body, status code, localized error,
  authentication decision, or client-visible field changes.

## Out of scope

- Globally changing either octo-lib or octo-server
  `util.GetClientPublicIP`; non-security display and geolocation consumers keep
  their existing behavior.
- Configuring Gin `SetTrustedProxies` or consolidating every local client-IP
  parser. The repository already has `OCTO_TRUSTED_PROXY_CIDRS` for i18n caller
  resolution, but wiring that policy into wkhttp/Gin and reconciling all parser
  consumers is a separate rollout with its own topology validation.
- Supporting an unverified CDN or multi-proxy topology. The rightmost-XFF rule
  is deliberately tied to the current single-CLB deployment contract.
- Changing the QR login confirmation UI to display IP/device evidence. The
  current qrcode code only documents why this evidence is not shown.
- Fixing the separate live `c.ClientIP()` uses in `modules/messages_search` and
  `modules/usersecret`; track and assess those independently.
- Replacing the independent `modules/incomingwebhook/ratelimit.go` client-IP
  parser. It has different XFF/X-Real-IP precedence and multi-header handling;
  move it to `wkhttp.ClientIP` with dedicated delivery-rate-limit tests in a
  follow-up rather than expanding this login-audit PR.
- Changing OIDC callback guard quotas, introducing a new rate-limit middleware,
  or altering login/account lockout policy.
- Changing empty-IP welcome-message rendering or the stale QR-code IP
  commentary. They are user-experience/documentation follow-ups, not part of
  audit attribution or callback-guard enforcement.
- Database migrations, historical audit-row repair, IP geolocation changes, or
  retroactive attribution of already-spoofed rows.

## Acceptance

### octo-lib

- Export `wkhttp.ClientIP(r *http.Request) string` with Go documentation that
  states the header priority, single-proxy assumption, and the required
  `X-Real-Ip`/XFF ownership and direct-connect controls.
- Shared global and strict IP rate-limit middleware call the exported function;
  no second implementation remains inside wkhttp.
- Unit tests prove all of the following:
  - The final XFF entry has priority over `X-Real-Ip` when both are present.
  - For `X-Forwarded-For: <forged>, <actual>`, the returned value is `<actual>`.
  - A proxy-added second XFF field line is treated as the final entry.
  - Duplicate fallback `X-Real-Ip` fields fail closed.
  - Invalid, oversized, sentinel-colliding, or zoned values fail closed.
  - Valid IPv4 and IPv6 values are canonicalized and IPv4-mapped IPv6 is
    unmapped.
  - a missing forwarded header falls back to IPv4 and IPv6 `RemoteAddr` values.
  - an unusable or non-IP request address returns the empty-string fallback.
- Existing wkhttp rate-limit tests continue to pass without quota, key-prefix,
  fail-open/fail-closed, or error-envelope changes.

### octo-server

- `go.mod` and `go.sum` point to octo-lib squash-merge revision
  `233dd6fcdda60ccf347811d1e7a069f1e9703bf8` containing `wkhttp.ClientIP`, and
  focused tests have passed against it.
- All 15 user-module and four OIDC direct
  `util.GetClientPublicIP(c.Request)` calls in scope use `wkhttp.ClientIP`.
- Directory-level source guards scan every production `.go` file under
  `modules/user` and `modules/oidc` (excluding `_test.go`), detect
  `util.GetClientPublicIP` even when imported with an alias, retain the 15/4
  `wkhttp.ClientIP` call inventory, and require a named, justified exemption
  for any intentional legacy use.
- `normalizeLoginIP` remains in both `recordSuccess` and `recordFailure`; its
  tests continue to cover canonical IPv4/IPv6 and rejection of malformed or
  overlong text. Its comments describe validation as defense in depth rather
  than as a proxy trust decision.
- A representative successful login and failed login test send a forged
  leftmost XFF plus an actual rightmost value and assert that `login_log` stores
  only the actual rightmost IP.
- At least one user `/v1/user/login` router test drives `ServeHTTP` and queries
  `login_log`, proving handler wiring (rather than only the audit DB boundary)
  stores the rightmost XFF value.
- OIDC tests prove that the same header shape supplies the actual rightmost IP
  to `StateData`/audit/session issuance and to the callback failure guard key.
- OIDC callback tests prove an invalid final XFF value still increments the
  stable unknown-IP bucket and reaches the existing callback threshold.
- No user/OIDC error response, login response, route, database schema, or i18n
  catalog changes.
- Focused tests pass in each repository:

  ```bash
  # octo-lib
  go test ./pkg/wkhttp/...

  # octo-server; integration cases require MySQL, Redis, and WuKongIM
  go test ./modules/user/... ./modules/oidc/...
  ```

- Broader `go test ./...` and lint results are recorded before merge, or an
  infrastructure-only blocker is documented with its exact error.

### Deployment gate

- Before production rollout, operations verifies with a request observed at
  the application that the CLB behavior matches both conditions:
  - a client-supplied XFF entry cannot replace the actual rightmost source;
  - a client-supplied `X-Real-Ip` is overwritten or stripped rather than passed
    through unchanged.
- Network policy/security-group evidence confirms that public clients cannot
  connect directly to the Pod/service endpoint and supply trusted headers
  without traversing the CLB.
- If either condition is false, this task is not considered production-complete;
  fix the proxy/header contract or implement explicit trusted-proxy processing
  before relying on the audit IP.

## Deployment and rollback

1. Merge and publish octo-lib PR #119 first. It exports and hardens the shared
   selector; existing rate-limit thresholds, key prefixes, and Redis failure
   behavior remain unchanged, while inconsistent or invalid header values now
   fail into the shared unknown-IP bucket. Replace the temporary PR-head pin
   with the final squash-merge revision before merging octo-server.
2. Complete the CLB header and direct-connect deployment gate before deploying
   the octo-server dependency bump.
3. Deploy octo-server as a normal rolling release. Monitor login-event IP
   cardinality, empty/invalid audit IP rates, OIDC callback rejection counts,
   HTTP 429 rates, and audit insert errors. A sharp shift to CLB/private proxy
   addresses indicates that the assumed header contract is wrong.
4. Roll back by deploying the previous octo-server binary and dependency set.
   No schema or persisted contract rollback is required. The octo-lib commit can
   remain published because dependency consumers opt into it by revision.
5. Do not repair historical audit IPs automatically: the original source cannot
   be reconstructed reliably after attribution was poisoned. Preserve affected
   rows and annotate the incident window operationally if needed.

## Validation record (2026-08-12)

- octo-lib PR #119 was approved with all required CI green and squash-merged as
  `233dd6fcdda60ccf347811d1e7a069f1e9703bf8`. Its latest blocking review finding
  was reproduced RED: a 64 KiB comma-dense XFF allocated 1,056,788 bytes per
  `ClientIP` call before rate limiting. Commit `0878bb2` pins the allocation
  bound and rightmost-IP behavior; commit `d1184f6` replaces the full
  `strings.Split` with suffix-only `strings.LastIndexByte` parsing and is GREEN.
  No explanatory review comment was added.
- octo-lib passes `go test ./...`, the focused `TestClientIP` race run,
  `go vet ./...`, `golangci-lint run ./...`, and `git diff --check` at
  `d1184f6`.
- The octo-server branch pins final pseudo-version
  `v0.0.0-20260811160929-233dd6fcdda6`. Commit `1274394a` is the RED checkpoint:
  in a detached worktree at that commit, the focused user target fails to build
  because the old octo-lib has no `wkhttp.ClientIP`. Commit `e68a52d7` is the
  GREEN production change. With `GOWORK=off`, focused user and OIDC client-IP
  tests pass, including source guards, OIDC state storage, session issuance,
  audit attribution, callback failure limiting, and bind context attribution.
  The focused race run, `go mod verify`, full-repo `go vet ./...`, full-repo
  `golangci-lint run ./...`, and `git diff --check` also pass after switching to
  final merge commit `233dd6f`.
- In isolated schema `octo_login_audit_ip_lusaka_20260811`,
  `TestLoginLog_RecordSuccessAndFailure_Integration` passes against octo-lib
  `233dd6f` and proves successful and failed login rows retain the rightmost XFF
  value. In independent schema `octo_login_audit_oidc_lusaka_20260811`, the
  complete `go test ./modules/oidc/... -count=1` run passes against the same
  octo-lib merge commit.
- The complete user package run is not counted as green. Its filtered failure
  list contains only pre-existing dashboard-reader manager tests; a focused
  rerun fails at test setup with `Error 1054 (42S22): Unknown column 'app_id'
  in 'field list'` while inserting `user.Model`. The isolated schema is missing
  the legacy `user.app_id` column declared by
  `modules/user/sql/20210204000001_user_legacy01.sql`. This is an isolated-schema
  fixture gap, not a login-audit assertion failure; a clean full user/repo run
  remains a pre-merge CI gate.
- Review follow-up RED checkpoint `662383a5` demonstrates the newly reachable
  empty-IP path: three invalid-final-XFF callback failures returned 400 and the
  fourth was incorrectly still 400 instead of 429. It also adds AST-backed
  directory source guards and a real `/v1/user/login` failure-route audit test.
  GREEN checkpoint `0cc82cf9` maps empty selector output to the guard's stable
  `__unknown_ip__` bucket without changing thresholds, window, Redis prefix, or
  reset semantics. The focused OIDC reproducer and user route/source-guard test
  pass; the complete OIDC package passes in
  `octo_login_audit_oidc_lusaka_20260811`.
- The review follow-up also passes focused `-race` runs for those OIDC and user
  targets, `GOWORK=off go vet ./...`, `GOWORK=off golangci-lint run ./...`
  (0 issues), `GOWORK=off go mod verify`, and `git diff --check`.
- The same complete OIDC command against the shared default `test` database is
  not counted green: setup panics at `unknown migration in database` for
  `20191106000001_event_legacy01.sql`. The isolated-schema pass above is the
  relevant package evidence; a clean CI database remains the merge gate.
