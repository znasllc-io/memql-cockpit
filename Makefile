# memQL Cockpit Makefile
# Source of truth for all build, test, run, and development commands.
#
# Usage:
#   make              Build memQL Cockpit (host platform, headless)
#   make help         Show all available targets
#   make cockpit-gui  Build GUI variant (CGO + RobotGo)
#   make test         Run all tests

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GO          := go
CGO_ENABLED := 0
BIN_DIR     := bin
# VERSION mirrors the plain semver in the VERSION file, which tracks
# the git tag -- the tag is the source of truth (see VERSIONING.md).
# Prefer the exact tag when building from a clean tagged checkout so
# release artifacts stamp the real version; fall back to the VERSION
# file otherwise.
VERSION     := $(shell t=`git describe --tags --exact-match 2>/dev/null`; if [ -n "$$t" ]; then echo "$$t" | sed 's/^v//'; else cat VERSION 2>/dev/null || echo "dev"; fi)
LDFLAGS     := -X main.version=$(VERSION)
GOFLAGS     := -v -ldflags "$(LDFLAGS)"

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

.PHONY: all cockpit cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64 cockpit-all-platforms cockpit-gui cockpit-gui-darwin-arm64 cockpit-gui-darwin-amd64 cockpit-gui-linux-amd64 cockpit-gui-linux-arm64 cockpit-gui-all-platforms

## Default target -- build memQL Cockpit (host platform, headless)
all: cockpit

## Build memQL Cockpit (host platform, headless)
cockpit:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit ./cmd/memql-cockpit

## Build Cockpit for macOS Apple Silicon
cockpit-darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-arm64 ./cmd/memql-cockpit

## Build Cockpit for macOS Intel
cockpit-darwin-amd64:
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-amd64 ./cmd/memql-cockpit

## Build Cockpit for Linux x86_64
cockpit-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-amd64 ./cmd/memql-cockpit

## Build Cockpit for Linux aarch64
cockpit-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-arm64 ./cmd/memql-cockpit

## Build Cockpit for all supported platforms (headless)
cockpit-all-platforms: cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64

## Build Cockpit GUI variant (CGO + RobotGo). Enables workerComputer.*
## actions in `memql-cockpit worker run`. Requires platform-native
## build tooling: macOS Xcode CLT, Linux gcc + libxtst-dev / libxinerama-dev /
## libxkbcommon-dev / libpng-dev. Default `make cockpit` is the
## headless variant and ships everywhere.
cockpit-gui:
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags gui -o $(BIN_DIR)/memql-cockpit-gui ./cmd/memql-cockpit

cockpit-gui-darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags gui -o $(BIN_DIR)/memql-cockpit-gui-darwin-arm64 ./cmd/memql-cockpit

cockpit-gui-darwin-amd64:
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags gui -o $(BIN_DIR)/memql-cockpit-gui-darwin-amd64 ./cmd/memql-cockpit

cockpit-gui-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags gui -o $(BIN_DIR)/memql-cockpit-gui-linux-amd64 ./cmd/memql-cockpit

cockpit-gui-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags gui -o $(BIN_DIR)/memql-cockpit-gui-linux-arm64 ./cmd/memql-cockpit

## Build Cockpit GUI variant for all supported platforms
cockpit-gui-all-platforms: cockpit-gui-darwin-arm64 cockpit-gui-darwin-amd64 cockpit-gui-linux-amd64 cockpit-gui-linux-arm64

# ---------------------------------------------------------------------------
# Distribution / packaging (I17 -- memql#2228)
# ---------------------------------------------------------------------------
#
# `make dist` produces the distributable artifacts operator machines and CI
# runners install: a versioned tar.gz per platform (binary renamed to the
# plain `memql-cockpit` + LICENSE + README) plus a single SHA256SUMS manifest.
# The headless variant ships everywhere (the GUI variant needs CGO + native
# tooling and is built per-host, so it is intentionally not in the dist set).

DIST_DIR := dist
DIST_PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64

.PHONY: dist dist-checksums

## Package versioned release archives (tar.gz) + SHA256 checksums for all
## headless platforms into dist/. Reuses cockpit-all-platforms.
dist: cockpit-all-platforms
	@rm -rf $(DIST_DIR) && mkdir -p $(DIST_DIR)
	@for triple in $(DIST_PLATFORMS); do \
		stage="$(DIST_DIR)/stage-$$triple"; \
		mkdir -p "$$stage"; \
		cp "$(BIN_DIR)/memql-cockpit-$$triple" "$$stage/memql-cockpit"; \
		cp LICENSE README.md "$$stage/" 2>/dev/null || true; \
		tar -czf "$(DIST_DIR)/memql-cockpit-$(VERSION)-$$triple.tar.gz" -C "$$stage" .; \
		rm -rf "$$stage"; \
		echo "  packaged $(DIST_DIR)/memql-cockpit-$(VERSION)-$$triple.tar.gz"; \
	done
	@$(MAKE) --no-print-directory dist-checksums

## Write a SHA256SUMS manifest over the packaged archives in dist/.
dist-checksums:
	@cd $(DIST_DIR) && { command -v sha256sum >/dev/null 2>&1 && sha256sum *.tar.gz || shasum -a 256 *.tar.gz; } \
		> memql-cockpit-$(VERSION)-SHA256SUMS \
		&& echo "  wrote $(DIST_DIR)/memql-cockpit-$(VERSION)-SHA256SUMS"

