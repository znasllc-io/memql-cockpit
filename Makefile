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

## Build memQL Cockpit with the voice tag -- enables push-to-talk
## microphone capture via malgo (miniaudio). Requires CGO. Default
## `make cockpit` ships without it so the headless cross-compiles
## stay CGO-free; opt in with this target when you want voice.
cockpit-voice:
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags voice -o $(BIN_DIR)/memql-cockpit-voice ./cmd/memql-cockpit

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
# Run targets
# ---------------------------------------------------------------------------

.PHONY: run-cockpit cockpit-gui-run

## Build and run memQL Cockpit. Reads ~/.memql/clusters.yaml for the
## active cluster + credential (PAT or cached OIDC token). Run
## `memql-cockpit authorize <url>` once to authorize against an
## identity service if you haven't already.
run-cockpit: cockpit
	./$(BIN_DIR)/memql-cockpit

## Build and run the GUI variant (CGO + RobotGo). Same TUI as
## `run-cockpit` plus the workerComputer.* capability for
## `memql-cockpit worker run`; runs bin/memql-cockpit-gui, NOT
## bin/memql-cockpit. See `make cockpit-gui` for build prerequisites.
cockpit-gui-run: cockpit-gui
	./$(BIN_DIR)/memql-cockpit-gui

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
	rm -rf $(BIN_DIR)/ coverage.out

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
	@echo "RUN"
	@echo "  make run-cockpit      Build and run memQL Cockpit (uses ~/.memql/clusters.yaml)"
	@echo "  make cockpit-gui-run  Build and run the GUI variant (computer-use worker)"
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
