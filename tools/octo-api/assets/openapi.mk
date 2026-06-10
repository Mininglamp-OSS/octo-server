# OpenAPI / swag toolchain Make targets.
#
# Included by the project root Makefile via:
#     include tools/octo-api/assets/openapi.mk
#
# All targets are self-contained — they reference scripts, configs, and
# fixtures in the skill package layout (tools/octo-api/{scripts,assets}).
# To consume in a different repo, copy tools/octo-api/ as-is and add the
# include line in the new repo's Makefile.
#
# Override-able variables:
#   SWAG_VERSION       swag CLI version pin (default v2.0.0-rc5)
#   OPENAPI_OUT_DIR    spec output directory (default docs/openapi)
#   OPENAPI_OUT_TYPES  swag output types (default yaml — single source of
#                      truth; add go for the runtime /swagger registrar,
#                      json for consumers that need it, e.g. yaml,go)
#   BASE_REF           git ref for openapi-diff (default origin/main)
#   OCTO_API_DIR       skill package root (default tools/octo-api)

SWAG_VERSION    ?= v2.0.0-rc5
OPENAPI_OUT_TYPES ?= yaml
OASDIFF_VERSION ?= v1.18.5
OPENAPI_OUT_DIR ?= docs/openapi
OCTO_API_DIR    ?= tools/octo-api

# ----------------------------------------------------------------------
# List available targets (the only command-line discovery entry).
# ----------------------------------------------------------------------
openapi-help:
	@echo "OpenAPI toolchain — available targets:"
	@echo "  make openapi-check     一键 4 道闸（coverage → verify → lint）"
	@echo "  make openapi-gen       重生 $(OPENAPI_OUT_DIR)/swagger.yaml（OPENAPI_OUT_TYPES 可加 json/go）"
	@echo "  make openapi-lint      单独跑 spectral 校验"
	@echo "  make openapi-verify    gen + drift 检测"
	@echo "  make openapi-coverage  检查 handler 是否都有 @Router"
	@echo "  make openapi-diff      跟 base ref diff，oasdiff 检测 breaking"
	@echo "  make openapi-preview   本地生成 HTML 预览（Redoc）"
	@echo "  make openapi-doctor    环境自检（注解/工具/baseline 前置是否就绪）"
	@echo ""
	@echo "Docs: $(OCTO_API_DIR)/references/toolchain.md"

# Resolve swag absolute path: prefer PATH (e.g. brew), else $GOPATH/bin
# (where `go install` lands). Makefile can't trust caller's PATH to
# contain $GOPATH/bin, so we fall back explicitly.
SWAG    := $(shell command -v swag    2>/dev/null || echo $$(go env GOPATH)/bin/swag)
OASDIFF := $(shell command -v oasdiff 2>/dev/null || echo $$(go env GOPATH)/bin/oasdiff)

# Derived from OPENAPI_OUT_TYPES: yaml→swagger.yaml json→swagger.json go→docs.go
comma := ,
OPENAPI_SPEC_FILES := $(foreach t,$(subst $(comma), ,$(OPENAPI_OUT_TYPES)),$(if $(filter go,$(t)),$(OPENAPI_OUT_DIR)/docs.go,$(OPENAPI_OUT_DIR)/swagger.$(t)))

# ----------------------------------------------------------------------
# Install swag v2 CLI if missing
# ----------------------------------------------------------------------
openapi-install:
	@$(SWAG) --version 2>/dev/null | grep -q "v2\." || { \
	  echo "Installing swag $(SWAG_VERSION) to $$(go env GOPATH)/bin..."; \
	  go install github.com/swaggo/swag/v2/cmd/swag@$(SWAG_VERSION); \
	}
	@$(SWAG) --version

# ----------------------------------------------------------------------
# Coverage: every gin handler has @Router swag annotation
# ----------------------------------------------------------------------
openapi-coverage:
	bash $(OCTO_API_DIR)/scripts/check-swag-coverage.sh modules

# ----------------------------------------------------------------------
# Generate OpenAPI 3.1 spec from Go source + swag annotations.
# --parseDependencyLevel 1 resolves models referenced from dependency
# modules (e.g. octo-lib pkg/envelope generics) — the importing file
# still needs a (blank) import of that package; see api-spec.md §B.
# ----------------------------------------------------------------------
openapi-gen: openapi-doctor-gen openapi-install
	$(SWAG) init -g main.go -d ./ -o $(OPENAPI_OUT_DIR) --v3.1 --parseDependencyLevel 1 --outputTypes $(OPENAPI_OUT_TYPES)
	@bash $(OCTO_API_DIR)/scripts/normalize-spec.sh $(OPENAPI_OUT_DIR)
	@echo "💡 Tip: 'make openapi-preview' renders the spec to a local HTML page."

