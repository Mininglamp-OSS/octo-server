---
type: Task
title: "Task: space-member-bot-active"
description: Denormalise the mutable bot enabled-state onto space_member so the directory's all and bot slices drop their robot join, with a sync hook and a reconciler to bound drift.
tags: ["space", "isolation", "acl", "bot-api", "wire-contract", "testing", "commit"]
timestamp: 2026-08-03T00:00:00Z
# --- octospec extension fields ---
slug: space-member-bot-active
upstream: none (follow-up to space-directory-api; numbers measured on its perf harness)
source: user
---

# Task: space-member-bot-active

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add `space_member.bot_active`, denormalised from `robot.status = 1`, so every
directory slice filters on `space_member` alone and the `robot` LEFT JOIN disappears
from `GET /v1/space/{space_id}/directory` entirely.

Predicates become:

| slice | today | after |
|---|---|---|
| `human` | `sm.is_bot = 0` | unchanged |
| `bot` | `sm.is_bot = 1 AND r.robot_id IS NOT NULL` | `sm.is_bot = 1 AND sm.bot_active = 1` |
| `all` | `sm.is_bot = 0 OR r.robot_id IS NOT NULL` | `sm.is_bot = 0 OR sm.bot_active = 1` |

**This is the risky half of the denormalisation, and it is deliberately a separate
task from the `is_bot` column that shipped with `space-directory-api`.** `is_bot`
copies `user.robot`, which is written once at account creation and never UPDATEd
anywhere in the repo, so it cannot go stale and needed no hook. `robot.status` is
mutable and is written from **another module**, so this column can drift. Everything
below is about bounding that drift.

## Background

Measured on the `space-directory-api` perf harness (`OCTO_PERF=1`, 10 Spaces x 6000
members = 1700 humans + 4300 bots, MySQL 8.0.46), comparing the current query against
the same query with the `robot` join removed and the predicate on `space_member`:

| case | now | with `bot_active` | delta |
|---|---|---|---|
| `all`, `limit=6000` (the contacts one-shot) | 61.6 ms | **39.1 ms** | −22 ms (−36%) |
| `bot`, `limit=50` | — | 27.4 ms | LIMIT becomes pushable |

Two things worth stating plainly, because an earlier reading of the numbers got this
wrong:

1. **The win is not mainly LIMIT pushdown.** The contacts page fetches whole Spaces
   in one request, and a full fetch has to touch every row no matter what. The 22 ms
   comes from deleting 6001 nested-loop single-row probes into `robot`. Pushdown is a
   bonus for paged consumers, of which there are none today.
2. **An index alone does nothing here.** That was already tried and reverted on the
   `space-directory-api` branch: the sort was only ~2 ms of the total. Only removing
   joins moves the number.

For context on where the remaining time goes: end-to-end the endpoint is ~115 ms for
a 707 KB body, of which the query is ~55 ms. This task addresses ~22 ms of that. The
other ~60 ms is serialisation and transfer, which only caching the serialised bytes
can touch — a separate decision.

Write sites for `robot.status` (the values this column must follow):
`modules/robot/db.go` (7 `Update("robot")` sites), `modules/botfather/db.go:123`.
Read side is `modules/space/db_directory.go`.

## Load-bearing list

- **space**, **isolation**, **acl** — `bot_active` participates in what a directory
  row *is*. A stale `1` surfaces a disabled bot in the AI tab; a stale `0` hides a
  live one. Neither crosses a Space boundary — the column is per `(space_id, uid)` and
  every query still filters `sm.space_id` — so drift is a correctness bug, not an
  isolation bug. Say so explicitly in review rather than letting "denormalised ACL-ish
  column" read as a security concern. (rules: space-isolation)
- **bot-api** — the sync path is driven from `modules/robot` and `modules/botfather`
  write paths. Those modules must not gain a compile-time dependency on
  `modules/space`; use the existing `modules/base/event` hook-variable pattern
  (`event.SpaceMemberCacheInvalidator` is the precedent — set by the consumer at
  init, called by the producer, nil-checked at the call site). (rules: space-isolation)
- **wire-contract** — `GET /v1/space/{space_id}/directory` must return byte-identical
  responses before and after. This is an internal representation change only; `kind`,
  `total` and ordering are unchanged. The deprecated `listMembers` / `spaceBots` must
  also be unaffected: like `is_bot`, the new column lands in `queryMembers`' and
  `searchMembers`' `SELECT sm.*`, so the name must not collide with any alias they
  add. `bot_active` collides with nothing today — assert it.
- **testing** — drift is the whole risk, so the tests are the deliverable as much as
  the column is. (rules: testing)
- **commit** — English Conventional Commits, `perf(space):` / `feat(space):`.

## Out of scope

- **Changing `is_bot`.** It ships in `space-directory-api` and is correct as is. In
  particular, do NOT collapse the two into one tri-state `member_kind` column: the
  value of keeping them apart is that one is provably immutable and one is not, and a
  merged column loses that distinction exactly where a reader needs it.
- **`GET /v1/space/:space_id/members` and `GET /v1/robot/space_bots`.** Both are
  frozen and marked `Deprecated:`. They keep their own `robot` joins and their own
  (defective) semantics.
- **Caching.** Independent decision, and it targets the serialisation half of the
  latency rather than the query half.
- **A generic "denormalise robot state onto members" framework.** One column, one
  hook, one reconciler.
- **Backfilling anything other than `space_member`.**

## Acceptance

- Migration adds `bot_active smallint NOT NULL DEFAULT 0`, backfills from
  `robot.status = 1` joined on `space_member.uid`, and replaces
  `spacemember_directory` with `(space_id, status, is_bot, bot_active, role DESC,
  created_at, uid)`.
- `directoryFromJoin` no longer joins `robot` for **any** slice; `grep robot
  modules/space/db_directory.go` returns only comments and the `is_bot`/`bot_active`
  column references.
- **Sync on write.** A hook in `modules/base/event` (named for what it does, e.g.
  `BotActiveStateChanged func(robotID string, active bool)`), registered by the space
  module at init, called from every `robot.status` write site listed in Background.
  A test asserts that toggling a bot's status through the robot module's own DB helper
  flips `space_member.bot_active` in every Space that bot belongs to.
- **Reconciler bounds drift.** A periodic job re-syncs `bot_active` from `robot`,
  in the shape of the existing space-welcome reconciler
  (`modules/notify/space_welcome*`). Rationale: the hook is fire-and-forget and a
  missed call would otherwise be permanent. The job must log a non-zero repair count
  at WARN — silent self-healing hides a broken hook, which is how this class of
  denormalisation rots. A test seeds a deliberate divergence and asserts the
  reconciler both repairs it and reports it.
- **Drift is detectable, not just repaired.** A test asserts the invariant
  `space_member.bot_active = (robot.status = 1)` holds across a fixture exercising
  enable, disable, bot-added-to-second-Space, and bot-removed-from-Space.
- `go test ./modules/space/... ./modules/robot/... ./modules/botfather/...` passes;
  `golangci-lint run ./...` clean; `make i18n-extract-check` and `make i18n-lint` pass.
- Perf harness re-run on 10x6000 confirms `all` at `limit=6000` lands at ~39 ms
  (from 61.6 ms). If it does not, the column is not earning its maintenance cost and
  the task should be reverted rather than kept "because it is already written".
- `TestDirectory_*` and the frozen-endpoint regression test
  (`TestDirectory_LegacyMembersUnaffectedByIsBot`) pass unchanged, and an equivalent
  assertion covers `bot_active` not disturbing `SELECT sm.*` consumers.
