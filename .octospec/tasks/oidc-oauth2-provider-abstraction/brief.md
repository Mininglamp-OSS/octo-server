---
type: Task
title: "Task: oidc-oauth2-provider-abstraction"
description: Introduce an AuthProvider abstraction in modules/oidc so a plain (non-OIDC) OAuth2 enterprise IdP can drive login, logout and profile claims; plus an HS256-JWT exchange entry for an upstream business backend that vends its own session JWT to bearer clients.
tags: [auth, trust-boundary, wire-contract, error-response, i18n, observability, testing]
timestamp: 2026-09-01T03:59:42Z
slug: oidc-oauth2-provider-abstraction
upstream: "Mininglamp-OSS/octo-server#830"
source: self
---

# Task: oidc-oauth2-provider-abstraction

## Goal

`modules/oidc` is a strict **OpenID Connect** implementation: it requires
Discovery, a signed `id_token`, and a JWKS key set, and it reads profile claims
from a spec-shaped `/userinfo` response. We need to onboard **two** identity
sources that speak protocols the current code does not handle:

1. An enterprise IdP that speaks **plain OAuth2 `authorization_code` only** — no
   Discovery, no `id_token`, no JWKS, an opaque UUID `access_token`, and a
   `/userinfo` response wrapped in a vendor-specific envelope (`{success, code,
   message, requestId, data:{...}}`). Pointing `DM_OIDC_PROVIDER_ISSUER` at such
   an IdP does not degrade gracefully: `oidc.NewProvider` fails, `o.client` stays
   nil, and every OIDC endpoint answers 500.

2. A bearer client (/ native-client) that completes SSO with the IdP
   itself (via a local HTTP callback server) and receives an **HS256-signed JWT**
   from the native client's own business backend. That JWT is not issued by the enterprise IdP
   and is not verifiable by a JWKS; it must be validated against a shared
   symmetric secret and its claims (`userId`, `domainAccount`) mapped into our
   identity model before we issue a session token.

Introduce an `AuthProvider` interface with two implementations for item 1 (the
existing OIDC one, and one for the plain-OAuth2 IdP), selected by a new
`OCTO_OIDC_PROVIDER_KIND` knob. For item 2, introduce a small standalone HS256 JWT
verification utility and an `/exchange-jwt` endpoint that uses it. Both exchange
endpoints share the `ResolveOrLink` + `IssueSession` tail with the browser
callback path, so the identity-persistence logic lives in exactly one place.

Business branching always reads a declared `ProviderCapabilities` struct, never
the kind — so adding a third IdP later does not mean grepping for `switch kind`.

**This is a refactor of a live authentication path, not a pure addition.** The
standard-OIDC provider is not left untouched: `oidc_client.go` is split behind the
new interface and the ~101-line inline callback sequence at `api.go:504-604`
collapses into one call. Any deployment already running a standard OIDC IdP
therefore executes rewritten login code. Treat "no behaviour change for the OIDC
kind" as an explicit requirement, not a side effect — see Acceptance.

Scope is deliberately **login / logout / profile claims only**. Directory
synchronisation (org tree, headcount feeds) is explicitly out of scope; see
"Out of scope".

One addition to that scope, agreed after the fact: a **standard `Authorization:
Bearer` compatibility shim** for inbound API calls. It is not cosmetic — without
it the integration is broken end to end. Our own `AuthMiddleware` (in the shared
library) only reads a custom `token` header, so a client written against ordinary
OAuth2 conventions authenticates successfully via the IdP, receives our session
token, and is then rejected with 401 on **every subsequent API call**. That
failure only surfaces during integration testing, which is exactly when it is
most expensive to discover.

## Background

The upstream protocol spec is vendor documentation and is **not checked in**
(it carries customer identifiers). A transcription lives in this workspace at
`.context/docs/` (gitignored). The relevant protocol facts:

### Source 1 — Enterprise IdP OAuth2 (plain-OAuth2 provider)

- `GET  {base}/oauth/authorize?response_type=code&scope=read&client_id=&redirect_uri=&state=`
  — `state` is documented as **optional on the IdP side**, no PKCE, no `nonce`.
- `POST {base}/oauth/token` — reference implementation puts credentials in the
  query string (including `client_secret`) with an empty body; the endpoint also
  accepts `Content-Type: application/x-www-form-urlencoded` form bodies (verified
  live during development — we moved to a form body to remove `*url.Error`
  credential leakage but kept credentials off the URL path).
- `GET  {base}/<vendor-path>/userinfo?access_token=` — token in the query string,
  **not** an `Authorization` header; claims nested under a `{success, code,
  message, requestId, data:{...}}` envelope, so the top-level `sub` that
  `go-oidc` expects is absent.
- Token response carries `refresh_token` but the spec **documents no refresh
  endpoint** (wire probe confirms the grant exists but no documented wire
  contract — capability therefore declared `RefreshToken=false` to avoid idling
  the SyncWorker).
- Logout is a front-channel redirect to a vendor SLO path keyed by an app id:
  `{host}/public/sp/slo/{appId}?redirect_url=...`. There is **no back-channel
  logout**, and the vendor's own docs state SCIM is not offered externally.

### Source 2 — native-client bearer JWT (HS256 exchange endpoint)

The bearer client receives an HS256 JWT signed by the native client's backend after the
user completes the enterprise IdP SSO in the system browser. The JWT payload is:

