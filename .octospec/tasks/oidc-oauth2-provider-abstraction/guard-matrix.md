# Guard matrix — identity paths × credential types

Written because the same defect class recurred through three different
un-enumerated routes across rounds 4–6. Patching per finding inherits the
reviewer's enumeration of routes, which is the thing that was incomplete. This
table enumerates the space once, including the degenerate cells.

Columns are the questions each round's blocking findings turned out to live in:
*which guard*, *at which stage*, *what lifetime*, *which issuer namespace*, and
**what happens when the component implementing the guard failed to construct**.
The last column is where both round-6 blockers were.

## Credential types

| id | credential | who holds it | verified how |
|---|---|---|---|
| C1 | authorization `code` | browser, via redirect | exchanged at the IdP; `state` binds the request |
| C2 | upstream credential presented directly — `id_token` under `kind=oidc`, opaque `access_token` under `kind=oauth2` | native / bearer clients that completed SSO themselves | provider-specific: signature verification vs `/userinfo` redemption |
| C3 | business-backend HS256 JWT (`userId`, `domainAccount`) | desktop client | HMAC under a dedicated shared secret |
| C4 | **credentials this service issues** — session token, `uk_` user API key, `bf_` bot token | our own clients | session store lookup / prefix |

C4 was the taxonomy's blind spot for one round: C1–C3 are all credentials the
*upstream* issues, and the provenance guard (G7) answered "is this an HS256 JWT we
signed?" — a true statement about a strict subset of "ours". A session token or a
`uk_` key is not a JWT at all, so it fell out of the classification entirely and was
forwarded to the vendor's `/userinfo` in a URL query. Both arrive on this header for
mundane reasons: the global `BearerTokenCompat` middleware makes
`Authorization: Bearer <session token>` the house convention, and `userAPIKeyAuth`
reads the *same* header on sibling routes in the *same* route group.

## Identity paths

| id | path | entry | credentials accepted |
|---|---|---|---|
| P1 | `GET /callback` | browser redirect | C1 |
| P2 | `POST /exchange` | JSON API | C2 |
| P3 | `POST /exchange-jwt` | JSON API | C3 |
| P4 | `modules/integration` — `GET /oidc/spaces`, `POST /oidc/exchange` | JSON API, `Authorization: Bearer` | C2 **and** C3 on the same header |
| P5 | bind (`Issue` → `Create`) | continuation of P1 when `ResolveOrLink` returns `ErrUnknownUser` | claims snapshot taken on P1 |

## Guards

