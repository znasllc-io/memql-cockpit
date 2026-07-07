// Carrier binary for the memQL Cockpit's own BFF (cockpit-bff).
//
// Part of the per-client BFF model (znasllc-io/memql-cockpit#288): the
// engine stays client-agnostic and every client owns its own
// backend-for-frontend edge. The CoPresent SPA has its BFF
// (memql-bff-copresent); the Cockpit gets THIS one -- a product-neutral
// node the Cockpit connects to at cockpit.<domain>.
//
// It wraps memql's shared app.Run lifecycle and, unlike a product
// carrier, blank-imports NO product DSL tree -- it is the pure engine
// BFF role (BUILD_TAGS=bff, the engine default). The whole point is
// product neutrality: no `copresent`/product code reaches this binary,
// mirroring the engine's own bff node. If the Cockpit ever needs
// cockpit-specific server-side projections, they plug in here through
// the engine's RegisterTree / RegisterPlugin seams -- but today this is
// intentionally a thin passthrough so behaviour tracks the memql repo.
//
// Build tag: bff. The default build target matches memql's bff binary
// (same MEMQL_NODE_TYPE, same env-file convention, same gRPC port), so
// the deploy overlay can run it as the `cockpit-bff` node with no
// environment surprises.
package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/service"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// versionFilePath mirrors memql/main.go's convention -- the binary
// reads a sibling VERSION file when the VERSION env isn't set. The
// docker build copies a VERSION file into the working directory so
// this resolves at runtime.
const versionFilePath = "VERSION"

func main() {
	serviceLogger := mustCreateServiceLogger()

	// Decrypt + apply the genesis envelope in-process at boot, mirroring
	// memql/main.go. Fail closed. No-op when MEMQL_GENESIS_AUTOLOAD is
	// unset, so local dev's env_file path is untouched.
	if res, err := genesis.AutoloadFromEnv(); err != nil {
		serviceLogger.Error("genesis envelope auto-load failed", "err", err)
		os.Exit(1)
	} else if res.Enabled {
		serviceLogger.Info("genesis envelope auto-loaded",
			"source", res.Source,
			"applied", len(res.Applied),
			"skipped", len(res.Skipped))
	}

	// Layer repo-root /.env on top of host-shell + envelope values,
	// mirroring memql/main.go so dev knobs work identically.
	if overridden, err := genesis.ApplyLocalOverride("."); err != nil {
		serviceLogger.Warn("local .env override failed -- continuing with envelope values", "err", err)
	} else if len(overridden) > 0 {
		serviceLogger.Info("local .env override applied", "vars", overridden)
	}

	// Bridge any pre-7.3 legacy env var names onto their new MEMQL_
	// names (set-if-absent, new wins). MUST mirror memql/main.go so a
	// cluster whose envelope still carries legacy names boots this node
	// identically to the engine nodes.
	genesis.ApplyLegacyEnvAliases(serviceLogger)

	app.Run(app.RunConfig{
		Logger:  serviceLogger,
		Version: resolveServiceVersion(),
		Overrides: app.Overrides{
			FatalWithLogger:   logger.FatalWithLogger,
			LoadServiceEnvOpt: service.LoadDefaultServiceEnvOptions,
		},
		SetHealth: func(deps []common.Dependency) {
			server.SetHealthDependencies(deps)
		},
	})
}

// resolveServiceVersion mirrors memql/main.go's helper. Env var wins;
// falls back to a sibling VERSION file; finally returns "dev".
func resolveServiceVersion() string {
	if value := strings.TrimSpace(os.Getenv("VERSION")); value != "" {
		return value
	}
	if data, err := os.ReadFile(versionFilePath); err == nil {
		if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
			return trimmed
		}
	}
	return "dev"
}

// mustCreateServiceLogger mirrors memql/main.go's helper. Writes to
// os.Stdout so container log capture sees startup INFO logs the same
// way it does for memql's bff binary.
func mustCreateServiceLogger() *slog.Logger {
	serviceOpts, err := service.LoadDefaultServiceEnvOptions()
	if err != nil {
		logger.Fatal("failed to load service environment options", "error", err)
	}
	level := slog.LevelInfo
	if strings.TrimSpace(serviceOpts.LoggerLevel) != "" {
		var parsedLevel slog.Level
		if err := parsedLevel.UnmarshalText([]byte(strings.ToLower(serviceOpts.LoggerLevel))); err != nil {
			logger.Fatal("invalid service log level", "error", err)
		}
		level = parsedLevel
	}
	return logger.New(common.ComponentName(serviceOpts.Name), os.Stdout, level)
}
