SHELL := /bin/bash

GO ?= go
BINARY ?= codexlb
CMD ?= ./cmd/codexlb
PREFIX ?= $(HOME)/.g/go
BINDIR ?= $(PREFIX)/bin
DESTDIR ?=

ROOT ?= $(HOME)/.codex-lb
BINARY_WORKDIR ?= $(PWD)
LISTEN ?= 127.0.0.1:8765
UPSTREAM ?= https://chatgpt.com/backend-api

.PHONY: help build install test test-real fmt clean run-proxy status install-daemons uninstall-daemons install-systemd install-launchd uninstall-systemd uninstall-launchd

help:
	@echo "Targets:"
	@echo "  make build              Build $(BINARY)"
	@echo "  make install            Build and install $(BINARY) to $(DESTDIR)$(BINDIR)"
	@echo "  make test               Run unit/integration/e2e tests (fake codex)"
	@echo "  make test-real          Run tests including real codex endpoint override check"
	@echo "  make fmt                Format Go code"
	@echo "  make clean              Remove build artifacts"
	@echo "  make run-proxy          Build and run local proxy"
	@echo "  make status             Query running proxy status table"
	@echo "  make install-daemons    Install daemon for current OS (systemd user or launchd)"
	@echo "  make uninstall-daemons  Uninstall daemon for current OS"
	@echo "  make install-systemd    Force install systemd --user unit"
	@echo "  make install-launchd    Force install macOS launchd agent"
	@echo ""
	@echo "Config vars: ROOT=$(ROOT) BINARY_WORKDIR=$(BINARY_WORKDIR) LISTEN=$(LISTEN) UPSTREAM=$(UPSTREAM) PREFIX=$(PREFIX) BINDIR=$(BINDIR) DESTDIR=$(DESTDIR)"

build:
	$(GO) build -o $(BINARY) $(CMD)

install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 "$(BINARY)" "$(DESTDIR)$(BINDIR)/$(BINARY)"
	@echo "Installed $(BINARY) -> $(DESTDIR)$(BINDIR)/$(BINARY)"

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

status: build
	./$(BINARY) status --root "$(ROOT)" --proxy-url "http://$(LISTEN)"

install-daemons: build
	./scripts/install-daemon.sh --binary "$(BINARY_WORKDIR)/$(BINARY)" --root "$(ROOT)"

uninstall-daemons:
	./scripts/uninstall-daemon.sh

install-systemd: build
	./scripts/install-daemon.sh --target systemd --binary "$(BINARY_WORKDIR)/$(BINARY)" --root "$(ROOT)"

install-launchd: build
	./scripts/install-daemon.sh --target launchd --binary "$(BINARY_WORKDIR)/$(BINARY)" --root "$(ROOT)"

uninstall-systemd:
	./scripts/uninstall-daemon.sh --target systemd

uninstall-launchd:
	./scripts/uninstall-daemon.sh --target launchd
