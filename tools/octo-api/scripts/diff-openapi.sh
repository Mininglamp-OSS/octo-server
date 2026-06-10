#!/usr/bin/env bash
#
# diff-openapi.sh — Compare current OpenAPI spec against a base git ref
# using oasdiff (semantic, breaking-change-aware).
#
# Usage:
#   bash tools/octo-api/scripts/diff-openapi.sh [base_ref]
#
# Examples:
#   bash tools/octo-api/scripts/diff-openapi.sh                 # compare HEAD against origin/main
#   bash tools/octo-api/scripts/diff-openapi.sh origin/release  # compare against release branch
#   bash tools/octo-api/scripts/diff-openapi.sh v0.5.0          # compare against a tag
#
# Exit codes:
#   0 — no spec change, OR changes are all info/warning (non-breaking)
#   1 — at least one 'error' severity change (breaking), OR base ref missing

set -euo pipefail

BASE_REF="${1:-origin/main}"
SPEC_FILE="docs/openapi/swagger.yaml"

if [ ! -f "$SPEC_FILE" ]; then
  echo "❌ Current spec not found: $SPEC_FILE"
  echo "   Run 'make openapi-gen' first."
  exit 1
fi

# Resolve oasdiff binary (PATH or $GOPATH/bin)
OASDIFF="$(command -v oasdiff 2>/dev/null || echo "$(go env GOPATH)/bin/oasdiff")"
if [ ! -x "$OASDIFF" ]; then
  echo "❌ oasdiff not found (expected at $OASDIFF)."
  echo "   Run 'make oasdiff-install' or 'go install github.com/oasdiff/oasdiff@latest'."
  exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Fetch base spec from git
if ! git show "$BASE_REF:$SPEC_FILE" > "$TMP/base.yaml" 2>/dev/null; then
  echo "⚠️  Base spec not found in $BASE_REF ($SPEC_FILE)."
  echo "    First-time setup? After this PR merges, future PRs can diff against $BASE_REF."
  exit 1
fi

echo "═══════════════════════════════════════════════════════"
echo "  oasdiff breaking: $BASE_REF → HEAD"
echo "═══════════════════════════════════════════════════════"

# --fail-on ERR sets exit=1 on any error-severity change.
EXIT=0
"$OASDIFF" breaking "$TMP/base.yaml" "$SPEC_FILE" --fail-on ERR || EXIT=$?

echo
if [ "$EXIT" = 0 ]; then
  echo "✅ No breaking changes detected."
elif [ "$EXIT" = 1 ]; then
  echo "🔴 Breaking change(s) detected (see 'error' lines above)."
  echo
  echo "Options:"
  echo "  - Roll back the breaking change"
  echo "  - Use the deprecate flow (see references/api-spec.md §H)"
  echo "  - Bump API version (e.g. /v2) and keep /v1 alongside"
  echo "  - Coordinate with API clients (octo-cli, etc.) and merge knowingly"
fi

exit "$EXIT"
