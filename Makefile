.PHONY: deploy nginx-log-reset nginx-log-rotate

IP := $(shell hostname -I | awk '{print $$1}' | tr '.' '-')
NGINX_LOG_DIR := /home/isucon/analysis/nginx
LOG_FILE := $(NGINX_LOG_DIR)/access.$(IP).log

deploy:
	$(MAKE) -C home/isucon/webapp/go isureal
	sudo systemctl restart isureal-go.service

nginx-log-reset:
	rm -f /var/log/nginx/access.log
	sudo systemctl reload nginx

nginx-log-rotate:
	mkdir -p $(NGINX_LOG_DIR)
	sudo cat /var/log/nginx/access.log | alp ltsv > $(LOG_FILE)
	cat $(LOG_FILE)