```json
{
  "userId": <int64>,            // 客户端后端内部用户主键(数字)
  "domainAccount": <string>,    // 域账号(登录名,仅做显示/审计)
  "payloadHash": <hex-sha256>,  // 客户端侧附加 userData 的完整性摘要
  "iat": <unixsec>,
  "exp": <unixsec>              // TTL ~15d
}
```

Key properties:
- No `iss`, `aud`, `nbf`, or `kid` field. No audience/purpose binding.
- Signed with a shared symmetric secret obtained separately from the OAuth2
  client credentials; configured via `OCTO_OIDC_BEARER_JWT_SECRET`.
- `userId` is an integer in a separate ID space from the enterprise IdP's
  18-digit `sub`. We therefore use an **isolated issuer namespace**: the
  configured upstream issuer plus a `#bearer-jwt` suffix, so the two identity
  sources cannot collide on `(issuer, subject)`. Deriving it rather than taking
  a second environment knob means per-environment isolation is inherited from
  the upstream issuer (already mandatory and validated at boot), and there is no
  way to misconfigure the two against each other. `#` cannot occur in a valid
  issuer URI, so the suffix is unambiguous. The derivation refuses an empty
  upstream issuer, an already-suffixed value, and anything that would exceed the
  255-byte `issuer` column (silent MySQL truncation could otherwise merge two
  issuers, and with them two people, under `uk_issuer_subject`).
- The JWT carries **no verified email / phone claims**, so autolink is fail-closed
  on this path — and, since the boot-time refusal, structurally so rather than by
  default value.

  An earlier version of this line claimed "by construction" while it was only true
  by policy: `service.go` evaluates `!RequireEmailVerified || claims.EmailVerified`,
  so with `RequireEmailVerified=false` the verified flag is never consulted and an
  unverified address becomes an account-linking key. The safe default was the only
  thing holding it — one configmap edit away from open. `pkg/oidcboot` now refuses
  that combination for the kind whose provider structurally cannot assert
  verification, which is what makes the original wording true.

### Where the protocol coupling actually lives

`go-oidc` / `golang.org/x/oauth2` are imported by exactly two production files —
`modules/oidc/oidc_client.go` and `modules/oidc/sync_worker.go`. `service.go`,
`db.go`, `model.go`, `bind_service.go` and `sql/` import neither. That is why the
abstraction boundary is the client layer and why `service.go` needs **zero**
changes: `ResolveOrLink` / `IssueSession` only consume `*IDTokenClaims` (aliased
as `IdentityClaims` to avoid renaming snapshot fields).

The one large edit was `api.go:504-604` (raw `id_token` extraction →
`VerifyIDToken` → nonce compare → `needUserInfo` merge → sub cross-check, ~101
lines). Those are OIDC protocol details; they collapse into a single
`provider.Identity(ctx, tok)` call so the plain-OAuth2 and HS256-JWT
implementations are not forced to stub them out.

### Corrections to existing comments (applied during this task)

- `oidc_client.go:255-258` previously claimed `go-oidc`'s `UserInfo` verifies the
  `sub` against the ID Token. It does not — `go-oidc/v3@v3.9.0/oidc/oidc.go:321-371`
  only issues the GET and decodes. The cross-check is **our** code (now in
  `oidcProvider.Identity`) and has explicit test coverage
  (`TestOIDCProvider_PreservesSecurityChecks/rejects_userinfo_sub_mismatch`).
- `api.go`'s `TrustedSSOCreate` argument previously cited JWKS verification as
  proof that `claims.Issuer == cfg.Provider.Issuer` is cryptographically
  guaranteed. For plain-OAuth2 that trust anchor is *not* cryptographic (the
  Issuer is operator-supplied, integrity comes from TLS); the comment was
  rewritten to distinguish the two kinds rather than leaving a misleading
  assurance.

### Decisions already taken

0. **Greenfield deployment — no pre-existing IM population to migrate.** Every
   user who arrives through SSO is new. First-login path is `AllowNewUser=true`
   (default); self-service binding is left off. Autolink stays off.

1. **Account deactivation / offboarding** — no synchronisation. The IdP gates
   login, we rely on that plus a shorter session ceiling for SSO users
   (`Cache.TokenExpire` tightened for SSO-issued sessions, see Pending below)
   and an operator-triggered `RevokeAll` primitive. The vendor does not offer
   SCIM externally.

2. **Space membership for SSO-created users** — keep current behaviour, no
   auto-join. Admins add users via existing invite flows.

3. **Logout blast radius** — our own session and IM kick are scoped to **the
   calling device only** (fixed in this PR; a previous implementation called
   `auth.Decode` on the raw UUID token and always returned 0, inadvertently
   kicking every device). The upstream SLO redirect clears the IdP's browser
   session globally; other devices keep existing tokens but must re-auth on the
   next SSO round trip.

4. **Employee-number recycling is fenced, not assumed away.** An earlier version of
   this decision stated as settled fact that `subject` is the IdP's internal
   directory id rather than the employee number. It is not settled: the vendor's
   userinfo field table calls it a sub-number with an 18-digit example, the
   quick-start demo comment calls it the employee number, and no live response has
   ever been captured. The guard in `subject_shape.go` was built for exactly that
   contradiction, so the brief cannot also assert it away.

   The reading matters because employee numbers are reused between leavers and
   joiners: under the second reading a new hire matches a former employee's
   identity row and logs into their account. A purely numeric subject shorter than
   ten digits is therefore refused at the trust boundary, before any row is
   written — which converts an irreversible data problem into a recoverable
   failure, and answers the question at first login instead of requiring the
   capture up front.

