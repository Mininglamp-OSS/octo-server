# Verification: my-ai-team-sessions

Date: 2026-09-07

## Automated checks

| Check | Command | Result |
| --- | --- | --- |
| Build | `go build ./...` | PASS |
| Unit suite | `ci/run-unit-tests.sh` | PASS — 52 unit packages |
| E2E shard 1 | `MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 1 4` | PASS |
| E2E shard 2 | `OCTO_MASTER_KEY=<32-byte-test-key> MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 2 4` | PASS |
| E2E shard 3 | `OCTO_MASTER_KEY=<32-byte-test-key> MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 3 4` | PASS after merging upstream `main` at `96b3b926` |
| E2E shard 4 | `OCTO_MASTER_KEY=<32-byte-test-key> MYSQL_CID=octo-ai-team-mysql REDIS_CID=octo-ai-team-redis ci/run-e2e-shard.sh 4 4` | PASS |
| Focused AI/message regression | `go test ./modules/ai_team ./modules/message` | PASS |
| Upstream admission compatibility | `go test -count=1 ./modules/group` and `go test -count=1 ./modules/space` on fresh databases | PASS — AI container creation and preset-group protection coexist with #846's single group admission funnel |
| Fresh DB AI API controls | `go test -count=1 ./modules/ai_team` | PASS — rename, pin ordering, mute, per-user clear, soft delete, ownership, scan-join guard, and migration preservation of operator settings |
| Production-shape collation regression | `go test -count=1 ./modules/ai_team -run TestAITeamQueriesSurviveProductionCollationShape` | PASS — real MySQL with 0900 legacy identity tables joined to general-ci AI/thread tables |
| Static analysis | `go vet ./...` | PASS |
| i18n extraction | `make i18n-extract` | PASS |
| i18n extraction consistency | `make i18n-extract-check` | PASS |
| i18n lint | `make i18n-lint` | PASS |
| WuKongIM persistence | `go test -tags pilote2e ./pilote2e -run '^TestSummaryCard_DispatchesAndPersistsInWuKongIM$'` | PASS — type-17 message read back from WuKongIM |

## Final review-blocker verification

After the last review round, the full gate was rerun against the updated worktree:

- `go build ./...` and `go vet ./...`: PASS.
- `ci/run-unit-tests.sh`: PASS — 52 unit packages.
- `MYSQL_CID=f34198f7a4f4 REDIS_CID=847a5f034ed2 ci/run-e2e-shard.sh <1..4> 4`: PASS — all four API/E2E shards, including `group`, `ai_team`, `thread`, and `space`.
- `go test -count=1 ./modules/ai_team` on a freshly recreated database: PASS.
- `go test -count=1 -tags pilote2e ./pilote2e -run '^TestSummaryCard_DispatchesAndPersistsInWuKongIM$'`: PASS.
- `make i18n-extract-check`, `make i18n-lint`, and `git diff --check`: PASS.

The review fixes keep collation conversion on the new AI-table operands so indexed
legacy identity/Space columns remain sargable; a production-shape fixture now runs
the shipped service queries and asserts the hot routing plan contains no full scan.
Org-sync mutations skip AI containers. Re-adding an owner after Space cleanup
restores exactly owner + Bot through the common admission funnel and recreates the
parent plus all retained thread subscribers in WuKongIM. Provision failures no
longer downgrade already-ready sessions, and rename follows the agent -> session ->
thread lock order used by creation.

Bot deletion now uses one lifecycle-cleanup service from all three deletion
surfaces. It includes hidden AI containers and opts into protected removal only
for those containers. Focused `modules/group` and `modules/botfather` tests verify
that product-facing group lists still hide the parent while the REST deletion
cascade sees and removes it; `go build ./...` and `go vet ./...` pass after the
interface change.

The final follow-up also verifies that org-employee exit events do not mutate AI
containers, category reads/writes exclude or reject them, and the archive worker
structurally exempts their threads. The AI migration no longer overwrites the
operator-owned global auto-archive setting; rollout still requires the documented
deployment check that its effective value is disabled.

## CI follow-up on 2026-09-08

GitHub E2E shard 2 exposed an ordering bug in the pre-existing asynchronous Space
removal regression test: it treated the cleanup job's claim-time `attempts` increment
as proof that every callback had finished, then read a callback-owned plain integer
from another goroutine. The test now uses an atomic counter and waits for both the
callback and the persisted release error that is written after all cleanup steps run.

