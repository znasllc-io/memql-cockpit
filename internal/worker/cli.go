package worker

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/znasllc-io/memql-cockpit/internal/config"
	"github.com/znasllc-io/memql-cockpit/internal/crash"
	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/backup"
	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// HandleCommand dispatches `memql worker <subcommand>`.
// Subcommands:
//
//	memql worker backup            Report (and optionally run) the watched-folder sweep
//	memql worker pair <code>       Redeem a pairing code, upsert a home, run worker
//	memql worker                   Open the pairing wizard (paste a code)
//	memql worker run [flags]       Run all enabled homes from workers.yaml
//	memql worker setup             Re-run TCC permissions check (computeruse builds)
//	memql worker setup --inference Install a model runtime, pull models, allow them
//	memql worker config            Print effective config (all homes)
//	memql worker unpair --cluster  Remove or disable one home
//
// `pair` is the primary entry: it walks the user from "I have an
// XXXX-XXXX code from CoPresent" through redemption, TCC, an additive
// workers.yaml upsert, and into the running worker -- one command. `run`
// stays as the headless / scripted path for users who want to
// reinvoke an already-configured worker (LaunchAgent uses this).
func HandleCommand(args []string) {
	// Wrap the whole dispatch in crash.Catch so any panic in a
	// subcommand (the wizard, the runner loop, tools.Dispatch, etc.)
	// is caught, sanitized for token-shaped strings, written to a
	// 0600 crash log, and surfaced as a friendly message rather than
	// a raw goroutine dump to stderr. Without this a panic
	// mid-`worker run` killed the process with sensitive locals
	// visible on the terminal.
	if rep := crash.Catch("worker:HandleCommand", func() {
		dispatchHandleCommand(args)
	}); rep != nil {
		fmt.Fprint(os.Stderr, crash.UserMessage(rep))
		os.Exit(1)
	}
}

func dispatchHandleCommand(args []string) {
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
	case "unpair":
		handleUnpair(args[1:])
	case "backup":
		handleBackup(args[1:])
	case "models":
		handleModels(args[1:])
	case "hardware":
		handleHardware(args[1:])
	case "probe":
		handleProbe(args[1:])
	case "consent":
		handleConsentCmd(args[1:])
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
// from a flag or interactive prompt. Run `memql cluster add
// <domain>` first.
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
	homeID := fs.String("home-id", "", "home id in workers.yaml (default: clusters.yaml name or URL host)")
	force := fs.Bool("force", false, "remap a home id onto a different cluster_url (not required for same-URL refresh)")
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
			fmt.Fprintln(os.Stderr, "Run `memql cluster add <domain>` first to register + log into a cluster.")
			fmt.Fprintln(os.Stderr, "Or supply --identity <url> + --token <mql_wkr_...> to pair against an explicit identity service.")
			os.Exit(1)
		}
		identityURL = resolved
	}

	opts := PairOptions{
		PairingCode: code,
		IdentityURL: identityURL,
		Token:       *token,
		HomeID:      *homeID,
		Force:       *force,
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
				return "", fmt.Errorf("cluster %q has no identity issuer configured (re-run `memql cluster add <domain>`)", c.Name)
			}
			return issuer, nil
		}
	}
	return "", fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml", resolvedName)
}