5. **Two exchange endpoints share the post-validation tail.** The ResolveOrLink
   → IssueSession → identity-insert-with-race-recovery → writeAudit sequence
   lives in exactly one place, `completeExchange` in
   `modules/oidc/exchange_complete.go`, taking a `verifiedIdentity` plus an
   `exchangeFlavour` that carries the only real differences (metric family,
   audit event pair, log name).

   This decision was recorded as settled before the refactor actually landed,
   and the gap was not harmless: while the tail was duplicated, the race-recovery
   defect existed in **both** copies and the phone-number masking fix reached
   only one of them. Those are the same class of bug the shared tail exists to
   prevent, so the ordering — claim first, extract later — is itself the lesson.

## Load-bearing list

- `auth` — the login identity decision (credential → `{uid, session}`). Security
  boundary. A regression here is an authentication bypass, not a UX bug.
- `trust-boundary`:
  - For the plain-OAuth2 provider claim integrity comes from TLS, not a
    signature. Three invariants enforced inside `oauth2Provider.Identity`:
    absolute-https enforcement (config-load time, with an explicit insecure
    escape hatch for pre-production), envelope `success`/`code` validation
    (`{"success":false}` with HTTP 200 rejected), non-empty subject with length
    cap.
  - For the bearer-JWT provider claim integrity comes from HS256 plus strict
    exp validation. `alg` is pinned to `HS256` at the parser level (alg
    confusion resistant); empty secret rejected at the parser entry; base64
    decoded in Strict mode to prevent signature-string ambiguity; issuer is
    **never** read from the token. Risk of cross-purpose token reuse (other
    internal services sharing the HS256 key) is documented; mitigated by
    operational key isolation (single-purpose secret).
- **`(issuer, subject)` identity mapping — irreversible.** `user_oidc_identity`
  has `uk_issuer_subject`; admitting an empty/colliding subject collapses users
  onto one row (account-takeover shape). The plain-OAuth2 provider caps subject
  length to the column width; the bearer-JWT provider converts `userId` to its
  decimal string with a non-zero guard.
- **`IDTokenClaims` JSON shape** preserved as `IdentityClaims = IDTokenClaims`
  alias; snapshot compatibility tested via `TestDecodeClaimsSnapshot_PreUpgradeSnapshotStillDecodes`.
- **Config required-field list mirror** in `system_settings.go` not touched.
  `DM_OIDC_PROVIDER_ISSUER` remains mandatory and doubles as the OAuth2
  identity namespace; new fields are optional or new-kind-only.
- **Logout scope**: `InvalidateCurrentToken` invalidates the *current* token
  (not all tokens for the UID); `QuitUserDevice` is scoped by `DeviceFlag` read
  from the Redis-cached payload (the raw UUID token is not decodable);
  `RevokeRefreshByUID` remains all-sessions (refresh tokens outlive devices).
- **Upstream logout URL** for plain-OAuth2 is constructed by `oauth2Provider.LogoutURL`
  with `appId` whitelisted (`[A-Za-z0-9_-]+`, no `.`/`..`) before `path.Join` so
  path traversal is impossible; redirect target pinned to configured
  `PostLogoutRedirectURI`. Upstream HTTP client rejects redirects
  (`CheckRedirect: ErrUseLastResponse`) to prevent SSRF and credential replay
  to a 3rd party.
- **SLO redirect remains a top-level browser navigation**, not a server-side
  proxy; backend returns the URL, frontend navigates.
- **autolink admission** fail-closed: the bearer JWT has no Email/Phone/Verified
  fields so autolink never fires on that path; OAuth2 provider does not set
  Verified flags (vendor envelope does not carry them). No boot-time guard
  against misconfigured `AutoLinkByEmail + RequireEmailVerified=false` yet;
  tracked in Pending.
- `pkg/accesslog` scrubbing extends the shared pattern to `code`/`state`/
  `access_token`/`refresh_token`/`id_token`/`client_secret`; the panic-dump sink
  uses the same regex (drift between the two sinks caused past incidents).
- `state` handling remains mandatory on our side (32-byte random + single-use
  Redis consume) even though the IdP treats it as optional; `AuthCodeURL`
  rejects empty State.
- **Inbound credential header contract** satisfied by the new Bearer shim:
  `token` header wins when present; `Authorization` header left untouched;
  query parameter not accepted; scheme is case-insensitive; token/credentials
  validated before backfill so an empty/malformed Bearer cannot turn "token
  missing" into "token invalid".
- `error-response` / `i18n` — user-facing failures use `httperr.ResponseErrorL`
  / `ResponseErrorLWithStatus` with registered `pkg/errcode` codes. Protocol
  endpoints (authorize/callback/logout) and the two 500 "provider/secret not
  initialized" returns on exchange endpoints are exempt in the lint baseline
  (raw browser-redirect semantics / operational 500s).
- **observability**: upstream `requestId` from the plain-OAuth2 envelope is
  logged alongside our `trace_id` on identity failure; both exchange endpoints
  have separate metrics/audit events with a fixed label set (no cardinality
  blow-up); bearer-JWT failures log the domainAccount for audit-trail correlation
  but never the raw token.

## Out of scope

- **Directory / headcount synchronisation** (vendor data-service APIs, org tree,
  per-person field mapping, joiner/leaver feeds). No new tables, no sync worker.
- The vendor's other three SSO protocols (JWT plugin, a vendor-proprietary ticket protocol,
  SAML RSA-SHA1).
- Two pre-existing defects noted while scoping: dead `InsertRefresh` caller
  chain (SyncWorker idling) and space search ignoring `u.status`/`u.is_destroy`.
