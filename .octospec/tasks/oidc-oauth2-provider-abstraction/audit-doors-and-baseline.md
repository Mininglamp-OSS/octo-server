# Exhaustive audit: guards × doors, and every change against the real baseline

Written after round 9, in place of another reactive patch round. The point is to
enumerate the remaining surface **before** a reviewer does, and to produce a number:
is the backlog 4 items or 40?

## 0. Methodology defect found first — the baseline I had been using was stale

`main` in this worktree is `40627cc0` (2026-08-04). The real baseline is
`origin/main` = `508d3c90` (2026-08-31) — the merge base both reviewers used.

Every `git show main:<file>` comparison used to justify a "this is new" or "this is
unchanged relative to main" claim in earlier rounds was therefore read against a tree
**27 days old**. `git diff main..HEAD` reports hundreds of unrelated files (card
templates, notify, opanalytics, session rollout); `git diff 508d3c90..HEAD` reports
**102 files, +14790/−311**, which matches the reviews.

Three load-bearing claims were re-verified against `508d3c90` and all three hold:

| claim | verified at `508d3c90` |
|---|---|
| baseline mounts only `authorize`/`callback`/`logout` | `api.go:316-319` — confirmed, 3 routes |
| baseline `oidcAuth` verified locally with no egress | `integration/api.go:136` `oidcClient.VerifyIDToken` — confirmed |
| baseline `extractPhone` handled only `+86` | `service.go:238-243` — confirmed |

The conclusions survived; the method did not. Use `origin/main` for every baseline
comparison from here.

## 1. Axis A — every guard against every door

Doors were found by grepping call sites of the *protected operation*, not by reading
the matrix.

| guard | doors (call sites of the protected operation) | covered |
|---|---|---|
| don't forward our credential upstream | 2: `api_exchange.go:197`, `integration/api.go:276` | invalid-secret ✅ both; **absent-secret ❌ both** |
| storable-identity bounds before side effects | 5 `Insert(&IdentityModel)` + 4 `IssueSession` sites | ✅ all — every one is downstream of `ResolveOrLink` (`service.go:127`), `IssueWithReason` (`bind_service.go:116`) or `recheckSnapshotIdentity` (`:857`) |
| byte-exact `(issuer, subject)` lookup | all callers | ✅ compile-enforced — raw query unexported |
| provenance split (`IsForeignToken`) | 2 | ✅ both |
| own-credential refusal | 2 | ✅ both |
| boot agreement between the two config readers | 6 fatal checks | **3 gaps — see §2** |

**Conclusion for axis A:** the door count is small (2 for the forwarding class). The
recurrence was never about door count — it was about fixing the demonstrated instance
instead of the predicate. Both open items are one predicate each, applied to 2 doors.

## 2. The lockout class, enumerated exhaustively

`modules/oidc/config.go` has exactly six boot-fatal checks. This is the complete list;
"mirrored" means `modules/common.isOIDCFullyConfigured` reaches the same verdict.

| # | fatal check | mirrored |
|---|---|---|
| 1 | `kindRefusal` → `oidcboot.ValidateKind` | ✅ both delegate to the one rule set |
| 2 | required env (`ISSUER`/`CLIENT_ID`/`CLIENT_SECRET`/`REDIRECT_URI`) | ✅ |
| 3 | `DM_OIDC_RT_ENC_KEY` base64 + 32 bytes | ✅ (`system_settings.go:565`) |
| 4 | `DM_OIDC_PROVIDER_ID` regex | ✅ (`oidcProviderIDRe`, :491) |
| 5 | `validateLogoutURL("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI")` (`config.go:237`) | ❌ **absent** |
| 6 | `validateLogoutURL("OCTO_OIDC_PROVIDER_END_SESSION_URL")` (`config.go:240`) | ❌ **absent** |

Plus one that is not a rule but a *normalisation* divergence:

| 7 | issuer fallback decides on the untrimmed value (`config.go:285` `p.BaseURL == ""`) while the mirror trims first | ❌ **diverges** — this is "recipe B" |

The mirror's own doc comment (`system_settings.go:516-529`) enumerates what it covers
and ends with *"We intentionally do NOT replicate non-fatal checks"*. Items 5 and 6 are
**fatal**, and absent — the comment is what made the gap invisible.

**Key-space measurement**: `modules/oidc` reads 42 `DM_OIDC_*`/`OCTO_OIDC_*` keys, the
mirror reads 21. The 21-key difference is almost entirely non-fatal (scopes, timeouts,
sync intervals, display name). Only the logout URLs above are fatal-and-unmirrored.

