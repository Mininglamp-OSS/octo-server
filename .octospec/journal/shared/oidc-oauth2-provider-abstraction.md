---
type: Journal
title: "Journal: oidc-oauth2-provider-abstraction"
description: Record of turning modules/oidc into a two-implementation AuthProvider so a plain-OAuth2 enterprise IdP can drive login, plus two token-exchange entries for clients that arrive holding a credential
tags: [auth, trust-boundary, wire-contract, oauth2, oidc, jwt, security, testing, observability]
timestamp: 2026-09-02T00:00:00+08:00
task: oidc-oauth2-provider-abstraction
source: self
---

# Journal: oidc-oauth2-provider-abstraction

## Current design

`modules/oidc` used to be a strict OpenID Connect client: Discovery, a signed
`id_token`, a JWKS, and a spec-shaped `/userinfo`. Pointing it at an IdP that
speaks plain OAuth2 did not degrade — `oidc.NewProvider` failed at boot, the
client stayed nil, and every endpoint answered 500.

The provider is now an interface with two implementations behind one selector:

- `AuthProvider` — `AuthCodeURL` / `Exchange` / `Identity` / `LogoutURL`, plus a
  `Capabilities()` descriptor. **Business branches read capabilities, never the
  kind.** The kind only selects an implementation, labels a metric, and drives
  boot-time config forking.
- `oidcProvider` wraps the existing client and keeps all four original security
  checks (id_token signature, missing-id_token rejection, nonce hand-off,
  userinfo/sub cross-check — the last of which had never been under test).
- `oauth2Provider` implements the plain-OAuth2 IdP: path constants instead of
  Discovery, a vendor envelope around `/userinfo`, an app id in a **path
  segment** for single logout.
- `OCTO_OIDC_PROVIDER_KIND` defaults to `oidc`, so deployed configurations behave
  exactly as before. A test asserts that an explicit `oidc` and an unset value
  agree field by field.

Two exchange endpoints serve clients that complete SSO themselves and arrive with
a credential rather than an authorization code: `/exchange` (upstream
`access_token` → `/userinfo` → session) and `/exchange-jwt` (locally verified
HS256 JWT → session, no outbound call).

## Why the envelope parser is the trust boundary

The plain-OAuth2 path has no signature anywhere. What OIDC gets for free from
`id_token` verification has to be done by hand in exactly one place, and missing
any one item is a login-grade security defect:

1. **`success` + `code`** — failures arrive as HTTP 200, so the status code is
   not a success signal. A missing `success` field is treated as false.
2. **Non-empty subject** — `subject` is `NOT NULL DEFAULT ''` under
   `UNIQUE(issuer, subject)`, so an empty-subject row is a *legal* row. The
   second subject-less login would then resolve onto the first one's uid: account
   takeover, not dirty data.
3. **Issuer injected, never read from the body** — otherwise one upstream config
   change (or a MITM) writes identities into a different namespace and bypasses
   the uniqueness semantics.
4. **No verified-claim fields are declared at all** — the protocol has no
   verified semantics, so `email_verified` / `phone_number_verified` are dropped
   at the JSON layer and the booleans stay false. Autolink is therefore
   fail-closed by construction rather than by a policy check.

## Subject shape guard

`(issuer, subject)` is the identity primary key and is immutable once written.
The vendor documentation contradicts itself about what the subject contains: the
userinfo field table calls it a sub-number with an 18-digit example, while the
quick-start demo comment calls it the employee number — and the employee number
already has its own field, alongside a separate user id.

The evidence favours an internal long id, but the cost of guessing wrong is not
recoverable: **employee numbers are reused between leavers and joiners**, so a
new hire would match a former employee's identity row and log straight into their
account.

So the guard converts an irreversible data problem into a recoverable failure: a
purely numeric subject shorter than ten digits is refused at the trust boundary,
before any row is written. It is deliberately narrow — long numeric ids pass, and
anything containing a non-digit passes, since it cannot be an employee number.
The error carries the length, never the value. No environment variable: this is a
security floor, so relaxing it should leave a code-review trace.

This also removes "capture a real userinfo response first" as a launch blocker.
If the subject really is an employee number, the first login fails loudly with
zero rows written — which is precisely the signal that capture was meant to give.

