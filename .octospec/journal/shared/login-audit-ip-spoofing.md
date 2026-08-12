---
type: Journal
title: "Journal: login-audit-ip-spoofing"
description: Login and OIDC audit attribution now share octo-lib's validated proxy-aware client-IP selector with rate limiting, while preserving the audit, response, and quota contracts.
tags: ["user", "oidc", "auth", "login-audit", "rate-limit", "trust-boundary", "security", "dependency"]
timestamp: 2026-08-12T00:04:11+08:00
task: login-audit-ip-spoofing
upstream: Mininglamp-OSS/octo-lib#119
source: self
---

# login-audit-ip-spoofing

## What was done

- octo-lib PR #119, squash-merged as `233dd6f`, exports `wkhttp.ClientIP`, makes
  both shared IP limiters use it, selects the proxy-appended rightmost XFF entry,
  validates and canonicalizes candidates, fails malformed or ambiguous input
  into the existing unknown-IP bucket, and bounds suffix parsing without
  splitting an attacker-sized header.
- octo-server replaces all 15 user-module and four OIDC security-sensitive
  `util.GetClientPublicIP(c.Request)` sources with `wkhttp.ClientIP`. This covers
  successful and failed login audit, account creation, logout, OIDC state and
  session propagation, bind audit, and the OIDC callback failure-counter key.
- `normalizeLoginIP` remains at the user audit insertion boundary as defense in
  depth. No route, response, error envelope, schema, quota, or welcome-message
  ordering changed; non-security `util.GetClientPublicIP` callers are untouched.
- Review follow-up closes the malformed-header bypass: when `wkhttp.ClientIP`
  deliberately returns empty for an invalid or ambiguous final header,
  `CallbackGuard` now uses its stable `__unknown_ip__` bucket for check,
  failure-recording, and reset instead of silently treating the request as
  unidentifiable. Its existing threshold/window/prefix and Redis fail-open
  contract remain intact.
- User and OIDC source guards now scan all production Go files in each package
  with AST import-alias handling, preserve the 15/4 `wkhttp.ClientIP` inventory,
  and reserve named exemptions for any intentional legacy use. A real
  `/v1/user/login` `ServeHTTP` test now verifies the persisted failed-login
  audit IP is the proxy-appended rightmost XFF value.

## Load-bearing decisions

- The immediate contract is the current single-CLB topology: the trusted proxy
  must own the final XFF entry and overwrite or strip `X-Real-Ip`; public clients
  must not reach the service directly. `wkhttp.ClientIP` is one parser shared by
  rate limiting and audit, so those paths cannot disagree on the source key.
- Parsing a syntactically valid IP is not a proxy trust decision. The retained
  database normalizer protects field integrity, while CLB header ownership and
  network reachability remain deployment gates.
- This was developed as a stacked change. The server PR opened while octo-lib
  #119 was under review; after the library squash merge, `go.mod` was updated to
  final merge revision `233dd6f` and the focused gates were rerun.

## Verification

- TDD RED: server commit `1274394a` fails in a detached worktree because the old
  octo-lib has no `wkhttp.ClientIP`; GREEN: `e68a52d7` passes the same focused
  user/OIDC targets and the user audit database integration case.
- The complete OIDC package passes against octo-lib merge commit `233dd6f` in an
  isolated schema. Focused user/OIDC tests also pass with the race detector;
  full-repo `go vet ./...`, `golangci-lint run ./...`, `go mod verify`, and diff
  checks pass.
- The complete user-package run is not claimed green: its filtered failures are
  pre-existing dashboard-reader tests whose isolated schema lacks legacy column
  `user.app_id`. A clean CI run remains the pre-merge full-suite gate.
- Review TDD evidence: `662383a5` is RED — three malformed-final-XFF callback
  failures did not consume the callback budget and a fourth request still got
  400. `0cc82cf9` is GREEN — the fourth request receives the established 429
  path. The focused user route/source-guard target passes, and the complete
  OIDC package passes in its independent isolated schema. The equivalent
  default-database OIDC run remains blocked before assertions by stale shared
  migration state (`unknown migration ... 20191106000001_event_legacy01.sql`).
- Focused race runs for the new user and OIDC coverage pass, as do
  `GOWORK=off go vet ./...`, `GOWORK=off golangci-lint run ./...` (0 issues),
  `GOWORK=off go mod verify`, and `git diff --check`.

## Rollout and rollback

- Before production, verify the actual CLB header bytes and direct-connect
  controls. Monitor audit-IP cardinality/empties, OIDC callback rejection and
  HTTP 429 rates; a shift to proxy addresses or the unknown bucket indicates a
  broken topology assumption.
- Roll back the octo-server binary/dependency set. No schema rollback or
  historical audit rewrite is required or safe.
- The independent incoming-webhook IP parser, global Gin trusted-proxy wiring,
  empty-IP welcome-message formatting, and the QR-code commentary remain
  explicitly out of scope for this PR and are documented as follow-ups in the
  task brief.
