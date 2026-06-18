---
id: space-isolation
tier: repo
priority: 92
load_bearing: true
inject_when:
  paths: ["modules/**/*.go", "internal/**/*.go"]
  touches: ["space", "isolation", "auth", "bot-api", "thread", "acl"]
source: self
supersedes: []
---

# Space isolation & access control

Handlers that access user data must enforce isolation and ownership. A read or
write must never cross a Space boundary.

## Rules

- Handlers accessing user data must go through the Space middleware.
- All routes go through `AuthMiddleware` unless explicitly excluded — if you
  skip it, document why in the code.
- Bot API (`modules/bot_api/`): validate bot ownership before any operation.
- Thread (`modules/thread/`): verify parent channel access before acting.
- API routes are prefixed `/v1/`.
- New modules: add a blank import in `internal/modules.go`.

## Why load-bearing

Space isolation and ownership checks are the core multi-tenant security
boundary; a missing or fail-open check is a cross-tenant data leak (P0).