- Auto-joining SSO users into a Space (decision 2).
- The `shortno` allocation pool bulk-provisioning path.
- PKCE / nonce for the plain-OAuth2 kind: the upstream protocol has no place
  to carry them.

## Acceptance

### Provider abstraction & plain-OAuth2 path

- A **conformance test suite** (`TestAuthProviderConformance`) runs the same
  contract table against both AuthProvider implementations and asserts the three
  invariants: non-empty `Issuer` and `Subject`, protocol validation completed
  inside `Identity()`, no credential-bearing URL in any returned error.
- Empty `subject` from `/userinfo` → login rejected, **no** `user_oidc_identity`
  row written (asserted at the provider layer; DB-layer assertion via existing
  unique-key tests).
- Envelope `{"success": false}` served with HTTP 200 → login rejected.
- **Credential-leak guard**: driving the provider against an unroutable address
  produces an error whose `.Error()` contains neither `client_secret=` nor
  `access_token=`, including when the error is wrapped with `%w`.
- `pkg/accesslog` scrubs OAuth2 credentials on both the access-log and
  panic-dump sinks.
- Legacy compatibility: a JSON claims snapshot produced before this change still
  decodes via `decodeClaimsSnapshot` (`TestDecodeClaimsSnapshot_PreUpgradeSnapshotStillDecodes`).
- Logout scoped to the calling device: `deviceFlagFromRequest` reads the flag
  from the Redis-cached token payload (not the raw UUID), and routes to
  `QuitUserDevice(uid, flag)` when non-zero, with a zero fallback to
  `killer.Kick` (all devices).

  Two corrections to an earlier version of this line. It claimed the behaviour
  was tested against the real `CacheTokenParser` path; no test went near that
  path, and the test double did not model the production store at all — it never
  deleted the record it was asked to invalidate, so it passed against behaviour
  production cannot produce. And the behaviour itself did not hold: the device
  flag was read *after* `InvalidateCurrentToken` had already removed the record
  it is read from, so every production logout fell through to kicking all
  devices. Both are fixed; the double now deletes on invalidate and returns
  pkg/auth's miss semantics (`TTL:-2`, nil error).
- Logout returns a non-empty upstream logout URL for the plain-OAuth2 provider
  with the app id in the path segment and the operator-pinned redirect target
  as the query parameter — and the URL contains no credential so it is safe to
  log (`TestBuildUpstreamLogoutURL_CarriesNoCredential`).
- The Bearer shim is verified for: `token`-header precedence (including
  conflicting values), case-insensitive scheme, rejection of non-Bearer
  schemes, rejection of a scheme with no credential (must not backfill empty
  header), refusal of query-parameter fallback, preservation of the
  `Authorization` header, and transparency to downstream status/body.
- `modules/oidc` existing suites pass unchanged, in particular the 12 cases in
  `config_test.go`.
- `make i18n-extract-check` and `make i18n-lint` pass; new handler files
  covered by lint baseline exemptions for 500 operational errors only.
- **No behaviour change for the standard-OIDC kind**: `TestOIDCProvider_PreservesSecurityChecks`
  explicitly verifies id_token signature verification, nonce surfaced to the
  handler, `/userinfo` sub cross-check against id_token sub, and RP-Initiated
  Logout URL carrying `id_token_hint`.
- The plain-OAuth2 callback path is exercised end to end against a mock
  `httptest.Server` (`mockOAuth2Provider`).
- HTTP client for the plain-OAuth2 provider disables redirect following
  (`CheckRedirect → ErrUseLastResponse`) and caps response body at 1 MiB.

### Bearer-JWT exchange (`/exchange-jwt`)

- HS256 JWT verification:
  - Alg pinned to `HS256` (case-sensitive); `none`, `RS256`, and other algs
    rejected.
  - Base64 decoded in `RawURLEncoding.Strict()` mode to reject non-canonical
    encodings.
  - Signature compared with `hmac.Equal` (constant time).
  - `exp` mandatory, must be a JSON integer, must be strictly greater than
    `now` (zero-width boundary); `now==exp` is expired.
  - Empty secret rejected at the parser entry (defence against missing config).
  - `userId` required and > 0; subject is `strconv.FormatInt(userId, 10)` under
    the bearer-JWT-specific issuer namespace.
  - Email/Phone/Verified fields all zero (fail-closed for autolink).
- Endpoint mounted under the same public group as `/exchange`, behind a shared
  `StrictIPRateLimitMiddleware` (2 rps / 10 burst, constants — matching how
  `modules/user` sets login/register/sms; an endpoint-level security threshold
  should change through code review, not through a deploy-time knob). Both
  endpoints cap the request body at 16 KiB.
- Both `/exchange` and `/exchange-jwt` fail closed when `o.provider == nil` or
  `o.service == nil` (500, no panic path).
- Audit events + metrics for the JWT path are separate from the OAuth2 path
  (`EventBearerExchangeOK` / `EventBearerExchangeFail`, `metricExchangeJWT*`).
- Real credentials and real user PII are **not** committed to the repository;
  all test tokens and secrets are synthetic values generated in-test.

## Pending / follow-up (not blocking this PR)

- **Session TTL ceiling for SSO users** — tightening `Cache.TokenExpire` (default
  30d) for SSO-issued sessions; depends on ops confirming the upstream
  offboarding latency. Currently tracked in code review as a P1 config change
  after this PR.
- **autolink boot-time guard** — refusing startup when a provider without
  verified-claim support is combined with `AutoLinkByEmail` and
  `RequireEmailVerified=false`. ProviderCapabilities does not yet carry a
  `VerifiedClaims` bit; adding that is a small follow-up.
