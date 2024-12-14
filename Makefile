all: rotate-all app-deploy

include .env

ifeq ($(SERVER),)
    $(error SERVER env is not set)
endif

# === 恐らく変更不要 ===

NGINX_ACCESS_LOG:=/var/log/nginx/access.ndjson
NGINX_CONF:=/etc/nginx

MYSQL_SLOW_LOG:=/var/log/mysql/mysql-slow.log
MYSQL_CONF:=/etc/mysql

# =====================


# === 変更が必要 ===

HOME:=/home/isucon
ENV_FILE:=env.sh
ENV_PATH:=$(HOME)/$(ENV_FILE)
WEBAPP:=$(HOME)/webapp
APP:=$(WEBAPP)/go
APP_BINARY:=isuride

SERVER_ETC:=$(WEBAPP)/etc/$(SERVER)

SERVICE:=isuride-go.service

# =====================

PPROF_EXEC_PORT:=6060
PPROF_WEBUI_PORT:=1080
PPROF_URL:=http://localhost:$(PPROF_EXEC_PORT)/debug/pprof/profile

.PHONY: rotate-all
rotate-all: rotate-access-log rotate-slow-log

.PHONY: rotate-access-log
rotate-access-log:
	echo "Rotating access log"
	if [ ! -d $(SERVER_ETC)/nginx ]; then echo "nginx not configured"; exit 0; fi
	if [ ! -f $(NGINX_ACCESS_LOG) ]; then echo "access log not found"; exit 0; fi
	sudo mv $(NGINX_ACCESS_LOG) $(NGINX_ACCESS_LOG).$(shell date +%Y%m%d)
	sudo systemctl restart nginx

.PHONY: rotate-slow-log
rotate-slow-log:
	echo "Rotating slow log"
	if [ ! -d $(SERVER_ETC)/mysql ]; then echo "mysql not configured"; exit 0; fi
	if [ ! -f $(MYSQL_SLOW_LOG) ]; then echo "slow log not found"; exit 0; fi
	sudo mv $(MYSQL_SLOW_LOG) $(MYSQL_SLOW_LOG).$(shell date +%Y%m%d)
	sudo systemctl restart mysql



.PHONY: dump-all
dump-all: dump-env dump-nginx dump-mysql

.PHONY: dump-nginx
dump-nginx:
	echo "dump nginx conf"
	mkdir -p $(SERVER_ETC)
	cp -r $(NGINX_CONF) $(SERVER_ETC)

.PHONY: dump-mysql
dump-mysql:
	echo "dump nginx conf"
	mkdir -p $(SERVER_ETC)
	cp -r $(MYSQL_CONF) $(SERVER_ETC)

.PHONY: dump-env
dump-env:
	echo "dump env"
	mkdir -p $(SERVER_ETC)
	cp $(ENV_PATH) $(SERVER_ETC)


.PHONY: alp
alp:
	echo "alp"
	alp json --config alp-config.yml

.PHONY: pt
pt:
	echo "pt-query-digest"
	sudo pt-query-digest $(MYSQL_SLOW_LOG)

.PHONY: pprof
pprof:
	echo "pprof"
	go tool pprof -seconds 60 -http=localhost:$(PPROF_WEBUI_PORT) $(PPROF_URL)


.PHONY: deploy-all
deploy-all: env-deploy nginx-deploy mysql-deploy app-deploy

.PHONY: env-deploy
env-deploy:
	echo "env deploy"
	if [ ! -f $(SERVER_ETC)/$(ENV_FILE) ]; then echo "env not configured"; exit 1; fi
	cp -f $(SERVER_ETC)/$(ENV_FILE) $(ENV_PATH)
	# sudo systemctl restart isuride-matcher.service

.PHONY: nginx-deploy
nginx-deploy:
	echo "nginx conf deploy"
	if [ ! -d $(SERVER_ETC)/nginx ]; then echo "nginx not configured"; exit 1; fi
	sudo cp -r $(SERVER_ETC)/nginx/* $(NGINX_CONF)
	sudo nginx -t
	sudo systemctl restart nginx

.PHONY: mysql-deploy
mysql-deploy:
	echo "mysql conf deploy"
	if [ ! -d $(SERVER_ETC)/mysql ]; then echo "mysql not configured"; exit 1; fi
	sudo cp -r $(SERVER_ETC)/mysql/* $(MYSQL_CONF)
	sudo systemctl restart mysql

.PHONY: app-deploy
app-deploy:
	echo "app deploy"
	cd $(APP) && go build -o $(APP_BINARY) *.go
	sudo systemctl restart $(SERVICE)

