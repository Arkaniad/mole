BIN     := bin/mole
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race vet lint cover clean tidy check seed

all: check build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/mole
	@echo "built $(BIN) ($(VERSION))"

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
