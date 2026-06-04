# AGENTS.md

This file provides guidance to coding agents when working in this repository.

## Project Overview

Octo-server is the Go backend for DMWork, an enterprise IM platform. It handles
business logic on top of WuKongIM for messaging transport.

- Go module: `github.com/Mininglamp-OSS/octo-server`
- Go version: 1.25
- Shared library: `github.com/Mininglamp-OSS/octo-lib` for config, wkhttp,
  testutil, register, and model packages
- Default branch: `main`

## Common Commands

```bash
# Build
docker build -t octo-server .

# Run tests for a module
go test ./modules/group/...
go test ./modules/message/ -run TestSendMsg

# Run all tests
go test ./...

# Lint
golangci-lint run ./...
```

## Architecture

### Request Flow

```text
HTTP (Gin/wkhttp) -> Auth Middleware (pkg/auth/) -> Space Middleware -> API Handler -> Service -> DB (MySQL/DBR)
                                                                                       |
                                                                                       v
                                                                                  WuKongIM (gRPC)
```

### Module System

Modules live in `modules/`. Each module is auto-registered with `init()` and
`register.AddModule()`.

Standard module layout:

- `1module.go`: registration entry using `init()` and `register.AddModule()`
- `api*.go`: HTTP handlers implementing `register.APIRouter.Route(r *wkhttp.WKHttp)`
- `service.go`: business logic, usually with an `IService` interface
- `db*.go`: database operations using `gocraft/dbr`
- `model.go`: data models and response structs
- `sql/`: SQL migrations embedded with `//go:embed sql`

### Key Packages

| Package | Purpose |
| --- | --- |
| `pkg/auth/` | Token parsing, `CacheTokenParser`, auth middleware |
| `pkg/errcode/` | Error code definitions per module, such as group, message, user, and OIDC |
| `pkg/httperr/` | `ResponseErrorL` and `ResponseErrorLWithStatus` error facades |
| `pkg/i18n/` | Localization SDK: code registry, localizer, renderer, language negotiation, and `locales/` |
| `internal/` | Internal wiring and module imports |
| `modules/base/event/` | Async event system |

## Error Handling and i18n

All user-facing error responses go through the i18n error envelope. Do not use
`c.ResponseError(errors.New(...))`, `c.ResponseErrorf(...)`,
`c.AbortWithStatusJSON(...)`, or non-OK `c.JSON(...)`; these are legacy patterns
and bypass the localized envelope.

Use the facades in `pkg/httperr`:

| Facade | Wire status | Use for |
| --- | --- | --- |
| `ResponseErrorL(c, code, params, details)` | Pinned 400 for D14 compatibility; real status in `error.http_status` | Default for legacy-bearing endpoints |
| `ResponseErrorLWithStatus(c, code, params, details)` | The code's real `HTTPStatus` | New endpoints only when no clients depend on fixed 400; maintainer sign-off is required when diverging from D14 |

```go
httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
```

Register error codes in `pkg/errcode/<module>.go`:

```go
ErrXxx = register(codes.Code{
    ID:             "err.server.<module>.<reason>", // or reuse err.shared.* codes
    HTTPStatus:     http.StatusBadRequest,
    DefaultMessage: "English source (D4).",
    SafeDetailKeys: []string{"field"},
    Internal:       false,
})
```

Error-code rules:

- 5xx codes must set `Internal=true`; 4xx codes must not. The renderer hides
  messages and details for internal errors, so log the cause with `zap.Error`
  before responding.
- Auth and verification failures use one generic anti-enumeration code, such as
  a single 401. Specific failure reasons go to logs only.
- `params` interpolate into the message template. `details` are structured
  client-facing fields filtered by `SafeDetailKeys`.
- Per-module helpers live in `modules/<module>/api_i18n.go`.
- `mustLookupSharedCode` resolves shared codes at init and should panic loudly
  when a shared code is not registered.

After adding or changing error codes, run:

```bash
make i18n-extract
make i18n-extract-check
make i18n-lint
```

Then add the zh-CN translation to `pkg/i18n/locales/active.zh-CN.toml` with one
`["id"]` block and `other = "..."` value per code.

Each migrated module has a `Test<Module>NoLegacyResponseError` source guard
that forbids legacy and raw error responses. Add new handler files to that
guard's file list. Protocol endpoints that intentionally keep raw responses,
such as OAuth2/OIDC browser-redirect flows, are exempted through
`tools/lint-direct-error-response/baseline.txt`.

