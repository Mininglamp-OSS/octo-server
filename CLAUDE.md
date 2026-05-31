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
| `pkg/errcode/` | Error code definitions per module (group.go, message.go, user.go) |
| `internal/` | Internal wiring, module imports |
| `modules/base/event/` | Async event system |

### Error Handling

Use `httperr.ResponseErrorL()` with typed error codes from `pkg/errcode/`:
```go
httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
```
Do NOT use raw `c.ResponseError(errors.New(...))` — that's legacy pattern.

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
- Space isolation: handlers that access user data must go through Space middleware
- Bot API (`modules/bot_api/`): validate bot ownership before operations
- Thread (`modules/thread/`): verify parent channel access
