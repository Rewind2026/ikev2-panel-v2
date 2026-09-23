# ikev2-panel v2 Makefile
#
# 速查:
#   make help           列出所有 target
#   make build          编译二进制到 ./ikev2-panel
#   make test           跑全部 go test
#   make vet            go vet 静态检查
#   make dev            ★ 本机 dev 模式启动(无需 docker)★
#   make dev-stop       停掉后台 dev 进程
#   make dev-logs       tail dev 日志
#   make docker-build   构建 docker 镜像(本地)
#   make docker-run     docker compose up -d
#
# 设计原则:
#   - webui 改动(HTML/CSS/JS)直接 make dev,无需打包 docker
#   - 后端 Go 改动 make build + make dev
#   - 真机部署走 docker 链路

GO              ?= go
BINARY          ?= ./ikev2-panel
DEV_PORT_HTTP   ?= 19090
DEV_PORT_HTTPS  ?= 18443
DEV_DATA_DIR    ?= ./dev-data
DEV_LOG         ?= /tmp/ikev2-panel-dev.log
DEV_PID_FILE    ?= /tmp/ikev2-panel-dev.pid

.PHONY: help build test vet race fmt dev dev-stop dev-logs clean docker-build docker-run

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## 编译 Go 二进制(本地开发用)
	$(GO) build -o $(BINARY) ./cmd/ikev2-panel

test: ## 跑全部单测
	$(GO) test ./...

race: ## 跑全部 race 检测(慢,需要 cgo)
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

vet: ## 静态检查
	$(GO) vet ./...

fmt: ## 格式化所有 Go 源码
	$(GO) fmt ./...

# ----------------------------------------------------------------------------
# dev 模式 —— 本机直接起服务,不需要 docker
#
# 自动行为:
#   - 检测 /etc/swanctl 不存在 → 自动切 SkipVici 模式(不调 charon)
#   - 模板从 ./web/templates 读(优先级高于 /app/web/templates)
#   - 静态资源从 ./web/static 读
#   - SQLite 写到 $(DEV_DATA_DIR)/panel.db
#   - 自动生成 self-signed 证书到 $(DEV_DATA_DIR)/
#   - 首次启动自动创建 admin 账号(密码写 INITIAL_ADMIN_PASSWORD.txt)
#   - HTTP 端口 $(DEV_PORT_HTTP),HTTPS 端口 $(DEV_PORT_HTTPS)
#
# 端口:避开 8443 / 9090(防跟 63 上的部署冲突)
#
# 改了模板/CSS 后: dev 进程不需要重启,浏览器 Ctrl+Shift+R 强刷即可
# (CSS/JS header 已配 Cache-Control: no-cache, must-revalidate)
#
# 改了 Go 代码后: make build && make dev-stop && make dev
# ----------------------------------------------------------------------------
dev: build ## 编译并以后台进程启动 dev 模式
	@if [ -f $(DEV_PID_FILE) ] && kill -0 $$(cat $(DEV_PID_FILE)) 2>/dev/null; then \
		echo "dev already running (pid $$(cat $(DEV_PID_FILE))), stop first: make dev-stop"; \
		exit 1; \
	fi
	@mkdir -p $(DEV_DATA_DIR)
	@rm -rf $(DEV_DATA_DIR)/*
	@echo "=== starting dev (logs → $(DEV_LOG)) ==="
	@IKEV2_DATA_DIR=$(DEV_DATA_DIR) \
	 IKEV2_LISTEN_ADDR=127.0.0.1:$(DEV_PORT_HTTPS) \
	 IKEV2_HTTP_LISTEN_ADDR=127.0.0.1:$(DEV_PORT_HTTP) \
	 IKEV2_CERT_MODE=self-signed \
	 $(BINARY) > $(DEV_LOG) 2>&1 & echo $$! > $(DEV_PID_FILE)
	@sleep 2
	@echo "=== status ==="
	@ps -p $$(cat $(DEV_PID_FILE)) -o pid,etime,cmd 2>/dev/null || echo "process died, see $(DEV_LOG)"
	@echo "=== quick info ==="
	@grep -E "默认管理员|listen_addr|HTTP panel|admin 密码|INITIAL_ADMIN_PASSWORD" $(DEV_LOG) | head -5
	@echo ""
	@echo "=== access ==="
	@echo "  HTTP  (no TLS) : http://127.0.0.1:$(DEV_PORT_HTTP)/login"
	@echo "  HTTPS (self-signed): https://127.0.0.1:$(DEV_PORT_HTTPS)/login"
	@echo "  管理员账号: admin"
	@echo "  密码: cat $(DEV_DATA_DIR)/panel-state/INITIAL_ADMIN_PASSWORD.txt"
	@echo ""
	@echo "=== stop ==="
	@echo "  make dev-stop"

dev-stop: ## 停掉后台 dev 进程
	@if [ -f $(DEV_PID_FILE) ]; then \
		PID=$$(cat $(DEV_PID_FILE)); \
		if kill -0 $$PID 2>/dev/null; then \
			echo "stopping dev (pid $$PID)"; \
			kill $$PID; \
			sleep 1; \
			kill -9 $$PID 2>/dev/null || true; \
		fi; \
		rm -f $(DEV_PID_FILE); \
	else \
		echo "no dev pid file at $(DEV_PID_FILE)"; \
	fi

dev-logs: ## tail dev 日志
	@tail -f $(DEV_LOG)

# ----------------------------------------------------------------------------
# docker 链路
# ----------------------------------------------------------------------------
docker-build: ## 构建 docker 镜像(本地)
	docker build -t ikev2-panel:dev .

docker-run: docker-build ## 本地 docker compose up
	docker compose up -d

clean: ## 清理构建产物 + dev 数据
	rm -f $(BINARY)
	rm -rf $(DEV_DATA_DIR)
	rm -f $(DEV_PID_FILE) $(DEV_LOG)
