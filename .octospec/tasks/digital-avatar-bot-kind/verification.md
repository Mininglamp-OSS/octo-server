# Verification: digital-avatar-bot-kind

Date: 2026-09-09

- Worktree: `/Users/kense/Projects/octo/octo-server-digital-avatar-bot-kind`
- Branch: `feat/digital-avatar-bot-kind`
- Current base: `0bc8edaca4201a43beaf40342bc48d08da4dcb1a` (`feat/multi-ai-teams`, PR #875).
- Latest result: **85 real-HTTP checks passed, 0 failed** after the final rebase;
  build, vet, i18n and regression-test compilation passed. Ready for the
  user-authorized commit, push and PR. Earlier sections retain historical
  verification and superseded policy context; the last section is current.

## Test isolation

Database-backed packages were run serially. Before each package, the `test`
database was dropped and recreated as `utf8mb4`/`utf8mb4_general_ci`, using the
MySQL password from macOS Keychain without printing it. Tests used:

```text
OCTO_MASTER_KEY=12345678901234567890123456789012
DM_AI_TEAM_ON=true
DM_THREAD_ON=true
```

One chained run initially hit a pre-test migration collision while starting the
Project package (`message_extra already exists`). No assertion ran or failed in
that attempt. With no residual test process present, an isolated database was
recreated and the identical Project test passed.

## Focused and integration tests

All commands below passed:

```text
go test ./pkg/botpolicy
go test ./internal/carddispatch -run '^TestDBAuthorizerPolicyMatrix$'
go test ./modules/robot -run '^(TestPlatformAvatarEnrollmentCoversExistingAndConcurrentNewSpace|TestAvatarManagerAuthorizationAndTokenLifecycle|TestAvatarCannotUseLegacyRobotManagementRoutes|TestAvatarLifecycleFailureRetriesWithoutRestoringRelationships)$'
go test ./modules/bot_api -run '^TestAvatar'
go test ./modules/group -run '^TestDigitalAvatarOrdinaryGroupAndThreadAdmission$'
go test ./modules/project -run '^TestDigitalAvatarRequiresProjectAdminAndProjectSeat$'
go test ./modules/ai_team -run '^(TestAITeamAgentLifecycleAndSessionIdempotency|TestDigitalAvatarMembershipIsIndependentPerUser)$'
go test ./modules/space -run '^(TestSpaceDirectoryReturnsHumanOwnersAndCloudAgents|TestSpaceDirectoryKeywordFiltersHumanAndVisibleBotNames)$'
go test ./modules/usersecret -run '^TestStore_QueryBotByToken_Integration$'
go test ./modules/user -run '^(TestUploadAvatar|TestUploadAvatarPostCommitNotificationFailuresRespondOK|TestFriendApply_SpaceIDNotMember_FallsBackAndLogs|TestFriendSure_SpaceIDNotMember_FallsBackAndLogs|TestAddBotFatherFriend_Bidirectional)$'
go test ./modules/botfather -run '^TestAvatarIsExcludedFromUserBotPersistencePaths$'
go test ./modules/bot_provision -run '^TestBotToken_AvatarUsesManagerCredentialPath_404$'
go test ./modules/message -run '^TestConversationAndSidebarShareCanonicalBotTypes$'
go test ./modules/app_bot -run '^(TestRegistryAddRemove|TestRegistryAtomicTokenRotation)$'
go test ./modules/botidentity -run '^(TestResolverResolve|TestResolverActive|TestResolverAgainstAuthoritativeBotTables)$'
```

## Repository gates

All commands below passed after the final code changes:

```text
make i18n-extract-check
make i18n-lint
go build ./...
go vet ./...
git diff --check
```

`make i18n-extract` was also run after registering the new error code; the
generated English catalog and zh-CN translation are included in the worktree.

## Acceptance trace

1. Shared-Space DM authorization, stale-friend rejection, independent per-user
   AI Team membership/session routing, removal isolation and historical reuse
   are covered by the Bot API and AI Team integration tests.
2. Ordinary-group admission, cross-Space rejection, inviter departure without
   Avatar cascade, and group send/sync/typing/read-receipt resource gates are
   covered by group and Bot API policy tests.
3. Parent-group/thread authorization and the explicit denial of thread create,
   delete and metadata mutation are covered by Bot API tests.
4. Project admin add/remove, ordinary-member rejection, project-seat gating and
   non-cascade behavior are covered by the Project integration test.
5. Existing/new Space enrollment, publish/create concurrency and absence of
   implicit group/project enrollment are covered by the manager concurrency test.
6. Mounted forbidden routes for group management, thread management, search,
   voice, OBO, principal and target resolution are invoked by the Bot API test;
   an unknown mounted route is also proven fail-closed.
7. Canonical kind is server-owned; register lifecycle, ownerless robot,
   BotFather, provisioning and user-secret bypasses are covered by focused tests.
8. Platform/Space manager boundaries, unrelated Space admins, ordinary users,
   token reveal/rotation and immediate old-token invalidation are covered by the
   manager test; `creator_uid` remains empty.
9. Unpublish/delete revoke local authority first, close relationships, persist
   retryable cleanup, revoke runtime state and do not restore historical
   relationships on republish; failure/retry is covered by the lifecycle test.
10. App Bot registry/token, User Bot identity, AI Team and Project compatibility
    regressions passed.
11. Focused integration tests and all documented repository gates passed with
    isolated test state as listed above.

## Live isolated BUA/API acceptance

The following was additionally exercised against a running isolated stack at
`127.0.0.1:8096` with a dedicated MySQL database and Redis/WuKongIM containers.
Only disposable data and the three dedicated acceptance users were used; bot
tokens, user tokens, and database credentials were never printed.

- A published Space Avatar completed `register`, ordinary-group
  `sendMessage`, thread `sendMessage`, `typing`, `messages/sync`, and
  `readReceipt`. The same Avatar could DM a member of its Space, while a DM to
  a user outside that Space was rejected (`err.server.bot_api.not_friend`).
- A second ordinary member independently added that Avatar to AI Team, created
  a session, removed it, then re-added it. The original administrator's Agent
  remained present and the member's historical session remained listed.
- A new ordinary-group thread was created through the human API; Avatar list
  and read access worked. Avatar thread creation and MD update returned
  `err.server.bot_api.avatar_unsupported`.
- A project administrator added and removed the Avatar successfully. An
  ordinary member's project-add attempt was rejected with
  `err.server.project.permission_denied`.
- All mounted forbidden Bot API families returned the registered Avatar denial:
  group create/update/member add/member remove, message search, voice, OBO
  grant, Space principal, and target resolution.
- Credential rotation immediately made the previous token fail `register` and
  allowed the replacement token. A normal member could not reveal credentials.
- A disposable platform Avatar, published before a new Space was created, had
  exactly two active Space seats (one existing, one new) and zero ordinary-group
  and project memberships. Its credential was rejected after unpublish.
- A disposable Space Avatar was added to an ordinary group, a project, and an
  AI Team. After unpublish, `register` was rejected and the active relation
  counts for Space/group/project/AI Team were all zero; it was then deleted.

### Web UI observation (not a backend failure)

The running `octo-web` AI Team page was tested using the browser UI. It logs
into the isolated Space correctly but its “添加数字员工” modal calls the legacy
`/app_bot/available` discovery surface and displays “当前 Space 暂无可用数字员工”
for an available Avatar. The Route B backend instead exposes the Avatar through
`/v1/robot/space_bots`. Thus a fully browser-only Avatar → AI Team selection
flow is currently blocked by the separate frontend's Route A discovery wiring.
`octo-web` is explicitly out of scope for this task and its existing dirty
worktree was not edited.

## Follow-up: AI Team-only group policy

The product rule changed after the earlier live exercise: Avatar may not join
ordinary or project groups; only server-owned AI Team container and aggregate
groups are valid. The earlier ordinary-group live rows were isolated acceptance
data for the superseded rule and are not evidence for this policy.

- `TestDigitalAvatarCanOnlyJoinAITeamGroups` proves ordinary group creation and
  member-add reject `ErrAvatarOrdinaryGroupDenied`, while the AI Team admission
  bridge succeeds only when its persisted `ai_team_agent` relation exists.
- `TestAvatarResourceAuthorizationCoversDMGroupThreadAndProject` proves an
  ordinary legacy group-member row grants no runtime access; an AI Team
  container does, and a project seat cannot reopen ordinary-group access.
- `TestDigitalAvatarRequiresProjectAdminAndProjectSeat` confirms project admin
  admission/removal remains supported without admitting the Avatar to a project
  group.
- `TestAITeamAgentLifecycleAndSessionIdempotency` and
  `TestDigitalAvatarMembershipIsIndependentPerUser` passed against the stricter
  persisted-relation check, confirming normal AI Team provisioning still works.
- Each database-backed package was run serially against a freshly recreated
  `test` database because the shared test harness fixes that database name.
  All three focused tests passed, as did `go build ./...`, the focused
  `go vet`, `make i18n-extract-check`, `make i18n-lint`, and `git diff --check`.

## Follow-up: route-family audit

The final policy audit added explicit negative coverage for Bot-token route
families mounted outside the main `/v1/bot` group: incoming Webhook management
(parent and thread), bulk user lookup, and Space-member enumeration. Avatars
must receive `err.server.bot_api.avatar_unsupported` for all of them; the
allowlist is therefore the only way a new Bot API capability can be exposed.

`go test ./modules/bot_api -run '^$'`, `go test ./pkg/botpolicy`,
`go build ./...`, focused `go vet`, `make i18n-extract-check`,
`make i18n-lint`, and `git diff --check` passed after this addition.

An attempt to rerun database-backed tests on 2026-09-09 was blocked before any
test assertion by the shared test harness's fixed `test` database: a concurrent
worktree had left migration `20191106000001_event_legacy01.sql` in its
`gorp_migrations` history, which this worktree does not contain. The harness
does not support a database override, and its cleanup only deletes table rows.
The existing isolated test results above remain the recorded validation; the
shared `test` database was not changed during this audit.

## 2026-09-09: multi-team rebase and API-only verification

- Rebased onto local `feat/multi-ai-teams` at `fe4f960a`; the Avatar commit is
  now `410ab8f6`. `git range-diff` confirmed the rebased Avatar commit itself
  is unchanged. The source commit hides custom AI teams from ordinary group,
  conversation and category surfaces.
- The subsequent integration audit found two obsolete table queries outside
  the AI Team module: `pkg/botpolicy/authority.go` and
  `modules/bot_api/groups.go` still queried `ai_team_group`. Both now use the
  same live owner/Agent relation predicate over `group` and `ai_team_agent`.
  Group membership, publication state, owner/Bot Space seats and tenant checks
  remain required. Project-bound groups remain forbidden to Avatars.
- Custom-team selection and internal membership admission now accept eligible
  published Avatars, using the existing Bot policy. Public ordinary-group
  admission remains forbidden. Extended existing regression tests cover
  independent users' custom teams, Agent removal and revoked owner seats.
- Reproduced before the fix using real HTTP requests: adding an Avatar to a
  custom team returned 403, while Bot reads of the automatic and custom team
  were rejected. After the fix the API run completed with **78 passed, 0
  failed**; no BUA/browser tests were performed, as requested.
- Live verification used `http://127.0.0.1:8097/api` (proxy to 8096), with
  explicit database `octo_avatar_api_20260909`. The database has **no
  `ai_team_group` table and no records for the two retired migrations**. The
  copied legacy table was renamed to `archived_ai_team_group`; the source
  database and a mode-0600 dump were preserved. No schema migration was added
  to the repository.
- HTTP coverage: OIDC session and User API Key exchange; three AI kinds;
  Agent registration; session create/list/read/rename/mute/archive/unarchive/
  delete; custom-team create/read/update/delete and member add/remove;
  Avatar Bot group reads and group/thread sends; ordinary-group rejection;
  categories, saved groups and recent conversations hiding AI groups after
  actual messages; category mutation rejection; cross-Space denial;
  immediate revocation after member/Agent removal and history reuse on re-add.
- Build and regression-test compilation passed:
  `go build -o /tmp/octo-avatar-bua.Jm62Fc/octo-server-api-current .`,
  `go test -c ./modules/ai_team`, and `go test -c ./modules/bot_api`
  (test binaries were written under the temporary verification directory).
  The shared test harness was not executed in this verification: its
  `NewTestServer` hardcodes `test` and deletes rows before migration setup.
  An earlier invocation in this session had removed the previous temporary
  `test`-database fixture; the current running server now explicitly uses the
  isolated database instead.
- Repository-wide SQL-reference search found no remaining reads/writes or
  DDL targeting the retired table. `ai_team_group` remains a valid group
  **purpose value**, not a table dependency.
- Final focused `go vet` (`pkg/botpolicy`, `modules/ai_team`, `modules/bot_api`,
  `modules/group`), `make i18n-extract-check`, `make i18n-lint`, and both
  staged/unstaged `git diff --check` passed.

## 2026-09-09: final rebase onto 0bc8edac and PR gates

- The source branch advanced again; rebased the Avatar commit from `410ab8f6`
  to `0f4c0a53` on `0bc8edac`. The only conflict was the Space directory
  regression fixture. Resolution retains the source's `octo_hosted`-only
  personal-bot display rule and the Avatar's separate `digital_employees`
  collection; no vendor-hosted visibility was reintroduced.
- Rebuilt and restarted the local backend against the same explicit isolated
  database, without the retired `ai_team_group` table. Re-ran the HTTP script:
  **85 passed, 0 failed**. All three AI kinds, AI-only group admission,
  custom-team lifecycle, group/thread messaging, ordinary-surface hiding,
  cross-Space denial, immediate removal revocation and historical reuse passed.
- Added live directory assertions: hosted clone appears under its real owner;
  local assistant does not appear there; Avatar appears independently, never
  under a human owner; keyword and `only_with_agents` filtering preserve the
  intended digital-employee behavior.
- Final commands passed: `go build ./...`, the runnable-server build,
  `go test ./pkg/botpolicy` (no database harness), `go test -c` for
  `modules/space`, `modules/ai_team` and `modules/bot_api`,
  `go vet ./...`, `make i18n-extract-check`, `make i18n-lint`, and
  `git diff --check`. Database-backed Go test binaries were compiled, not
  executed, because the existing harness destructively targets the shared
  `test` database. No BUA/browser testing was performed in this pass.
- SQL-reference audit again found no reads, writes or DDL targeting the
  retired table. The `ai_team_group` purpose string remains intentional.
- The PR targets upstream `main` and depends on source PR #875. Local
  `assets/web/js/config.js`, authentication bridge, credentials, runtime
  scripts, binaries and test data are excluded from the commit.
