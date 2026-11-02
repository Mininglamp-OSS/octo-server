#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root. MCP_POLICY_TEST_MYSQL_DSN optionally enables
# the isolated MySQL suite; it must refer to a disposable MySQL 8 server.
go test -race -count=1 ./pkg/mcppolicy ./pkg/errcode ./pkg/httperr ./pkg/i18n/...
go test -race -count=1 ./modules/robot ./modules/common -run 'TestMCP|TestRobotNoLegacyResponseError'

# user.TestMain unconditionally starts a full DB/Redis rollout, including for
# pure mock tests. Compile all production files with the focused mock and
# envelope tests so this check stays service-free. Full integration tests keep
# the existing TestMain and run in the repository's E2E lane.
(
  cd modules/user
  sources=()
  while IFS= read -r source; do
    sources+=("$source")
  done < <(go list -f '{{range .GoFiles}}{{println .}}{{end}}' .)
  go test -race -count=1 -run 'TestMCP|TestUserAPINoLegacyResponseError' \
    "${sources[@]}" api_mcp_policy_test.go api_i18n_test.go
)
