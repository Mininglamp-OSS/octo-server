# Implementation report

## Review fixes

- Loaded the botfather migrations in the external robot test package so the
  existing `TestOwnedBots` schema contains `agent_hosting` and
  `agent_reported_hosting_at` without introducing an import cycle.
- Kept hosting explicitly self-reported and non-authoritative, and paired it
  with `agent_reported_hosting_at` on both new response surfaces.
- Treated cooling-off and terminally destroyed Bot/owner accounts as inactive
  even when their legacy `status` and membership rows remain active.
- Kept account activity and Space membership as separate facts while requiring
  both identities to be active members before returning owner Project roles.
- Turned a context row disappearing after credential verification into a
  fail-closed `context_error` path that preserves present-but-empty Bot and
  owner contexts, including `projects: []`.
- Narrowed cross-principal Project answers exposed to a Bot credential to
  `member` and `role`; owner capabilities and membership epochs remain private
  to owner-authenticated APIs. Renamed the nested scope echo to
  `requested_space_id`.
- Excluded destroying and destroyed Bot user rows from `owned_bots`, with both
  fixture-level and real-MySQL coverage.
- Strengthened the existing owned-Bot integration test, pinned every SQL
  predicate that gates owner Project facts, and added an isolated MySQL matrix
  for disabled/destroyed principals, revoked memberships, and inactive scopes.

## Verification

Passed locally:

```text
go test ./modules/user -run '^TestBotOwnerContext' -count=1
OCTO_ASSISTANT_TEST_MYSQL_DSN='root:demo@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=true' go test ./modules/robot -run '^TestOwnedBotsMetadataMySQLContract$' -count=1
OCTO_ASSISTANT_TEST_MYSQL_DSN='root:demo@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=true' go test ./modules/user -run '^TestBotOwnerContextMySQLLivenessContract$' -count=1
go test ./modules/robot -run '^TestOwnedBots$' -count=1 # fresh isolated MySQL/Redis/WuKongIM containers
go test ./modules/robot -run '^TestRobotNoLegacyResponseError$' -count=1
go test ./modules/user -run '^Test(UserAPINoLegacyResponseError|ManagerAPINoLegacyResponseError|MigratedUserFilesNoLegacyResponseError)$' -count=1
go test ./modules/group -run '^TestGroupNoLegacyResponseError$' -count=1
go test ./pkg/project -count=1
go vet ./modules/robot ./modules/user ./modules/group
go test -c ./modules/robot
go test -c ./modules/user
go test -c ./modules/group
git diff --check
```