func handleRun(args []string) {
	fs := flag.NewFlagSet("worker run", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml (workers.yaml is preferred beside it)")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml multi-home registry")
	cluster := fs.String("cluster", "", "cluster URL (overrides config; forces single-home mode)")
	tokenFile := fs.String("token-file", "", "path to a 0600 file whose contents are the worker token (overrides config)")
	name := fs.String("name", "", "worker name (overrides config)")
	logLevel := fs.String("log-level", "", "log level: debug | info | warn | error")
	metricsPort := fs.Int("metrics-port", 9100, "loopback port for the prometheus metrics endpoint (0 disables)")
	// --token (the literal token on argv) was retained as a hidden
	// alias for backwards-compatibility with legacy LaunchAgent /
	// systemd unit invocations. Tokens on argv leak into `ps`,
	// shell history, and process listings -- we accept the flag but
	// emit a loud WARN every time it's used. Prefer --token-file or
	// MEMQL_WORKER_TOKEN.
	tokenInline := fs.String("token", "", "DEPRECATED: worker token literal. Use --token-file or MEMQL_WORKER_TOKEN to avoid leaking into ps/shell history.")
	fs.Parse(args)

	legacyCfg, err := LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if *cluster != "" {
		legacyCfg.ClusterURL = *cluster
	}
	// Token resolution order (each step overrides the previous):
	//   1. worker.yaml / workers.yaml (loaded below for multi-home).
	//   2. MEMQL_WORKER_TOKEN env var (in-process memory, not argv).
	//   3. --token-file path (read once, 0600 enforced).
	//   4. --token literal (DEPRECATED; logs a WARN).
	cliToken := ""
	if env := os.Getenv("MEMQL_WORKER_TOKEN"); env != "" {
		cliToken = env
	}
	if *tokenFile != "" {
		if err := config.VerifyCredentialFileMode(*tokenFile); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: --token-file: %v\n", err)
			os.Exit(1)
		}
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: --token-file: %v\n", err)
			os.Exit(1)
		}
		cliToken = strings.TrimSpace(string(raw))
	}
	if *tokenInline != "" {
		fmt.Fprintln(os.Stderr, "WARNING: --token <literal> leaks the token to `ps` and shell history.")
		fmt.Fprintln(os.Stderr, "WARNING: Use --token-file or MEMQL_WORKER_TOKEN instead. Continuing for backwards compatibility.")
		cliToken = *tokenInline
	}
	if cliToken != "" {
		legacyCfg.Token = cliToken
	}
	if *name != "" {
		legacyCfg.Name = *name
	}
	if *logLevel != "" {
		legacyCfg.LogLevel = *logLevel
	}

	// Single-home override: any explicit cluster/token on the CLI keeps
	// the legacy one-stream path (scripts, debugging). Otherwise load
	// the multi-home registry and run every enabled home.
	singleHome := *cluster != "" || cliToken != ""
	var workers WorkersFile
	if !singleHome {
		workers, err = LoadWorkers(*workersPath, *configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		if *name != "" {
			workers.WorkerName = *name
		}
		if *logLevel != "" {
			workers.LogLevel = *logLevel
		}
		if err := workers.ValidateRun(); err != nil {
			// Fall back to legacy single-file if it validates — a
			// machine mid-migration with only worker.yaml still runs.
			if err2 := legacyCfg.Validate(); err2 == nil {
				singleHome = true
			} else {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "Configure the worker with one of:")
				fmt.Fprintln(os.Stderr, "  - ~/.memql/workers.yaml (one or more homes)")
				fmt.Fprintln(os.Stderr, "  - ~/.memql/worker.yaml (legacy single home; auto-migrates)")
				fmt.Fprintln(os.Stderr, "  - --cluster <url> --token-file <path>")
				os.Exit(1)
			}
		}
	} else if err := legacyCfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Configure the worker with one of:")
		fmt.Fprintln(os.Stderr, "  - ~/.memql/workers.yaml")
		fmt.Fprintln(os.Stderr, "  - ~/.memql/worker.yaml")
		fmt.Fprintln(os.Stderr, "  - --cluster <url> --token-file <path>")
		fmt.Fprintln(os.Stderr, "  - MEMQL_WORKER_TOKEN env var")
		os.Exit(1)
	}

	logLevelEffective := legacyCfg.LogLevel
	if !singleHome && workers.LogLevel != "" {
		logLevelEffective = workers.LogLevel
	}
	logger := newLogger(logLevelEffective)

	policyDir := filepath.Dir(*configPath)
	if !singleHome {
		policyDir = filepath.Dir(*workersPath)
	}
	policyPath := filepath.Join(policyDir, "policy.yaml")
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		logger.Warn("policy load failed; using defaults", "error", err)
		policy = tools.DefaultPolicy()
	}

	// Consent gate (memql-cockpit#64). ONE socket for the whole
	// supervisor — multi-home does not multiply consent.
	consentMgr := consent.NewManager()
	consentSrv := consent.NewServer(consentMgr, consent.DefaultSocketPath(), logger)
	consentCtx, consentCancel := context.WithCancel(context.Background())
	if err := consentSrv.Listen(consentCtx); err != nil {
		logger.Warn("consent socket failed to start; running with default-deny gate but no IPC control",
			"error", err)
	}
	defer consentCancel()

	dispatcher := tools.NewDispatcher(logger, policy, consentMgr)

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

	discoverer := &models.Discoverer{}
	modelInventory := NewModelInventory(policy, discoverer)
	appInv := NewAppInventory(policy)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	var (
		fleet  *Fleet
		runner *Runner
		// stopSessions is invoked on SIGTERM so MCP configs die with us.
		stopSessions func(string)
	)

	if singleHome {
		sessions := appsession.NewManager(appsession.Options{
			Logger:     logger,
			StateDir:   legacyCfg.StateDir,
			ClusterURL: legacyCfg.ClusterURL,
			Allowed: func(appID string) bool {
				for _, allowed := range policy.AppsAllow() {
					if strings.EqualFold(strings.TrimSpace(allowed), appID) {
						return true
					}
				}
				return false
			},
			CheckWorkspace: policy.CheckPath,
		})
		stopSessions = sessions.StopAll
		calls := modelcall.NewManager(modelcall.Options{
			Logger:    logger,
			Inventory: modelInventory,
		})
		backups := backup.New(backup.Options{
			Logger:     logger,
			StateDir:   legacyCfg.StateDir,
			BaseURL:    backupBaseURL(legacyCfg.ClusterURL),
			Bearer:     backupBearer(legacyCfg.ClusterURL, logger),
			CheckPath:  policy.CheckBackupPath,
			HTTPClient: &http.Client{Timeout: 0},
		})
		runner, err = NewRunner(Options{
			Logger:         logger,
			Config:         legacyCfg,
			Tools:          dispatcher,
			Apps:           appInv,
			Models:         modelInventory,
			Calls:          calls,
			Sessions:       sessions,
			Metrics:        metrics,
			InferenceServe: policy.InferenceServe,
			ModelPull: &ModelPullOptions{
				PolicyPath:   policyPath,
				OllamaBase:   discoverer.ResolvedOllamaBaseURL,
				PullAllowed:  policy.ModelsPullAllowed,
				ReloadPolicy: policy.Reload,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		go backups.Run(ctx, runner.RegistrationId)
		logger.Info("worker starting (single-home)",
			"cluster_url", legacyCfg.ClusterURL,
			"name", legacyCfg.Name,
			"capabilities", legacyCfg.Capabilities,
		)
	} else {
		fleet, err = NewFleet(FleetOptions{
			Logger:     logger,
			Workers:    workers,
			Policy:     policy,
			PolicyPath: policyPath,
			Tools:      dispatcher,
			Apps:       appInv,
			Models:     modelInventory,
			Discoverer: discoverer,
			Metrics:    metrics,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		logger.Info("worker starting (multi-home fleet)",
			"homes", len(workers.EnabledHomes()),
			"name", workers.WorkerName,
			"capabilities", workers.Capabilities,
		)
	}

	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				serveBefore := policy.InferenceServe()
				if err := policy.Reload(); err != nil {
					logger.Warn("policy reload failed", "error", err)
				} else {
					logger.Info("policy reloaded")
					if after := policy.InferenceServe(); after != serveBefore {
						logger.Info("inference.serve changed; it takes effect on the next reconnect",
							"from", serveBefore, "to", after)
					}
					// Fan out to every home stream.
					if fleet != nil {
						fleet.RequestImmediateReadvertise()
					} else if runner != nil {
						runner.RequestImmediateReadvertise()
					}
				}
			default:
				logger.Info("worker shutting down", "signal", sig.String())
				if stopSessions != nil {
					stopSessions("the cockpit is shutting down")
				}
				cancel()
				return
			}
		}
	}()

	restoreTermios := enableQuitHotkeys(ctx, cancel, logger)
	defer restoreTermios()

	var runErr error
	if fleet != nil {
		runErr = fleet.Run(ctx)
	} else {
		runErr = runner.Run(ctx)
	}
	if runErr != nil && ctx.Err() == nil {
		logger.Error("worker exited with error", "error", runErr)
		if metrics != nil {
			metrics.Stop()
		}
		os.Exit(1)
	}
	if metrics != nil {
		metrics.Stop()
	}
}

// setupNonInteractive makes the permission pre-flight report-and-fail
// instead of pausing for approval -- for scripted installs and CI.
var setupNonInteractive bool

// setupFlags is `worker setup`'s command line, parsed.
//
// A struct rather than five locals so the ONE decision this command
// makes -- which of two entirely different things it is -- is assertable
// in a test without a terminal, a machine, or an os.Exit. See
// TestSetupFlags.
type setupFlags struct {
	nonInteractive bool
	inference      bool
	configPath     string
	runtimeFlag    string
	modelIDs       []string
}

func parseSetupFlags(args []string) setupFlags {
	fs := flag.NewFlagSet("worker setup", flag.ExitOnError)
	nonInteractive := fs.Bool("non-interactive", false, "never prompt; report what is missing and exit 3 (a question could not be asked), 4 (not granted) or 5 (probe failed)")
	inference := fs.Bool("inference", false, "set this machine up to serve local models: runtime, models, models.allow")
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml (its directory holds policy.yaml)")
	runtimeFlag := fs.String("runtime", "", "with --inference: docker | native, overriding the runtime this platform would choose. On its own: kokoro | image, to install that runtime")
	var modelIDs repeatedFlag
	fs.Var(&modelIDs, "model", "with --inference: a model id to pull; repeatable (default: the recommended set for this machine's class)")
	_ = fs.Parse(args)
	return setupFlags{
		nonInteractive: *nonInteractive,
		inference:      *inference,
		configPath:     *configPath,
		runtimeFlag:    *runtimeFlag,
		modelIDs:       modelIDs,
	}
}

// runtimeModeFor decides which of --runtime's two meanings applies.
//
// It returns ("", false) when the flag is absent or names a model
// runtime alongside --inference (the existing path, unchanged); the
// runtime name when this is an install; and (sentence, true) for a
// combination that is refused.
//
// A PURE FUNCTION of the parsed flags, so every combination is
// assertable without a terminal or a machine -- the same reason
// setupFlags is a struct.
func runtimeModeFor(f setupFlags) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(f.runtimeFlag))
	if name == "" {
		return "", false
	}

	installable := false
	for _, r := range inference.InstallableRuntimes() {
		if name == r {
			installable = true
			break
		}
	}

	switch {
	case installable && f.inference:
		return wrapJoin(fmt.Sprintf(
			"--runtime %s installs a speech or image runtime, and --runtime docker or native chooses"+
				" how Ollama runs. They are different questions, so they are different commands: run"+
				" memql worker setup --runtime %s on its own.", name, name)), true

	case installable:
		return name, false

	case f.inference:
		// docker | native alongside --inference: the existing meaning.
		// applyRuntimeFlag validates it and refuses a combination the
		// platform cannot serve, in its own words.
		return "", false

	case name == "docker" || name == "native":
		return wrapJoin(fmt.Sprintf(
			"--runtime %s chooses how Ollama runs, which is a question only --inference asks."+
				" Run memql worker setup --inference --runtime %s, or drop the flag.", name, name)), true

	default:
		return wrapJoin(fmt.Sprintf(
			"--runtime takes docker or native with --inference, or %s on its own. It does not take %q.",
			strings.Join(inference.InstallableRuntimes(), " or "), f.runtimeFlag)), true
	}
}

