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
  remain in place.

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
- Disabling the switch blocks sessions created before the setting changed while
  it remains disabled.
- Focused Go tests, race tests for the authorization store, i18n extraction/lint,
  formatting, and vet pass, or any infrastructure-only blocker is recorded with
  the exact command and error.

## Rollout and rollback

- Deploy server code with the default enabled; this preserves existing behavior
  while clients adopt `scan_login_enabled` and `disabled`.
- In deployments that use only external identity login, set
  `login.scan_enabled=false` through the System Setting management API. Replicas
  converge through the existing snapshot reload mechanism.
- Roll back operationally by restoring `login.scan_enabled=true`; no schema
  migration is required. Code rollback is safe because the default remains the
  historical enabled behavior.