Localized email templates live in
`modules/base/common/emailtmpl/templates/{lang}/`. Send functions take a `lang`
argument resolved through `i18n.OutboundLanguage(ctx)`; do not hardcode
localized subject or body strings.

## Rate Limiting

Use the shared middleware in octo-lib `pkg/wkhttp/ratelimit.go`. Do not
hand-roll Redis `INCR` and TTL counters for generic request-frequency limiting.

The rate limit layers set `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
`X-RateLimit-Scope`, and `X-RateLimit-Retry-After` headers, return the i18n
`rate.limited` response, and fail open on Redis errors.

| Middleware | Scope header | Dimension | Use for |
| --- | --- | --- | --- |
| `RateLimitMiddleware` | `ip` | Global per-IP | DDoS floor; already mounted globally in `main.go` via `route.Use`, so do not re-add it |
| `StrictIPRateLimitMiddleware(tag, rps, burst)` | `strict:{tag}` | Per-IP, per-endpoint | Unauthenticated sensitive endpoints such as login, register, SMS, search, group invite, and space invite |
| `SharedUIDRateLimiter(r, ctx)` | `uid` | Per-login-user shared bucket `ratelimit:uid:{uid}` | Default for authenticated endpoints |

`SharedUIDRateLimiter` wraps `UIDRateLimitMiddleware` and is defined in
`pkg/wkhttp/ratelimit_helper.go`. It is a process-wide singleton with one quota
per UID across all mounted routes. Defaults are 2 rps and burst 60, tunable via
`DM_API_UID_RATELIMIT_RPS` and `DM_API_UID_RATELIMIT_BURST`.

Mount `SharedUIDRateLimiter` after `AuthMiddleware`; otherwise it cannot read
the UID and silently fails open.

```go
auth := r.Group("/v1/foo", ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, ctx))
```

Per-resource cooldowns keyed by business identity, such as phone, email, or bind
session, may use a hand-written Redis counter when IP and UID buckets cannot
express the rule. Existing examples include `sms_rate_limit:{zone}@{phone}`,
`email_rate_limit:{email}`, and OIDC bind attempt caps. These are intentional;
generic HTTP request-frequency limiting is not.

Tests that hit UID-limited routes must reset the bucket in setup, such as
`ratelimit:uid:*`. The bucket persists in Redis and is not cleared by
`CleanAllTables`.

## Database

- ORM: `gocraft/dbr` v2
- Migration files: `modules/<name>/sql/<yyyyMMdd>-<seq>_<name>.sql`
- SQL migrations are embedded via `//go:embed sql`
- Field naming uses underscore conversion through `util.AttrToUnderscore()`

## Testing

Use the shared test utilities:

```go
_, ctx := testutil.NewTestServer()
defer testutil.CleanAllTables(ctx)
```

Tests require MySQL, Redis, and WuKongIM to be running. Check CI or
`make env-test` in dmworkim for the expected test environment.

When changing behavior, run the narrowest relevant `go test` package first. Run
broader tests when the change touches shared behavior, cross-module contracts,
middleware, auth, rate limiting, or database migrations.

## Coding Conventions

- Use English Conventional Commits when committing, such as `feat:`, `fix:`,
  `test:`, and `refactor:`.
- API routes use the `/v1/` prefix.
- Add a blank import in `internal/modules.go` for new modules.
- All routes go through `AuthMiddleware` unless explicitly excluded; document
  the reason when skipping auth.
- User-facing errors use `httperr.ResponseErrorL` with a registered
  `pkg/errcode` code. Do not use raw `c.ResponseError`, `c.JSON`, or
  `AbortWithStatusJSON` for non-OK responses.
- Run `make i18n-extract-check` and `make i18n-lint` after touching error
  codes or localized error behavior.
- Authenticated routes should mount `SharedUIDRateLimiter`.
- Unauthenticated sensitive endpoints should mount `StrictIPRateLimitMiddleware`.
- Do not hand-roll Redis counters for generic HTTP request-frequency limiting.
- Handlers that access user data must go through the Space middleware.
- Bot API code in `modules/bot_api/` must validate bot ownership before
  operations.
- Thread code in `modules/thread/` must verify parent channel access.
- Follow existing module, service, DB, and handler patterns before introducing
  new abstractions.
- Keep changes scoped to the requested behavior and avoid unrelated refactors.
- Run `gofmt` on edited Go files.
