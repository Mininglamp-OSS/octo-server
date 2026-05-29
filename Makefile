build:
	docker build -t octo-server .
push:
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/wukongchatserver:latest
deploy:
	docker build -t octo-server . --platform linux/amd64
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
deploy-v2:
	docker build -t octo-server . --platform linux/amd64
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:v2
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:v2

run-dev:
	@echo "run-dev has been retired — the bundled docker-compose stack moved to"; \
	echo "  https://github.com/Mininglamp-OSS/octo-deployment"; \
	echo "Use that repo's setup.sh + docker compose up -d, or see QUICKSTART.md"; \
	echo "Option 2 (Local Go build) for the dev loop in this repo."; \
	exit 1
stop-dev:
	@echo "stop-dev has been retired alongside run-dev — see"; \
	echo "  https://github.com/Mininglamp-OSS/octo-deployment"; \
	exit 1
env-test:
	docker-compose -f ./testenv/docker-compose.yaml up -d

# ---- i18n message marker pipeline (TODOS §0.8 / D18) ----------------------
#
# i18n-extract  : regenerate tools/i18nmarkers/{shared,server}/active.en-US.toml
#                 from codes.Register / errcode.register AST call sites.
# i18n-extract-check : CI guard — fails (exit 3) when on-disk markers diverge
#                      from what extraction would emit. Wired up alongside
#                      the rest of the 0.10 lint suite.
# i18n-merge    : optional convenience target that feeds BOTH generated marker
#                 files (shared + server) into the upstream `goi18n` CLI to
#                 produce translate.<lang>.toml stubs for new keys. Requires:
#                     go install github.com/nicksnyder/go-i18n/v2/goi18n@v2.6.1
#                 First-run side effect: goi18n rewrites active.<lang>.toml
#                 into its canonical format (entries sorted by ID, content
#                 hashes added, top-of-file comments stripped). This is
#                 expected once translators adopt the goi18n workflow; hashes
#                 are how goi18n detects source drift requiring re-translation.

# i18n-lint     : run the Phase 0.10 migration gates locally (the same checks
#                 CI runs in the "i18n Lint" job):
#                   - lint-direct-error-response: D23 no-new-AbortWithStatusJSON
#                     ratchet vs tools/lint-direct-error-response/baseline.txt
#                   - lint-unregistered-code: no inline codes.Code{} literals
#                     passed to httperr.ResponseErrorL (registry bypass)

.PHONY: i18n-extract i18n-extract-check i18n-merge i18n-lint

i18n-extract:
	go run ./pkg/i18n/cmd/octo-i18n-extract

i18n-lint:
	go run ./tools/lint-direct-error-response
	go run ./tools/lint-unregistered-code

i18n-extract-check:
	go run ./pkg/i18n/cmd/octo-i18n-extract -check

i18n-merge: i18n-extract
	@command -v goi18n >/dev/null 2>&1 || { \
	  echo "goi18n not on PATH — install with:"; \
	  echo "  go install github.com/nicksnyder/go-i18n/v2/goi18n@v2.6.1"; \
	  exit 1; \
	}
	# CRITICAL: feed BOTH shared and server marker files to goi18n as source
	# inputs. With `-sourceLanguage en-US`, goi18n treats the union of source
	# files as the canonical message set and rewrites any existing translation
	# file (active.zh-CN.toml) to that set — entries not present in the source
	# inputs are removed. Omitting the server marker file destructively wipes
	# every err.server.* zh-CN translation from active.zh-CN.toml (verified
	# against goi18n@v2.6.1 by PR #186 reviewers; preserving server
	# translations is the contract this target must hold).
	goi18n merge -sourceLanguage en-US -outdir pkg/i18n/locales \
	  tools/i18nmarkers/shared/active.en-US.toml \
	  tools/i18nmarkers/server/active.en-US.toml \
	  pkg/i18n/locales/active.zh-CN.toml
