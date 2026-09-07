# Verification: my-ai-team-sessions

Date: 2026-09-07

## Automated checks

| Check | Command | Result |
| --- | --- | --- |
| Build | `go build ./...` | PASS |
| Unit suite | `ci/run-unit-tests.sh` | PASS — 52 unit packages |
| E2E shard 1 | `MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 1 4` | PASS |
| E2E shard 2 | `MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 2 4` | PASS |
| E2E shard 3 | `MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 3 4` | PASS |
| E2E shard 4 | `MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 4 4` | PASS |
| Focused AI/message regression | `go test ./modules/ai_team ./modules/message` | PASS |
| Static analysis | `go vet ./...` | PASS |
| i18n extraction | `make i18n-extract` | PASS |
| i18n extraction consistency | `make i18n-extract-check` | PASS |
| i18n lint | `make i18n-lint` | PASS |
| WuKongIM persistence | `go test -tags pilote2e ./pilote2e -run '^TestSummaryCard_DispatchesAndPersistsInWuKongIM$'` | PASS — type-17 message read back from WuKongIM |

The E2E/API coverage includes add/remove/re-add, replay and idempotency conflicts,
concurrent single-parent creation, the exact two-member invariant, missing Space and
foreign-Bot rejection, ordinary mutation protection, and hiding both AI parent groups
and their thread sessions from recent/follow lists.

## Fresh-database migration verification

The test database was dropped and recreated before running the module migration:

```bash
docker exec -e MYSQL_PWD=demo octo-ai-team-mysql mysql -uroot \
  -e "DROP DATABASE IF EXISTS test; CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
go test -count=1 ./modules/ai_team \
  -run '^TestAITeamMigrationDisablesGlobalThreadAutoArchive$'
docker exec -e MYSQL_PWD=demo octo-ai-team-mysql mysql -uroot -Nse \
  "SELECT category,key_name,value FROM test.system_setting WHERE category='thread' AND key_name='auto_archive_enabled';"
```

Result:

```text
ok  github.com/Mininglamp-OSS/octo-server/modules/ai_team
thread  auto_archive_enabled  0
```

## Known external/baseline limitation

The full `go test -tags pilote2e ./pilote2e` suite reaches the real MySQL, Redis,
and WuKongIM services, but an unrelated existing card-template fixture fails with:

```text
cardtmpl registry/template unavailable: default catalog not wired
```

The focused WuKongIM dispatch-and-persistence test passes. Client/adapter E1-E5 from
the source brief remains a cross-repository deployment handoff and is not claimed as
verified by this server checkout.

`golangci-lint` is not installed in the local environment; `go vet ./...` and the
repository's build/test/i18n checks were used instead.
