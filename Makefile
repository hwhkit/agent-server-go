# ir-agent-server-go Makefile
# =============================
# Go 版常用开发、校验、交叉构建、部署与运维入口。

.DEFAULT_GOAL := help

.PHONY: help bootstrap-hwhkit build release dev run check test race test-pg \
        lint lint-shell fmt bench verify test-deploy-scripts ci clean \
        deploy-check deploy deploy-build deploy-upload deploy-restart deploy-pi deploy-full \
        logs logs-follow status health ssh db-shell

# ─── 变量 ────────────────────────────────────────────────────────────
GO              ?= go
BINARY_NAME     ?= ir-agent-server
BUILD_DIR       ?= dist
BUILD_ARTIFACT  := $(BUILD_DIR)/$(BINARY_NAME)
DEPLOY_ARTIFACT := $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64
DEPLOY_CHECKSUM := $(DEPLOY_ARTIFACT).sha256
ENV_DEV         ?= .env.dev

# go.mod intentionally resolves the unpublished hwhkit modules from this sibling
# checkout.  Keep the exact commit here so local and CI builds use identical code.
HWHKIT_ROOT       := $(abspath ../.hwhkit-go-src)
HWHKIT_REPOSITORY ?= https://github.com/hwhkit/hwhkit-go
HWHKIT_REF        ?= 5e35b70bd062c600f265758955beff8b3d27299a

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
GIT_SHA    ?= $(shell git rev-parse HEAD 2>/dev/null || printf 'unknown')
BUILD_TIME ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || printf 'unknown')
BUILD_LDFLAGS := -X main.version=$(VERSION) \
                 -X github.com/hwhkit/hwhkit-go/buildinfo.Version=$(VERSION) \
                 -X github.com/hwhkit/hwhkit-go/buildinfo.GitSHA=$(GIT_SHA) \
                 -X github.com/hwhkit/hwhkit-go/buildinfo.BuildTime=$(BUILD_TIME)
RELEASE_LDFLAGS := -s -w $(BUILD_LDFLAGS)

REMOTE_HOST    ?= ubuntu@mota-test
REMOTE_ROOT    ?= /opt/ir-agent-server-go
REMOTE_BIN     ?= $(REMOTE_ROOT)/bin/$(BINARY_NAME)
REMOTE_SERVICE ?= ir-agent-server-go
REMOTE_ENV     ?= $(REMOTE_ROOT)/env.conf
HEALTH_URL     ?= http://127.0.0.1:8080/health/ready
REMOTE_HEALTH_URL      ?= http://127.0.0.1:8080/health/ready
REMOTE_HEALTH_ATTEMPTS ?= 10
REMOTE_HEALTH_INTERVAL ?= 2
PI_NPM_PACKAGE ?=
DEPLOY_DRY_RUN ?= 0

BOOTSTRAP_SCRIPT      := scripts/bootstrap-hwhkit.sh
DEPLOY_UPLOAD_SCRIPT  := scripts/deploy-upload.sh
DEPLOY_RESTART_SCRIPT := scripts/deploy-restart.sh
DEPLOY_PI_SCRIPT      := scripts/deploy-pi.sh
REMOTE_SERVICE_SCRIPT := scripts/remote-service.sh
shell_quote = '$(subst ','"'"',$(1))'
EXPECTED_SHA256 ?= $(shell sh scripts/file-sha256.sh $(call shell_quote,$(DEPLOY_ARTIFACT)) 2>/dev/null)

export GOCACHE := $(CURDIR)/.gocache

# ─── 默认目标 ────────────────────────────────────────────────────────
help: ## 显示帮助信息
	@echo "ir-agent-server-go 常用命令"
	@echo ""
	@echo "  开发:"
	@echo "    make bootstrap-hwhkit 固定版本初始化/校验 hwhkit 源码"
	@echo "    make dev          编译并运行（自动加载可选 .env.dev）"
	@echo "    make build        编译本机二进制"
	@echo "    make release      编译精简的本机 release 二进制"
	@echo "    make test         运行全部测试"
	@echo "    make race         使用 race detector 运行全部测试"
	@echo "    make test-pg      使用 TEST_DATABASE_URL 运行含 PostgreSQL 的测试"
	@echo "    make lint         gofmt 检查 + go vet + ShellCheck"
	@echo "    make fmt          格式化全部已跟踪 Go 文件"
	@echo "    make check        编译全部包和测试，但不运行测试"
	@echo "    make bench        运行 Go benchmark"
	@echo "    make verify       校验 go.mod/go.sum 与模块整洁性"
	@echo "    make ci           执行 lint + test + race + build + deploy-build + verify"
	@echo "    make clean        清理构建产物和项目内 Go 缓存"
	@echo ""
	@echo "  部署 (→ $(REMOTE_HOST)):"
	@echo "    make deploy-build   交叉编译 Linux ARM64 静态二进制"
	@echo "    make deploy-upload  校验 SHA256 后唯一暂存二进制"
	@echo "    make deploy-restart 原子切换、就绪检查并在失败时回滚"
	@echo "    make deploy         build + upload + restart"
	@echo "    make deploy-pi      安装固定版本 pi-agent 并同步路径"
	@echo "    make deploy-full    build + pi-agent + upload + restart"
	@echo ""
	@echo "  运维:"
	@echo "    make status       查看远程服务状态"
	@echo "    make logs         查看最近 50 行日志"
	@echo "    make logs-follow  实时跟踪日志"
	@echo "    make health       检查健康接口 ($(HEALTH_URL))"
	@echo "    make ssh          SSH 到服务器"
	@echo "    make db-shell     进入 PostgreSQL shell"

