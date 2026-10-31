.PHONY: deploy nginx-log-reset nginx-log-rotate

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
