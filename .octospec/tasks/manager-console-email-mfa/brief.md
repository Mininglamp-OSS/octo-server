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

No management-console token is issued until the OTP has been accepted by the
configured SMTP server, the corresponding `sent` state has been committed, and
the code and challenge have been atomically consumed. This does not guarantee
final mailbox delivery.

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

This task does not add MFA gating to ordinary-user authentication or
credential-recovery flows. It does not separate ordinary-user and
management-console token formats, and it does not add an MFA assurance claim to
tokens. The public `/v1/user/email/sendcode` endpoint rejects the reserved
`CodeTypeManagerLogin` value so an unauthenticated ordinary-user request cannot
interfere with the management OTP keyspace. Ordinary code types are not routed
through the management challenge flow and retain their existing user-facing
request and verification flow. The shared email-code key helpers are
CodeType-scoped; this namespace isolation is not a claim that every shared
storage detail remains byte-identical for ordinary code types.

Here, “ordinary-user behavior is unchanged” specifically means that ordinary
users do not enter the management-console MFA flow: ordinary-user login,
registration, password recovery, OAuth/OIDC, and QR-code paths do not create a
management challenge, send a management OTP, verify a management OTP, or gain
new management-token semantics from this task. The reserved manager code type
is the explicit exception at the public code-sending boundary. The shared SMTP
configuration, transport, and CodeType-scoped email-code storage are
infrastructure used by multiple mail flows; this task does not turn ordinary
users into MFA users or redefine their ordinary login/recovery flow.

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
- `OCTO_MASTER_KEY` is an upgrade prerequisite and must be configured as the
  existing valid 32-byte deployment key before upgrading. This task does not
  introduce that dependency: earlier versions already require it for
  encrypted application data and key-encrypted system settings. If it is
  missing or changed, encrypted settings cannot be written or decrypted; the
  manager-MFA configuration write and database-snapshot paths that need this
  key fail closed until operations restore it. A missing, invalid, or replaced
  key is an unsupported deployment state and is outside the management-console
  MFA flow.
- On startup, if the MFA row does not exist, startup attempts to insert an
  explicit `0` row. If all manager-MFA SMTP rows are absent and the static
  configuration contains non-empty sender address, SMTP endpoint, and password,
  startup encrypts the password and attempts to insert the complete default set
  into `system_setting`; the password is encrypted before any SMTP seed is
  persisted. The default-off MFA policy row is persisted independently first.
  An encryption failure leaves no partial SMTP bootstrap set, while the
  explicit MFA default row remains durable. Existing rows, including explicit
  empty values, are authoritative for the manager-MFA database snapshot and are
  not silently replaced by YAML values. Seed writes use a database-level
  insert-if-absent operation, so an administrator write racing startup
  initialization cannot be overwritten.
- For manager MFA, the shared SMTP keys `support.email`, `support.email_smtp`,
  and `support.email_pwd` are read from a database-only snapshot. On a fresh
  database, YAML values are copied once when all three rows are missing and
  all three defaults are non-empty; later YAML edits do not override the
  manager-MFA snapshot. The ordinary
  `SupportEmail*()` getters retain their legacy behavior: they use a non-empty
  database value and fall back to YAML when the row/value is absent or empty,
  including when an encrypted value cannot be decrypted. The manager-facing
  GET and SMTP self-test do not use that YAML fallback. This distinction does
  not enroll ordinary users in MFA or change their authentication flow.
- The SuperAdmin SMTP test endpoint uses the same database-backed SMTP snapshot
  as the manager-console MFA send path. It must not report YAML-fallback health
  for a partial database snapshot that MFA cannot use. Ordinary user mail
  continues to use the existing `SupportEmail*()` fallback behavior.
