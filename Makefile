GO        ?= go
NPM       ?= npm
BIN_DIR   ?= bin
BINARY    ?= $(BIN_DIR)/redmine-agent
WEB_DIR   ?= web
GO_PKG    ?= ./cmd/redmine-agent
GO_PKGS   ?= ./cmd/... ./internal/... ./tests/...

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT_AT  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT_AT)

# Windows PowerShell equivalents (no GNU make required):
#   $env:CGO_ENABLED=0; go build -trimpath -ldflags "-s -w -X main.version=dev" -o bin\redmine-agent.exe .\cmd\redmine-agent
#   pushd web; npm ci; npm run build; popd
#   go test ./cmd/... ./internal/... ./tests/... -count=1
#   golangci-lint run ./...
#
# Go package patterns are explicit rather than ./... because web/node_modules can
# contain third-party Go files that must never be built or tested.

.PHONY: all build build-web build-all test test-race lint lint-web fmt fmt-check run clean

all: build

build: build-web
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(GO_PKG)

build-web:
	cd $(WEB_DIR) && $(NPM) ci --no-audit --no-fund && $(NPM) run build

build-all: build-web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/redmine-agent-linux-amd64 $(GO_PKG)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/redmine-agent-linux-arm64 $(GO_PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/redmine-agent-windows-amd64.exe $(GO_PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/redmine-agent-windows-arm64.exe $(GO_PKG)

test:
	$(GO) test $(GO_PKGS) -count=1

test-race:
	$(GO) test -race $(GO_PKGS) -count=1

lint:
	golangci-lint run $(GO_PKGS)

lint-web:
	cd $(WEB_DIR) && $(NPM) run check && npx eslint .

fmt:
	$(GO) fmt $(GO_PKGS)
	cd $(WEB_DIR) && npx prettier --write .

fmt-check:
	gofmt -l cmd internal tests

run:
	$(GO) run $(GO_PKG) serve --config config.yaml

clean:
	$(GO) clean
	rm -rf $(BIN_DIR) $(WEB_DIR)/.svelte-kit $(WEB_DIR)/build
