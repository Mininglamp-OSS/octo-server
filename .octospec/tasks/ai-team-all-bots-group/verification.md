# Verification

Verified on 2026-09-09 from the integrated branch and a detached clean
verification worktree.

## PR review remediation

- Rebasing onto `origin/main` preserved both the project all-member-group
  admission entry (`a12`) and the AI-team projection entry (`a13`).
- A DB-converged `GET /v1/ai-team/agents` no longer replays WuKongIM writes.
  Failed lazy repairs are logged without turning the list into a 503; explicit
  Agent mutations remain the retry surface for external projection failures.
- Explicit Agent activation remains the repair operation for both private and
  aggregate containers. A committed Agent removal now returns a retryable error
  when its external projection is temporarily unavailable, so callers do not
  mistake an incomplete subscriber revocation for full success.
- Aggregate-group projection holds no transaction or pooled connection across
  WuKongIM HTTP. Its snapshot version comes from the existing monotonic
  `group_member.version`; a stale HTTP completion replays the latest roster.
- The automatic group itself is discovered by `purpose + space_id + creator`.
  First creation locks the owner's existing `space_member` row, so the feature
  needs no `ai_team_group` registry table or migration.
- Joined locking reads use `FOR UPDATE OF a` / `FOR UPDATE OF r`; identity,
  Space and membership tables are validated without adding them to the lock
  set. The steady-state list path performs only read queries.
- The aggregate group's group-level and per-Bot no-mention switches are
  configurable, while every other protected group mutation remains rejected.
- Category and operational-analytics group surfaces now select only ordinary
  `purpose=''` groups, hiding both managed AI purposes.
- The first-message title update has a single writer in the AI-target Robot
  path, so ordinary subarea messages do not pay for the AI-only update and
  non-text AI messages retain their display-text fallback.
- Deadlocks (`1213`) remain bounded-retry mutations; lock-wait timeouts (`1205`)
  return immediately instead of repeating the server's full timeout window.

## Passing checks

- `go test -p 1 ./modules/ai_team -count=1`
- `go test -race ./modules/ai_team -run 'TestRetryAITeamMutation|TestAITeamConcurrentAgentActivationCreatesOnePairAndOneTeamGroup|TestAITeamGroupAndAgentContainersConvergeTogether' -count=1`
- `go test ./modules/group -run '^TestNoGroupMemberWritesOutsideTheAdmissionFunnel$' -count=1`
- `go test ./modules/botfather -run '^TestAITeamProvisioner|^TestEveryUserBotCreationPathTriggersAITeamProvisioning$' -count=1`
- `go test ./pkg/aiteam -count=1`
- `go test ./modules/message -count=1`
- `go test ./modules/thread -count=1`
- `go test ./modules/robot -count=1`
- `go test ./modules/group -run 'AITeam|ManagerGroupQueries|Lifecycle' -count=1`
- every package returned by `go list ./...`, run one package at a time after
  rebuilding the disposable `test` database between packages
- `go test ./modules/bot_api -run '^$' -count=1`
- `go vet ./...`
- `make i18n-extract-check`
- `make i18n-lint`
- `git diff --check`

Additional focused checks after review remediation:

- `go build ./...`
- `go test ./modules/ai_team -count=1`
- `go test ./modules/group -run '^TestSyncAITeamGroupSubscribersAddsAndRemovesParentAndSubarea$' -count=1`
- `go test ./modules/robot -count=1`
- `go test ./modules/category -count=1`
- `go test ./modules/opanalytics -count=1`

Final round-4 review remediation checks:

- `go test ./modules/ai_team -count=1`
- `go test ./modules/group -run 'AITeam|LockOrder' -count=1`
- `go test ./modules/robot -count=1`
- `go test ./modules/thread -count=1`
- `go test ./modules/ai_team -run '^TestRetryAITeamMutation$' -count=1`
- `go build ./...`
- `go vet ./...`
- `make i18n-extract-check && make i18n-lint`

Owner lifecycle projection checks:

- aggregate snapshots and ready CAS checks use the same owner Space-liveness
  predicate as managed-group admission;
- owner removal racing a blocked aggregate projection ends with empty parent
  and subarea subscriptions and a converged `group_member` roster;
- owner removal racing a blocked private-container projection compensates the
  stale upsert by removing both participants from the parent and every session
  channel, while leaving Agent activation intent available for a later rejoin;