# ----------------------------------------------------------------------
# Environment doctor — fail fast with adoption pointers instead of
# cryptic swag/spectral errors. gen (and thus verify/check) runs the
# gen-scope checks automatically; the standalone target also requires
# the committed baseline.
# ----------------------------------------------------------------------
openapi-doctor:
	@bash $(OCTO_API_DIR)/scripts/check-prereqs.sh $(OPENAPI_OUT_DIR) --require-baseline

openapi-doctor-gen:
	@bash $(OCTO_API_DIR)/scripts/check-prereqs.sh $(OPENAPI_OUT_DIR)

# ----------------------------------------------------------------------
# Verify: regenerate spec and assert no drift vs committed baseline.
# ----------------------------------------------------------------------
openapi-verify: openapi-gen
	@DRIFT=$$(git status --porcelain -- $(OPENAPI_SPEC_FILES)); \
	if [ -n "$$DRIFT" ]; then \
	  echo "❌ OpenAPI spec drift detected:"; \
	  echo "$$DRIFT"; \
	  echo ""; \
	  echo "Run 'make openapi-gen' and commit $(OPENAPI_SPEC_FILES)."; \
	  exit 1; \
	fi
	@echo "✅ Generated spec matches committed baseline"

# ----------------------------------------------------------------------
# Lint spec against spectral.yaml
# ----------------------------------------------------------------------
openapi-lint:
	@test -f $(OPENAPI_OUT_DIR)/swagger.yaml || { echo "$(OPENAPI_OUT_DIR)/swagger.yaml missing — run 'make openapi-gen' first"; exit 1; }
	npx -y @stoplight/spectral-cli@latest lint $(OPENAPI_OUT_DIR)/swagger.yaml --ruleset $(OCTO_API_DIR)/assets/spectral.yaml --fail-severity error

# ----------------------------------------------------------------------
# Four-gate check (run before pushing).
# Runs ALL gates and aggregates instead of using make prerequisites —
# prerequisites stop at the first failure, which on staged-migration
# repos (coverage red for months) would mask every verify/lint finding
# behind it.
# ----------------------------------------------------------------------
openapi-check:
	@C=PASS; V=PASS; L=PASS; FAILED=0; \
	$(MAKE) --no-print-directory openapi-coverage || { C=FAIL; FAILED=1; }; \
	$(MAKE) --no-print-directory openapi-verify   || { V=FAIL; FAILED=1; }; \
	$(MAKE) --no-print-directory openapi-lint     || { L=FAIL; FAILED=1; }; \
	echo ""; \
	echo "═══ openapi-check gate summary ═══"; \
	[ $$C = PASS ] && echo "  ✅ coverage" || echo "  ❌ coverage"; \
	[ $$V = PASS ] && echo "  ✅ verify (gen + drift)" || echo "  ❌ verify (gen + drift)"; \
	[ $$L = PASS ] && echo "  ✅ lint" || echo "  ❌ lint"; \
	echo ""; \
	if [ $$FAILED -eq 1 ]; then echo "❌ openapi-check failed — fix the ❌ gates above"; exit 1; fi; \
	echo "✅ OpenAPI four-gate check passed (coverage → gen → verify → lint)"

# ----------------------------------------------------------------------
# Install oasdiff CLI if missing (semantic OpenAPI diff, breaking detection)
# ----------------------------------------------------------------------
oasdiff-install:
	@$(OASDIFF) --version 2>/dev/null | grep -q "oasdiff" || { \
	  echo "Installing oasdiff $(OASDIFF_VERSION) to $$(go env GOPATH)/bin..."; \
	  go install github.com/oasdiff/oasdiff@$(OASDIFF_VERSION); \
	}
	@$(OASDIFF) --version

# ----------------------------------------------------------------------
# Diff current spec against a base git ref (default origin/main).
# Uses oasdiff to classify each change as error / warning / info by
# OpenAPI semantics (breaking detection — no AI / human judgment needed).
# Exit code 1 if any 'error' severity change is found.
# ----------------------------------------------------------------------
openapi-diff: openapi-gen oasdiff-install
	@bash $(OCTO_API_DIR)/scripts/diff-openapi.sh $(BASE_REF)

# ----------------------------------------------------------------------
# Build a standalone HTML preview of the spec using Redoc.
# Output is throwaway — add $(OPENAPI_OUT_DIR)/index.html to .gitignore.
# ----------------------------------------------------------------------
openapi-preview: openapi-gen
	@npx -y @redocly/cli@latest build-docs $(OPENAPI_OUT_DIR)/swagger.yaml \
	  -o $(OPENAPI_OUT_DIR)/index.html
	@echo ""
	@echo "✓ open $(OPENAPI_OUT_DIR)/index.html (macOS: open …; Linux: xdg-open …)"

.PHONY: openapi-help openapi-install oasdiff-install openapi-coverage openapi-gen openapi-verify openapi-lint openapi-check openapi-diff openapi-preview openapi-doctor openapi-doctor-gen
