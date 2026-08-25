---
type: Task
title: "Task: manager-console-email-mfa"
description: Add email OTP MFA to the system management console login flow.
tags: ["auth", "mfa", "manager-console", "smtp"]
timestamp: 2026-08-22T00:00:00Z
slug: manager-console-email-mfa
upstream: self
source: self
---

# Task: manager-console-email-mfa

## Goal

Add email OTP multi-factor authentication to the system management console
password-login flow. When MFA is enabled, the flow is:

`password verification -> challenge -> explicit OTP send -> OTP verification -> token issuance`.

No management-console token is issued before a valid OTP has been delivered,
recorded as sent, and atomically consumed.

## Scope

This task protects only these management-console endpoints:

- `/v1/manager/login`
- `/v1/manager/login/send`
- `/v1/manager/login/resend`
- `/v1/manager/login/verify`

System-setting controls, SMTP configuration validation, administrator email
maintenance, the challenge lifecycle, atomic OTP consumption, audit records,
error codes, Swagger, and regression tests are in scope only when they directly
support this management-console MFA flow.

This task does not modify ordinary-user authentication or credential-recovery
behavior. It does not separate ordinary-user and management-console token
formats, and it does not add an MFA assurance claim to tokens. The public
`/v1/user/email/sendcode` endpoint rejects `CodeTypeManagerLogin` so an
unauthenticated ordinary-user request cannot interfere with the management OTP
keyspace. Ordinary code types retain their existing ordinary-platform behavior.

## Deployment prerequisite and security boundary

Management authorization continues to rely on the management role carried by
the token. Enabling `login.manager_email_mfa_on` therefore does not, by itself,
prevent a management-role account from obtaining or recovering credentials
through ordinary-platform authentication paths.

Before enabling management-console MFA, the deployment must use a gateway,
route isolation, or equivalent access control to prevent accounts with the
`admin`, `superAdmin`, `dashboardReader`, or `marketAdmin` roles from using
ordinary-platform paths that issue or recover credentials, including:

- `/v1/user/login`
- `/v1/user/usernamelogin`
- `/v1/user/emaillogin`, OAuth/OIDC, and QR-code login paths
- `/v1/user/email/forgetpwd` and its ordinary code-sending path

Those ordinary-platform paths are intentionally not modified by this task.
Blocking them for management roles is a deployment responsibility. Without
that isolation, the deployment must assume that management-console MFA does
not cover those alternate credential paths.

## Configuration ownership and SMTP policy

- `login.manager_email_mfa_on` is stored in `system_setting` and is off by
  default.
- On startup, if the MFA row does not exist, startup writes an explicit `0`
  row. If all three manager-MFA SMTP rows (`support.email`,
  `support.email_smtp`, and `support.email_pwd`) are absent and the static
  configuration contains a complete SMTP setup, startup encrypts and writes
  those defaults to `system_setting`. Existing rows, including explicit empty
  values, are authoritative and are not silently replaced by YAML values.
- Startup and ordinary `Load`/`Reload` operations do not send a probe email and
  do not perform SMTP network I/O. They only load the database snapshot.
- The management MFA send path reads SMTP only from the successfully loaded
  database snapshot. It does not fall back to YAML at login time. If the
  snapshot has never loaded successfully, management MFA remains unavailable
  and fails closed.
- Enabling MFA performs format, completeness, sender-address, and real SMTP
  preflight checks on the merged final MFA/SMTP configuration before the
  transaction commits. A failed check leaves the database unchanged.
- An SMTP update performs the same checks on the merged final configuration
  before committing, whether MFA is currently on or off. A non-clearing update
  must produce a complete, syntactically valid configuration and pass the real
  preflight; a failed check leaves the database unchanged. The only exception
  is an intentional SMTP clear, which is allowed while MFA is disabled and is
  rejected while MFA is enabled.