- `go test -race ./modules/ai_team -run '^TestAITeamOwnerRemovalCannotBeOverwrittenByStale(Team|Container)Projection$' -count=1`
- `go test ./modules/ai_team -count=1`
- `go test ./modules/group -run '^$' -count=1`
- `go build ./...`
- `go vet ./...`
- `make i18n-extract-check && make i18n-lint`
- GeelyOcto production build: `pnpm --filter @octo/web build`
- Browser E2E: opening My AI Team now clears a stale right pane; clicking New
  Session creates `新对话`; the first no-mention text renamed both the list row
  and chat header and received a real OpenClaw reply.

Blocking projection-resource remediation checks:

- `go test ./modules/ai_team -run 'TestAITeamProjectionReleasesDBBeforeIMAndReplaysStaleRoster' -count=1`
- `go test ./modules/ai_team -count=1`
- `go build ./...`
- `go vet ./...`
- `git diff --check`
- `go test ./...` remains environment-limited as documented below: package
  binaries race on the shared `test` database, and source guards include the
  intentionally retained nested worktree.

Final blocker remediation checks:

- Bot authorization compares identities as a set, independent of MySQL
  collation ordering; the production-collation fixture covers `bot_bot` and
  `bot2_bot` together.
- The post-IM check re-reads effective eligibility, while protected lifecycle
  cleanup atomically deactivates affected Agents before removing membership.
- Managed groups no longer count against the owner's daily manual group quota.
- `DELETE /v1/ai-team/agents/:bot_id` surfaces an unavailable subscriber
  projection as a retryable 503 while retaining the durable removal. A second
  DELETE retries the external revocation and remains 503 if the projection is
  still unavailable.
- `go test ./modules/ai_team -run 'TestAITeamManagedGroupsDoNotConsumeDailyGroupCreationQuota|TestAITeamProjectionRechecksEligibilityBeforeMarkingReady|TestAITeamLifecycleRemovalCannotBeOverwrittenByStaleProjection|TestAITeamRemoveSurfacesPendingProjectionAfterDurableMutation|TestAITeamQueriesSurviveProductionCollationShape' -count=1`
- `go test -c ./modules/group` and `go test -c ./modules/ai_team`
- `go test ./modules/ai_team -run '^TestAITeamRemoveSurfacesPendingProjectionAfterDurableMutation$' -count=1`
- `go test ./modules/ai_team -count=1`
- `go build ./...`
- `go vet ./...`
- `make i18n-extract-check && make i18n-lint`

The AI Team integration suite used the local MySQL, Redis, and WuKongIM test
services. It covers eager pair-group creation, exact team-group projection,
removal/reactivation, stale member repair, mixed collations, IM failure recovery,
and concurrent first activation. The concurrent projection tests also pass with
the Go race detector.

The local integrated runtime (`octo-server` on `:8090`, local MySQL/Redis and
WuKongIM) was also exercised through the temporary GeelyOcto AI Team page:

- entering AI Team leaves the conversation area empty;
- creating a session produces a `新对话` CommunityTopic only on demand;
- the first owner text message changes the persisted thread name to the message
  summary, and the dedicated page refreshes its sidebar and chat header;
- a no-mention first message received a real OpenClaw reply, with the user and
  Bot messages matching in the page, MySQL and WuKongIM sync API;
- neither the private two-member parent nor the aggregate team group is exposed
  through ordinary recent/contact/group-list surfaces.

The local WuKongIM container was still wired to the older compose-managed server,
so the final title listener was exercised by replaying that real message payload
into the new server's signed message-notify endpoint. Production deployments
route the same WuKongIM notification directly to the deployed server.

## Environment-limited checks

- A fresh `go test ./...` still is not reliable in this checkout because Go
  runs package binaries in parallel while they share the hard-coded MySQL
  database `test`; the observed failures were duplicate/unknown migrations.
  Running the touched DB-backed packages separately with a fresh disposable
  database passed.
- Source guards scan from the Git top-level and therefore also inspect the
  intentionally retained `.claude/worktrees` directory in the main checkout.
  The same guards pass in the detached clean verification worktree.
- `golangci-lint` is not installed in this environment; `go vet ./...` and the
  repository's Go-based lint targets above pass.
