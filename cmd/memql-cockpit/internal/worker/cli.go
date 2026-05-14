package worker

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/visionarys-io/memql-cockpit/cli/config"
	"github.com/visionarys-io/memql-cockpit/cmd/memql-cockpit/internal/worker/tools"
)

// HandleCommand dispatches `memql-cockpit worker <subcommand>`.
// Subcommands:
//
//	memql-cockpit worker pair <code>       Redeem a pairing code, write yaml, run worker
//	memql-cockpit worker                   Open the pairing wizard (paste a code)
//	memql-cockpit worker run [flags]       Run the worker (assumes worker.yaml already written)
//	memql-cockpit worker setup             Re-run TCC permissions check (gui builds)
//	memql-cockpit worker config            Print effective config
//
// `pair` is the primary entry: it walks the user from "I have an
// XXXX-XXXX code from CoPresent" through redemption, TCC, yaml
// write-out, and into the running worker -- one command. `run`
// stays as the headless / scripted path for users who want to
// reinvoke an already-configured worker (LaunchAgent uses this).
func HandleCommand(args []string) {
	if len(args) == 0 {
		// No-args entry: open the wizard with a paste field. On
		// non-TTY callers this falls back to a clear usage hint.
		handlePair(nil)
		return
	}
	switch args[0] {
	case "pair":
		handlePair(args[1:])
	case "run":
		handleRun(args[1:])
	case "setup":
		handleSetup(args[1:])
	case "config":
		handleConfig(args[1:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown worker subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

// handlePair is the front-door wizard entry. The cockpit MUST
// have an authorized cluster before pair runs -- the identity URL
// is read from ~/.memql/clusters.yaml's selected cluster, NOT
// from a flag or interactive prompt. Run `memql-cockpit authorize
// <url>` first.
//
// With a code argument, the wizard skips the paste step. With none,
// it opens to a paste prompt. The advanced --identity / --token
// flags stay as escape hatches for power users who want to pair
// against an explicit identity URL or who already hold a worker
// token.
func handlePair(args []string) {
	fs := flag.NewFlagSet("worker pair", flag.ExitOnError)
	clusterName := fs.String("cluster", "", "cluster NAME from ~/.memql/clusters.yaml (defaults to the active selection)")
	overrideIdentity := fs.String("identity", "", "advanced: override identity service URL (skips active-cluster lookup)")
	token := fs.String("token", "", "advanced: worker token (skip redeem; assumes pre-existing token)")
	logLevel := fs.String("log-level", "info", "log level")
	_ = fs.Parse(args)

	var code string
	if fs.NArg() > 0 {
		code = fs.Arg(0)
	}

	logger := newLogger(*logLevel)

	identityURL := strings.TrimSpace(*overrideIdentity)
	if identityURL == "" {
		resolved, err := resolveActiveClusterIdentityURL(*clusterName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n\n", err)
			fmt.Fprintln(os.Stderr, "Run `memql-cockpit authorize <url>` first to register + log into a cluster.")
			fmt.Fprintln(os.Stderr, "Or supply --identity <url> + --token <mql_wkr_...> to pair against an explicit identity service.")
			os.Exit(1)
		}
		identityURL = resolved
	}

	opts := PairOptions{
		PairingCode: code,
		IdentityURL: identityURL,
		Token:       *token,
		Logger:      logger,
	}
	if err := RunPairWizard(opts); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

// resolveActiveClusterIdentityURL reads ~/.memql/clusters.yaml and
// returns the IDENTITY service URL (the OIDC issuer) of the named
// cluster (or the SelectedCluster when name is empty). When only
// one cluster is registered, it's treated as active even without an
// explicit SelectedCluster stamp.
//
// Returns the identity URL because pairing redeems are HTTP calls
// to the identity service; the gRPC endpoint is for the worker's
// later WorkerService.Stream connection (returned by the redeem
// reply itself).
func resolveActiveClusterIdentityURL(name string) (string, error) {
	clusters, err := config.LoadClusters()
	if err != nil {
		return "", err
	}
	resolvedName := strings.TrimSpace(name)
	if resolvedName == "" {
		resolvedName = strings.TrimSpace(clusters.SelectedCluster)
	}
	if resolvedName == "" && len(clusters.Clusters) == 1 {
		resolvedName = clusters.Clusters[0].Name
	}
	if resolvedName == "" {
		return "", fmt.Errorf("no active cluster found in ~/.memql/clusters.yaml")
	}
	for _, c := range clusters.Clusters {
		if c.Name == resolvedName {
			issuer := strings.TrimSpace(c.Issuer)
			if issuer == "" {
				return "", fmt.Errorf("cluster %q has no identity issuer configured (re-run `memql-cockpit authorize <url>`)", c.Name)
			}
			return issuer, nil
		}
	}
	return "", fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml", resolvedName)
}

func handleRun(args []string) {
	fs := flag.NewFlagSet("worker run", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml")
	cluster := fs.String("cluster", "", "cluster URL (overrides config)")
	token := fs.String("token", "", "worker token (overrides config)")
	name := fs.String("name", "", "worker name (overrides config)")
	logLevel := fs.String("log-level", "", "log level: debug | info | warn | error")
	metricsPort := fs.Int("metrics-port", 9100, "loopback port for the prometheus metrics endpoint (0 disables)")
	fs.Parse(args)

	cfg, err := LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if *cluster != "" {
		cfg.ClusterURL = *cluster
	}
	if *token != "" {
		cfg.Token = *token
	} else if env := os.Getenv("MEMQL_WORKER_TOKEN"); env != "" && cfg.Token == "" {
		cfg.Token = env
	}
	if *name != "" {
		cfg.Name = *name
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Configure the worker with one of:")
		fmt.Fprintln(os.Stderr, "  - ~/.memql/worker.yaml")
		fmt.Fprintln(os.Stderr, "  - --cluster <url> --token mql_wkr_...")
		fmt.Fprintln(os.Stderr, "  - MEMQL_WORKER_TOKEN env var")
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)

	policy, err := tools.LoadPolicy(filepath.Join(filepath.Dir(*configPath), "policy.yaml"))
	if err != nil {
		logger.Warn("policy load failed; using defaults", "error", err)
		policy = tools.DefaultPolicy()
	}
	dispatcher := tools.NewDispatcher(logger, policy)

	var metrics *Metrics
	if *metricsPort > 0 {
		metrics = NewMetrics()
		if err := metrics.Listen(*metricsPort); err != nil {
			logger.Warn("metrics endpoint failed to start; continuing without metrics", "error", err)
			metrics = nil
		} else {
			logger.Info("metrics endpoint listening", "addr", metrics.ListenAddr())
		}
	}

	runner, err := NewRunner(Options{
		Logger:  logger,
		Config:  cfg,
		Tools:   dispatcher,
		Metrics: metrics,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				if err := policy.Reload(); err != nil {
					logger.Warn("policy reload failed", "error", err)
				} else {
					logger.Info("policy reloaded")
				}
			default:
				logger.Info("worker shutting down", "signal", sig.String())
				cancel()
				return
			}
		}
	}()

	// Enable Ctrl+Q (and keep Ctrl+C via signals) when stdin is
	// a TTY. Restored on exit so the user's shell goes back to
	// canonical mode.
	restoreTermios := enableQuitHotkeys(ctx, cancel, logger)
	defer restoreTermios()

	logger.Info("worker starting",
		"cluster_url", cfg.ClusterURL,
		"name", cfg.Name,
		"capabilities", cfg.Capabilities,
	)
	if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("worker exited with error", "error", err)
		if metrics != nil {
			metrics.Stop()
		}
		os.Exit(1)
	}
	if metrics != nil {
		metrics.Stop()
	}
}

func handleSetup(_ []string) {
	if err := runSetupWizard(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func handleConfig(args []string) {
	fs := flag.NewFlagSet("worker config", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml")
	fs.Parse(args)
	cfg, err := LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Cluster URL: %s\n", emptyOrValue(cfg.ClusterURL))
	fmt.Printf("Name:        %s\n", emptyOrValue(cfg.Name))
	fmt.Printf("Token:       %s\n", maskToken(cfg.Token))
	fmt.Printf("Capabilities:%s\n", " "+strings.Join(cfg.Capabilities, ", "))
	fmt.Printf("Concurrency: %v\n", cfg.Concurrency)
	fmt.Printf("State dir:   %s\n", cfg.StateDir)
	fmt.Printf("Log level:   %s\n", cfg.LogLevel)
	if err := cfg.Validate(); err != nil {
		fmt.Printf("\nValidation: ERROR -- %v\n", err)
	} else {
		fmt.Printf("\nValidation: OK\n")
	}
}

func printUsage() {
	fmt.Println("memql-cockpit worker -- run cockpit as a memql worker")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql-cockpit worker pair <code>   Redeem an XXXX-XXXX pairing code from")
	fmt.Println("                                     CoPresent's Settings -> Computer Use card,")
	fmt.Println("                                     write worker.yaml, run worker. The primary")
	fmt.Println("                                     enrollment path -- one command.")
	fmt.Println("  memql-cockpit worker               Same flow with a paste prompt (no code arg).")
	fmt.Println("  memql-cockpit worker run           Run an already-configured worker (used by")
	fmt.Println("                                     LaunchAgent / scripts).")
	fmt.Println("  memql-cockpit worker setup         Re-run TCC permissions check (GUI builds only).")
	fmt.Println("  memql-cockpit worker config        Print the effective config.")
	fmt.Println("")
	fmt.Println("PAIR FLAGS")
	fmt.Println("  --cluster <url>      Advanced: cluster URL (skip redeem; useful when the")
	fmt.Println("                       cockpit already has a worker token from elsewhere).")
	fmt.Println("  --token <token>      Advanced: worker token (skip redeem).")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("")
	fmt.Println("RUN FLAGS")
	fmt.Println("  --config <path>      Path to worker.yaml (default ~/.memql/worker.yaml)")
	fmt.Println("  --cluster <url>      Cluster URL (overrides config)")
	fmt.Println("  --token <token>      Worker token (mql_wkr_...; overrides config + MEMQL_WORKER_TOKEN)")
	fmt.Println("  --name <name>        Worker name (overrides config)")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("  --metrics-port <p>   Loopback port for prometheus metrics (default 9100; 0 disables)")
}

func newLogger(level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func emptyOrValue(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func maskToken(t string) string {
	if t == "" {
		return "(unset)"
	}
	if len(t) <= 16 {
		return "(set)"
	}
	return t[:12] + "..." + t[len(t)-4:]
}
