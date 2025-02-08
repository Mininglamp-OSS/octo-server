build:
	docker build -t octo-server .
push:
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/wukongchatserver:latest
deploy:
	docker build -t octo-server . --platform linux/amd64
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:latest
deploy-v1.5:
	docker build -t octo-server . --platform linux/amd64
	docker tag octo-server registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:v1.5
	docker push registry.cn-shanghai.aliyuncs.com/wukongim/octo-server:v1.5
run-dev:
	docker-compose build;docker-compose up -d
stop-dev:
	docker-compose stop
env-test:
	docker-compose -f ./testenv/docker-compose.yaml up -d 