- `go test -race -count=1 ./modules/project -run '^TestSpaceRemovalStillRemovesFromGroupsWhenProjectStepFails$'`: PASS.
- `go test -race -shuffle=on -count=1 ./modules/project`: PASS (72.847s).

## Final review-blocker follow-up on 2026-09-08

The two blocking findings at reviewed head `b35c9f23` are closed: the
super-admin delete route now proves `robot_id` resolves to a robot before any
membership cleanup, and lifecycle cleanup treats the group-creator no-op as a
warn-and-skip outcome. Ownership transfer now rejects Bot targets so the state
cannot be newly created through the product API.

The carried visibility gaps are also closed: owner mention-preference routes,
Bot resolve-target search, and manager group listings exclude protected AI
containers (and resolve-target search also excludes their threads).

The subsequent route-family sweep is closed as well. All four user/Bot,
group/thread incoming-webhook management mounts reject protected containers
after membership authorization. The deprecated `/v1/coversations` response
filters both the parent and thread conversations, and operational analytics
excludes protected containers from source dimensions, aggregate group counts,
channel listings, and direct channel-member lookup.

Checks run against freshly recreated local `test` databases where applicable:

- Focused `modules/group` lifecycle/transfer/manager-list regressions: PASS.
- Focused `modules/robot` manager-delete and mention-preference regressions: PASS.
- Focused `modules/bot_api` resolve-target regression: PASS.
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/group`: PASS (31.352s).
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/robot`: PASS (7.030s).
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/bot_api`: PASS (51.036s).
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/incomingwebhook`: PASS (19.149s).
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/message`: PASS (3.262s).
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/opanalytics`: PASS (8.971s).
- `make i18n-extract`, `make i18n-extract-check`, `make i18n-lint`,
  `go build ./...`, `go vet ./...`, and `git diff --check`: PASS.

The complete unit and four MySQL/Redis/WuKongIM API/E2E shards had passed at
the preceding pushed head. They are intentionally delegated to the new GitHub
CI run after this blocker-fix commit; `check-sprint` is project-board metadata
and is explicitly outside this repair scope.

Focused commands run on 2026-09-08 against freshly recreated test databases:

- `go test -count=1 ./modules/group`: PASS.
- `go test -count=1 ./modules/category`: PASS.
- `go test -count=1 ./modules/thread`: PASS.
- `OCTO_MASTER_KEY=<32-byte-test-key> go test -count=1 ./modules/botfather`: PASS.
- `go test -count=1 ./modules/robot`: PASS.
- `OCTO_MASTER_KEY=<32-byte-test-key> DM_AI_TEAM_ON=true DM_THREAD_ON=true go test -count=1 ./modules/ai_team`: PASS.
- `go build ./...`, focused `go vet`, `make i18n-extract-check`, `make i18n-lint`, and `git diff --check`: PASS.

The E2E/API coverage includes add/remove/re-add, replay and idempotency conflicts,
concurrent single-parent creation, the exact two-member invariant, missing Space and
foreign-Bot rejection, ordinary mutation protection, and hiding both AI parent groups
and their thread sessions from recent/follow lists. The final review round also covers
Space-removal lifecycle cleanup, preset-group and QR/scan-join bypasses, Bot API
mutation rejection, inactive-seat routing rejection, and the personal session controls
shown by the client: rename, pin ordering, mute, per-user history clear, and soft delete.

After merging upstream `main` at `96b3b926` (`#846`), the new source guard correctly
identified the AI container's former direct `group_member` insert. Container creation
now enters the group admission funnel through a narrow same-transaction bridge that
verifies the parent Space, owner, purpose and empty project binding before admitting
exactly the owner and Bot. All four E2E/API shards above were rerun after that merge.

## Fresh-database migration verification

The test database was dropped and recreated before running the module migration:

```bash
docker exec -e MYSQL_PWD=demo octo-ai-team-mysql mysql -uroot \
  -e "DROP DATABASE IF EXISTS test; CREATE DATABASE test CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"
go test -count=1 ./modules/ai_team \
  -run '^TestAITeamMigrationPreservesGlobalThreadAutoArchive$'
```

Result:

```text
ok  github.com/Mininglamp-OSS/octo-server/modules/ai_team
```

The fixture seeds `thread.auto_archive_enabled=1`; both migration up and down leave
that value unchanged. AI session threads are protected independently by the
`ArchiveStaleBatch` purpose predicate, while deployment policy remains responsible
for keeping the global effective setting false during rollout.

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
