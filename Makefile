.PHONY: build dev soak test test-race install-golangci-lint lint check

BIN ?= tgfile
GO ?= go
GOBIN ?= $(CURDIR)/bin
GOCACHE ?= $(CURDIR)/.cache/go-build
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.cache/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint
SOAK_DURATION ?= 15m
SOAK_WORKERS ?= 4
SOAK_SEED ?=
SOAK_CLIENT_DELAY ?= 5ms
SOAK_BACKEND_DELAY ?= 5ms

build:
	GOCACHE=$(GOCACHE) $(GO) build -o $(BIN) ./cmd

dev:
	TGFILE_DEV_GO="$(GO)" TGFILE_DEV_CONFIG="$(CONFIG)" ./scripts/dev.sh

soak:
	TGFILE_SOAK_DURATION="$(SOAK_DURATION)" TGFILE_SOAK_WORKERS="$(SOAK_WORKERS)" \
		TGFILE_SOAK_SEED="$(SOAK_SEED)" TGFILE_SOAK_CLIENT_DELAY="$(SOAK_CLIENT_DELAY)" \
		TGFILE_SOAK_BACKEND_DELAY="$(SOAK_BACKEND_DELAY)" GOCACHE="$(GOCACHE)" $(GO) run ./tools/soak

test:
	GOCACHE=$(GOCACHE) $(GO) test -count=1 ./...

test-race:
	GOCACHE=$(GOCACHE) $(GO) test -count=1 -race ./...

install-golangci-lint:
	GOBIN=$(GOBIN) GOCACHE=$(GOCACHE) $(GO) install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	PATH=$(dir $(GO)):$$PATH GOCACHE=$(GOCACHE) GOLANGCI_LINT_CACHE=$(GOLANGCI_LINT_CACHE) \
		$(GOLANGCI_LINT) run --config .golangci.yml ./...

check:
	test -z "$$($(GO)fmt -l .)"
	$(GO) vet ./...
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) lint
