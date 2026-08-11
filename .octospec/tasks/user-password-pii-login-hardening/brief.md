---
type: Task
title: "Task: user-password-pii-login-hardening"
description: Enforce account-password policy, add encrypted phone shadow storage, and make login auditing independent of welcome messages
tags: [user, auth, password, pii, encryption, login-audit, rate-limit, migration, i18n]
timestamp: 2026-08-10T00:00:00+08:00
slug: user-password-pii-login-hardening
source: self
---

# Task: user-password-pii-login-hardening

## Goal

Close three security and compliance gaps in the user module:

1. Apply one account-password strength policy to every password creation and
   reset path.
2. Introduce encrypted phone shadow storage and blind-index lookups without an
   unsafe flag-day removal of the existing plaintext column.
3. Record successful and failed login attempts independently of the optional
   welcome-message feature, without persisting raw phone numbers, email
   addresses, or usernames in the login audit fields.

## Background

Password entry points previously enforced inconsistent minimum lengths, phone
numbers existed only in plaintext, and login history was written as a side
effect of sending a welcome message. Existing phone rows require an online,
resumable backfill, so this task is the first stage of a two-stage phone-storage
cutover: new shadow columns become usable now while plaintext remains available
for compatibility and rollback.

## Load-bearing list

- `auth` — registration, password reset, manager account creation, and login
  all handle account credentials or issue sessions.
- `password-policy` — account passwords must be 8 or more Unicode code points,
  no more than bcrypt's 72-byte input limit, and contain at least two character
  classes. Existing MD5-to-bcrypt migration remains compatible with legacy weak
  passwords after a successful login.
- `pii-encryption` — phone ciphertext uses AES-256-GCM with a versioned,
  length-prefixed plaintext format and equality lookup uses an independently
  versioned HMAC-SHA256 blind index derived from the dedicated
  `OCTO_PII_ENCRYPTION_SECRET`.
- `degraded-write` — when the PII key is missing or invalid, a non-empty phone
  write stores the plaintext column and leaves the three shadow columns empty
  rather than failing the request, so phone registration — the product's primary
  registration path — survives a deployment that has not provisioned the key yet.
  A degraded row is shape-identical to a legacy un-backfilled row, so the pending
  count includes it and the backfill repairs it once the key is configured.
  A write that fails while the key *is* present remains fail-closed: that is an
  internal error, not a configuration gap.
- `migration` — four additive migrations introduce phone shadow columns,
  login-audit fields and indexes, user login/lookup indexes, and sufficient
  ciphertext width for the existing utf8mb4 zone/phone column bounds.
- `backfill` — a superAdmin-only, UID-rate-limited endpoint performs bounded,
  resumable batches and rewrites empty or non-current blind-index versions.
  Compare-and-swap predicates prevent concurrent account destruction or another
  worker from overwriting the selected row.
- `lookup-compatibility` — phone lookups merge blind-index and plaintext results
  during the mixed backfill state so duplicate accounts are not hidden. The user
  lookup compares the earliest candidate from both paths before returning.
  Completed account destruction is excluded even if rollback through an older
  binary left stale shadow data behind.
- `login-audit` — success and failure records contain fully bounded account
  masks plus a blind index, status, type, and a canonical parsed IP. Neither an
  attacker-controlled `@` suffix nor invalid proxy-header text can inject PII
  or make the audit insert exceed its columns. The previous successful login is
  captured before the current row is inserted so welcome-message content stays
  correct.
- `rate-limit` — unauthenticated manager login uses a dedicated strict per-IP
  bucket before failed attempts can create audit rows.
- `wire-contract` — password validation and authentication failures use the
  localized error envelope and preserve anti-enumeration behavior.

## Out of scope

- Removing the plaintext `user.phone` column or decrypting phone ciphertext for
  general read paths; that is a later cutover after backfill convergence.
- A production key-rotation runbook beyond the versioned ciphertext and blind
  index formats introduced here.
- Manager account-scoped lockout or `LoginGuard`; this task adds the required
  strict per-IP protection.
- Forcing already-existing weak passwords to change or rejecting them during
  legacy MD5-to-bcrypt migration.
- Changing device-local chat or lock-screen PIN policy.

## Acceptance

- Every account-password creation and reset path rejects passwords outside the
  shared length and character-class policy with localized errors.
- Missing or invalid `OCTO_PII_ENCRYPTION_SECRET` degrades a non-empty phone
  write to plaintext with empty shadow columns instead of failing it, logs one
  process-level error at startup, keeps the row retrievable through the plaintext
  lookup leg, and lets a later backfill repair it to the current hash version.
- Initial superAdmin creation has no synthetic phone and therefore remains
  available before the PII key is provisioned; its password policy still fails
  closed.
- New phone writes populate ciphertext, blind index, and last-four fields; users
  without a phone remain unaffected.
- The backfill can resume after failed or skipped rows, repairs non-current hash
  versions, reports completion only when no eligible rows remain, and cannot
  resurrect phone data from a destroyed account.
- OIDC and user phone lookups return the union of encrypted-shadow and plaintext
  matches throughout the rollout; a single-result lookup returns the globally
  earliest duplicate across both sources.
- A completed destroyed account with a stale rollback-era blind index cannot be
  returned by phone lookup or continue occupying its released phone number.
- Successful and failed login auditing works when welcome messages are disabled,
  never stores a raw login account or unparsed IP, and preserves the actual
  previous-login welcome message.
- Manager login rejects the sixth immediate request from one IP with HTTP 429
  under the dedicated burst-five strict limiter, before handler or database work.
- Focused unit, integration, race, source-guard, build, i18n, and lint checks pass,
  or an infrastructure-only blocker is documented with its exact error.

## Deployment and rollback

1. Configure the same 32-byte `OCTO_PII_ENCRYPTION_SECRET` on every replica
   before enabling phone-bearing account creation or starting backfill. Without
   it, account creation still succeeds but every new phone is stored in plaintext
   with empty shadow columns, and the backfill endpoint refuses to run — a
   backfill with no key would be meaningless. The startup error log naming the
   variable is the only signal, so treat it as a deployment blocker even though
   it is not a request-path failure. Initial superAdmin creation no longer
   carries a synthetic phone and is independent of this key; `AdminPwd` must
   still satisfy the account-password policy or the seed is rejected.
2. Apply the additive migrations and allow the user-table index build to finish
   during the planned database change window.
3. Deploy the application, then repeatedly call the superAdmin phone-shadow
   backfill endpoint until its status reports `remaining=0`. Rows with empty or
   v1 hashes are selected and rewritten to the current v2 hash/ciphertext format.
4. Monitor phone-encryption errors, backfill progress, failed login volume,
   rate-limit rejections, database latency, and login-log growth.
5. For rollback, stop backfill calls and deploy the previous binary. Keep the
   additive columns and indexes in place: the plaintext phone column is retained,
   and the new login-log columns have backward-compatible defaults. Do not run
   destructive down migrations during an application rollback.
6. Before rolling forward after an older binary has handled account destruction,
   clear shadow data from completed tombstones. Phone lookup already excludes
   these rows, so this cleanup is for PII retention rather than availability:

   ```sql
   UPDATE user
   SET phone_encrypted = NULL, phone_hash = '', phone_last4 = ''
   WHERE is_destroy = 2
     AND (phone_encrypted IS NOT NULL OR phone_hash <> '' OR phone_last4 <> '');
   ```