- Existing deployments are an upgrade prerequisite: before upgrading, if
  management MFA is already enabled in the database, the database-backed
  `support.email`, `support.email_smtp`, and required `support.email_pwd` values
  must form a valid SMTP configuration. A partial database set is not silently
  completed from YAML, because existing database rows—including explicit empty
  values—remain authoritative. After loading such an invalid state, startup
  emits an `ERROR` identifying the incompatible MFA/SMTP configuration and
  the normal manager-MFA send path cannot obtain a new code until operations
  repair the database values. Verification still requires a previously sent,
  valid, unconsumed code and the active challenge. This startup check validates
  presence and format only; it does not send probe mail.
- Startup and ordinary `Load`/`Reload` operations do not send a probe email and
  do not perform SMTP network I/O. They only load the database snapshot.
- The management MFA send path reads SMTP only from the successfully loaded
  database snapshot. It does not fall back to YAML at login time. If the
  snapshot has never loaded successfully, management MFA remains unavailable
  and fails closed.
- Enabling MFA performs endpoint, sender-address, and real SMTP preflight
  checks on the merged final MFA/SMTP configuration before the transaction
  commits. The SMTP password is required, consistent with the existing
  deployment configuration contract; an empty password is rejected. A failed
  real preflight leaves the database unchanged for that update request.
- An SMTP update performs the same checks on the merged final configuration
  before committing, whether MFA is currently on or off. A non-clearing update
  must provide a valid SMTP endpoint and sender address and pass the real
  preflight; a failed check leaves the database unchanged for that update
  request. The only exception
  is an intentional SMTP clear, which is allowed while MFA is disabled and is
  rejected while MFA is enabled. If the merged SMTP values are identical to
  the current database snapshot, saving the settings does not send another
  preflight email. Likewise, when manager MFA is already enabled and the
  submitted MFA value remains enabled, saving unrelated settings does not send
  a preflight email or revalidate the operator account.
- If SMTP later becomes unavailable because of a provider outage, network
  failure, account suspension, or another delivery-infrastructure failure, the
  actual OTP send fails closed and no token is issued. Recovery of that external
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
- Existing IP rate-limiting mechanisms, localized error envelopes, and audit
  records are preserved. The manager password endpoint and manager MFA
  sub-endpoints use their respective shared per-IP buckets. Management MFA send
  cooldowns use
  `err.server.user.manager_mfa_rate_limited`; verification lockouts use the
  distinct `err.server.user.manager_mfa_verification_locked` code and expose
  the remaining lock time as `details.retry_after`. Ordinary-user endpoints are
  not routed through the manager MFA buckets; their ordinary user-facing flow
  remains outside this task.

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

- At startup, the missing MFA row is initialized to explicit off when the
  database insert succeeds. A successfully loaded snapshot with no MFA row is
  interpreted as off; an unavailable snapshot never collapses into off and
  never issues a management token directly.
- Startup, `Load`, and `Reload` do not send probe email. Startup attempts to
  initialize missing rows according to the documented database/default rules;
  database or encryption failures are logged and do not create partial SMTP
  bootstrap state.
- An upgrade with an already-enabled manager MFA policy must have a complete,
  valid database-owned SMTP configuration. A partial or invalid database
  configuration is not merged from YAML; startup emits an `ERROR` and the
  management MFA send path remains fail-closed until it is repaired.
- Enabling MFA and modifying SMTP reject missing endpoint/sender values,
  syntactically invalid values, or real-connection-failing configurations
  before committing them; combined updates validate the merged final values.
  An empty SMTP password is invalid. An intentional SMTP clear is accepted
  only while MFA is disabled.
- With MFA on, password login returns a challenge only. Only the latest code
  whose SMTP transaction was accepted and whose corresponding `sent` commit
  both succeeded can be consumed for a token; this does not assert final
  mailbox delivery.
- A later SMTP outage fails the actual send path closed and never issues a
  token; handling recovery of that outage is outside this task.
- Public code sending rejects `CodeTypeManagerLogin`; ordinary code types keep
  their existing user-facing request and verification flow, while their shared
  email-code keys remain CodeType-scoped.
- New administrators and the legacy email-maintenance path enforce the email
  preconditions needed by the management-console MFA flow.
