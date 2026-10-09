SHELL   := /bin/bash
BIN     := agentshare
PKG     := github.com/adfoke/agentshare

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

# CGO stays off everywhere: the SQLite driver is pure Go, so the result is a
# static binary that runs on any Linux without libc surprises.
#
# It is exported (not just set on the build commands) so that `make test` and
# `make smoke` also run in the shipped configuration. Testing with cgo on and
# shipping with it off would exercise a different SQLite code path than the one
# that ends up on the server.
export CGO_ENABLED = 0

.PHONY: all build build-linux test vet fmt run smoke tidy clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/agentshare
	@bin/$(BIN) --version

build-linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN)-linux-amd64 ./cmd/agentshare
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN)-linux-arm64 ./cmd/agentshare
	@ls -l bin/$(BIN)-linux-*

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

run:
	go run ./cmd/agentshare --data-dir ./data --listen 127.0.0.1:8080

# Against an instance that is already running.
smoke:
	./scripts/smoke.sh "$${BASE_URL:-http://127.0.0.1:8080}"

tidy:
	go mod tidy

clean:
	rm -rf bin
