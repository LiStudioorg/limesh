# Limesh Makefile
#
# 常用目标:
#   make build        当前平台构建到 ./mesh
#   make build-main   7 个主流平台构建到 ./dist/
#   make build-all    30+ 平台构建到 ./dist/
#   make run          本地运行一个 node
#   make test         运行全部测试
#   make clean        清理产物

BINARY  := mesh
PKG     := github.com/LiStudioorg/limesh
CMD     := ./cmd/mesh
DIST    := dist

# VERSION 取自 git describe;无 git 仓库时回退为 dev。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo unknown)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildTime=$(BUILT)

GOFLAGS  :=
GO       := go
CGO      := CGO_ENABLED=0

# 7 个主流平台
MAIN_PLATFORMS := \
	linux/amd64   linux/arm64   linux/arm \
	darwin/amd64  darwin/arm64 \
	windows/amd64 \
	freebsd/amd64

# 30+ 平台矩阵
ALL_PLATFORMS := \
	linux/amd64 linux/arm64 linux/386 linux/arm \
	linux/arm/v5 linux/arm/v6 \
	linux/mips linux/mipsle linux/mips64le \
	linux/riscv64 linux/loong64 linux/ppc64le linux/s390x \
	darwin/amd64 darwin/arm64 \
	windows/amd64 windows/arm64 windows/386 \
	freebsd/amd64 freebsd/arm64 \
	openbsd/amd64 openbsd/arm64 \
	netbsd/amd64 netbsd/arm64 \
	dragonfly/amd64 \
	solaris/amd64 \
	illumos/amd64 \
	aix/ppc64

.PHONY: all build build-main build-all run test test-race clean fmt vet tidy help

all: build

## build: 当前平台构建到 ./mesh
build:
	$(CGO) $(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

## build-main: 7 个主流平台构建到 ./dist/
build-main:
	@mkdir -p $(DIST)
	@for p in $(MAIN_PLATFORMS); do \
		$(MAKE) --no-print-directory _build-one PLATFORM=$$p || exit 1; \
	done
	@echo "构建完成 → $(DIST)/"

## build-all: 30+ 平台构建到 ./dist/
build-all:
	@mkdir -p $(DIST)
	@for p in $(ALL_PLATFORMS); do \
		$(MAKE) --no-print-directory _build-one PLATFORM=$$p || exit 1; \
	done
	@echo "构建完成 → $(DIST)/"

# 内部目标:构建单个平台。PLATFORM=os/arch[/goarm|gomips]
_build-one:
	@os=$${PLATFORM%%/*}; rest=$${PLATFORM#*/}; arch=$${rest%%/*}; variant=$${rest#*/}; \
	if [ "$$variant" = "$$rest" ]; then variant=""; fi; \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch}$${variant:+_$$variant}$$ext"; \
	env CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
		$${variant:+GOARM=$${variant#v}} \
		$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$$out" $(CMD) \
		&& echo "  ok  $$out" || { echo "  FAIL $$out"; exit 1; }

## run: 本地运行一个 node
run: build
	./$(BINARY) node -c config.example.toml

## test: 运行全部测试
test:
	$(GO) test ./...

## test-race: 带竞态检测运行测试
test-race:
	$(GO) test -race ./...

## fmt: 格式化
fmt:
	$(GO) fmt ./...

## vet: 静态检查
vet:
	$(GO) vet ./...

## tidy: 整理依赖
tidy:
	$(GO) mod tidy

## clean: 清理产物
clean:
	@rm -rf $(DIST) $(BINARY) $(BINARY).exe
	@echo "已清理 $(DIST)/ 与 $(BINARY)"

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