// wrapJoin renders a refusal at the width the rest of this surface
// wraps to, so a usage error and a plan refusal look like one product.
func wrapJoin(text string) string {
	return strings.Join(wrapText(text, inferenceWrapWidth), "\n")
}

func handleSetup(args []string) {
	f := parseSetupFlags(args)
	setupNonInteractive = f.nonInteractive

	// --inference IS ITS OWN COMMAND, and the branch is here rather than
	// inside the wizard so the computer-use pre-flight is UNTOUCHED when
	// the flag is absent. That pre-flight is reached by `worker pair` on
	// every machine that enrolls; folding a model setup into it would
	// mean an enrollment could now fail for a reason that has nothing to
	// do with the permissions it was asking about -- on the majority of
	// machines, which are below the hardware floor and were never going
	// to serve a model.
	// --runtime MEANS TWO DIFFERENT THINGS, and the ambiguity is
	// resolved by refusing the mix rather than by guessing.
	//
	// Alongside --inference it chooses HOW OLLAMA RUNS (docker or
	// native). On its own it installs an ADDITIONAL runtime (kokoro or
	// image). Those are different questions -- one is about a runtime
	// this machine is getting either way, the other about whether it
	// gets a second one at all -- and a person who typed the wrong
	// combination is better served by a sentence than by whichever
	// meaning happened to win.
	if verdict, refused := runtimeModeFor(f); refused {
		fmt.Fprintln(os.Stdout, verdict)
		os.Exit(SetupExitUsage)
	} else if verdict != "" {
		os.Exit(runRuntimeSetup(context.Background(),
			newRuntimeSetup(f.configPath, verdict, f.nonInteractive)))
	}

	if f.inference {
		os.Exit(runInferenceSetup(context.Background(),
			newInferenceSetup(f.configPath, f.modelIDs, f.nonInteractive, f.runtimeFlag)))
	}

	if err := runSetupWizard(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		// The wizard's own code, not a flat 1: an install script reading
		// `$?` has to be able to tell "a permission has not been granted"
		// (exit 4, the operator has an action) from "the probe itself
		// failed" (exit 5). See setup_exit.go.
		os.Exit(SetupExitCode(err))
	}
}

