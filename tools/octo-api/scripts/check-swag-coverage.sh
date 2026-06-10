#!/usr/bin/env bash
#
# check-swag-coverage.sh - Detect handlers missing @Router swag annotation
#
# This script identifies gin/wkhttp handler functions (signature has
# `c *wkhttp.Context` or `c *gin.Context`) and verifies each has a `@Router`
# swag annotation in its godoc block. Handlers without annotations are
# reported and the script exits with code 1.
#
# Used as the first gate in .github/workflows/openapi.yml (PR CI). Spectral
# can only inspect generated OpenAPI yaml, which silently drops endpoints
# whose handlers lack annotations — leaving coverage as a blind spot that
# this script fills before swag generation runs.
#
# Usage:
#   bash scripts/check-swag-coverage.sh [root_dir...]
#
# Defaults to ./modules if no root provided.

set -euo pipefail

if [ "$#" -eq 0 ]; then
  ROOTS=("modules")
else
  ROOTS=("$@")
fi

TMP_ALL=$(mktemp)
TMP_ANNOTATED=$(mktemp)
trap 'rm -f "$TMP_ALL" "$TMP_ANNOTATED"' EXIT

# Extract all handler function names whose signature has `c *wkhttp.Context`
# or `c *gin.Context`. Match form:
#   func (ba *BotAPI) sendMessage(c *wkhttp.Context) { ... }
# captures `sendMessage`.
{ grep -rhE '^func \([a-z]+ \*?[a-zA-Z_][a-zA-Z0-9_]*\) [a-zA-Z_][a-zA-Z0-9_]*\(c \*(wkhttp|gin)\.Context\)' "${ROOTS[@]}" 2>/dev/null || true; } \
  | sed -E 's/^func \([^)]+\) ([a-zA-Z_][a-zA-Z0-9_]*)\(.*/\1/' \
  | sort -u > "$TMP_ALL"

# Extract handler function names that are preceded by @Router annotation.
# awk tracks whether the most recent comment block included @Router, then
# captures the next non-comment `func (...) Name(` line. Uses POSIX awk
# (sub/match without 3rd arg) for portability across gawk/mawk/BSD awk.
find "${ROOTS[@]}" -name '*.go' -type f 2>/dev/null \
  | xargs awk '
      /^\/\/ @Router/ { has_router=1; next }
      /^\/\// { next }                              # other godoc lines
      /^func \([a-z]+ \*?[a-zA-Z_][a-zA-Z0-9_]*\) [a-zA-Z_][a-zA-Z0-9_]*\(/ {
        if (has_router) {
          s = $0
          sub(/^func \([^)]+\) /, "", s)
          sub(/\(.*$/, "", s)
          print s
        }
        has_router=0
        next
      }
      /^[[:space:]]*$/ { next }                     # blank lines
      { has_router=0 }                              # any other code line resets
    ' \
  | sort -u > "$TMP_ANNOTATED"

TOTAL_HANDLERS=$(wc -l < "$TMP_ALL" | tr -d ' ')
TOTAL_ANNOTATED=$(wc -l < "$TMP_ANNOTATED" | tr -d ' ')
MISSING=$(comm -23 "$TMP_ALL" "$TMP_ANNOTATED")

echo "Roots scanned:           ${ROOTS[*]}"
echo "Handlers found:          $TOTAL_HANDLERS"
echo "Annotated with @Router:  $TOTAL_ANNOTATED"

if [ -z "$MISSING" ]; then
  echo "Coverage:                100.0%"
  echo
  echo "✅ All handlers have @Router swag annotation"
  exit 0
fi

MISSING_COUNT=$(echo "$MISSING" | wc -l | tr -d ' ')
PCT=$(awk "BEGIN {printf \"%.1f\", ($TOTAL_ANNOTATED / $TOTAL_HANDLERS) * 100}")
echo "Missing annotations:     $MISSING_COUNT"
echo "Coverage:                ${PCT}%"
echo
echo "❌ Handlers missing @Router swag annotation:"
echo "$MISSING" | sed 's/^/   - /'
echo
echo "💡 100% coverage is required. Options:"
echo "   - Add @Router + swag annotations to the handlers above"
echo "   - For brownfield repos (large legacy code), do NOT add"
echo "     'Swag Annotation Coverage' to branch protection yet — see"
echo "     references/adoption.md '存量仓库接入' for the phased path"
exit 1