- **Bearer-JWT audience/purpose claim** — the JWT currently has no `aud` or
  `purpose` field, so cross-purpose token reuse is only mitigated by
  operational key isolation (single-purpose secret). Adding a required `aud`
  once the client backend issues one is a coordinated change with the upstream team.
- **stateTTL configurability** — currently a constant (5 min); enterprise
  login + 2FA may need a longer window.
- **Production bearer-JWT secret and per-environment appId** for SLO — supplied
  by ops via env at deploy time; code changes are not required.

## Changes applied since initial implementation (review fixes)

- **CRITICAL** `deviceFlagFromToken` previously called `auth.Decode` directly on
  the raw UUID token, which always failed (Decode expects the cached JSON
  payload, not the token itself) — "logout current device" was silently kicking
  *all* devices. Fixed to read the cached payload via
  `auth.TokenRecordReader` (the `InvalidateCurrentToken` path already does
  this correctly); renamed to `deviceFlagFromRequest(ctx, token)`.
- **CRITICAL** `exchangeJWT` missed the `o.provider == nil`/`o.service == nil`
  guard and could nil-panic when provider construction failed at boot but the bearer-JWT
  secret was configured; added to match `/exchange`.
- **HIGH** Both exchange endpoints now mount `StrictIPRateLimitMiddleware`
  (2 rps / 10 burst). The global 500 rps bucket is too wide for
  login-equivalent endpoints that trigger outbound HTTP and DB writes.
- **HIGH** Plain-OAuth2 HTTP client now sets `CheckRedirect: ErrUseLastResponse`
  to prevent SSRF / credential replay on 3xx responses.
- **MEDIUM** Exchange endpoints cap request body at 16 KiB via
  `http.MaxBytesReader`.
- **MEDIUM** `VerifyHS256JWT` rejects empty secret at entry; base64 decoding
  uses `Strict()` to reject non-canonical encodings.
- **MEDIUM** The duplicated post-verification tail between `/exchange` and
  `/exchange-jwt` was extracted into a single helper `completeVerifiedExchange`;
  previously ~65 lines of safety-critical logic (race recovery on
  `uk_issuer_subject`, identity insert, audit write, response) were copied
  verbatim across both handlers.
- **MEDIUM** Real credentials/PII (test-environment bearer-JWT secret, real
  user `userId`/`domainAccount` from a captured login session) were removed
  from tests and replaced with synthetic values; tests now sign tokens inline
  using `crypto/hmac`.
- **LOW** Comment corrections for `o.client` (only used by SyncWorker, not by
  authorize/callback/logout as the old comment claimed), for the JWTSigning
  trust anchor (OAuth2 path relies on TLS, not signatures), and for the
  accesslog regex (word boundary, not alternation order, prevents accidental
  matches on `auth_code=`).

### Follow-up pass (post-rename)

- **HIGH** `/exchange-jwt` called `VerifyHS256JWT` directly and re-implemented
  the `userId > 0` constraint inline, leaving `verifyBearerJWT` (and the ~10
  test cases in `bearer_jwt_test.go` that exercise it) as dead code that did
  not guard the production path. The handler now calls `verifyBearerJWT`, so
  the constraint exists in exactly one place and the existing tests cover the
  real path. Failure reason stays in the log via the returned error; the client
  still gets a single generic 401 code (anti-enumeration).
- **HIGH** The de-identification rename of the vendor/client keywords was done
  as a literal string substitution, which produced an identifier containing a
  hyphen in `bearer_jwt_test.go`. The whole `modules/oidc` test binary failed
  to compile as a result (`go build ./...` still passed because it does not
  compile `_test.go` files, so the break was invisible to a build-only check).
  Fixed, and the substitution fallout was swept: mangled identifiers, stale
  file/function names in comments, CJK/Latin words run together, and four files
  left unformatted because the alignment blocks were not re-flowed.
- **MEDIUM** Newly introduced environment variables were standardised on the
  `OCTO_` prefix (`OCTO_OIDC_PROVIDER_KIND`, `OCTO_OIDC_PROVIDER_BASE_URL`,
  `OCTO_OIDC_PROVIDER_APP_ID`). Pre-existing `DM_OIDC_*` variables are left
  untouched — renaming those would break deployed configmaps.
- **MEDIUM** The new-variable count was cut from 8 to 5 by removing knobs that
  did not need to exist: the two exchange rate-limit parameters became constants
  (`modules/user` treats the equivalent login/register/sms thresholds the same
  way), and the bearer-JWT environment selector was dropped in favour of
  deriving the issuer namespace from the upstream issuer. The five that remain
  each carry information the process cannot obtain otherwise: which provider
  implementation to construct, the upstream site root (optional, falls back to
  the issuer), the logout app id, the shared HS256 secret, and the plaintext-
  upstream escape hatch for a pre-production host that is documented as `http`.
- **MEDIUM** Two de-identification leaks that survived the first pass: a
  vendor-proprietary protocol name in the out-of-scope list, a vendor-derived
  issuer literal in a test fixture, and a local credential filename in a test
  comment. All replaced with neutral values.
- **MEDIUM** New environment-variable surface trimmed from 8 to 5. The two
  rate-limit knobs became constants, and the bearer-JWT environment marker was
  dropped in favour of deriving the issuer namespace from the upstream issuer.
  Every variable removed is one fewer way to misconfigure a value that cannot
  be changed after go-live.