- If SMTP later becomes unavailable because of a provider outage, network
  failure, account suspension, or another delivery-infrastructure failure, the
  actual OTP send fails and no token is issued. Recovery of that external
  service is an operational responsibility; this task does not implement an
  SMTP health-check, delivery queue, retry system, or settings self-healing.

## MFA and challenge behavior

- When the policy is off, management login preserves the existing direct-token
  behavior.
- When the policy is on, a successful password check creates only a challenge;
  `/v1/manager/login` does not issue a token. The challenge lasts 15 minutes,
  has an absolute deadline, and each UID has at most one active challenge.
- The code is six digits generated with `crypto/rand`, is single-use, and is
  isolated by `CodeTypeManagerLogin` from ordinary code keys, limits, counters,
  and locks.
- Sending is explicit. A resend invalidates the prior code before sending. A
  send attempt has a cooldown, maximum-attempt limit, and bounded in-flight
  lock. SMTP success must be followed by an atomic `sent` state commit; if
  sending or the state commit fails, the code cannot authenticate and the old
  code is not restored.
- Verification atomically consumes the latest sent code and the active
  challenge. Expired, replayed, concurrently reused, or account-snapshot-
  mismatched challenges cannot issue a token. The final account check still
  validates the current password fingerprint, status, email, UID, and
  management role.
- Existing IP rate limiting, localized error envelopes, and audit records are
  preserved. Management MFA send cooldowns use
  `err.server.user.manager_mfa_rate_limited`; verification lockouts use the
  distinct `err.server.user.manager_mfa_verification_locked` code and expose
  the remaining lock time as `details.retry_after`. Ordinary-user rate-limit
  behavior is unchanged.

## Administrator email maintenance

- New management administrators must provide a syntactically valid email even
  when MFA is currently off. This prevents an account from becoming unusable
  when MFA is enabled later. The bootstrap super-admin is a legacy exception
  and must be repaired through the maintenance endpoint before MFA is enabled.
- `PUT /v1/manager/user/admin/email` is a SuperAdmin-only compatibility and
  repair path for legacy administrator accounts. It validates the address,
  checks application-level occupancy, records an audit event, and treats a
  repeated update as success when the target row still matches. A zero-row
  update caused by a missing account or concurrent identity change is reported
  as an error rather than silently returning success.
- This task does not add a database-wide email uniqueness index or define global
  email ownership semantics under concurrent writes.

## Explicitly out of scope

- Ordinary-user login, OAuth/OIDC, QR-code login, password recovery hardening,
  ordinary-platform MFA, token separation, and role authorization changes.
- Group, space, market, and other business authorization behavior.
- Role changes, role-cache invalidation, session revocation, and session-cache
  infrastructure.
- Multi-instance system-setting consistency, concurrent system-setting update
  infrastructure, readiness convergence, recovery probing, periodic SMTP
  health checks, configuration self-healing, and retry loops after an external
  SMTP outage. These are system-configuration or mail-infrastructure concerns.
- SMTP delivery queues, provider retries, final mailbox delivery confirmation,
  and provider rate-limit policy.
- Changes to `octo-lib`.

## Acceptance criteria

- A successfully loaded database snapshot with no MFA row is initialized to
  explicit off. An unavailable snapshot never collapses into off and never
  issues a management token directly.
- Startup, `Load`, and `Reload` do not send probe email. Missing initial rows
  are initialized from the documented database/default rules.
- Enabling MFA and modifying SMTP reject incomplete, syntactically invalid, or
  real-connection-failing configurations before committing them; combined
  updates validate the merged final values. An intentional SMTP clear is
  accepted only while MFA is disabled.
- With MFA on, password login returns a challenge only. Only the latest code
  whose SMTP send and `sent` commit both succeeded can be consumed for a token.
- A later SMTP outage fails the actual send path closed and never issues a
  token; handling recovery of that outage is outside this task.
- Public code sending rejects `CodeTypeManagerLogin`; ordinary code types keep
  their existing behavior.
- New administrators and the legacy email-maintenance path enforce the email
  preconditions needed by the management-console MFA flow.