# ─── 开发 ────────────────────────────────────────────────────────────
bootstrap-hwhkit: ## 初始化/校验固定版本 hwhkit sibling checkout
	sh $(BOOTSTRAP_SCRIPT) $(call shell_quote,$(HWHKIT_ROOT)) $(call shell_quote,$(HWHKIT_REPOSITORY)) $(call shell_quote,$(HWHKIT_REF))

build: bootstrap-hwhkit ## 编译本机二进制
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "$(BUILD_LDFLAGS)" -o $(BUILD_ARTIFACT) ./cmd/ir-agent-server

release: bootstrap-hwhkit ## 编译精简的本机 release 二进制
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(BUILD_ARTIFACT) ./cmd/ir-agent-server

dev: bootstrap-hwhkit ## 编译并运行（自动加载可选 .env.dev）
	@[ -f $(ENV_DEV) ] && set -a && . ./$(ENV_DEV) && set +a; $(GO) run ./cmd/ir-agent-server

run: dev ## dev 的别名

check: bootstrap-hwhkit ## 编译全部包和测试，但不运行测试
	$(GO) test -run '^$$' ./...

test: bootstrap-hwhkit ## 运行全部测试
	$(GO) test -count=1 ./...

race: bootstrap-hwhkit ## 使用 race detector 运行全部测试
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

test-pg: bootstrap-hwhkit ## 使用隔离 PostgreSQL 测试库运行测试
	@test -n "$(TEST_DATABASE_URL)" || { echo "错误: 请设置 TEST_DATABASE_URL"; exit 1; }
	TEST_DATABASE_URL=$(call shell_quote,$(TEST_DATABASE_URL)) $(GO) test -count=1 ./...

lint: bootstrap-hwhkit lint-shell ## gofmt 检查 + go vet + ShellCheck
	@files="$$(gofmt -l $$(git ls-files '*.go'))"; test -z "$$files" || { echo "以下文件需要 gofmt:"; echo "$$files"; exit 1; }
	$(GO) vet ./...