- **MEDIUM** Phone normalisation now accepts the bare-number form the vendor's
  own userinfo example uses, plus the `+86` / `0086` / `86` prefix spellings and
  embedded separators, mapping all of them to `zone=0086`. Previously only `+86`
  was recognised, so the documented example itself would have been dropped.
  Deliberately still *not* guessing: an E.164 country code cannot be split off
  by syntax alone (they are 1-3 digits), and guessing wrong stores a different
  person's number — a failure that is invisible in the row it writes. Anything
  ambiguous returns an empty pair and the caller treats the user as having no
  phone. `extractZone`/`extractPhone` keep their signatures and their
  all-or-nothing invariant, which `checkClaimsForCreate` and `UIDsByPhone` both
  rely on. Overseas numbers are still not stored: only the 0086 SMS channel is
  proven, and a number that cannot receive a code is worse than none — it makes
  phone-based account recovery look available when it is not.
- **MEDIUM** The "phone dropped" warning logged the subscriber's full number.
  It now logs the masked tail plus the length: enough to classify the input,
  not enough to identify the person. Log retention outlives the diagnostic need.

### Pre-PR pass: acceptance gaps closed + subject shape guard

- **Subject shape guard** (`modules/oidc/subject_shape.go`). `(issuer, subject)`
  is the identity primary key and is immutable after go-live, yet the vendor
  documentation contradicts itself about what `sub` contains: the userinfo field
  table calls it a "sub-number" with an 18-digit example, while the quick-start
  demo comment says it is the employee number — and the employee number already
  has its own field (`username`, "employee number preferred"), alongside a
  separate `user_id`. The evidence favours an internal long id, but a wrong guess
  is unrecoverable: employee numbers are reused between leavers and joiners, so a
  new hire would match the former employee's identity row and log straight into
  their account.

  The guard therefore converts an irreversible data problem into a recoverable
  failure: a purely numeric subject shorter than 10 digits is refused at the
  trust boundary, before any row is written. It is deliberately narrow — 18- and
  20-digit ids pass, anything containing a non-digit passes (it cannot be an
  employee number), only the short-numeric shape is refused. The error carries
  the length, never the subject value. No environment variable: this is a
  security floor, so relaxing it should leave a code-review trace rather than
  become a deploy-time knob.

  This also removes "capture a real userinfo response first" as a launch
  blocker: if `sub` really is an employee number, the first login fails loudly
  with zero rows written, which is exactly the signal we wanted from that
  capture.

- **Logout device scoping now has tests** (acceptance item: two devices, only
  the current one is kicked). Required a small refactor first: `logout` reached
  straight into `o.ctx.QuitUserDevice`, bypassing the existing `sessionKiller`
  abstraction, so the blast radius of a logout had no injection point. `Kick`
  (all devices) and `KickDevice` (one device) are now both on that interface, and
  the tests assert the two are mutually exclusive — including the negative
  direction, which is the one that regressed before.

  The tests also pinned down a **behavioural boundary worth recording**:
  `device_flag` only exists in v3 session payloads. The v2 encoder serialises
  only `uid` and `name`, so on a v2 session the flag can never be resolved and
  logout degrades to kicking every device. "Logout affects only the current
  device" is therefore a v3-only property. The degradation direction is safe
  (over-kicking beats a no-op) and is asserted rather than fixed — extending the
  v2 payload belongs to session encoding, not to this module.

- **Empty subject is now asserted against the real database** (acceptance item).
  The provider-level test only proved the parser returns an error; it could not
  prove the handler does not write a row before returning it. `subject` is
  `NOT NULL DEFAULT ''` under `UNIQUE(issuer, subject)`, so an empty-subject row
  is a perfectly legal row — and the second subject-less login would resolve onto
  the first one's uid. That is account takeover, not dirty data, so the assertion
  has to look at the table.

- **Plain-OAuth2 callback is now driven end to end** (acceptance item). The
  handler chain — state issue and single-use consumption, Exchange/Identity order,
  ResolveOrLink → IssueSession → ThirdAuthcode, the 302 back to `return_to` —
  had only ever been exercised under `KindOIDC`. Coverage now includes the
  existing-user and new-user paths, state replay rejection, and an upstream token
  failure writing nothing. The authorize URL is also asserted to carry *no*
  `nonce` / `code_challenge` (this upstream rejects unknown parameters) and
  `scope=read`.

### Review round 2 — six blocking defects and four spec deviations

An external review at head `dce3b6e0` found six P1 defects and four places where
this brief or the PR body asserted properties the code did not have. Every finding
was re-derived from the source before being acted on; all ten held.

- **P1-1 / P1-2 — per-device logout never worked in production, twice over.**
  The device flag was read *after* `InvalidateCurrentToken`, which deletes the very
  record it is read from, so the lookup always missed. And `0` was used as the
  "unresolved" sentinel while `config.APP` *is* 0, so even with the ordering fixed
  every APP logout would still have kicked every device. Resolution now happens
  before invalidation and returns `(flag, known)`.

  The reason the first round's tests did not catch this is the more useful finding:
  the test double recorded the invalidate call without deleting anything, and
  returned an error on a cache miss where pkg/auth returns `TTL:-2` with a nil
  error. It passed against behaviour production cannot produce. The double now
  models both, and reverting the fix makes four cases fail.

- **P1-3 — both exchange endpoints handed back the wrong session.** After a
  first-login race they checked `recovered != nil` and then kept using the ghost's
  session, so the client received a token for an account with no identity row and
  the audit recorded success against it. The callback path had always been correct.

