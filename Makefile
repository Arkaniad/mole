BIN     := bin/mole
MCP     := bin/mole-mcp
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build install test race vet lint cover clean tidy check seed release-check

all: check build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/mole
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(MCP) ./cmd/mole-mcp
	@echo "built $(BIN) and $(MCP) ($(VERSION))"

# Install into GOBIN (or ~/go/bin), which is the from-source path the README
# documents. Uses the same ldflags as a release build, so `mole version` reports
# the tag rather than "dev".
install:
	CGO_ENABLED=0 go install -ldflags '$(LDFLAGS)' ./cmd/mole ./cmd/mole-mcp
	@echo "installed mole and mole-mcp ($(VERSION))"

# Dry-run the release pipeline locally: builds every platform, packages, and
# writes to dist/ without publishing anything.
release-check:
	goreleaser release --snapshot --clean --skip=publish

test:
	go test $(PKG)

# The concurrency guarantees in internal/budget are the point of M0; they are
# only meaningfully tested under -race.
race:
	go test -race -count=2 $(PKG)

vet:
	go vet $(PKG)

cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

check: vet race

tidy:
	go mod tidy

# Write a synthetic session so `mole trace` has something to show before M1.
seed: build
	./$(BIN) dev seed

clean:
	rm -rf bin coverage.out
