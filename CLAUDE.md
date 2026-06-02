# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## Project Overview

Octo-server is the Go backend for DMWork (enterprise IM platform). It handles business logic on top of [WuKongIM](https://github.com/WuKongIM/WuKongIM) for messaging transport.

- **Go Module**: `github.com/Mininglamp-OSS/octo-server`
- **Go Version**: 1.25
- **Shared Library**: `github.com/Mininglamp-OSS/octo-lib` (config, wkhttp, testutil, register, model)
- **Default Branch**: `main`

## Common Commands

```bash
# Build
docker build -t octo-server .

# Run tests (single module)
go test ./modules/group/...
go test ./modules/message/ -run TestSendMsg

# Run all tests
go test ./...

# Lint
golangci-lint run ./...
```

## Architecture

### Request Flow

```
HTTP (Gin/wkhttp) → Auth Middleware (pkg/auth/) → Space Middleware → API Handler → Service → DB (MySQL/DBR)
                                                                          ↓
                                                                    WuKongIM (gRPC)
```

### Module System

27 modules in `modules/`, each auto-registered via `init()` + `register.AddModule()`.

Standard module structure:
- `1module.go` — registration entry (`init()` + `register.AddModule()`)
- `api*.go` — HTTP handlers implementing `register.APIRouter.Route(r *wkhttp.WKHttp)`
- `service.go` — business logic, typically defines `IService` interface
- `db*.go` — database operations using `gocraft/dbr`
- `model.go` — data models and response structs
- `sql/` — SQL migrations embedded via `//go:embed sql`

### Key Packages

| Package | Purpose |
|---------|---------|
| `pkg/auth/` | Token parsing, CacheTokenParser, auth middleware |
| `pkg/errcode/` | Error code definitions per module (group.go, message.go, user.go, oidc.go) |
| `pkg/httperr/` | `ResponseErrorL` / `ResponseErrorLWithStatus` error facades |
| `pkg/i18n/` | Localization SDK: codes registry, localizer, renderer, language negotiation, `locales/` |
| `internal/` | Internal wiring, module imports |
| `modules/base/event/` | Async event system |

### Error Handling & i18n (Localization)

All user-facing error responses go through the i18n error envelope. **Never** use
`c.ResponseError(errors.New(...))`, `c.ResponseErrorf(...)`, `c.AbortWithStatusJSON(...)`,
or non-OK `c.JSON(...)` — these are legacy and bypass the localized envelope.

**Two facades** (`pkg/httperr`) — the envelope body is identical, only the wire status differs:

| Facade | Wire status | Use for |
|---|---|---|
| `ResponseErrorL(c, code, params, details)` | pinned **400** (D14 compat); real status in `error.http_status` | **default** — every legacy-bearing endpoint |
| `ResponseErrorLWithStatus(c, code, params, details)` | the code's real `HTTPStatus` | **new endpoints only** with no clients depending on fixed-400 (currently just `modules/oidc` bind); diverging from D14 needs maintainer sign-off |

```go
httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
```

**Error codes** — register in `pkg/errcode/<module>.go`:
```go
ErrXxx = register(codes.Code{
    ID:             "err.server.<module>.<reason>", // or reuse err.shared.* (auth/rate/param/internal/not_found)
    HTTPStatus:     http.StatusBadRequest,
    DefaultMessage: "English source (D4).",          // zh-CN runtime translation goes in active.zh-CN.toml
    SafeDetailKeys: []string{"field"},               // whitelist for details; all other keys are dropped
    Internal:       false,                            // see invariant below
})
```
- **5xx ⟺ `Internal=true`** (renderer hides the message + details; log the cause via `zap.Error` before responding). 4xx codes must NOT be Internal.
- **Anti-enumeration**: auth / verify failures map to ONE generic code (e.g. a single 401), never a per-reason code — the specific reason goes to logs only.
- **Params vs Details** (D15): `params` interpolate into the message template; `details` are structured fields surfaced to the client, filtered by `SafeDetailKeys`.

**Per-module helpers** live in `modules/<module>/api_i18n.go` (`respond<Module>Xxx` for detail-carrying shapes; `mustLookupSharedCode` resolves shared codes at init, panicking loudly if unregistered).

**After adding/changing any code, these must pass** (also enforced in CI):
```bash
make i18n-extract        # regenerate en-US markers from codes.Register call sites
make i18n-extract-check  # 100% recall: every registered code has a marker
make i18n-lint           # D23 guard (no new raw error responses) + unregistered-code check
```
Then add the zh-CN translation to `pkg/i18n/locales/active.zh-CN.toml` (one `["id"]` + `other = "..."` block per code).

**Guard test**: each migrated module has a `Test<Module>NoLegacyResponseError` source guard forbidding legacy/raw responses — add any new handler files to its list. Protocol endpoints that intentionally keep raw responses (e.g. OAuth2/OIDC browser-redirect flow) are exempted and tracked in `tools/lint-direct-error-response/baseline.txt`.

**Emails**: localized templates live in `modules/base/common/emailtmpl/templates/{lang}/` (per-language `subject`/`html`/`text`, go:embed). Send functions take a `lang` arg resolved via `i18n.OutboundLanguage(ctx)` — never hardcode subject/body strings.

### Rate Limiting

Use the shared middleware in octo-lib `pkg/wkhttp/ratelimit.go` — do NOT hand-roll Redis `INCR`/TTL counters for request-frequency limiting. Three layers, each sets `X-RateLimit-Limit/Remaining/Scope/Retry-After` headers, returns i18n `rate.limited`, and is **fail-open** on Redis errors:

| Middleware | Scope header | Dimension | Use for |
|---|---|---|---|
| `RateLimitMiddleware` | `ip` | global per-IP | DDoS floor — already mounted globally in `main.go` (`route.Use`), don't re-add |
| `StrictIPRateLimitMiddleware(tag, rps, burst)` | `strict:{tag}` | per-IP, per-endpoint | unauthenticated sensitive endpoints (login/register/sms/search/group_invite/space_invite) |
| `SharedUIDRateLimiter(r, ctx)` (wraps `UIDRateLimitMiddleware`) | `uid` | per-login-user, shared bucket `ratelimit:uid:{uid}` | **default for authenticated endpoints** |

`SharedUIDRateLimiter` (`pkg/wkhttp/ratelimit_helper.go`) is a process-wide singleton — one quota per UID across all mounted routes (default 2 rps / burst 60, tunable via `DM_API_UID_RATELIMIT_RPS`/`_BURST`). **Mount it AFTER `AuthMiddleware`** on the route group, else it can't read the uid and silently fails open:

```go
auth := r.Group("/v1/foo", ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, ctx))
```

**Exception** — per-resource cooldowns keyed by a business identity (phone/email/bind-session), which the IP/UID buckets cannot express, may use a hand-written Redis counter: e.g. `sms_rate_limit:{zone}@{phone}` (`base/common/service_sms.go`), `email_rate_limit:{email}` (`base/common/service_email.go`), OIDC bind attempt caps. These are intentional; generic HTTP request-frequency limiting is not.

Tests that hit UID-limited routes must reset the bucket in setup (`ratelimit:uid:*`) — see `category` test's `resetUIDRateLimit`; the bucket persists in Redis and is NOT cleared by `CleanAllTables`.

### Database

- ORM: `gocraft/dbr` v2
- Migration files: `modules/<name>/sql/<yyyyMMdd>-<seq>_<name>.sql`, embedded via `//go:embed sql`
- Field naming: underscore (`util.AttrToUnderscore()`)

## Testing

```go
_, ctx := testutil.NewTestServer()
defer testutil.CleanAllTables(ctx)
```

Tests require MySQL + Redis + WuKongIM running (see CI or `make env-test` in dmworkim).

## Coding Conventions

- Commit messages: English, Conventional Commits (`feat:`, `fix:`, `test:`, `refactor:`)
- API routes: prefix `/v1/`
- New modules: add blank import in `internal/modules.go`
- Auth: all routes go through `AuthMiddleware` unless explicitly excluded — document why if skipping
- i18n: user-facing errors use `httperr.ResponseErrorL` + a registered `pkg/errcode` code; never raw `c.ResponseError`/`c.JSON`/`AbortWithStatusJSON`. Run `make i18n-extract-check` + `make i18n-lint` after touching codes (see Architecture › Error Handling & i18n)
- Rate limiting: mount `SharedUIDRateLimiter` (auth routes) or `StrictIPRateLimitMiddleware` (unauth) — never hand-roll a Redis counter for request-frequency limiting (see Architecture › Rate Limiting)
- Space isolation: handlers that access user data must go through Space middleware
- Bot API (`modules/bot_api/`): validate bot ownership before operations
- Thread (`modules/thread/`): verify parent channel access
