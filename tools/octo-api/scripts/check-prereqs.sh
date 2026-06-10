#!/usr/bin/env bash
#
# check-prereqs.sh — environment doctor for the openapi toolchain.
#
# Verifies repo prerequisites BEFORE swag generation / validation runs, so
# a missing setup step fails fast with a pointer to the adoption guide
# instead of surfacing as a cryptic swag/spectral error downstream.
#
# Wired into `make openapi-gen` automatically (every gen/verify/check run
# passes through it); also exposed standalone as `make openapi-doctor`
# which additionally requires the committed baseline.
#
# Checks:
#   1. main.go present                  (must run from the API repo root)
#   2. swag global annotations present  (@title + @BasePath in main.go)
#   3. ≥1 handler carries @Router       (otherwise gen emits an empty spec)
#   4. go toolchain available           (swag/oasdiff are go-installed)
#   5. npx available                    (warn only — needed by openapi-lint)
#   6. baseline committed to git        (only with --require-baseline)
#
# Usage:
#   check-prereqs.sh [out_dir] [--require-baseline]
#
#   out_dir             spec output dir (default docs/openapi)
#   --require-baseline  also require <out_dir>/swagger.yaml tracked in git

set -euo pipefail

OUT_DIR="docs/openapi"
REQUIRE_BASELINE=0
for arg in "$@"; do
  case "$arg" in
    --require-baseline) REQUIRE_BASELINE=1 ;;
    *) OUT_DIR="$arg" ;;
  esac
done

ERRORS=0
ok()   { echo "✅ $1"; }
warn() { echo "⚠️  $1"; }
fail() {
  echo "❌ $1"
  echo "   ↳ $2"
  ERRORS=$((ERRORS + 1))
}

# 1. API repo shape
if [ -f main.go ]; then
  ok "main.go present"
else
  fail "main.go not found in current directory" \
       "run from the API repo root — this toolchain targets Go API servers (references/adoption.md「接入判断」)"
fi

# 2. swag global annotations
if [ -f main.go ]; then
  if grep -qE '^//[[:space:]]*@title' main.go && grep -qE '^//[[:space:]]*@BasePath' main.go; then
    ok "swag global annotations present (@title / @BasePath)"
  else
    fail "main.go lacks swag global annotations (@title / @BasePath)" \
         "add the global block above func main() — template: references/api-spec.md §E「全局 main.go 必带」(adoption.md step 2)"
  fi
fi

# 3. at least one annotated handler — swag would otherwise emit a spec
#    with empty paths, and every downstream gate runs against nothing.
ANNOTATION_ROOT="modules"
[ -d "$ANNOTATION_ROOT" ] || ANNOTATION_ROOT="."
if grep -rqE --include='*.go' --exclude-dir=vendor '^//[[:space:]]*@Router' "$ANNOTATION_ROOT" 2>/dev/null; then
  ok "at least one handler carries @Router"
else
  fail "no @Router annotation found under $ANNOTATION_ROOT/" \
       "annotate at least one handler — workflow: SKILL.md §1, template: references/api-spec.md §E (adoption.md step 3)"
fi

# 4. go toolchain (swag / oasdiff are installed via 'go install')
if command -v go >/dev/null 2>&1; then
  ok "go toolchain available"
else
  fail "go not found in PATH" \
       "install Go — swag/oasdiff CLIs are auto-installed via 'go install'"
fi

# 5. npx (spectral lint) — warn only: gen works without it
if command -v npx >/dev/null 2>&1; then
  ok "npx available (openapi-lint ready)"
else
  warn "npx not found — 'make openapi-lint' will fail until Node.js is installed"
fi

# 6. committed baseline (verify/diff need a git-tracked spec to compare)
if [ "$REQUIRE_BASELINE" = 1 ]; then
  if git ls-files --error-unmatch "$OUT_DIR/swagger.yaml" >/dev/null 2>&1; then
    ok "baseline committed ($OUT_DIR/swagger.yaml tracked in git)"
  else
    fail "baseline not committed ($OUT_DIR/swagger.yaml not tracked in git)" \
         "run 'make openapi-gen' then 'git add $OUT_DIR/ && git commit' (adoption.md steps 4-5)"
  fi
fi

echo
if [ "$ERRORS" -gt 0 ]; then
  echo "❌ $ERRORS prerequisite(s) missing — fix the items above, full guide: references/adoption.md"
  exit 1
fi
echo "✅ environment ready"
