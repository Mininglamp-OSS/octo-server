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

Add email OTP multi-factor authentication to the system management console login flow. When MFA is enabled, a management-console login must complete the sequence “password verification → challenge → explicit OTP send → OTP verification → final token issuance”; a token must not be issued before a valid management-console MFA flow completes.

## Background

This task protects only the management-console administrator password-login endpoints and their supporting challenge routes:

- `/v1/manager/login`
- `/v1/manager/login/send`
- `/v1/manager/login/resend`
- `/v1/manager/login/verify`

System-setting controls, SMTP validation, administrator email maintenance, challenge lifecycle, atomic OTP consumption, audit records, error codes, Swagger, and regression tests are in scope only when they directly support the management-console MFA flow.

This task does not extend MFA to the ordinary user platform and does not separate ordinary-user tokens from management-console tokens. Ordinary user login, OAuth, QR-code login, password recovery, and other ordinary-platform paths do not enter the management-console challenge/OTP flow. To protect the management-console OTP keyspace, the public `/v1/user/email/sendcode` endpoint accepts only the ordinary `Register`, `EmailLogin`, and `ForgetLoginPWD` code types and rejects `CodeTypeManagerLogin`; this is a boundary check that prevents a public endpoint from interfering with management-console MFA, not ordinary-platform MFA.

## Deployment prerequisite / Security boundary

This PR protects only the management-console password-login flow. It does not change ordinary user authentication endpoints and does not add an MFA assurance claim to tokens. Management authorization continues to rely on the management role carried in the token. Therefore, enabling `login.manager_email_mfa_on` alone does not prevent the same management account from obtaining or recovering management credentials through other authentication or credential-recovery paths.

Before populating an email address for, and enabling MFA for, any `admin`, `superAdmin`, `dashboardReader`, or `marketAdmin` account, the deployment must use a gateway, route isolation, or equivalent access control to prevent those management roles from using the following ordinary-platform paths to issue or recover management credentials:

- `/v1/user/login`
- `/v1/user/usernamelogin`
- `/v1/user/emaillogin`, including OAuth/OIDC and QR-code ordinary-platform login paths
- `/v1/user/email/forgetpwd`, including the ordinary-platform code-sending path used by that recovery flow

These paths are not modified by this PR. The deployment must treat this isolation as a prerequisite for enabling management-console MFA. If the gateway cannot distinguish and block these paths for the target management roles, it must use route isolation that guarantees management roles cannot use them. Otherwise, a management role may bypass the management-console challenge flow or reset its password through the same mailbox and then log in to the console.

## Load-bearing list

- MFA is disabled by default; when the policy is disabled, existing management-console login behavior is preserved.
- If the successfully loaded system-settings snapshot does not configure the MFA switch, treat it as disabled. If the snapshot is unavailable or the switch value is invalid, treat it as unavailable and fail closed; do not fall back to disabled and issue a token directly.
- When MFA is enabled, a successful password check may create only a challenge and must not issue a token directly. A challenge has a 15-minute absolute deadline, and each UID may have only one active challenge at a time.
- An OTP may be consumed only after delivery has explicitly succeeded and its verifiable status has been committed. Expired, replayed, concurrently reused, or account-snapshot-mismatched challenges must not issue a token. Starting a resend invalidates the old code immediately; a failed send must not restore the old code.
- Enabling MFA, modifying SMTP while MFA is enabled, and loading an already-enabled MFA configuration at startup must perform the format, completeness, and real-availability checks required by the current flow. A combined MFA/SMTP update must validate the merged final configuration.
- When another instance observes an effective MFA/SMTP configuration change through automatic reload, it performs one bounded SMTP preflight. Preflight results are published by configuration generation; an asynchronous result for an old configuration must not mark a new configuration ready, and an ordinary reload tick must not send repeated probe mail.
- An instance that observes an effective MFA/SMTP configuration change through an explicit `Reload()` also performs one bounded SMTP preflight. A synchronous preflight already completed by the management-settings write path must not send a duplicate probe.
- If the startup SMTP preflight fails, the service must not panic or automatically disable MFA. It must emit an operational warning, while management-console login remains fail-closed.
- New administrators must provide a valid email address regardless of the current MFA switch state. This ensures that an administrator account cannot become unusable when MFA is later enabled.
- Administrator creation and email-maintenance endpoints perform an application-level check for an email already used by another user and reject an occupied address. This task does not create a database-wide unique index and does not guarantee absolute uniqueness or email-ownership semantics under concurrent writes.
- Ordinary code types continue through their existing ordinary-user business flows; they do not enter the management-console challenge flow. To isolate the management code type, verification codes, send limits, failure counters, and locks are partitioned by code type. This task does not preserve the old shared cross-code-type quota or internal Redis key naming.

## Out of scope

- System-settings availability recovery probing, periodic SMTP health checks, and system-settings self-healing.
- Automatic recovery probing, background retry, or an alerting loop after a startup preflight failure when no configuration change occurs.
- SMTP delivery queues, retry systems, and final delivery confirmation.
- System-settings concurrent-update infrastructure, global locking, and multi-instance consistency.
- Immediate convergence after a successful configuration transaction is followed by a local `Reload()` failure, configuration-infrastructure recovery, and cross-instance consistency. The service only emits a warning and retains the last successfully loaded local snapshot; the existing automatic reload is responsible for retrying later.
- Ordinary user login endpoints, OAuth, QR-code login, password-recovery hardening, and ordinary-platform MFA. Isolation of management roles from these paths is a deployment responsibility and is not implemented by this PR.
- Database-level global uniqueness constraints for administrator email, email ownership, and identity-model semantics. The email-maintenance endpoint exists to repair missing email addresses for legacy administrators and performs an application-level occupancy check.
- Group, space, and market business authorization.
- Role changes, role-cache invalidation, and ordinary-session revocation. MFA only rechecks that the account still has a management role when the challenge is consumed.
- Changes to `octo-lib`.

## Acceptance

- Based on the instance’s last successfully loaded system-settings snapshot: when MFA is disabled, management-console login preserves its existing behavior; when MFA is enabled, no management-console token is returned before OTP verification.
- Only the latest OTP that was explicitly sent successfully and atomically consumed can complete management-console login; an OTP cannot be replayed or consumed concurrently.
- When SMTP configuration is incomplete or the startup preflight fails, management-console login fails closed and must not issue a token directly because preflight failed.
- If a configuration transaction succeeds but the local `Reload()` fails, the service only emits a warning and retains the old snapshot and readiness state. Until the next automatic reload succeeds, the instance is not required to converge immediately to the latest database policy. If the old snapshot still has MFA disabled, management-console login continues under the old disabled policy; this configuration-infrastructure failure is not an MFA-flow acceptance criterion.
- A startup preflight failure produces a warning and does not panic. Continued probing, automatic recovery, and readiness management after that failure belong to system-settings or mail-infrastructure work.
- Creating an administrator through `/v1/manager/user/admin` without a valid email fails. The bootstrap super-admin created during initial startup is a legacy exception and must receive an email through the maintenance endpoint before MFA is enabled.
- Administrator creation and email maintenance fail when the requested email is already used by another user. A database unique index and an absolute guarantee under concurrent writes are not acceptance criteria for this task.
- The public user code-sending endpoint rejects `CodeTypeManagerLogin`; ordinary code types continue through their existing business flows and cannot enter the management-console challenge flow.
- Multi-instance system-settings consistency, recovery probing without a configuration change, and self-healing are not acceptance criteria. This task only accepts one bounded preflight after an instance observes an MFA/SMTP configuration change and fail-closed MFA behavior for the current loaded snapshot.
