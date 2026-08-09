---
type: Task
title: "Task: scan-login-authorization"
description: Add a deployment-wide scan-login switch and require explicit confirmation before one-shot redemption
tags: [auth, security, qrcode, wire-contract, error-response, system-settings, test]
timestamp: 2026-08-09T00:00:00+08:00
slug: scan-login-authorization
source: self
---

# Task: scan-login-authorization

## Goal

Make the server the policy authority for scan login.

Add the DB-backed System Setting `login.scan_enabled`, defaulting to `true` for
backward compatibility, and expose it as `scan_login_enabled` from both
`GET /v1/common/appconfig` response branches. When the setting is `false`, the
server must reject creation, scanning, confirmation, and redemption of scan-login
sessions; status polling must return an explicit `disabled` state.

Harden the authorization flow so scanning a QR code creates only a pending
confirmation. A redeemable authorization is created only after the scanner calls
the authenticated confirmation endpoint. Redemption must be bound to the browser
session's existing `poll_secret` and must be atomic and one-shot.

## Load-bearing list

- `auth` — scan login issues a full user session token; pending, confirmed, and
  consumed states are authentication boundaries.
- `wire-contract` — keep the existing mobile confirmation response field
  `auth_code`; add `scan_login_enabled` and the polling state `disabled` without
  changing unrelated QR-code response shapes.
- `error-response` — disabled entry points use the localized error envelope and
  one registered user error code.
- `system-settings` — `login.scan_enabled` is hot-reloaded from the shared
  snapshot and defaults to enabled when absent.
- `multi-instance` — pending promotion and final consumption use shared Redis;
  atomicity must hold across replicas and concurrent requests.
- `secret-handling` — `poll_secret` remains hashed at rest, is required for
  redemption, and must stay redacted from access and recovery logs.
- `rate-limit` — existing strict per-IP limits on anonymous scan-login endpoints
  remain in place, and non-finite RPS environment values fall back to safe
  defaults instead of degrading the Redis limiter into fail-open behavior.

## State model

1. `loginuuid` creates the QR state and a browser-only `poll_secret`.
2. Scanning creates a pending authorization tied to scanner UID and QR UUID. The
   QR state becomes `scanned` and contains no UID or redeemable credential.
3. `grant_login` verifies the authenticated scanner and atomically promotes the
   pending record into a redeemable authorization. The QR state becomes `authed`.
4. `login_authcode` validates the authorization type and UUID, validates the
   matching `poll_secret`, then atomically consumes the authorization before
   issuing login side effects.

## Out of scope

- Client-side hiding and disabled-state UX; the server contract is delivered
  first for later web and mobile adoption.
- Changes to user-profile, group, or other QR-code types.
- Device/IP display in the mobile confirmation view.
- Local-account login and OIDC policy changes.
- Changes to unrelated authentication-code consumers.

## Acceptance

- `login.scan_enabled` defaults to `true`, accepts a DB override, and is returned
  as `scan_login_enabled` from both appconfig branches.
- With the switch disabled, `loginuuid`, scan-login QR handling, `grant_login`,
  and `login_authcode` are rejected; `loginstatus` returns `disabled` immediately.
- Non-login QR codes are unaffected.
- Scanning alone cannot create or redeem a ready authorization and does not put
  UID or credential material in the scanned polling state.
- Explicit confirmation atomically promotes exactly one matching pending record.
- Missing or wrong `poll_secret` does not consume a ready authorization.
- Correct `poll_secret` permits exactly one atomic consume; concurrent or replayed
  redemption cannot issue a second login.
- Two independently constructed server instances sharing the same Redis permit
  exactly one pending promotion and one session issuance. Every replica can read
  the confirmed state, and an already `authed` state returns immediately instead
  of retaining a 10-second long-poll goroutine.
- Non-finite `DM_API_SCANLOGIN_*_RATELIMIT_RPS` values such as `NaN` and `+Inf`
  are replaced with the corresponding finite default before limiter creation.
- Disabling the switch blocks sessions created before the setting changed while
  it remains disabled.
- Focused Go tests, race tests for the authorization store, i18n extraction/lint,
  formatting, and vet pass, or any infrastructure-only blocker is recorded with
  the exact command and error.

## Client and rollout contract

- Production currently disables local login and registration and uses OIDC as
  the unified authentication flow. Pre-create `login.scan_enabled=false` before
  starting the new binaries so Web does not expose scan login and mobile exits
  the flow on `err.server.user.scan_login_disabled` or polling state `disabled`.
- `POST /v1/user/login_authcode/{code}` now requires the `poll_secret` returned
  by `GET /v1/user/loginuuid`. The `scanned` polling state no longer includes a
  scanner UID; clients must not depend on that field before confirmation.
- The mobile client must present a real user confirmation step before calling
  `grant_login`. The server closes scan-alone redemption and binds redemption to
  the browser session, but does not by itself guarantee protection when a client
  automatically confirms a scan.
- Rolling deployment can invalidate in-flight scan sessions when old and new
  replicas handle different steps. Users should restart the scan flow after the
  deployment window; do not treat old sessions as backward-compatible.
- `poll_secret` remains a query parameter for current CORS compatibility. It is
  scrubbed by application access/recovery logs; ingress, CDN, and APM URL logs
  must also redact it.
- Old binaries ignore the pre-created `login.scan_enabled` key, so use a
  blue/green cutover or
  temporarily block the anonymous scan-login entry points until old replicas
  have drained; do not expose a mixed-version scan-login window.
- Runtime setting changes are immediate only on the replica handling the admin
  write. Peer replicas inherit the existing System Settings auto-reload budget
  of up to 60 seconds. Incident response that requires an immediate global stop
  must also block the scan-login routes at the ingress or restart all replicas.
- QR status push channels remain process-local. A poll already waiting on a
  different replica can wait until the existing 10-second timeout before its
  next shared-Redis read; this affects latency only and does not bypass the
  `poll_secret`, confirmation, or one-shot consume gates. Sticky routing can
  reduce this bounded delay but is not required for correctness.
- Other deployments keep the backward-compatible default
  `login.scan_enabled=true` while clients adopt `scan_login_enabled`, `disabled`,
  and `poll_secret`.
- Roll back operationally by restoring `login.scan_enabled=true`; no schema
  migration is required. Code rollback is safe because the default remains the
  historical enabled behavior.
