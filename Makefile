.PHONY: cockpit cockpit-gui cockpit-all-platforms cockpit-gui-all-platforms \
        cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64 \
        cockpit-gui-darwin-arm64 cockpit-gui-darwin-amd64 cockpit-gui-linux-amd64 cockpit-gui-linux-arm64 \
        run-cockpit clean test vet

GO     ?= go
GOFLAGS ?=
BIN_DIR ?= bin

## Build memQL Cockpit (host platform, headless)
cockpit:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit ./cmd/memql-cockpit

cockpit-darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-arm64 ./cmd/memql-cockpit

cockpit-darwin-amd64:
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-darwin-amd64 ./cmd/memql-cockpit

cockpit-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-amd64 ./cmd/memql-cockpit

cockpit-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN_DIR)/memql-cockpit-linux-arm64 ./cmd/memql-cockpit

cockpit-all-platforms: cockpit-darwin-arm64 cockpit-darwin-amd64 cockpit-linux-amd64 cockpit-linux-arm64

## Build Cockpit GUI variant (CGO + RobotGo). Enables workerComputer.*
## actions in `memql-cockpit worker run`. Requires platform-native
## build tooling: macOS Xcode CLT, Linux gcc + libxtst-dev / libxinerama-dev /
## libxkbcommon-dev / libpng-dev.
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

cockpit-gui-all-platforms: cockpit-gui-darwin-arm64 cockpit-gui-darwin-amd64 cockpit-gui-linux-amd64 cockpit-gui-linux-arm64

run-cockpit: cockpit
	./$(BIN_DIR)/memql-cockpit

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN_DIR)
