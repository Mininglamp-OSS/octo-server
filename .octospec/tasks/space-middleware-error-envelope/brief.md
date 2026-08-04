---
type: Task
title: "Task: space-middleware-error-envelope"
description: Move SpaceMiddleware's 401/403/500 exits from raw c.AbortWithStatusJSON bodies into the i18n error envelope, so every endpoint mounting it has an error code clients can branch on.
tags: ["space", "isolation", "auth", "error-handling", "i18n", "wire-contract", "testing", "commit"]
timestamp: 2026-08-04T00:00:00Z
# --- octospec extension fields ---
slug: space-middleware-error-envelope
upstream: split out of space-directory-api (#692) during review
source: user
---

# Task: space-middleware-error-envelope

## Goal

`SpaceMiddleware` answers its three failure exits with
`c.AbortWithStatusJSON(status, gin.H{"msg": "..."})` — a bare body with no error
code and a hardcoded Chinese string that does not respond to `Accept-Language`.
Move all three onto the repo's i18n error envelope so clients can branch on
`error.code` instead of guessing from an HTTP status, and so the primary
auth-failure exit of every endpoint behind this middleware stops sitting outside
the OpenAPI contract.

Codes reuse existing registrations — `err.shared.auth.required` (401),
`err.server.space.not_member` (403), `err.server.space.query_failed` (500). No new
codes, so no new i18n markers.

## Background

**Why it is a separate task.** It was originally implemented inside
`space-directory-api` (#692) and split back out in review, for two reasons.

1. It violated that task's own brief, which says at `brief.md:259` that the
   middleware's "401/403 branches MUST be unchanged" and at `:287` to use
   `ResponseErrorL` (pinned 400), **not** `ResponseErrorLWithStatus`. The
   implementation did the opposite of both, and unlike the path-priority decision
   in the same task, the brief was never updated to match — so it silently stopped
   describing the code.
2. Blast radius. #692's governing principle is not changing what existing clients
   see — strict enough that it freezes two endpoints carrying known defects rather
   than repair them. This change alters the error body of **all 13 route groups**
   mounting `SpaceMiddleware`, none of which are in that task's scope. That deserves
   its own review, not a ride-along.

   The full list, enumerated rather than recalled (an earlier draft of this brief
   named a set that matched neither branch — it omitted `robot` and counted two
   endpoints that exist only on #692):

   | module | group |
   |---|---|
   | `channel` | `/v1` (`api.go:55`) |
   | `user` | `/v1/user/pinned` (`api.go:265`), `/v1/user/space` (`:274`) |
   | `robot` | `/v1` → `GET /v1/robot/space_bots` (`api.go:420`) |
   | `search` | `/v1/search` (`api.go:44`) |
   | `message` | `/v1/sidebar` (`api_sidebar.go:170`), `/v1/message` (`api.go:356`, `:399`), `/v1/reactions` (`:391`), `/v1/reaction` (`:395`), `/v1/conversation` (`api_conversation.go:111`) |
   | `conversation_ext` | `/v1/follow/*` (`1module.go:62`) |
   | `messages_search` | `/v1/messages/_search*` (`api.go:136`) |

   Reproduce with:
   `grep -rn "space\.SpaceMiddleware(\|spacepkg\.SpaceMiddleware(" --include=*.go modules/ pkg/ internal/ | grep -v _test.go`

**This is not purely additive, and the earlier claim that it was is false.** #692's
description said the envelope is dual-form so a client reading only `msg` is
unaffected. The `msg` *field* does survive; its *value* does not, because the
renderer recomputes it from the code's localized message rather than passing the
old hardcoded string through:

| exit | before | after |
|---|---|---|
| 401 | `请先登录` | `请先登录！` |
| 403 | `无权访问该 Space` | `你不是该空间成员。` |
| 500 | `校验 Space 成员身份失败` | `服务器内部错误。` |

The 500 is the largest change: `ErrSpaceQueryFailed` is `Internal: true`, so the
renderer short-circuits to the generic message and the specific cause leaves the
body entirely (by repo convention it goes to logs only).

**Nothing caught it.** The existing middleware tests assert status codes and never
bodies — which is the whole surface this change touches. That gap is why the wrong
claim survived into a PR description, and closing it is part of this task.

## Load-bearing list

- **wire-contract** — the response body of every endpoint mounting
  `SpaceMiddleware`. Any client matching on `msg` **text** is affected; clients
  branching on HTTP status or on the new `error.code` are not. The wire status of
  each exit is unchanged (401/403/500). (rules: error-handling)
- **`ResponseErrorLWithStatus` vs `ResponseErrorL`** — this uses `WithStatus`, and
  that is a deliberate divergence from the D14 default that CLAUDE.md says needs
  maintainer sign-off. The reason is specific to a middleware: `ResponseErrorL`
  pins the wire status at 400, and these exits have always been 401/403/500, so the
  D14 default would itself be the breaking change — it would break every client
  branching on status, which is strictly worse than the `msg` text change. The
  facade's doc comment lists its permitted consumers; this adds the middleware to
  that list. (rules: error-handling)
- **anti-enumeration** — the 403 body must not echo the `space_id`. A non-member
  should not have the Space's existence confirmed. Pinned by test. (rules:
  space-isolation)
- **`Internal=true` ⟺ 5xx** — `ErrSpaceQueryFailed` is Internal, so the cause must
  be logged, never returned. Note the logging half was **not** already true: neither
  `spaceMiddleware` nor `CheckMembership` logged the DB error, and the dbr session
  is built with a `NullEventReceiver`, so nothing did. Before this change the body
  at least carried `校验 Space 成员身份失败`, which set this exit apart from other
  500s; afterwards it is byte-identical to every Internal error in the fleet. This
  change therefore owes the `zap.Error` call, and adds it. (rules: error-handling)
- **i18n** — messages now come from the code registry and follow
  `Accept-Language`. Previously they were hardcoded zh-CN regardless of the
  request. (rules: error-handling)
- **`tools/lint-direct-error-response/baseline.txt`** — `pkg/space/middleware.go`
  is recorded there with a count of 4. Once the raw responses are gone the count
  drops to 0. (rules: error-handling)
- **testing** — the middleware's existing tests assert status codes only. This task
  adds body assertions, including the exact post-migration `msg` strings, so the
  behavior change is machine-checked rather than described in prose. (rules:
  testing)
- **commit** — English Conventional Commits. (rules: commit-style)

## Out of scope

- **Changing any wire status.** 401 stays 401, 403 stays 403, 500 stays 500.
- **The middleware's authorization logic**, its cache TTLs, and how it resolves
  `space_id` (query then `X-Space-ID`, inline at `middleware.go:136-139`) — all
  unchanged here. (A path-param lookup is added by `space-directory-api`; it does
  not exist on this branch.)
- **Migrating other modules' raw error responses.** The baseline still tracks 14
  other files; they migrate on their own schedule.
- **Registering new error codes.** All three exits reuse existing registrations.
- **`GET /v1/space/{space_id}/directory`'s own spec.** That endpoint lives on the
  `space-directory-api` branch, not here. Its `directory.yaml` documents the
  *current* raw middleware shape via a `MiddlewareRawError` schema and points at
  this task; updating it to the envelope is a follow-up once both land.
- **`.octospec/journal/shared/message-reaction-hardening.md:129`**, which records
  the raw `{"msg":"无权访问该 Space"}` body as a contract octo-web must handle on
  the reaction endpoints. That statement stops being true when this lands. Journals
  are historical records, so it is not edited here — but it is the only place in
  either repo where the old shape is written down as a client obligation, so it is
  named rather than left to be discovered.

## Acceptance

- All three exits go through `httperr.ResponseErrorLWithStatus`; no
  `c.AbortWithStatusJSON` remains in `pkg/space/middleware.go`, and the file's
  baseline count drops from 4 to 0.
- New tests assert **bodies**, not just statuses, using the real i18n renderer
  (not `wkhttp`'s `defaultErrorRenderer` fallback, whose shape does not exist in
  production):
  - each exit's `error.code`, `error.http_status`, wire status, and legacy
    `msg`/`status`;
  - the **exact** post-migration `msg` string for all three, so the "is this
    additive?" question is answered by an assertion instead of a claim;
  - the 403 body does not contain the requested `space_id`;
  - the cache-hit 403 and the DB-checked 403 produce identical bodies (two separate
    code paths that can drift);
  - the internal cause never reaches the 500 body;
  - a member still passes through with the handler's own 200 body untouched;
  - `Accept-Language: en-US` yields the English message and `Content-Language:
    en-US`, while an unsupported language falls back to the default. This one is
    also an assembly test: it only passes if `i18n.EarlyMiddleware` is mounted
    ahead of this middleware, as `main.go:180` does, so it guards against that
    ordering regressing.
- The test router mirrors `main.go`'s wiring (`SetErrorRenderer` at `:139`,
  `UseGin(EarlyMiddleware)` at `:180`) rather than approximating it, and every one
  of the new tests must fail if the middleware is reverted to
  `c.AbortWithStatusJSON` — verified by doing exactly that.
- The DB-check failure is logged with `zap.Error` plus `space_id` and `uid` before
  responding, satisfying the 5xx/Internal invariant's second half.
- `go test ./pkg/space/...` passes; `make i18n-lint` and `make i18n-extract-check`
  pass; `golangci-lint run ./...` clean.
- The PR description states the `msg` value change explicitly, with the before/after
  table, and names status-code branching and `error.code` as the unaffected paths.