| guard | property of | where enforced | covers |
|---|---|---|---|
| G1 `state` / CSRF binding | the transport | `callback_guard.go` | P1 |
| G2 credential verification | the protocol | `AuthProvider.Identity` / `IdentityFromClientCredential` (P1, P2, P4-C2); `BearerJWTVerifier` (P3, P4-C3) | all |
| G3 non-empty subject | the unique key | `requireStorableIdentity` at `ResolveOrLink` + `BindService.IssueWithReason`; also asserted in both providers | all |
| G4 length bound on `issuer` / `subject` | the **column** (`VARCHAR(255)`, compared in bytes — intentionally conservative) | `requireStorableIdentity`; `issuer` additionally at provider construction | all |
| G5 short-numeric refusal ("looks like an employee number") | the **producer** — requires a personnel system that reuses numbers | both `AuthProvider` implementations, pinned by the conformance table | P1, P2, P4-C2 only. **Deliberately not** P3/P4-C3: that subject is our own DB primary key, never reused |
| G6 byte-exact `(issuer, subject)` recheck | the `utf8mb4_general_ci` collation | `DB.QueryIdentityExact`; the raw query is unexported so this is compiler-enforced | all read paths |
| G7 credential provenance split ("is this ours?") | the HMAC | `IsForeignToken`, allowlist over `ErrJWTForeign` | P4 only (the only path with two credential types) |
| G8 freshness | the purpose | `VerifyForRedemption` (10 min from `iat`) vs `VerifyForAuthentication` (token's own `exp`) | P3 uses the former, P4-C3 the latter |
| G11 own-credential refusal | which system minted the credential | `oidc.OwnCredentialDetector` (`uk_`/`bf_` prefix, then session-store lookup), called immediately before any upstream call | P2, P4. Fails **closed**: if the session store is unavailable the answer is "undecided", and undecided must not forward |
| G10 IP rate limit | the endpoint being unauthenticated | `StrictIPRateLimitMiddleware` on each unauthenticated group | strict limiter on P2, P3, P4; **P1 has only the global IP floor**. Each of P2–P4 triggers an outbound call, so the limiter is what stops them being amplifiers |
| G9 autolink admission | verified-claim availability | `ResolveOrLink` + `pkg/oidcboot.ValidateKind` at boot | P1, P2; fail-closed on P3/P4-C3 (no Email/Phone/Verified fields) |

## Matrix

| | P1 callback | P2 /exchange | P3 /exchange-jwt | P4 integration | P5 bind |
|---|---|---|---|---|---|
| credential | C1 | C2 | C3 | C2 + C3 | snapshot of P1 |
| G1 state | ✅ | n/a (TLS channel) | n/a | n/a | inherited |
| G2 verify | ✅ | ✅ | ✅ | ✅ both | done on P1 |
| G3 non-empty sub | ✅ | ✅ | ✅ | ✅ | ✅ |
| G4 length bound | ✅ | ✅ | ✅ | ✅ (read-only) | ✅ |
| G5 short-numeric | ✅ | ✅ | **n/a by design** | C2 ✅ / C3 n/a | inherited from P1 |
| G6 exact recheck | ✅ | ✅ | ✅ | ✅ | ✅ |
| G7 provenance split (is it our JWT?) | n/a (one type) | n/a | n/a | ✅ | n/a |
| G11 own-credential refusal (C4) | n/a (code, not a bearer) | ✅ | n/a (no upstream call) | ✅ | n/a |
| G8 freshness | id_token `exp` | `kind=oidc`: id_token `exp`. `kind=oauth2`: **none we can see** — the opaque token's validity is the IdP's judgement, established by the `/userinfo` redemption | 10 min from `iat` | C2 as P2; C3 token's own `exp` (~15d) | bind session TTL |
| G10 IP rate limit | global IP floor **only** (`authorize`/`callback` carry no strict limiter) | `StrictIPRateLimitMiddleware(2 rps / 10 burst)`, shared with P3 | same limiter as P2 | `StrictIPRateLimitMiddleware`, `DM_INTEGRATION_IP_RATELIMIT_*` (predates this PR) | inherits P1 |
| G9 autolink | ✅ | ✅ | fail-closed | fail-closed | Create refuses on conflict |
| issuer namespace | upstream | upstream | upstream + `#bearer-jwt` | upstream (C2) / `#bearer-jwt` (C3) | upstream |
| writes identity rows | yes | yes | yes | **no** (read-only) | yes |

## Lifetime column — credentials and snapshots that outlive a deploy

A guard placed where a value is *produced* does not cover a value produced by the
previous binary. The bind path is the one place this service deliberately consumes
state written by an older version (`bind_claims_compat_test.go` pins that a legacy
snapshot must still decode), so its snapshots cross the deployment boundary.

| artefact | lifetime | produced by | re-validated on consumption? |
|---|---|---|---|
| `BindSession.ClaimsSnapshot` | `defaultBindTokenTTL` = 5 min, operator-configurable upward | `IssueWithReason` (guarded) — **or a previous binary** (unguarded) | ✅ `recheckSnapshotIdentity` in `Confirm` and `Create`, before any mutation |
| `StateData` (callback) | `stateTTL` = 5 min | `authorize` | consumed within one deploy in practice; claims are re-derived from the IdP, not restored |
| session token | `Cache.TokenExpire` (default 30d) | login | payload version is handled (`IsV3()`); identity bounds are not re-applied — the identity row already exists |
| `uk_` API key | until revoked | `/exchange` | resolved against the DB each request |

The general rule the bind case teaches: **if an artefact can be read by a binary
newer than the one that wrote it, the guard must run on the read side too.** The
matrix marked P5's `G3`/`G4` ✅ on the strength of the write-side guard; that was
the unenumerated cell.

## Construction-failure column

The cell both round-6 blockers occupied. For each component a guard depends on:
what does the affected path do when it failed to build?

| component | absent (legal) | construction failed (operator error) | verified by |
|---|---|---|---|
| `AuthProvider` | n/a — required | P1/P2/P3 `provider == nil` → 500; P4 → 500 on every request | `new_wiring_integration_test.go` |
| `BearerJWTVerifier` | `(nil, nil)` → P3 returns 500 ("not configured"); P4 accepts C2 only | `(nil, err)` → P3 500; **P4 refuses every credential**, including C2 | `TestBearerJWT_VerifierConstructionFailureFailsClosed`, `TestBearerJWT_AbsentSecretKeepsUpstreamPathWorking` |
| id_token store (`redisIDTokenStore`) | `PostLogoutRedirectURI` unset → RP-logout off, logged at Info | encryptor build fails → logged at Error, RP-logout off | `TestNew_IDTokenStoreConstructorIsReachableFromProduction_Integration` |
| exchange rate limiters | n/a | Redis unavailable → **fail-open** (deliberate: matches `pkg/wkhttp` policy) | `exchange_limiter_lifecycle_test.go` |
| audit sink | n/a | write failure logged, request proceeds | existing audit tests |

Why P4 refuses C2 as well when the C3 verifier failed: without a constructible
verifier the question "is this credential ours?" (G7) has no answer, and any
fall-through hands an unattributable credential to a path that puts it in a URL
query. Routing by token shape is not a substitute. An **absent** secret stays a
legal deployment shape; an **invalid** one must not silently become one.

## Cells that are open, not closed

- **G8 on P4-C3** — replay window equals the token's `exp` (~15 days), with no
  `aud` and no `jti`, on an endpoint that mints a user API key. Correct given the
  upstream JWT's claim set; the operational precondition is that the secret stays
  dedicated to this purpose. Adding `aud` needs upstream coordination.
- **G6 diagnostics** — a case-folded collision returns `(nil, nil)`, so the
  operator's log line is indistinguishable from a genuinely unbound user. A warn
  inside `QueryIdentityExact` would make it findable.
- **G4 units** — bytes vs characters; conservative on purpose, see the brief.
- **P2 field naming** — `access_token` carries an `id_token` under `kind=oidc`.
  Documented rather than renamed (breaking change for existing clients).
- **Capability-gated mounting** — P2 is mounted regardless of whether the
  configured provider can actually interpret a directly-presented credential.
