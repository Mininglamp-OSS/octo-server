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