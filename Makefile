.PHONY: build dev

BIN ?= tgfile
GO ?= go

build:
	$(GO) build -o $(BIN) ./cmd

dev:
	TGFILE_DEV_GO="$(GO)" TGFILE_DEV_CONFIG="$(CONFIG)" ./scripts/dev.sh