## Load-bearing details

- **Logout is scoped to the calling device.** The first implementation fed the
  raw token string to `auth.Decode` to read `device_flag`, but a raw token is a
  UUID with no structured fields, so parsing always failed and every logout fell
  through to disconnecting *all* devices. It now reads the cached payload through
  `auth.TokenRecordReader`, matching what the token-invalidation path already did.
- **`device_flag` exists only in v3 session payloads.** The v2 encoder
  serialises just uid and name. On a v2 session the flag can never be resolved
  and logout degrades to kicking every device, so "logout affects only the
  current device" is a **v3-only property**. The degradation direction is safe
  (over-kicking beats a no-op) and is asserted rather than fixed; extending the
  v2 payload belongs to session encoding, not to this module.
- **Both exchange endpoints are unauthenticated and each triggers an outbound
  call or a DB write**, so they carry an endpoint-level `StrictIPRateLimitMiddleware`
  (2 rps / 10 burst) on top of the global IP bucket, and cap request bodies at
  16 KiB. The thresholds are constants, matching how `modules/user` sets
  login/register/sms.
- **Credential leak paths were closed at three points**: `*url.Error` embeds the
  full URL, and this IdP carries `client_secret` / `access_token` in the query
  string, so transport errors are sanitised before they reach a log; the HTTP
  client sets `CheckRedirect: ErrUseLastResponse`, because following a 3xx would
  carry those credentials to the redirect target; and accesslog scrubs `code` /
  `state` in **both** sinks, including the panic-dump writer.
- **The bearer JWT verifier is hand-written (~100 lines, no dependency).** The
  point is not to avoid a library but to pin the algorithm: the classic JWT
  failure is a library that helpfully accepts `alg: none` or lets an RS256 public
  key be used as an HMAC secret. Base64 uses `RawURLEncoding.Strict()` so one
  signature cannot correspond to multiple token strings, `exp` is mandatory, and
  an empty secret is refused (an HMAC under an empty key is computable by anyone).
- **The bearer-JWT issuer namespace is derived, not configured.** `userId` from
  that backend and the IdP's subject are different ID spaces and must not collide
  on `(issuer, subject)`. The namespace is the configured upstream issuer plus a
  `#bearer-jwt` suffix — `#` cannot occur in a valid issuer URI, so the suffix is
  unambiguous. Deriving it inherits per-environment isolation from the upstream
  issuer, which is already mandatory and validated at boot, and removes a whole
  class of "two environment markers disagree" misconfiguration. The derivation
  refuses an empty upstream issuer, an already-suffixed value, and anything
  exceeding the 255-byte column (silent MySQL truncation could otherwise merge
  two issuers, and with them two people).
- **Boot-time config forking fails loud rather than silently ignoring.**
  Operators copy configuration between kinds as a matter of course, and a
  silently ignored setting reads as "logout is configured" when it is not. Setting
  a post-logout redirect without an app id, or an end-session override on a kind
  that derives its logout URL from a path segment, now refuses to start and says
  which key is at fault.

## Gotchas worth remembering

- **`go build ./...` does not compile `_test.go`.** A de-identification rename
  done as a literal string substitution produced an identifier containing a
  hyphen; the entire `modules/oidc` test binary stopped compiling while a
  build-only check stayed green. Any rename needs `go vet` or `go test` before it
  can be called done.
- **A helper can be dead code while its tests pass.** `/exchange-jwt` called the
  generic verifier directly and re-implemented the `userId > 0` constraint inline,
  leaving the wrapper — and the ten cases exercising it — covering nothing that
  runs in production. Grepping for the *call site*, not the definition, is what
  surfaces this.
- **Reaching past an existing abstraction removes the test seam.** `logout` called
  `ctx.QuitUserDevice` directly even though a `sessionKiller` interface already
  existed, so the blast radius of a logout had no injection point. Both "kick all"
  and "kick one" now live on that interface and the tests assert they are mutually
  exclusive — the negative direction is the one that regressed.
- **Fixture values can carry real identifiers.** A shape-guard test used an
  employee number taken from a documentation screenshot watermark. Caught by the
  pre-commit de-identification sweep, not by review.
