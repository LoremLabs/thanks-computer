.PHONY: all lint test build clean install

VERSION = "0.2.3"
WORKSPACE ?= $(shell pwd)
BUILD_TIMESTAMP = $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GIT_HASH ?= $(shell git rev-parse HEAD)
LDFLAGS = -ldflags "-X main.BuildTimestamp=$(BUILD_TIMESTAMP) -X main.CommitId=$(GIT_HASH) -X main.Version=$(VERSION)"

# Path to the txco command package, relative to the module root.
TXCO_CMD = ../cmd/txco

all: lint test build
benchmark:
	go test ./... -bench=.
clean:
	rm -rf ${WORKSPACE}/bin
fetch-tools:
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
lint:
	golangci-lint run
test:
	go test -v -race -cover ./...
qtest:
	go test -race -cover ./...
cover:
	go test -race -coverprofile="/tmp/go-cover.$$.tmp" ./... && go tool cover -html="/tmp/go-cover.$$.tmp"

# Builds the txco binary locally into chassis/bin/txco. start.sh uses this
# for the in-repo dev loop. For a system-wide install (txco on PATH), use
# `make install` from the repo root.
build:
	mkdir -p ${WORKSPACE}/bin
	go build -o ${WORKSPACE}/bin/txco $(LDFLAGS) ${TXCO_CMD}

# Installs `txco` to $GOBIN (default ~/go/bin). Add that dir to your PATH
# so the binary is callable from anywhere.
install:
	go install $(LDFLAGS) ${TXCO_CMD}