- **P1-4 — the bearer-JWT trust anchor was under-constrained.** Any non-empty
  secret was accepted, where the same module already requires the refresh-token key
  to be exactly 32 bytes and refuses boot otherwise; a short HMAC key can be
  recovered offline from one valid token. And `exp` was the only freshness control,
  so a captured assertion could mint sessions for its whole ~15-day life, including
  after logout. Now: a 32-byte floor, a mandatory `iat`, a 10-minute ceiling from
  `iat`, and 60s of clock skew. Audience binding remains Pending — it needs the
  upstream to emit a claim.

- **P1-5 — `RequireEmailVerified=false` was an account-takeover primitive under
  the plain-OAuth2 kind.** That provider structurally cannot assert verification,
  so the flag turns an unverified upstream address into a permanent binding to
  whichever existing account holds it. Refused at boot for that kind.

- **P1-6 — a typo in the provider kind could lock every user out.** The five new
  fatal `LoadConfig` conditions were never mirrored into
  `modules/common.isOIDCFullyConfigured`, so a bad kind produced 404 on every OIDC
  endpoint while `login.local_off` was still honoured — an SSO-only deployment with
  no working login and no recovery short of a redeploy. The warning about exactly
  this drift was already in the code, written by the same hand that then drifted it.

  Fixed by removing the mirror rather than updating it: the refusal rules now live
  in `pkg/oidcboot`, a leaf package both sides import, and `oidcboot.RefusedScenarios`
  pins both sides' tests to one table. Removing the delegation makes all nine
  scenarios fail on the `modules/common` side, which is how we know the drift was
  live.

- **P2-3 — the bare-number phone inference stored strangers' numbers.** NANP
  numbers are `1` + a three-digit area code whose first digit is 2-9, so roughly
  seven eighths of them are byte-identical to a valid mainland mobile:
  `13861234567` is both +1 386-123-4567 and a real 138-prefix number. The
  inference was added on the strength of a documented example that is not itself a
  valid mainland number, so it never had evidence behind it. Reverted to requiring
  an explicit country code.

- **P2-2 — `(issuer, subject)` was compared case-insensitively.** The table is
  `utf8mb4_general_ci`, so two subjects differing only in case fold onto one
  identity row. Rather than rebuild a unique index on a live table or defeat the
  index with an inline `COLLATE`, the adapter now re-checks the returned row
  byte-for-byte and treats a folded hit as absent — which degrades to a loud
  refusal instead of a silent merge.

- **P2-5 / P2-6 — audit rows from the exchange paths were labelled
  `EventCallbackFail`** (misdirecting the investigation those rows exist for), and
  the endpoint rate limiter's Redis clients were never closed while every other
  client in the module is. Both fixed.

- **Spec deviations.** The brief claimed the de-duplication refactor had landed
  (it had not, and P1-3 plus the missed phone masking were the duplicate-copy bugs
  it was meant to prevent); claimed per-device logout was tested against the real
  session-store path (no test went near it); called autolink fail-closed "by
  construction" when it was by policy; and asserted `subject` is the internal
  directory id while `subject_shape.go` treats that as unresolved. All four
  corrected in place, each with the reason the original wording was wrong.

### Review round 3 — the integration endpoints were never adapted

`modules/integration` exposes two endpoints that authenticate a caller with an
upstream credential and resolve it to an already-linked local user:

- `GET  /v1/integrations/oidc/spaces`
- `POST /v1/integrations/oidc/exchange` (mints a `uk_` API key)

Both went through `oidcAuth()`, which called `oidcClient.VerifyIDToken()`
unconditionally, and `Integration.New()` called `oidc.NewClient()` — i.e. ran
Discovery — regardless of the configured provider kind. Since the plain-OAuth2
upstream has no Discovery document and issues no `id_token`, switching to that
kind left the client nil and **both endpoints answering 500**. Making a new kind
selectable without adapting these two was an incomplete change, not a
non-blocking gap.

Two things were extracted rather than branched a second time:

- **`oidc.NewAuthProvider`** (`provider_factory.go`) is now the single place that
  turns a configuration into a provider. `modules/oidc.New` and
  `modules/integration.New` both call it. Copying the `switch` into the second
  caller is exactly what produced the login-lockout drift in round 2, so the
  dispatch stays in one place by construction.

- **`AuthProvider.IdentityFromClientCredential`** interprets a credential the
  *client* presents, as opposed to one we obtained through a code exchange. The
  distinction is protocol-level and belongs to the provider: under standard OIDC
  the client's verifiable credential is an `id_token`; under plain OAuth2 it is an
  opaque `access_token` that can only be resolved by asking `/userinfo`.

  Putting the choice behind the interface is what keeps the callers from
  guessing — and **guessing by token shape is not acceptable**: an opaque access
  token may happen to be JWT-shaped, and a shape sniffer would then feed an
  unverified payload into a local verification path. A test drives a JWT-shaped
  string at the plain-OAuth2 endpoint specifically to pin that it is refused.

This also closed a defect the round-2 review had raised as non-blocking: `/exchange`
hard-coded `&TokenSet{AccessToken: ...}`, which `oidcProvider.Identity` always
rejects, so under the standard kind every existing deployment had gained an
unauthenticated endpoint that could only ever answer 401. It now routes through
the same method and is meaningful under both kinds.

**Still open — the third credential type.** A business-signed HS256 JWT (the one
`/exchange-jwt` accepts) is *not* accepted on these two endpoints. It cannot be
added by configuration alone: with a bearer-JWT secret configured under the
plain-OAuth2 kind, two different credential types would arrive in the same
`Authorization: Bearer` header with no way to tell them apart short of sniffing
the shape, which is the thing being ruled out. It therefore needs an explicit
client-visible signal — a separate path (as `/exchange` vs `/exchange-jwt`
already does) or a declared credential type — and that is an API-contract
decision, so it is Pending rather than guessed at. Today such a token is refused
fail-closed, because the upstream does not recognise it.

