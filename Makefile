# mota-agent-server-go Makefile
# =============================
# Go 版常用开发、校验、交叉构建、部署与运维入口。

.DEFAULT_GOAL := help

.PHONY: help build release dev run check test test-pg lint fmt bench verify ci clean \
        deploy deploy-build deploy-upload deploy-restart deploy-pi deploy-full \
        logs logs-follow status health ssh db-shell

# ─── 变量 ────────────────────────────────────────────────────────────
GO              ?= go
BINARY_NAME     ?= mota-agent-server
BUILD_DIR       ?= dist
BUILD_ARTIFACT  := $(BUILD_DIR)/$(BINARY_NAME)
DEPLOY_ARTIFACT := $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64
ENV_DEV         ?= .env.dev

REMOTE_HOST    ?= ubuntu@mota-test
REMOTE_ROOT    ?= /opt/mota-agent-server-go
REMOTE_BIN     ?= $(REMOTE_ROOT)/bin/$(BINARY_NAME)
REMOTE_SERVICE ?= mota-agent-server-go
REMOTE_ENV     ?= $(REMOTE_ROOT)/env.conf
HEALTH_URL     ?= http://127.0.0.1:8080/health
PI_NPM_PACKAGE ?= @earendil-works/pi-coding-agent@latest

export GOCACHE := $(CURDIR)/.gocache

# ─── 默认目标 ────────────────────────────────────────────────────────
help: ## 显示帮助信息
	@echo "mota-agent-server-go 常用命令"
	@echo ""
	@echo "  开发:"
	@echo "    make dev          编译并运行（自动加载可选 .env.dev）"
	@echo "    make build        编译本机二进制"
	@echo "    make release      编译精简的本机 release 二进制"
	@echo "    make test         运行全部测试"
	@echo "    make test-pg      使用 TEST_DATABASE_URL 运行含 PostgreSQL 的测试"
	@echo "    make lint         gofmt 检查 + go vet"
	@echo "    make fmt          格式化全部已跟踪 Go 文件"
	@echo "    make check        编译全部包和测试，但不运行测试"
	@echo "    make bench        运行 Go benchmark"
	@echo "    make verify       校验 go.mod/go.sum 下载内容"
	@echo "    make ci           执行 lint + test + build + verify"
	@echo "    make clean        清理构建产物和项目内 Go 缓存"
	@echo ""
	@echo "  部署 (→ $(REMOTE_HOST)):"
	@echo "    make deploy-build   交叉编译 Linux ARM64 静态二进制"
	@echo "    make deploy-upload  上传二进制并备份上一版本"
	@echo "    make deploy-restart 重启远程服务"
	@echo "    make deploy         build + upload + restart"
	@echo "    make deploy-pi      安装/更新 pi-agent 并同步路径"
	@echo "    make deploy-full    build + upload + pi-agent + restart"
	@echo ""
	@echo "  运维:"
	@echo "    make status       查看远程服务状态"
	@echo "    make logs         查看最近 50 行日志"
	@echo "    make logs-follow  实时跟踪日志"
	@echo "    make health       检查健康接口 ($(HEALTH_URL))"
	@echo "    make ssh          SSH 到服务器"
	@echo "    make db-shell     进入 PostgreSQL shell"

# ─── 开发 ────────────────────────────────────────────────────────────
build: ## 编译本机二进制
	@mkdir -p $(BUILD_DIR)
	$(GO) build -o $(BUILD_ARTIFACT) .

release: ## 编译精简的本机 release 二进制
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "-s -w" -o $(BUILD_ARTIFACT) .

dev: ## 编译并运行（自动加载可选 .env.dev）
	@[ -f $(ENV_DEV) ] && set -a && . ./$(ENV_DEV) && set +a; $(GO) run .

run: dev ## dev 的别名

check: ## 编译全部包和测试，但不运行测试
	$(GO) test -run '^$$' ./...

test: ## 运行全部测试
	$(GO) test ./...

test-pg: ## 使用隔离 PostgreSQL 测试库运行测试
	@test -n "$(TEST_DATABASE_URL)" || { echo "错误: 请设置 TEST_DATABASE_URL"; exit 1; }
	$(GO) test ./...

lint: ## gofmt 检查 + go vet
	@files="$$(gofmt -l $$(git ls-files '*.go'))"; test -z "$$files" || { echo "以下文件需要 gofmt:"; echo "$$files"; exit 1; }
	$(GO) vet ./...

