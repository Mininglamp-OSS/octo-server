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
| C4 | **credentials this service issues** — session token, `uk_` user API key, `bf_` bot token, `app_` App Bot token | our own clients | session store lookup / prefix |

C4 was the taxonomy's blind spot for one round: C1–C3 are all credentials the
*upstream* issues, and the provenance guard (G7) answered "is this an HS256 JWT we
signed?" — a true statement about a strict subset of "ours". A session token or a
`uk_` key is not a JWT at all, so it fell out of the classification entirely and was
forwarded to the vendor's `/userinfo` in a URL query. Both arrive on this header for
mundane reasons: the global `BearerTokenCompat` middleware makes
`Authorization: Bearer <session token>` the house convention, and `userAPIKeyAuth`
reads the *same* header on sibling routes in the *same* route group.

The row was still incomplete when first written — `app_` App Bot tokens were missed,
and so was C3 on `/exchange` (below). A prefix list is a denylist of types someone
remembered, so `own_credential_coverage_test.go` now scans the credential-minting
packages for `*TokenPrefix`/`*KeyPrefix` constants and fails if `Classify` does not
recognise one. That turns the next omission into a CI failure naming the constant,
rather than a leak.

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
| G5 short-numeric refusal ("looks like an employee number") | the **producer** — requires a personnel system that reuses numbers | gated on `Capabilities().SubjectMayBeReusedPersonnelID`: declared **true** by `oauth2Provider` only. Per-kind expectations pinned by `TestAuthProviderConformance_ShortNumericSubjectFollowsCapability` | `kind=oauth2` on P1/P2/P4-C2, plus P5 snapshot consumption via the same bit. **Not** under `kind=oidc`: that is the generic client existing deployments already run against arbitrary IdPs, and a self-hosted IdP emitting its own primary keys as `sub` (`1001`, `42`) is normal. **Not** P3/P4-C3: that subject is our own DB primary key, never reused |
| G6 byte-exact `(issuer, subject)` recheck | the `utf8mb4_general_ci` collation | `DB.QueryIdentityExact`; the raw query is unexported so this is compiler-enforced | all read paths |
| G7 credential provenance split ("is this ours?") | the HMAC | `IsForeignToken`, allowlist over `ErrJWTForeign` | **P2 and P4** — both accept two credential types on one field. The row said "P4 only" for three rounds while the row below already recorded P2; `/exchange` had no verifier stage at all until round 9 |
| G8 freshness | the purpose | **P3 (redemption):** the redemption ledger — first redemption within `F` of `iat` (default 24h), repeats within `T` of the last one (default 7d), `F` capped at `T`. **P4-C3 (standing authenticator):** the token's own `exp`, no ledger. | P3 uses the former, P4-C3 the latter. *(Superseded by #843: `VerifyForRedemption`'s 10-minute `iat` ceiling — `bearerJWTMaxAge` — is deleted; that method is now freshness-agnostic and the control lives in `modules/oidc/redemption_ledger.go`.)* |
| G11 own-credential refusal | which system minted the credential | `oidc.OwnCredentialDetector` — `uk_` / `bf_` / `app_` prefix, then a session-store lookup — called immediately before any upstream call. Prefix coverage is pinned by a source scan over all of `modules/`, so a new credential-minting module is a CI failure rather than a leak | P2, P4. Fails **closed**: if the session store is unavailable the answer is "undecided", and undecided must not forward |
| G12 JWT-shape refusal | the provider declaring `OpaqueClientCredential` | `JWTShapedCredentialMustNotBeForwarded` — refuse **any** JWT-shaped credential that reaches the fall-through, whether or not our bearer verifier is configured. Not shape-based routing: this vendor's `access_token` is an opaque UUID, so a JWT provably cannot succeed here. Vendor fact, hence a capability — the standard kind's client credential *is* a JWT. **Must run after HMAC verification**, since a valid business JWT is also JWT-shaped | P2, P4 under `kind=oauth2` only |
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
| G7 provenance split applied to C3 on P2 | n/a | ✅ (`bearerJWT` runs before the fall-through, mirroring P4) | n/a | ✅ | n/a |
| G8 freshness | id_token `exp` | `kind=oidc`: id_token `exp`. `kind=oauth2`: **none we can see** — the opaque token's validity is the IdP's judgement, established by the `/userinfo` redemption | redemption ledger: `F` from `iat` on a first redemption, `T` from `last_at` on a repeat *(was: 10 min from `iat`, removed in #843)* | C2 as P2; C3 token's own `exp` (~15d) | bind session TTL |
| G10 IP rate limit | global IP floor **only** (`authorize`/`callback` carry no strict limiter) | `StrictIPRateLimitMiddleware(2 rps / 10 burst)`, shared with P3 | same limiter as P2 | `StrictIPRateLimitMiddleware`, `DM_INTEGRATION_IP_RATELIMIT_*` (predates this PR) | inherits P1 |
| G9 autolink | ✅ | ✅ | fail-closed | fail-closed | Create refuses on conflict |
| issuer namespace | upstream | upstream | upstream + `#bearer-jwt` | upstream (C2) / `#bearer-jwt` (C3) | upstream |
| writes identity rows | yes | yes | yes | **no** (read-only) | yes |

## Cells that are open, not closed

**Closed since the JWT-shape refusal became unconditional** — kept here as a record of
why it was open and what closed it, because the reasoning changed rather than the risk
appetite.

It was open on the grounds that a non-HS256 header is refused *before* the signature is
computed (algorithm-confusion defence), the failure carries `ErrJWTForeign`, and an
unattributable credential can only be forwarded — classifying every pre-signature failure
as ours would cut off C2, which is why these endpoints exist. The accepted risk was that
anyone could make this service write a string of their choosing into the vendor's access
log, with the invariant intact because a token we did not sign is not our credential.

What closed it is a different argument, not a stricter reading of that one: this vendor's
`access_token` is an **opaque UUID**, so a JWT-shaped value cannot be a valid credential
for it whatever its signature says. Forwarding therefore cannot succeed — it can only
leak. G12 refuses on shape × capability, so the signature question does not arise, and the
"no secret needed to mint one" observation stops mattering.

The generic worry that closing it would break C2 does not apply here, because the
capability bit is what scopes it: a provider whose access tokens really are JWTs simply
does not declare `OpaqueClientCredential`. Pinned by
`TestForeignJWTShape_IsRefusedNotForwarded`, with the paired negative
(`TestExchange_AbsentSecretStillForwardsOpaqueCredential`) asserting an opaque token still
reaches `/userinfo`.

**Prefixless legacy bot tokens.** `modules/bot_api/auth.go` still accepts bot tokens with
no prefix, and those have no decidable shape and are not in the session store, so
`OwnCredentialDetector` cannot recognise them. Deciding by a `robot`-table lookup would put
a DB round trip on every unauthenticated request and turn these endpoints into an
existence oracle for bot tokens. Whether any such token still exists in production is an
operations question, on the human-verify list.

## Reachability column — a guard that costs availability where there is nothing to guard

`/exchange` and the two `modules/integration` endpoints now consult the session store on every
request to decide credential provenance. Under `kind=oidc` the credential is never forwarded
(`oidcProvider.IdentityFromClientCredential` verifies the id_token locally), so on that kind
the guard can only cost availability — a session-store outage turns working endpoints into
500s. Recorded as an open item rather than fixed: gating it needs a capability bit meaning
"the credential leaves the process unverified", and inventing one for a single call site is
worse than the cost it saves. Revisit if a third kind appears.

Related and fixed this round: under `kind=oidc` those same endpoints were issuing a
`/userinfo` request on **every** request with an empty `access_token` — architecturally
guaranteed to 401, swallowed, one warn line per request, on a path that was purely local
before this change. `oidcProvider.Identity` now returns early when there is no access token.

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
| `BearerJWTVerifier` | `(nil, nil)` → P2 accepts C2 only, P3 returns 500 ("not configured"), P4 accepts C2 only | `(nil, err)` → P3 500; **P2 and P4 both refuse every credential**, including C2 | `TestExchange_FailsClosedWhenVerifierConstructionFailed`, `TestNew_RetainsBearerVerifierConstructionError_Integration`, `TestBearerJWT_VerifierConstructionFailureFailsClosed`, `TestExchange_AbsentSecretKeepsUpstreamPathWorking` |
| id_token store (`redisIDTokenStore`) | `PostLogoutRedirectURI` unset → RP-logout off, logged at Info | encryptor build fails → logged at Error, RP-logout off | `TestNew_IDTokenStoreConstructorIsReachableFromProduction_Integration` |
| exchange rate limiters | n/a | Redis unavailable → **fail-open** (deliberate: matches `pkg/wkhttp` policy) | `exchange_limiter_lifecycle_test.go` |
| audit sink | n/a | write failure logged, request proceeds | existing audit tests |

**P2 was the empty cell in this column for one round.** The round-8 verifier stage on
`/exchange` was written as `if o.bearerJWT != nil`, and `modules/oidc`'s `New()` logged the
construction error and discarded it — so a 31-byte secret produced a nil verifier that meant
both "not configured" (legal) and "misconfigured" (must refuse), and the stage was skipped
silently. `modules/integration` had already been fixed for exactly this in round 6 and kept
the error on the struct; the fix was copied to `/exchange`, the failure direction was not.

The distinction that makes it a leak rather than a confusing 401: an **absent** secret means
no C3 credential can exist, but an **invalid** one is shared out-of-band with the client
backend, which signs with it. HMAC does not care about key length — the 32-byte floor is our
admission policy — so the token carries a signature that is valid under our configured secret.

**The absent case was open for one round after that.** `(nil, nil)` also skipped the stage,
and the argument that closed the invalid case applies unchanged: the client backend holds and
signs with its own secret regardless of our configuration. It is now closed by G12 — with no
key there is no attribution to be had, so a JWT-shaped credential is refused on the grounds
that it provably cannot succeed against an opaque-token provider.

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
