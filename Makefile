SHELL := /bin/bash

GO ?= go
BINARY ?= codexlb
CMD ?= ./cmd/codexlb

ROOT ?= $(HOME)/.codex-lb
LISTEN ?= 127.0.0.1:8765
UPSTREAM ?= https://chatgpt.com/backend-api

.PHONY: help build test test-real fmt clean run-proxy install-daemons uninstall-daemons install-systemd install-launchd uninstall-systemd uninstall-launchd

help:
	@echo "Targets:"
	@echo "  make build              Build $(BINARY)"
	@echo "  make test               Run unit/integration/e2e tests (fake codex)"
	@echo "  make test-real          Run tests including real codex endpoint override check"
	@echo "  make fmt                Format Go code"
	@echo "  make clean              Remove build artifacts"
	@echo "  make run-proxy          Build and run local proxy"
	@echo "  make install-daemons    Install daemon for current OS (systemd user or launchd)"
	@echo "  make uninstall-daemons  Uninstall daemon for current OS"
	@echo "  make install-systemd    Force install systemd --user unit"
	@echo "  make install-launchd    Force install macOS launchd agent"
	@echo ""
	@echo "Config vars: ROOT=$(ROOT) LISTEN=$(LISTEN) UPSTREAM=$(UPSTREAM)"

build:
	$(GO) build -o $(BINARY) $(CMD)

test:
	$(GO) test ./...

test-real:
	CODEXLB_RUN_REAL_CODEX_TEST=1 $(GO) test ./...

fmt:
	gofmt -w ./cmd ./internal

clean:
	rm -f $(BINARY)

run-proxy: build
	./$(BINARY) proxy --root "$(ROOT)" --listen "$(LISTEN)" --upstream "$(UPSTREAM)"

install-daemons: build
	./scripts/install-daemon.sh --binary "$(PWD)/$(BINARY)" --root "$(ROOT)" --listen "$(LISTEN)" --upstream "$(UPSTREAM)"

uninstall-daemons:
	./scripts/uninstall-daemon.sh

install-systemd: build
	./scripts/install-daemon.sh --target systemd --binary "$(PWD)/$(BINARY)" --root "$(ROOT)" --listen "$(LISTEN)" --upstream "$(UPSTREAM)"

install-launchd: build
	./scripts/install-daemon.sh --target launchd --binary "$(PWD)/$(BINARY)" --root "$(ROOT)" --listen "$(LISTEN)" --upstream "$(UPSTREAM)"

uninstall-systemd:
	./scripts/uninstall-daemon.sh --target systemd

uninstall-launchd:
	./scripts/uninstall-daemon.sh --target launchd
