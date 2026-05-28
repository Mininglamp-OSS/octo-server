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
# i18n-merge    : optional convenience target that takes the freshly emitted
#                 marker file and merges it into the runtime locale files via
#                 the upstream `goi18n` CLI. Requires:
#                     go install github.com/nicksnyder/go-i18n/v2/goi18n@v2.6.1
#                 The merge surfaces new keys as translate.<lang>.toml stubs
#                 that translators fill in and then move into active.<lang>.toml.

.PHONY: i18n-extract i18n-extract-check i18n-merge

i18n-extract:
	go run ./pkg/i18n/cmd/octo-i18n-extract

i18n-extract-check:
	go run ./pkg/i18n/cmd/octo-i18n-extract -check

i18n-merge: i18n-extract
	@command -v goi18n >/dev/null 2>&1 || { \
	  echo "goi18n not on PATH — install with:"; \
	  echo "  go install github.com/nicksnyder/go-i18n/v2/goi18n@v2.6.1"; \
	  exit 1; \
	}
	goi18n merge -sourceLanguage en-US -outdir pkg/i18n/locales \
	  tools/i18nmarkers/shared/active.en-US.toml \
	  pkg/i18n/locales/active.zh-CN.toml 