# ---------------------------------------------------------------------------
# Run targets
# ---------------------------------------------------------------------------

.PHONY: run-cockpit cockpit-gui-run

# Credential store for the make-launched `*-run` targets. A cockpit
# started via `make` is ALWAYS a local dev run -- the shipped binary is
# run directly, not through make -- and locally-built binaries aren't
# stably code-signed, so the macOS Keychain re-prompts for every saved
# cluster token on each rebuild. Default these dev launches to the file
# backend (tokens in ~/.memql/, mode 0600) so there are no prompts.
# Override to exercise the Keychain path:
#   make cockpit-gui-run CRED_STORE=keyring
# The shipped binary's own default is unchanged (auto-probe -> Keychain
# on macOS); this only affects make-launched runs.
CRED_STORE ?= file

## Build and run memQL Cockpit. Reads ~/.memql/clusters.yaml for the
## active cluster + credential (PAT or cached OIDC token). Run
## `memql-cockpit authorize <url>` once to authorize against an
## identity service if you haven't already. Uses the file credential
## store by default (CRED_STORE=keyring to use the macOS Keychain).
run-cockpit: cockpit
	MEMQL_COCKPIT_CRED_STORE=$(CRED_STORE) ./$(BIN_DIR)/memql-cockpit

## Build and run the GUI variant (CGO + RobotGo). Same TUI as
## `run-cockpit` plus the workerComputer.* capability for
## `memql-cockpit worker run`; runs bin/memql-cockpit-gui, NOT
## bin/memql-cockpit. See `make cockpit-gui` for build prerequisites.
## Uses the file credential store by default (CRED_STORE=keyring for
## the macOS Keychain).
cockpit-gui-run: cockpit-gui
	MEMQL_COCKPIT_CRED_STORE=$(CRED_STORE) ./$(BIN_DIR)/memql-cockpit-gui

# ---------------------------------------------------------------------------
# Test targets
# ---------------------------------------------------------------------------

.PHONY: test test-v test-cover

## Run all tests
test:
	$(GO) test ./...

## Run all tests with verbose output
test-v:
	$(GO) test -v ./...

## Run tests with coverage report
test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------

.PHONY: vet fmt lint tidy generate

## Run go vet on all packages
vet:
	$(GO) vet ./...

## Format all Go files
fmt:
	$(GO) fmt ./...

## Run vet + fmt (quick lint)
lint: fmt vet

## Tidy go.mod dependencies
tidy:
	$(GO) mod tidy

## Run code generation (protobuf, etc.)
generate:
	$(GO) generate ./...

# ---------------------------------------------------------------------------
# Utility targets
# ---------------------------------------------------------------------------

.PHONY: clean version help

## Remove build artifacts
clean:
	rm -rf $(BIN_DIR)/ $(DIST_DIR)/ coverage.out

## Print the current version. Resolves to the exact git tag when the
## checkout sits on one (the source of truth, see VERSIONING.md),
## else the VERSION file.
version:
	@echo $(VERSION)

## Show all available targets
help:
	@echo "memQL Cockpit Makefile -- v$(VERSION)"
	@echo ""
	@echo "BUILD"
	@echo "  make                                Build memQL Cockpit (host platform, headless)"
	@echo "  make cockpit                        Build memQL Cockpit (host platform, headless)"
	@echo "  make cockpit-darwin-arm64           Cross-build for macOS Apple Silicon"
	@echo "  make cockpit-darwin-amd64           Cross-build for macOS Intel"
	@echo "  make cockpit-linux-amd64            Cross-build for Linux x86_64"
	@echo "  make cockpit-linux-arm64            Cross-build for Linux aarch64"
	@echo "  make cockpit-all-platforms          Cross-build for all platforms (headless)"
	@echo "  make cockpit-gui                    Build GUI variant (CGO + RobotGo)"
	@echo "  make cockpit-gui-darwin-arm64       GUI cross-build for macOS Apple Silicon"
	@echo "  make cockpit-gui-darwin-amd64       GUI cross-build for macOS Intel"
	@echo "  make cockpit-gui-linux-amd64        GUI cross-build for Linux x86_64"
	@echo "  make cockpit-gui-linux-arm64        GUI cross-build for Linux aarch64"
	@echo "  make cockpit-gui-all-platforms      GUI cross-build for all platforms"
	@echo ""
	@echo "DISTRIBUTION"
	@echo "  make dist                           Package versioned tar.gz archives + SHA256SUMS into dist/"
	@echo ""
	@echo "RUN"
	@echo "  make run-cockpit      Build and run memQL Cockpit (file cred store; no Keychain prompts)"
	@echo "  make cockpit-gui-run  Build and run the GUI variant (computer-use worker)"
	@echo "                        (both: CRED_STORE=keyring to use the macOS Keychain)"
	@echo ""
	@echo "TEST"
	@echo "  make test         Run all tests"
	@echo "  make test-v       Run all tests (verbose)"
	@echo "  make test-cover   Run tests with coverage report"
	@echo ""
	@echo "QUALITY"
	@echo "  make vet          Run go vet"
	@echo "  make fmt          Format all Go files"
	@echo "  make lint         Run fmt + vet"
	@echo "  make tidy         Tidy go.mod dependencies"
	@echo "  make generate     Run code generation (protobuf)"
	@echo ""
	@echo "UTILITY"
	@echo "  make clean         Remove build artifacts"
	@echo "  make version       Print current version (git tag, else VERSION file)"
	@echo "  make help          Show this help"
