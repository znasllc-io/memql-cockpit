# memQL Cockpit Makefile
# Source of truth for all build, test, run, and development commands.
# Run `make help` for the full, auto-generated list.
#
# Two build variants:
#   headless      (default)  -- ships everywhere; shell/fs/http worker tools
#   computeruse   (-tags computeruse, CGO)  -- adds the workerComputer.* surface
#                                              (screenshot/mouse/keyboard/window)
# "computeruse" is the computer-use control surface, NOT a graphical UI.

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GO          := go
CGO_ENABLED := 0
BIN_DIR     := bin
# VERSION mirrors the plain semver in the VERSION file, which tracks the git
# tag -- the tag is the source of truth (see VERSIONING.md). Prefer the exact
# tag on a clean tagged checkout so release artifacts stamp the real version.
VERSION     := $(shell t=`git describe --tags --exact-match 2>/dev/null`; if [ -n "$$t" ]; then echo "$$t" | sed 's/^v//'; else cat VERSION 2>/dev/null || echo "dev"; fi)
LDFLAGS     := -X main.version=$(VERSION)
GOFLAGS     := -v -ldflags "$(LDFLAGS)"

# Run-target configuration.
MEMQL_NS  ?= memql
BFF_SVC   ?= bff
BFF_PORT  ?= 50051
# Credential store for make-launched runs. A cockpit started via `make` is
# always a local dev run of a locally-built (unsigned) binary, so the macOS
# Keychain re-prompts for every saved token on each rebuild. Default these runs
# to the file backend (tokens in ~/.memql/, mode 0600) to avoid the prompts;
# override with CRED_STORE=keyring. The shipped binary's own default is
# unchanged (auto-probe -> Keychain on macOS).
CRED_STORE ?= file

# ---------------------------------------------------------------------------
##@ Build
# ---------------------------------------------------------------------------

.PHONY: all cockpit cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64 cockpit-all-platforms
.PHONY: cockpit-computeruse cockpit-computeruse-darwin-arm64 cockpit-computeruse-darwin-amd64 cockpit-computeruse-linux-amd64 cockpit-computeruse-linux-arm64 cockpit-computeruse-all-platforms

all: cockpit  ## Build memQL Cockpit (host platform, headless) -- default

cockpit: ## Build memQL Cockpit (host platform, headless)
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit ./cmd/memql-cockpit

cockpit-darwin-arm64: ## Cross-build headless for macOS Apple Silicon
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-arm64 ./cmd/memql-cockpit

cockpit-darwin-amd64: ## Cross-build headless for macOS Intel
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-amd64 ./cmd/memql-cockpit

cockpit-linux-amd64: ## Cross-build headless for Linux x86_64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-amd64 ./cmd/memql-cockpit

cockpit-linux-arm64: ## Cross-build headless for Linux aarch64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-arm64 ./cmd/memql-cockpit

cockpit-all-platforms: cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64  ## Cross-build headless for all platforms

# The computeruse variant adds the workerComputer.* control surface (CGO +
# RobotGo). Requires native tooling: macOS Xcode CLT; Linux gcc + libxtst-dev /
# libxinerama-dev / libxkbcommon-dev / libpng-dev. Built per-host (not in dist).
cockpit-computeruse: ## Build the computer-use variant (CGO + RobotGo; adds workerComputer.*)
	CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags computeruse -o $(BIN_DIR)/memql-cockpit-computeruse ./cmd/memql-cockpit

cockpit-computeruse-darwin-arm64: ## Computer-use cross-build for macOS Apple Silicon
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags computeruse -o $(BIN_DIR)/memql-cockpit-computeruse-darwin-arm64 ./cmd/memql-cockpit

cockpit-computeruse-darwin-amd64: ## Computer-use cross-build for macOS Intel
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags computeruse -o $(BIN_DIR)/memql-cockpit-computeruse-darwin-amd64 ./cmd/memql-cockpit

cockpit-computeruse-linux-amd64: ## Computer-use cross-build for Linux x86_64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags computeruse -o $(BIN_DIR)/memql-cockpit-computeruse-linux-amd64 ./cmd/memql-cockpit

cockpit-computeruse-linux-arm64: ## Computer-use cross-build for Linux aarch64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 $(GO) build $(GOFLAGS) -tags computeruse -o $(BIN_DIR)/memql-cockpit-computeruse-linux-arm64 ./cmd/memql-cockpit

cockpit-computeruse-all-platforms: cockpit-computeruse-darwin-arm64 cockpit-computeruse-darwin-amd64 cockpit-computeruse-linux-amd64 cockpit-computeruse-linux-arm64  ## Computer-use cross-build for all platforms