lint-shell: ## 静态检查部署脚本（需 ShellCheck）
	shellcheck --shell=sh -S warning scripts/*.sh

fmt: ## 格式化全部已跟踪 Go 文件
	gofmt -w $$(git ls-files '*.go')

bench: bootstrap-hwhkit ## 运行 Go benchmark
	$(GO) test -run '^$$' -bench . ./internal/...

verify: bootstrap-hwhkit ## 校验模块下载内容与 go.mod/go.sum 整洁性
	$(GO) mod verify
	$(GO) mod tidy -diff

test-deploy-scripts: ## 运行部署脚本离线自测 / dry-run
	sh scripts/test-deploy-scripts.sh

ci: ## 执行与 GitHub Actions 对齐的完整校验（强制串行）
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) race
	$(MAKE) build
	$(MAKE) deploy-build
	$(MAKE) verify
	$(MAKE) test-deploy-scripts

clean: ## 清理构建产物和项目内 Go 缓存
	rm -rf $(BUILD_DIR) .gocache

# ─── 部署 ────────────────────────────────────────────────────────────
deploy-build: bootstrap-hwhkit ## 交叉编译 Linux ARM64 静态二进制
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(DEPLOY_ARTIFACT) ./cmd/ir-agent-server
	@sha="$$(sh scripts/file-sha256.sh $(call shell_quote,$(DEPLOY_ARTIFACT)))"; \
	  printf '%s  %s\n' "$$sha" "$(notdir $(DEPLOY_ARTIFACT))" > $(DEPLOY_CHECKSUM)
	@echo ""
	@echo "✅ 产物: $(DEPLOY_ARTIFACT)"
	@ls -lh $(DEPLOY_ARTIFACT) | awk '{print "   大小:", $$5}'
	@awk '{print "   SHA256:", $$1}' $(DEPLOY_CHECKSUM)

deploy-check: ## 拒绝脏工作区、陈旧产物和非当前提交产物
	@test -s $(DEPLOY_ARTIFACT) || { echo "错误: 部署产物不存在，请先运行 make deploy-build"; exit 1; }
	@if [ "$(ALLOW_DIRTY)" != "1" ] && test -n "$$(git status --porcelain)"; then \
	  echo "错误: 工作区有未提交修改；提交后再部署，或显式设置 ALLOW_DIRTY=1"; exit 1; fi
	@artifact_rev="$$(go version -m $(call shell_quote,$(DEPLOY_ARTIFACT)) | sed -n 's/^[[:space:]]*build[[:space:]]*vcs.revision=//p')"; \
	  current_rev="$$(git rev-parse HEAD)"; \
	  test "$$artifact_rev" = "$$current_rev" || { echo "错误: 部署产物不是当前提交（artifact=$$artifact_rev, HEAD=$$current_rev）"; exit 1; }
	@if [ "$(ALLOW_DIRTY)" != "1" ]; then \
	  modified="$$(go version -m $(call shell_quote,$(DEPLOY_ARTIFACT)) | sed -n 's/^[[:space:]]*build[[:space:]]*vcs.modified=//p')"; \
	  test "$$modified" = "false" || { echo "错误: 部署产物包含未提交修改"; exit 1; }; fi

deploy-upload: deploy-check ## 校验并暂存部署二进制
	@test -n "$(EXPECTED_SHA256)" || { echo "错误: 无法计算部署产物 SHA256"; exit 1; }
	DEPLOY_DRY_RUN=$(call shell_quote,$(DEPLOY_DRY_RUN)) sh $(call shell_quote,$(DEPLOY_UPLOAD_SCRIPT)) $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_BIN)) $(call shell_quote,$(DEPLOY_ARTIFACT)) $(call shell_quote,$(EXPECTED_SHA256))
	@echo "✅ 已暂存到 $(REMOTE_HOST):$(REMOTE_BIN).new.$(EXPECTED_SHA256)"

deploy-restart: ## 重启远程服务
	@test -n "$(EXPECTED_SHA256)" || { echo "错误: 无法计算部署产物 SHA256，请先运行 make deploy-build"; exit 1; }
	DEPLOY_DRY_RUN=$(call shell_quote,$(DEPLOY_DRY_RUN)) sh $(call shell_quote,$(DEPLOY_RESTART_SCRIPT)) $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_SERVICE)) $(call shell_quote,$(REMOTE_BIN)) $(call shell_quote,$(REMOTE_HEALTH_URL)) $(call shell_quote,$(REMOTE_HEALTH_ATTEMPTS)) $(call shell_quote,$(REMOTE_HEALTH_INTERVAL)) $(call shell_quote,$(EXPECTED_SHA256))
	@echo "✅ 服务已重启"

deploy: ## build + upload + restart（强制串行）
	$(MAKE) deploy-build
	$(MAKE) deploy-upload
	$(MAKE) deploy-restart

deploy-pi: ## 安装固定版本 pi-agent，并写入实际路径
	DEPLOY_DRY_RUN=$(call shell_quote,$(DEPLOY_DRY_RUN)) sh $(call shell_quote,$(DEPLOY_PI_SCRIPT)) $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_ENV)) $(call shell_quote,$(PI_NPM_PACKAGE))
	@echo "✅ pi-agent 已安装并同步配置"

deploy-full: ## build + upload + pi-agent + restart（强制串行）
	$(MAKE) deploy-build
	$(MAKE) deploy-pi
	$(MAKE) deploy-upload
	$(MAKE) deploy-restart

# ─── 运维 ────────────────────────────────────────────────────────────
status: ## 查看远程服务状态
	sh $(call shell_quote,$(REMOTE_SERVICE_SCRIPT)) status $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_SERVICE))

logs: ## 查看最近 50 行日志
	sh $(call shell_quote,$(REMOTE_SERVICE_SCRIPT)) logs $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_SERVICE))

logs-follow: ## 实时跟踪日志
	sh $(call shell_quote,$(REMOTE_SERVICE_SCRIPT)) logs-follow $(call shell_quote,$(REMOTE_HOST)) $(call shell_quote,$(REMOTE_SERVICE))

health: ## 检查健康接口
	@curl -fsS $(call shell_quote,$(HEALTH_URL)) | python3 -m json.tool

ssh: ## SSH 到服务器
	ssh $(call shell_quote,$(REMOTE_HOST))

db-shell: ## 进入 PostgreSQL shell
	ssh -t $(call shell_quote,$(REMOTE_HOST)) 'docker exec -it multica-postgres-1 psql -U mota_agent -d mota_agent'
