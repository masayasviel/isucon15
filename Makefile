.PHONY: deploy nginx-log-reset nginx-log-rotate

IP := $(shell hostname -I | awk '{print $$1}' | tr '.' '-')
LOG_FILE := /home/isucon/analysis/nginx/access.$(IP).log

deploy:
	$(MAKE) -C home/isucon/webapp/go isureal
	sudo systemctl restart isureal-go.service

nginx-log-reset:
	rm -f /var/log/nginx/access.log
	sudo systemctl reload nginx

nginx-log-rotate:
	sudo cat /var/log/nginx/access.log | alp ltsv > $(LOG_FILE)
	cat $(LOG_FILE)