**So the lockout backlog is exactly 3 items, not an open-ended class.**

## 3. Axis B — every change against the real baseline, from the existing deployment's side

32 production Go files changed; **14 modify pre-existing files**, which is the entire
axis-B scope. Going through all 14:

| file | change | effect on an existing `kind=oidc` deployment |
|---|---|---|
| `service.go` | `extractZone`/`extractPhone` → `normalizePhone` | **behaviour change, undisclosed.** Looser (`86…`, `0086…`, separators now parse) and stricter (`+86` + landline now yields `""`). With `AutoLinkByPhone` defaulting true, a first SSO login that previously created an account can now link into an existing one — an irreversible identity write. No test connects the parser to `ResolveOrLink`. |
| `db.go` + `user_adapter.go` | login lookup now byte-exact | **behaviour change, undisclosed.** Stricter on the live path: if an IdP ever changed the case of its `sub`, those users go from "linked" to "not linked" and, with `AllowNewUser` defaulting true, get a **new empty account** plus a new irreversible identity row. Safe for a consistent IdP; unlisted either way. |
| `accesslog.go` | scrub regex gains `client_secret`, `refresh_token`, `access_token`, `id_token`, `code`, `state` | **service-wide, undisclosed.** `code`/`state` are generic parameter names — `modules/group/api.go:4861,5028` uses `?code=` for invite codes, so those are now redacted everywhere. Direction is fail-safe (over-redaction) and arguably desirable, but it is not an OIDC-scoped change. |
| `metrics.go` | new result label sets | 4 labels added in rounds 8–9 (`own_business_jwt`, `own_credential`, `provenance_undecided`, `verifier_unavailable`) are **not in the pre-warm list**, so they are absent from `/metrics` until first occurrence — an alert on them cannot be written in advance. |
| `main.go` | global `BearerTokenCompat` | disclosed and in scope. Its stated assumption re-verified: bot-token routes either do not mount `AuthMiddleware` (`usersecret/api.go:105`) or explicitly take a user token (`bot_api/obo_api.go:5`), and a bot token cannot resolve in the session cache anyway. |
| `api.go` | routes, logout ordering, id_token wiring, exchange opt-in | exchange endpoints now opt-in ✅; logout device-scope ordering fixed ✅ |
| `bind_service.go` | snapshot recheck, capability gate | ✅ |
| `config.go` | kind handling, `ExchangeEnabled` | recipe B open (§2 item 7) |
| `sync_worker.go` | interface gains `KickDevice` | additive |
| `model.go` | new audit events | additive |
| `errcode/oidc.go` | new codes | additive |
| `common/system_settings.go` | delegates to `oidcboot` | ✅, minus §2 items 5–7 |
| `integration/api.go` | provider kinds, bearer JWT, own-credential | absent-secret open (§1) |
| `oidc/metrics.go` | (see above) | |

## 4. The number

| class | open items |
|---|---|
| credential forwarding | **1** predicate (absent secret) × 2 doors |
| boot agreement / lockout | **3** (2 unmirrored fatal checks + 1 normalisation divergence) |
| undisclosed existing-deployment behaviour | **3** (phone→autolink, byte-exact unlink, accesslog widening) |
| observability | **1** (4 labels unwarmed) |
| artefact accuracy | **3** (matrix G7, G11, broken table) |
| methodology | **1** (stale baseline — fixed by using `origin/main`) |

**Total: 12 items, all enumerated, none open-ended.** Four came from reviewers this
round; three the audit found that no reviewer has raised; the rest are known.

This is the number that was missing from the split-or-continue argument. The recurring
classes are finite and now bottomed out: 2 doors for forwarding, 6 fatal checks for
lockout, 14 files for existing-deployment impact.

## 5. What the audit says about method

Two of round 9's four findings were introduced *by round 8's fix*, and one of round 9's
own edits broke the artefact it was editing. That is the real driver of the round count —
not the size of the diff. The countermeasures that follow from this audit:

1. **Fix the predicate, not the demonstrated instance.** Both recurring blockers were
   "reviewer showed case X, X was fixed, case Y was in the same review text".
2. **Enumerate doors by grepping the protected operation**, never by reading the matrix —
   the matrix is downstream of the code and has been wrong three rounds running.
3. **Prefer a test over a documented claim.** The prefix-coverage guard caught `app_`
   immediately; the mirror's doc comment hid two fatal checks for the whole PR.
4. **Baseline is `origin/main`,** not the local `main` ref.
