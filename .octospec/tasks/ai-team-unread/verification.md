# Verification: ai-team-unread

The PR branch is based directly on `main` at `1d75e21e` in the separate
`octo-server-ai-team-unread` worktree. Four inherited AI Team integration
commits were excluded before final validation; the feature needs no aggregate
AI Team group implementation or schema.

## Final behavior

- Existing IM fetch/parameters, normal response fields, filters, cursors and
  clear-unread logic are unchanged. Only optional `ai_team_conversations`
  response assembly is added.
- AI metadata APIs retain the base schema and service implementation. There is
  no server unread cache, new IM request or mutation hook.
- One indexed metadata query maps authorized candidate sessions in the existing
  IM response. A batch with no AI candidates performs no additional query.
- The frontend must consume the new field, maintain absolute channel counts
  using full/delta mode, and aggregate by bot ID. No badge UI was implemented.

## Final checks on the PR base

All passed after isolating the change onto main:

- `go test ./modules/ai_team -run TestAITeamConversationUnread -count=1`
- `go test ./modules/ai_team -count=1`
- `go test ./modules/message -run 'AITeam|Unread|ConversationSyncCursor|SpaceID|RecentFilter|ConvSync|NoLegacyResponseError' -count=1`
- `go build ./...`
- `go vet ./modules/message/... ./modules/ai_team/...`
- `make i18n-extract-check i18n-lint`
- gofmt, `git diff --check`, and Swagger YAML parsing.

HTTP tests compare every original response field and IM request parameter with
AI data enabled/disabled. They cover archived/muted sessions, owner/Space and
parent membership checks, wrong-type/wrong-parent/unregistered channels,
deleted/non-ready sessions, full/delta/empty results, explicit zero and optional
mapping failure. A nil-DB unit test verifies the no-AI fast path.

Real WuKongIM tests show a bot reply as unread 1, the existing HTTP clearUnread
operation as zero on the next sync, and a deleted session absent on next sync.

Module tests used isolated MySQL 8.0, Redis 7 and WuKongIM v2.2.4 services.
Temporary `-modfile=/tmp/octo-ai-team-unread-env/test.mod` and
`-overlay=/tmp/octo-ai-team-unread-env/overlay.json` redirect existing hardcoded
test addresses. The overlays were regenerated from the final main-based source;
repository dependencies and production configuration were not modified.
The isolated database was reset between package runs.

IM query cost is unchanged, but metadata lookup/mapping add work proportional
to the AI candidates in the current IM response. No production load benchmark
was performed. The new result follows the existing IM sync scope/limits.

The original worktree's edits and inherited integration changes are excluded.
No deployment or merge is included in this task.
