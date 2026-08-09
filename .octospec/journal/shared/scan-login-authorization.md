---
type: Journal
title: "Journal: scan-login-authorization"
description: Record of the deployment-wide scan-login policy gate and explicit, browser-bound, one-shot authorization flow.
tags: [auth, security, qrcode, wire-contract, multi-instance, redis, rate-limit]
timestamp: 2026-08-09T13:25:48+08:00
task: scan-login-authorization
source: self
---

# Journal: scan-login-authorization

## What was done

- Added DB-backed System Setting `login.scan_enabled`, exposed it as
  `scan_login_enabled`, and enforced it at QR creation, scan, confirmation,
  polling, and redemption.
- Split scan from authorization: scan creates a non-redeemable pending record;
  authenticated `grant_login` atomically promotes it only after the scanner is
  verified. Redemption atomically consumes the exact ready record before login
  side effects, so concurrent replicas cannot issue two sessions.
- Bound polling and redemption to a browser-only `poll_secret`, stored as a
  digest and redacted from application access/recovery logs. Unauthorized
  pollers receive only allow-listed public state.
- Added finite-value sanitization to the two scan-login limiter configurations,
  rollback of failed QR-state publication, and an audit warning when a consumed
  authorization does not complete login.
- Documented the client and rollout contract, including process-local status
  push latency, ambiguous Redis outcomes, consume-first response loss, mixed
  versions, the System Settings reload window, and the OIDC-only production
  requirement to keep scan login disabled.

## Verification

- `go test ./modules/user -run 'Test(ScanLogin|LoginWithAuthCode|GrantLogin|GetLogin)' -count=1`
- `go test -race ./modules/user -run 'Test(ScanLoginRedemptionAuditWarnsOnlyWhenLoginIsIncomplete|ScanLoginAuthorization_(PromoteAndConsumeAreAtomic|RollbackPromotionRestoresPending|MultipleReplicasRedeemOnce)|ScanLoginRateLimitsRejectNonFiniteRPSConfig)$' -count=1`
- `go test ./modules/qrcode -count=1`
- `go test ./modules/common -run 'Test(GetAppConfig_ScanLogin|SystemSettings_ScanLogin)' -count=1`
- `go test ./pkg/accesslog -count=1`
- `go build ./...`
- `go vet ./modules/user ./modules/qrcode ./modules/common ./pkg/accesslog`
- `make i18n-extract-check` and `make i18n-lint`

The full `modules/common` package reached MySQL's existing 151-connection limit
in `TestManagerSystemSetting_OrderingRejectsFromThreadSide`. The focused
scan-login common tests pass against a freshly created `test` database using
`utf8mb4_general_ci`; this infrastructure limitation is not reported as a green
full-package run.

## Learnings and gotchas

- Correctness must live in shared Redis state; process-local QR push channels
  are latency optimizations only. An in-flight poll on another replica can still
  wait for its 10-second timeout.
- Consume-first is the safe replay posture, but a downstream failure burns the
  authorization. Clients must restart the complete scan flow rather than retry
  the same credential.
- Redis write errors are outcome-ambiguous. Compensation must handle the case
  where the server committed the QR state before the caller observed an error.
- Test packages with different embedded migration sets must not share one
  concurrently mutated test database. Recreate the `test` schema with
  `utf8mb4_general_ci` between incompatible package runs.

## Residuals and rollout

- `poll_secret` remains in the query string for current CORS compatibility;
  ingress, CDN, and APM URL logs must redact it.
- Other `ParseRPSFromEnv` call sites still need a repository-wide finite-number
  hardening change; this task sanitizes only the two scan-login endpoints.
- OIDC-only production must pre-create `login.scan_enabled=false`. Old binaries
  ignore the key, so mixed-version deployment and rollback require an ingress
  block or blue/green cutover.
