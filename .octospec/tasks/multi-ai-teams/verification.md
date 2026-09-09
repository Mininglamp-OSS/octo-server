# Verification

Verified on 2026-09-09 against the local MySQL, Redis, WuKongIM, Octo Server,
GeelyOcto Web, and OpenClaw stack.

## Server checks

- `go test ./modules/ai_team`
- `go test ./modules/group`
- `go test ./modules/message`
- `go test ./modules/bot_api`
- `go test ./modules/botfather`
- `go test ./pkg/aiteam`
- `go build ./...`
- `go vet ./...`
- `make i18n-extract-check`
- `make i18n-lint`
- `git diff --check`

The DB-backed packages were run separately after recreating the shared `test`
database with `utf8mb4_general_ci`. The BotFather package used the existing
local runtime secrets through environment injection; no secret or environment
configuration is part of the change.

The final server implementation adds no database migration: both the automatic
all-agent group and custom teams reuse the existing `group` and `group_member`
tables. `group_member` is the sole persisted roster for either group type.

## Web checks

- Five focused Vitest files passed, covering 76 tests for the AI Team page,
  service contract, conversation updates, channel settings, and restricted
  group management.
- `pnpm i18n:check`
- `pnpm lint:css` (zero errors; existing repository warnings only)
- `pnpm --filter @octo/web build`
- `git diff --check`

## End-to-end verification

- Created multiple custom teams with distinct group IDs and overlapping Agent
  choices.
- Renamed a team and removed one selected Agent without affecting the automatic
  all-agent group or the Agent's private session.
- Confirmed an unmentioned message does not trigger an Agent, while an explicit
  mention receives the expected OpenClaw reply.
- Confirmed the automatic all-agent group also routes explicit mentions.
- Confirmed custom teams enter ordinary recent conversations and contacts after
  use, while the automatic all-agent group and private containers remain hidden.
- Confirmed custom-team settings keep name, avatar, personal preferences,
  history clearing, and disbanding, while member edits, owner transfer, manager,
  Bot-admin, no-mention, and leave controls are not exposed.
- Confirmed OpenClaw created the expected runtime session and used the local LLM
  configuration supplied only at process startup.

## Environment limits

`go test ./...` is not reliable in this checkout because package binaries share
the hard-coded MySQL `test` database and race their different migration sets.
The affected packages were therefore verified independently against a freshly
recreated database. Local authentication, LLM, endpoint, and generated build
configuration were intentionally excluded from version control.