func handleConfig(args []string) {
	fs := flag.NewFlagSet("worker config", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	fs.Parse(args)

	w, err := LoadWorkers(*workersPath, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Workers file: %s\n", *workersPath)
	fmt.Printf("Name:         %s\n", emptyOrValue(w.WorkerName))
	fmt.Printf("Capabilities:%s\n", " "+strings.Join(w.Capabilities, ", "))
	fmt.Printf("Concurrency:  %v\n", w.Concurrency)
	fmt.Printf("State dir:    %s\n", w.StateDir)
	fmt.Printf("Log level:    %s\n", w.LogLevel)
	fmt.Printf("Homes:        %d (%d enabled)\n", len(w.Homes), len(w.EnabledHomes()))
	if len(w.Homes) == 0 {
		fmt.Println("\n(no homes enrolled — run `memql worker pair` or the install script)")
	}
	for _, h := range w.Homes {
		en := "enabled"
		if !h.IsEnabled() {
			en = "disabled"
		}
		fmt.Printf("\n  [%s] %s\n", h.ID, en)
		fmt.Printf("    Cluster URL: %s\n", emptyOrValue(h.ClusterURL))
		fmt.Printf("    Token:       %s\n", maskToken(h.Token))
		fmt.Printf("    State dir:   %s\n", w.ConfigForHome(h).StateDir)
	}
	if err := w.Validate(); err != nil {
		fmt.Printf("\nValidation: ERROR -- %v\n", err)
	} else if err := w.ValidateRun(); err != nil {
		fmt.Printf("\nValidation: OK (registry); run: %v\n", err)
	} else {
		fmt.Printf("\nValidation: OK\n")
	}
}

func handleUnpair(args []string) {
	fs := flag.NewFlagSet("worker unpair", flag.ExitOnError)
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	cluster := fs.String("cluster", "", "home id to remove (see `memql worker config`)")
	disable := fs.Bool("disable", false, "disable the home instead of removing it")
	fs.Parse(args)
	id := strings.TrimSpace(*cluster)
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "ERROR: unpair requires --cluster <home-id>")
		os.Exit(1)
	}
	w, err := RemoveHome(*workersPath, id, *disable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	action := "removed"
	if *disable {
		action = "disabled"
	}
	fmt.Printf("%s home %q (%d homes remain, %d enabled)\n",
		action, id, len(w.Homes), len(w.EnabledHomes()))
	fmt.Println("Restart the worker (launchctl unload/load the LaunchAgent) to drop the stream.")
}

func printUsage() {
	fmt.Println("memql worker -- run cockpit as a memql worker")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql worker pair <code>   Redeem an XXXX-XXXX pairing code from")
	fmt.Println("                                     CoPresent's Settings -> Computer Use card,")
	fmt.Println("                                     upsert a home in workers.yaml, run worker.")
	fmt.Println("                                     Additive: sibling homes are kept.")
	fmt.Println("  memql worker               Same flow with a paste prompt (no code arg).")
	fmt.Println("  memql worker run           Run an already-configured worker (used by")
	fmt.Println("                                     LaunchAgent / scripts).")
	fmt.Println("  memql worker setup         Re-run TCC permissions check (computeruse builds only).")
	fmt.Println("  memql worker setup --inference")
	fmt.Println("                                     Set this machine up to serve local models:")
	fmt.Println("                                     the runtime, the models, and models.allow.")
	fmt.Println("  memql worker setup --runtime kokoro")
	fmt.Println("                                     Install the speech runtime, so this machine")
	fmt.Println("                                     can serve text to speech. --runtime image")
	fmt.Println("                                     does the same for image generation.")
	fmt.Println("  memql worker config        Print the effective config (all homes).")
	fmt.Println("  memql worker unpair --cluster <id>")
	fmt.Println("                                     Remove (or --disable) one home; siblings stay.")
	fmt.Println("  memql worker models        Print the local models this machine would offer,")
	fmt.Println("                                     or the reason it offers none. --pull <id>")
	fmt.Println("                                     pulls one; --allow <id> offers one.")
	fmt.Println("  memql worker hardware      Print what this machine reports about itself:")
	fmt.Println("                                     chip, memory, GPU, runtimes, and the class")
	fmt.Println("                                     that decides which models are recommended.")
	fmt.Println("  memql worker probe         Measure a local model against the probe suite:")
	fmt.Println("                                     structured output, tool calls, throughput.")
	fmt.Println("  memql worker backup        Print the folders this machine backs up into the")
	fmt.Println("                                     Library, or the reason it backs up none.")
	fmt.Println("                                     --once runs one sweep now.")
	fmt.Println("  memql worker consent <op>  Manage the per-call consent gate (grant/revoke/status/watch).")
	fmt.Println("")
	fmt.Println("PAIR FLAGS")
	fmt.Println("  --cluster <name>     Cluster NAME from clusters.yaml (identity lookup).")
	fmt.Println("  --home-id <id>       Home id in workers.yaml (default: URL host / cluster name).")
	fmt.Println("  --force              Remap a home id onto a different cluster_url (siblings kept).")
	fmt.Println("  --identity <url>     Advanced: override identity service URL.")
	fmt.Println("  --token <token>      Advanced: worker token (skip redeem).")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("")
	fmt.Println("RUN FLAGS")
	fmt.Println("  --workers <path>     Path to workers.yaml (default ~/.memql/workers.yaml)")
	fmt.Println("  --config <path>      Legacy worker.yaml (default ~/.memql/worker.yaml)")
	fmt.Println("  --cluster <url>      Cluster URL (forces single-home mode)")
	fmt.Println("  --token <token>      Worker token (mql_wkr_...; overrides config + MEMQL_WORKER_TOKEN)")
	fmt.Println("  --name <name>        Worker name (overrides config)")
	fmt.Println("  --log-level <l>      Log level: debug | info | warn | error")
	fmt.Println("  --metrics-port <p>   Loopback port for prometheus metrics (default 9100; 0 disables)")
	fmt.Println("")
	fmt.Println("SETUP --INFERENCE FLAGS")
	fmt.Println("  --model <id>         Model to pull; repeatable. The default is the set")
	fmt.Println("                       recommended for this machine's class; run")
	fmt.Println("                       memql worker hardware to see it.")
	fmt.Println("  --runtime <r>        docker | native, overriding the runtime this platform")
	fmt.Println("                       would choose. A combination the platform cannot serve")
	fmt.Println("                       is refused rather than ignored. Without --inference the")
	fmt.Println("                       flag takes kokoro | image instead and installs that")
	fmt.Println("                       runtime; the two meanings are never mixed silently.")
	fmt.Println("  --non-interactive    Never ask. A runtime install it would have asked about")
	fmt.Println("                       is refused with exit 3 and nothing is installed.")
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