# ---------------------------------------------------------------------------
##@ Run
# ---------------------------------------------------------------------------
# The RUN family, distinct by WHERE/HOW they connect:
#   run             -> your active ~/.memql/clusters.yaml cluster (or --endpoint)
#   run-local       -> a LOCAL k3d cluster, auto port-forwarding svc/bff
#   run-computeruse -> like `run`, but the computer-use variant
#   forward         -> port-forward svc/bff only (no launch; for SDKs/other clients)
# Extra flags reach the binary via ARGS=..., e.g. `make run ARGS="--cluster staging"`.

.PHONY: run run-local run-computeruse forward

run: cockpit ## Build + run headless Cockpit against the active clusters.yaml cluster
	MEMQL_COCKPIT_CRED_STORE=$(CRED_STORE) ./$(BIN_DIR)/memql-cockpit $(ARGS)

run-local: cockpit ## Build + run against a LOCAL k3d cluster (auto port-forwards svc/bff; guards non-k3d contexts)
	@MEMQL_NS=$(MEMQL_NS) BFF_SVC=$(BFF_SVC) BFF_PORT=$(BFF_PORT) \
		MEMQL_COCKPIT_CRED_STORE=$(CRED_STORE) bash scripts/run-local.sh $(ARGS)

run-computeruse: cockpit-computeruse ## Build + run the computer-use variant against the active clusters.yaml cluster
	MEMQL_COCKPIT_CRED_STORE=$(CRED_STORE) ./$(BIN_DIR)/memql-cockpit-computeruse $(ARGS)

forward: ## Port-forward svc/bff to localhost:$(BFF_PORT) only (no launch; ctrl-c to stop)
	@echo "port-forward svc/$(BFF_SVC) ($(MEMQL_NS)) -> localhost:$(BFF_PORT) (ctrl-c to stop)"
	kubectl port-forward -n $(MEMQL_NS) svc/$(BFF_SVC) $(BFF_PORT):$(BFF_PORT)

# ---------------------------------------------------------------------------
##@ Distribution
# ---------------------------------------------------------------------------
# `make dist` produces the distributable artifacts operators + CI install: a
# versioned tar.gz per platform (binary renamed to plain `memql-cockpit` +
# LICENSE + README) plus a SHA256SUMS manifest. Headless only -- the computeruse
# variant needs CGO + native tooling and is built per-host, so it is not shipped.

DIST_DIR := dist
DIST_PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64

.PHONY: dist dist-checksums

dist: cockpit-all-platforms ## Package versioned tar.gz archives + SHA256SUMS into dist/ (headless, all platforms)
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

dist-checksums: ## Write a SHA256SUMS manifest over the packaged archives in dist/
	@cd $(DIST_DIR) && { command -v sha256sum >/dev/null 2>&1 && sha256sum *.tar.gz || shasum -a 256 *.tar.gz; } \
		> memql-cockpit-$(VERSION)-SHA256SUMS \
		&& echo "  wrote $(DIST_DIR)/memql-cockpit-$(VERSION)-SHA256SUMS"

# ---------------------------------------------------------------------------
##@ Test
# ---------------------------------------------------------------------------

.PHONY: test test-v test-cover

test: ## Run all tests
	$(GO) test ./...

test-v: ## Run all tests (verbose)
	$(GO) test -v ./...

test-cover: ## Run tests with a coverage summary
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out
	@rm -f coverage.out

# ---------------------------------------------------------------------------
##@ Quality
# ---------------------------------------------------------------------------

.PHONY: vet fmt lint tidy generate

vet: ## Run go vet on all packages
	$(GO) vet ./...

fmt: ## Format all Go files
	$(GO) fmt ./...

lint: fmt vet ## Run fmt + vet (quick lint)

tidy: ## Tidy go.mod dependencies
	$(GO) mod tidy

generate: ## Run code generation (protobuf, etc.)
	$(GO) generate ./...

# ---------------------------------------------------------------------------
##@ Utility
# ---------------------------------------------------------------------------

.PHONY: clean version help

clean: ## Remove build artifacts (bin/, dist/, coverage)
	rm -rf $(BIN_DIR)/ $(DIST_DIR)/ coverage.out

version: ## Print the current version (git tag, else VERSION file)
	@echo $(VERSION)

help: ## Show this help (auto-generated from target comments)
	@echo "memQL Cockpit -- v$(VERSION)"
	@echo "Usage: make <target> [ARGS=...] [CRED_STORE=file|keyring]"
	@awk 'BEGIN {FS = ":.*?## "} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next} \
		/^[a-zA-Z0-9_%-]+:.*?## / {printf "  \033[36m%-34s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
