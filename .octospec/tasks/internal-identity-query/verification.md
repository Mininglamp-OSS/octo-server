# Verification

Latest local checks passed on GitHub main `c97b3ab9` using the normal shared Go caches:

- All 79 in-package tests passed with race detection, isolated MySQL 8 and temporary Redis on port 6379. This includes existing authentication/source guards and production routing tests.
- Real MySQL query tracking confirms one SELECT without `bot_id`, two with it. A missing user/Space stops before the Bot query. An unbound lookup still succeeds when the Bot table is unavailable.
- BF Bot lookup checks ownership, active robot/profile state and the current name. Tests run without an app_bot table and verify that a separate Bot Space seat is not required.
- Invalid user/Space/member or supplied Bot returns the existing not-found error; missing binding alone returns `bot: null`. Byte-exact IDs, mixed collations, canceled requests, database failures and the overall three-second pool/query deadline are covered.
- Production identity-route tests verify the unchanged internalToken authentication and enablement checks; no separate identity Secret or middleware is introduced.
- Production-route Redis tests verify that identity requests consume the existing shared strict-IP bucket before Secret authentication, with the same scope/configuration as membership/project routes and no-store on route-level rejection.
- `go build ./...`, `go vet ./modules/internal_membership ./pkg/errcode`, `make i18n-extract-check i18n-lint`, and `git diff --check` passed.

During feature development, regressions detected the intermediate app_bot/Bot-membership dependency and identity-route limiter bypass; the final implementation removes both.

Identity tests live in api_test.go and need no build tag. The database case reads OCTO_IDENTITY_TEST_MYSQL_DSN and skips when absent. The unfiltered external-package HTTP suite was not run against its hardcoded shared MySQL at 127.0.0.1:3306/test; no unrelated database was altered. This is not a full-repository or fresh cross-service test claim.

Only Octo Server changes are included in this branch. No merge or deployment is requested.