## Integration tests written (this PR)

All tests pass under `go test -race ./modules/oidc/ ./pkg/wkhttp/ ./pkg/accesslog/`.

### Provider / protocol layer (no DB required)

- `TestBearerJWTIssuerFromUpstream_*` — 5 cases over the issuer derivation:
  derived namespace differs from the upstream issuer, test and prod derive to
  different values (the property that replaced the dropped environment knob),
  empty upstream rejected, an already-derived value rejected rather than
  double-suffixed, and the 255-byte column boundary accepted at exactly the
  limit / rejected one byte over.

- `TestAuthProviderConformance` — same contract table for both providers
  (Kind/Issuer non-empty, AuthCodeURL rejects empty state, AuthCodeURL carries
  state & no secret, Exchange returns TokenSet, Identity enforces non-empty
  subject, LogoutURL produces https URL with no credential, etc.).
- `TestOIDCProvider_PreservesSecurityChecks` — 4 sub-tests covering id_token
  signature verification, missing id_token rejection, nonce hand-off to
  handler, and `/userinfo` sub cross-check.
- `TestParseUserInfoEnvelope` + variants — envelope success/code/requestId
  parsing; `success=false`/missing `success` rejected; code accepted as
  string or number; issuer not taken from body; error message does not echo
  body contents.
- `TestOAuth2Provider_ExchangeSendsParamsInQuery` — token endpoint shape
  (POST, query parameters, `Content-Type: form-urlencoded`) asserted against
  a mock server; verifies no `code_verifier`, proper percent-encoding.
- `TestOAuth2Provider_ExchangeErrorDoesNotLeakSecret` /
  `TestOAuth2Provider_IdentityErrorDoesNotLeakToken` — driving transport
  failures against an unreachable address, verifying sanitized errors contain
  no credential.
- `TestOAuth2Provider_IdentityPutsTokenInQuery` — userinfo endpoint receives
  `access_token` in the query string, not the Authorization header.
- `TestOAuth2Provider_LogoutURL` — SLO URL shape, appId whitelist, redirect
  parameter.
- `TestBuildUpstreamLogoutURL` + `_CarriesNoCredential` — redirect target
  pinned; no token/secret in URL; bad appId rejected.
- `TestSanitizeTransportErr_*` (5 cases) — strips credential-bearing URLs
  from `*url.Error`, preserves context errors, passes through non-URL errors,
  nil stays nil.
- `TestLiveUpstream_*` (5 cases, gated on `OCTO_OIDC_LIVE_TEST`) — optional
  live-upstream probes against the real IdP (bad-code / bad-token / proxy
  behaviour / sanitisation / authorize URL print); skipped by default.

### JWT layer

- `TestVerifyHS256JWT_*` (9 cases) — happy-path custom claims, signature uses
  HMAC on signing input, `now==exp` boundary, exp not a number, typ
  case/omission, realistic synthetic token end-to-end, empty secret rejected,
  non-canonical base64 rejected.
- `TestBEARERJWT_*` (9 cases) — happy path, wrong secret, expired, malformed
  segments/JSON/base64, alg must be HS256 (none/RS256 rejected), missing
  userId, userId=0 rejected, toIdentityClaims fail-closed (email/phone/verified
  all zero).
- `TestVerifyHS256JWT_RealisticShapeToken` uses only a synthetic token/secret
  generated in-test.

### Handler layer (HTTP, fake store/users)

- `api_exchange_test.go` (7 cases) — handler exists, missing body, bad JSON,
  blank access_token, IdP rejects token (401, anti-enumeration), happy path
  returns session with correct identity row, route is public (no auth
  middleware).
- `api_exchange_jwt_test.go` (10 cases) — handler exists, missing body, blank
  token, secret not configured (500), bad signature/expired/zero-userId/
  garbage token all 401, happy path returns session with the bearer-JWT issuer and
  decimal-string subject, issuer derived from config (prod vs test).
- Tests use hand-built `*OIDC` with fakes (no `New()`/`Init()`), except for
  the real DB-backed suites in `api_test.go` / `db_integration_test.go` which
  exercise the OIDC callback end-to-end.

### Middleware / logging

- `TestBearerTokenCompat` (14 sub-cases) — token wins, Bearer backfills, case
  insensitivity, whitespace, Basic/unknown schemes ignored, no-credential/
  only-spaces/extra-segment not backfilled, query not accepted, nothing
  provided stays empty, conflicting values don't error.
- `TestBearerTokenCompat_IsTransparent` — downstream status/body preserved.
- `TestScrubPath_MasksOAuthCallbackCredentials`,
  `TestScrubbingErrorWriter_MasksOAuthCredentials`,
  `TestScrubPath_ExistingMaskersStillApply` — accesslog and panic-dump sinks
  both mask the new credential parameters; existing maskers still apply.

### Config

- `TestLoadConfig_KindDefaultsToOIDC`, `_ExplicitOIDCKindMatchesDefault`,
  `_UnknownKindRejected`, `_OAuth2Kind` — new `OCTO_OIDC_PROVIDER_KIND` knob
  defaulting to OIDC, unknown values rejected, OAuth2 kind loads without
  requiring Discovery-related fields.
- Existing `config_test.go` (12 cases) unchanged — required-env list not
  disturbed.
