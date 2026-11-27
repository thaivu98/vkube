APP_NAME := vkube
PKG      := ./cmd/vkube
BIN_DIR  := bin
DIST_DIR := dist

VERSION ?= v1.36.7-vkube.1-dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64 \
	windows/arm64

.PHONY: help build release-all test vet fmt clean

help:
	@echo "vkube Makefile targets:"
	@echo "  make build        Build vkube for the current OS/arch ($(BIN_DIR)/$(APP_NAME))"
	@echo "  make release-all  Cross-compile for linux, darwin, windows (amd64 & arm64) into $(DIST_DIR)/"
	@echo "  make test         Run unit tests"
	@echo "  make vet          Run go vet"
	@echo "  make fmt          Format Go source files"
	@echo "  make clean        Remove build artifacts ($(BIN_DIR)/ and $(DIST_DIR)/)"

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP_NAME) $(PKG)

release-all:
	@mkdir -p $(DIST_DIR)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="$(DIST_DIR)/$(APP_NAME)-$$os-$$arch$$ext"; \
		echo "Building $$out ..."; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$out" $(PKG) || exit 1; \
	done

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
