.PHONY: deploy nginx-log-reset nginx-log-rotate index index-drop index-verify

IP := $(shell hostname -I | awk '{print $$1}' | tr '.' '-')
NOW := $(shell date +%Y%m%d-%H%M%S)

NGINX_LOG_DIR := /home/isucon/analysis/nginx
NGINX_LOG_FILE := $(NGINX_LOG_DIR)/access.$(IP).log
NGINX_LOG_BRANCH := nginx-log-$(NOW)

deploy:
	$(MAKE) -C home/isucon/webapp/go isureal
	sudo systemctl restart isureal-go.service

nginx-log-reset:
	rm -f /var/log/nginx/access.log
	sudo systemctl reload nginx

nginx-log-rotate:
	mkdir -p $(NGINX_LOG_DIR)
	sudo cat /var/log/nginx/access.log | alp ltsv > $(NGINX_LOG_FILE)
	git switch -c $(NGINX_LOG_BRANCH)
	git add $(NGINX_LOG_FILE)
	git commit -m "nginx log $(NOW)"
	git push -u origin $(NGINX_LOG_BRANCH)
	gh pr create --fill
	git switch main

ENV_FILE := /home/isucon/env.sh
SQL_DIR := home/isucon/webapp/sql

PSQL = set -eu; \
	. $(ENV_FILE); \
	PGPASSWORD="$$ISUREAL_DB_PASSWORD" psql \
		--no-psqlrc \
		--no-password \
		--host="$$ISUREAL_DB_HOST" \
		--port="$$ISUREAL_DB_PORT" \
		--username="$$ISUREAL_DB_USER" \
		--dbname="$$ISUREAL_DB_NAME" \
		--set=ON_ERROR_STOP=1

# 900-extra-index.sql の追加インデックスを適用する。
index:
	$(PSQL) --file=$(SQL_DIR)/900-extra-index.sql

# 効果が無かったインデックスを名前で指定して外す (make index-drop NAME=xxx)。
index-drop:
	test -n "$(NAME)" || { echo 'usage: make index-drop NAME=<index_name>' >&2; exit 1; }
	$(PSQL) --command='DROP INDEX IF EXISTS $(NAME)'

# インデックスが存在し、実際に Index Scan で使われているかを確認する。
index-verify:
	$(PSQL) --command='\d login_sessions'
	$(PSQL) --command="EXPLAIN SELECT s.user_id, s.token_hash, s.created_at, u.region \
		FROM login_sessions s JOIN users u ON u.id = s.user_id \
		WHERE s.token_hash = 'dummy' AND s.active = true AND u.status = 'active'"
