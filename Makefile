.PHONY: build dev test test-race install-golangci-lint lint check

BIN ?= tgfile
GO ?= go
GOBIN ?= $(CURDIR)/bin
GOCACHE ?= $(CURDIR)/.cache/go-build
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.cache/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint

build:
	GOCACHE=$(GOCACHE) $(GO) build -o $(BIN) ./cmd

dev:
	TGFILE_DEV_GO="$(GO)" TGFILE_DEV_CONFIG="$(CONFIG)" ./scripts/dev.sh

test:
	GOCACHE=$(GOCACHE) $(GO) test ./...

test-race:
	GOCACHE=$(GOCACHE) $(GO) test -race ./...

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
