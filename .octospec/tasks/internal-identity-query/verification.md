# Verification

Local review follow-up checks passed on 2026-11-11, based on GitHub main `c97b3ab9` and PR head `0bc6e56f`, using the normal shared Go caches:

- All 80 in-package tests passed with race detection, isolated MySQL 8 and temporary Redis on port 6379. This includes existing authentication/source guards and production routing tests.
- Real MySQL query tracking confirms one SELECT without `bot_id`, two with it. A missing user/Space stops before the Bot query. An unbound lookup still succeeds when the Bot table is unavailable.
- BF Bot lookup checks ownership, active robot/profile state and the current name. User/Space membership and user/Bot ownership are independent relationships. Tests run without an app_bot table and verify that a separate Bot Space seat is not required, including an owned Bot seated in S1 when the owner requests S2. Returned facts do not grant the Bot Space membership or resource access.
- Invalid user/Space/member or supplied Bot returns the existing not-found error; omitted, empty and JSON null `bot_id` all return `bot: null`. Byte-exact IDs, mixed collations, canceled requests, database failures and the overall three-second pool/query deadline are covered.
- Production identity-route tests verify the unchanged internalToken authentication and enablement checks; no separate identity Secret or middleware is introduced.
- `TestIdentityProductionRouteMySQL` exercises the default reader wired by `New` through the production `Route`, real MySQL and Redis, and HTTP serialization. Its 12 cases cover omitted/empty/null Bot input, owned Bot and independent Space seat semantics, unavailable identities, wrong owner, exact IDs, invalid field type and Secret, and no-store/error envelopes.
- Production-route Redis tests verify that identity requests consume the existing shared strict-IP bucket before Secret authentication, with the same scope/configuration as membership/project routes and no-store on route-level rejection.
- `go build ./...`, `go vet ./modules/internal_membership ./pkg/errcode`, `make i18n-extract-check i18n-lint`, and `git diff --check` passed.

During feature development, regressions detected the intermediate app_bot/Bot-membership dependency and identity-route limiter bypass; the final implementation removes both.

Identity tests live in api_test.go and need no build tag. The database case reads OCTO_IDENTITY_TEST_MYSQL_DSN and skips when absent. The unfiltered external-package HTTP suite was not run against its hardcoded shared MySQL at 127.0.0.1:3306/test; no unrelated database was altered. This is not a full-repository or fresh cross-service test claim.

Only Octo Server changes are included in this branch. This review follow-up clarifies the existing contract and adds regression coverage; production query and decoding behavior are unchanged. No merge or deployment is part of this follow-up.