fmt: ## 格式化全部已跟踪 Go 文件
	gofmt -w $$(git ls-files '*.go')

bench: ## 运行 Go benchmark
	$(GO) test -run '^$$' -bench . ./mota

verify: ## 校验模块下载内容
	$(GO) mod verify

ci: lint test build verify ## 执行与 CI 对齐的完整校验

clean: ## 清理构建产物和项目内 Go 缓存
	rm -rf $(BUILD_DIR) .gocache

# ─── 部署 ────────────────────────────────────────────────────────────
deploy-build: ## 交叉编译 Linux ARM64 静态二进制
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "-s -w" -o $(DEPLOY_ARTIFACT) .
	@echo ""
	@echo "✅ 产物: $(DEPLOY_ARTIFACT)"
	@ls -lh $(DEPLOY_ARTIFACT) | awk '{print "   大小:", $$5}'
	@shasum -a 256 $(DEPLOY_ARTIFACT) | awk '{print "   SHA256:", $$1}'

deploy-upload: ## 上传二进制并备份上一版本
	@test -s $(DEPLOY_ARTIFACT) || { echo "错误: 部署产物不存在，请先运行 make deploy-build"; exit 1; }
	scp $(DEPLOY_ARTIFACT) $(REMOTE_HOST):$(REMOTE_BIN).new
	ssh $(REMOTE_HOST) 'set -eu; test -s $(REMOTE_BIN).new; chmod +x $(REMOTE_BIN).new; if test -f $(REMOTE_BIN); then cp -p $(REMOTE_BIN) $(REMOTE_BIN).previous; fi; mv $(REMOTE_BIN).new $(REMOTE_BIN)'
	@echo "✅ 已上传到 $(REMOTE_HOST):$(REMOTE_BIN)"

deploy-restart: ## 重启远程服务
	ssh $(REMOTE_HOST) 'sudo systemctl restart $(REMOTE_SERVICE) && sleep 2 && systemctl is-active $(REMOTE_SERVICE)'
	@echo "✅ 服务已重启"

deploy: ## build + upload + restart（强制串行）
	$(MAKE) deploy-build
	$(MAKE) deploy-upload
	$(MAKE) deploy-restart

deploy-pi: ## 安装/更新 pi-agent，并写入实际路径
	ssh $(REMOTE_HOST) 'set -eu; command -v npm >/dev/null 2>&1 || { echo "错误: 远程主机未安装 npm"; exit 1; }; sudo npm install -g --ignore-scripts "$(PI_NPM_PACKAGE)"; pi_path=$$(command -v pi); if sudo grep -q "^LOCAL_AGENT__PI_BINARY=" $(REMOTE_ENV); then sudo sed -i "s|^LOCAL_AGENT__PI_BINARY=.*|LOCAL_AGENT__PI_BINARY=$$pi_path|" $(REMOTE_ENV); else printf "%s\n" "LOCAL_AGENT__PI_BINARY=$$pi_path" | sudo tee -a $(REMOTE_ENV) >/dev/null; fi; echo "pi: $$pi_path"'
	@echo "✅ pi-agent 已安装并同步配置"

deploy-full: ## build + upload + pi-agent + restart（强制串行）
	$(MAKE) deploy-build
	$(MAKE) deploy-upload
	$(MAKE) deploy-pi
	$(MAKE) deploy-restart

# ─── 运维 ────────────────────────────────────────────────────────────
status: ## 查看远程服务状态
	ssh $(REMOTE_HOST) 'systemctl status $(REMOTE_SERVICE) --no-pager -l | head -15; echo "---"; ss -tlnp | grep $(BINARY_NAME) || echo "(not listening)"'

logs: ## 查看最近 50 行日志
	ssh $(REMOTE_HOST) 'sudo journalctl -u $(REMOTE_SERVICE) -n 50 --no-pager'

logs-follow: ## 实时跟踪日志
	ssh $(REMOTE_HOST) 'sudo journalctl -u $(REMOTE_SERVICE) -f --no-pager'

health: ## 检查健康接口
	@curl -fsS $(HEALTH_URL) | python3 -m json.tool

ssh: ## SSH 到服务器
	ssh $(REMOTE_HOST)

db-shell: ## 进入 PostgreSQL shell
	ssh -t $(REMOTE_HOST) 'docker exec -it multica-postgres-1 psql -U mota_agent -d mota_agent'
