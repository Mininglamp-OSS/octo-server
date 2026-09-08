# Verification

Verified on 2026-09-08 in worktree
`/Users/kense/Projects/octo/octo-server-ai-team-all-bots-group`.

## Passing checks

- `go test -p 1 ./modules/ai_team -count=1`
- `go test -race ./modules/ai_team -run 'TestRetryAITeamMutation|TestAITeamConcurrentAgentActivationCreatesOnePairAndOneTeamGroup|TestAITeamGroupAndAgentContainersConvergeTogether' -count=1`
- `go test ./modules/group -run '^TestNoGroupMemberWritesOutsideTheAdmissionFunnel$' -count=1`
- `go test ./modules/botfather -run '^TestAITeamProvisioner|^TestEveryUserBotCreationPathTriggersAITeamProvisioning$' -count=1`
- `go test ./pkg/aiteam -count=1`
- `go test ./modules/bot_api -run '^$' -count=1`
- `go vet ./...`
- `make i18n-extract-check`
- `make i18n-lint`
- `git diff --check`

The AI Team integration suite used the local MySQL, Redis, and WuKongIM test
services. It covers eager pair-group creation, exact team-group projection,
removal/reactivation, stale member repair, mixed collations, IM failure recovery,
and concurrent first activation. The concurrent projection tests also pass with
the Go race detector.

## Environment-limited checks

- `go test ./... -count=1` is not a reliable supported mode for this checkout:
  package tests run in parallel against the same hard-coded MySQL database named
  `test`. Several packages replace shared tables with test-local schemas while
  others run migrations or clean all tables. The run failed across unrelated
  packages with `unknown migration in database`, missing-column errors, and
  cross-package data deletion. The local disposable `test` database was rebuilt
  afterward and the target AI Team suite passed serially.
- `go test -p 1 ./modules/group -count=1` and the full BotFather suite encounter
  the repository's legacy migration-ledger mismatch (`event_legacy01.sql` or
  `report_legacy01.sql`). Their focused source guard and AI Team hook tests pass.
- `golangci-lint` is not installed in this environment; `go vet ./...` and the
  repository's Go-based lint targets above pass.
