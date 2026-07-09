// memql-cockpit is a memQL Cockpit -- terminal-native IDE and operations console.
// It provides a multi-tab TUI with an editor (Sense integration), cluster
// topology viewer (pixel canvas), and an agents directory.
//
// Usage:
//
//	memql-cockpit                                   Launch TUI (default cluster: local)
//	memql-cockpit --cluster staging                 Launch TUI connected to staging
//	memql-cockpit --endpoint https://bff.<domain>   Launch TUI with explicit endpoint
//	memql-cockpit cluster add <name> ...            Add a cluster configuration
//	memql-cockpit cluster list                      List configured clusters
//	memql-cockpit cluster remove <name>             Remove a cluster configuration
//	memql-cockpit login <cluster>                   Authenticate with a cluster
//	memql-cockpit logout <cluster>                  Remove cached credentials
//	memql-cockpit --version                         Print version
//
// The built-in "local" cluster is a name slot with no baked-in
// endpoint. `genesis init` reads IDENTITY_BOOTSTRAP_DOMAIN from the
// operator's .env and seeds the local row in ~/.memql/clusters.yaml
// with `https://cockpit.${DOMAIN}`. The TUI shows "not configured" until
// that bootstrap (or an explicit `L:Authorize`) has happened.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	coregenesis "github.com/znasllc-io/memql/component/genesis"

	"github.com/znasllc-io/memql-cockpit/cli"
	"github.com/znasllc-io/memql-cockpit/cli/auth"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/crash"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/deploy"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/harness"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/lint"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/setupproject"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/worker"
)

// version is the cockpit's semantic version. The git tag is the
// source of truth (see VERSIONING.md); this constant tracks the
// VERSION file and is bumped together with it on each release.
const version = "0.9.0"

// deployRunner routes the Deployments C/G/B controls to the blessed
// deployEngineCluster automation path (znasllc-io/memql-cockpit#292). It runs
// THIS binary's `deploy` subcommand for a forward env-level deploy (the only
// automation in the bundle), capturing its output so nothing corrupts the
// TUI. Cut / historical-rollback have no automation equivalent and are slated
// for owner-gated retirement (memql#2229), so they report that honestly
// instead of dialing the identity-only DeployControlService that a bff-role
// node (including cockpit-bff) never serves.
func deployRunner(env, action, targetID string) (string, bool) {
	switch action {
	case "deploy":
		exe, err := os.Executable()
		if err != nil {
			return "ERROR: locate cockpit binary: " + err.Error(), false
		}
		args := []string{"deploy", "--apply"}
		if strings.TrimSpace(env) != "" {
			args = append(args, "--env="+env)
		}
		out, runErr := exec.Command(exe, args...).CombinedOutput()
		s := string(out)
		line := lastMeaningfulLine(s)
		// The subcommand exits 0 even when it BLOCKS on a missing engine DB
		// (the honest owner-gated report). A real success is exit 0 with no
		// BLOCKED / ERROR marker in the captured output.
		ok := runErr == nil && !strings.Contains(s, "BLOCKED") && !strings.Contains(s, "ERROR")
		if line == "" {
			if runErr != nil {
				line = "deploy failed: " + runErr.Error()
			} else {
				line = "deploy completed (no output)"
			}
		}
		return line, ok
	case "cut", "rollback":
		return "not available on the automation deploy path yet (memql#2229) -- " +
			"use G to deploy the current spec, or `memql-cockpit deploy` / the identity /admin/deployments portal", false
	default:
		return "unknown deploy action: " + action, false
	}
}

// lastMeaningfulLine returns the last non-empty, trimmed line of s -- the
// deploy subcommand's final status line -- for surfacing in the TUI result.
func lastMeaningfulLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func main() {
	// Repo-root .env override sits above whatever genesis sealed in,
	// matching the contract documented in memql/component/genesis/
	// localenv.go. Errors here are warnings: the cockpit can run
	// without a local .env (production case) and shouldn't refuse to
	// launch just because the file has a stray syntax error.
	//
	// Suppress the warning for the "file doesn't exist" case -- it's
	// the common case for cockpit launches outside a memql checkout
	// and there's nothing for the user to do about it. The check uses
	// errors.Is (vs the older os.IsNotExist) because the genesis
	// package wraps with `%w` and a non-unwrapping check returns
	// false, which is what was producing the noisy warning every
	// single boot.
	if overridden, err := coregenesis.ApplyLocalOverride("."); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "memql-cockpit: warning: local .env override failed: %v\n", err)
		}
	} else if len(overridden) > 0 {
		fmt.Fprintf(os.Stderr, "memql-cockpit: local .env override applied: %v\n", overridden)
	}

	// Check for subcommands before parsing flags.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "cluster":
			handleClusterCmd(os.Args[2:])
			return
		case "login":
			handleLoginCmd(os.Args[2:])
			return
		case "logout":
			handleLogoutCmd(os.Args[2:])
			return
		case "worker":
			worker.HandleCommand(os.Args[2:])
			return
		case "creds":
			handleCredsCmd(os.Args[2:])
			return
		case "genesis":
			// Removed: genesis setup lives in the TUI's first-launch
			// wizard now. Catch the legacy invocation so anyone with
			// muscle memory or a stale script gets a one-line pointer.
			fmt.Fprintln(os.Stderr, "memql-cockpit genesis has moved into the TUI.")
			fmt.Fprintln(os.Stderr, "Launch `memql-cockpit`. If genesis.znas is missing, the")
			fmt.Fprintln(os.Stderr, "setup wizard will walk you through creating it.")
			os.Exit(1)
		case "lint":
			os.Exit(lint.HandleCommand(os.Args[2:]))
		case "setup":
			os.Exit(setupproject.HandleCommand(os.Args[2:]))
		case "deploy":
			os.Exit(deploy.HandleDeploy(os.Args[2:], version))
		case "run":
			os.Exit(deploy.HandleRun(os.Args[2:], version))
		case "harness":
			os.Exit(harness.HandleCommand(os.Args[2:]))
		case "help":
			printUsage()
			return
		case "authorize":
			// Removed: authorization lives in the TUI now (press L
			// on a cluster row + paste the discovery URL into the
			// form). Trap the old invocation so anyone with muscle
			// memory or a stale install script gets a one-line
			// pointer instead of the TUI swallowing their URL.
			fmt.Fprintln(os.Stderr, "memql-cockpit authorize has moved into the TUI.")
			fmt.Fprintln(os.Stderr, "Launch `memql-cockpit`, highlight the cluster row, press L,")
			fmt.Fprintln(os.Stderr, "and paste the discovery URL into the form.")
			os.Exit(1)
		}
	}

	// TUI mode — parse flags and launch the IDE.
	clusterName := flag.String("cluster", "", "cluster name from ~/.memql/clusters.yaml")
	endpoint := flag.String("endpoint", "", "gRPC endpoint (overrides cluster config)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("memql-cockpit %s\n", version)
		os.Exit(0)
	}

	cluster, err := resolveCluster(*clusterName, *endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Pick the active credential store and install it for the rest of
	// the cockpit to route through (cli/config.LoadToken / SaveToken /
	// DeleteToken). Auto-probe prefers the OS keyring; MEMQL_COCKPIT_CRED_STORE
	// can force "file" or "keyring". See memql-cockpit#65.
	credStore, credErr := config.Resolve(config.ResolveOptions{Logger: logger})
	if credErr != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", credErr)
		os.Exit(1)
	}
	config.SetActiveStore(credStore)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	app := cli.NewApp(cli.AppConfig{
		Cluster:      cluster,
		Logger:       logger,
		Version:      version,
		DeployRunner: deployRunner,
	})

	errCh := make(chan error, 1)
	// app.Run is wrapped in crash.Catch so a panic that escapes
	// every in-app recovery layer (per-tab Draw / per-tab HandleEvent
	// / main-loop iteration) lands here as a Report instead of
	// killing the process with a stack trace. The Catch'd goroutine
	// returns normally so the select below sees errCh close cleanly.
	var lastResort *crash.Report
	go func() {
		if r := crash.Catch("app.Run", func() {
			errCh <- app.Run()
		}); r != nil {
			lastResort = r
			errCh <- fmt.Errorf("cockpit hit an unexpected error (code %s)", r.Code)
		}
	}()

	select {
	case sig := <-sigCh:
		logger.Info("signal received, shutting down", "signal", sig.String())
		app.Quit()
	case err := <-errCh:
		if lastResort != nil {
			// Print the friendly message to stderr AFTER tcell has
			// had a chance to unwind. The deferred app.Quit isn't
			// strictly necessary here -- tcell's screen finalizer
			// ran on the way out of app.Run -- but it's harmless
			// and belt-and-suspenders.
			fmt.Fprint(os.Stderr, crash.UserMessage(lastResort))
			os.Exit(1)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
	}
}

// ---------------------------------------------------------------------------
// Subcommand: cluster
// ---------------------------------------------------------------------------

func handleClusterCmd(args []string) {
	if len(args) == 0 {
		printClusterUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "list", "ls":
		handleClusterList()
	case "remove", "rm":
		handleClusterRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown cluster subcommand %q\n\n", args[0])
		printClusterUsage()
		os.Exit(1)
	}
}

func handleClusterList() {
	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-15s %-35s %-15s %s\n", "NAME", "ENDPOINT", "AUTH", "ISSUER")
	fmt.Printf("%-15s %-35s %-15s %s\n", "----", "--------", "----", "------")

	// `local` is rendered first whether or not it lives in clusters.yaml.
	// When the yaml has no entry (fresh install, no `genesis init` yet)
	// the endpoint is blank and auth shows "not configured" -- same as
	// what the TUI surfaces.
	local, hasLocal := clusters.Get("local")
	if !hasLocal {
		local = config.ClusterConfig{Name: "local"}
	}
	printClusterRow(local)
	for _, c := range clusters.Clusters {
		if c.Name == "local" {
			continue
		}
		printClusterRow(c)
	}
}

func printClusterRow(c config.ClusterConfig) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "-"
	}
	issuer := c.Issuer
	if issuer == "" {
		issuer = "-"
	}
	fmt.Printf("%-15s %-35s %-15s %s\n", c.Name, endpoint, clusterAuthLabel(c), issuer)
}

// clusterAuthLabel returns a human-readable description of what kind
// of credential is configured for a cluster. Kept here (instead of on
// ClusterConfig) so the CLI label stays in sync with what the TUI
// detail panel renders -- both call into the same logic.
func clusterAuthLabel(c config.ClusterConfig) string {
	switch {
	case c.PAT != "":
		return "PAT"
	case c.Issuer != "" && c.ClientId != "":
		return "OIDC"
	default:
		return "not configured"
	}
}

func handleClusterRemove(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql-cockpit cluster remove <name>")
		os.Exit(1)
	}
	name := args[0]

	if name == "local" {
		fmt.Fprintln(os.Stderr, "ERROR: the \"local\" cluster cannot be removed (it is a permanent default)")
		os.Exit(1)
	}

	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	found := false
	var remaining []config.ClusterConfig
	for _, c := range clusters.Clusters {
		if c.Name == name {
			found = true
			continue
		}
		remaining = append(remaining, c)
	}

	if !found {
		fmt.Fprintf(os.Stderr, "ERROR: cluster %q not found\n", name)
		os.Exit(1)
	}

	clusters.Clusters = remaining
	if err := config.SaveClusters(clusters); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// Also remove cached credentials.
	_ = config.DeleteToken(name)

	fmt.Printf("Cluster %q removed\n", name)
}

// ---------------------------------------------------------------------------
// Subcommand: login / logout
// ---------------------------------------------------------------------------

func handleLoginCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql-cockpit login <cluster>")
		os.Exit(1)
	}
	name := args[0]

	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	c, ok := clusters.Get(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: cluster %q not found in ~/.memql/clusters.yaml\n", name)
		os.Exit(1)
	}

	fmt.Printf("Opening browser to authenticate with %q...\n", name)
	token, err := auth.EnsureValidToken(context.Background(), c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	if token != "" {
		fmt.Printf("Authenticated with %q. Token cached at ~/.memql/credentials/%s.json\n", name, name)
	}
}

func handleLogoutCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql-cockpit logout <cluster>")
		os.Exit(1)
	}
	name := args[0]

	if err := auth.Logout(name); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Logged out from %q. Cached credentials removed.\n", name)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// defaultLocalEndpoint is the port-forward target the `local` cluster is
// reached on (see `make forward`: kubectl port-forward svc/bff 50051). Local is
// just another clusters.yaml entry -- the ONLY thing that differs from
// staging/prod is this endpoint (a port-forward vs an ingress). clusters.yaml
// wins if it sets one; otherwise this default lets `run --cluster local` work
// out of the box.
const defaultLocalEndpoint = "localhost:50051"

// withLocalDefault fills in the localhost:50051 endpoint for the `local`
// cluster when the yaml left it blank.
func withLocalDefault(c config.ClusterConfig) config.ClusterConfig {
	if c.Name == "local" && strings.TrimSpace(c.Endpoint) == "" {
		c.Endpoint = defaultLocalEndpoint
	}
	return c
}

func resolveCluster(clusterName, endpoint string) (config.ClusterConfig, error) {
	if endpoint != "" {
		return config.ClusterConfig{
			Name:     "direct",
			Endpoint: strings.TrimSpace(endpoint),
		}, nil
	}

	if clusterName != "" {
		clusters, err := config.LoadClusters()
		if err != nil {
			return config.ClusterConfig{}, fmt.Errorf("load clusters config: %w", err)
		}
		if c, ok := clusters.Get(clusterName); ok {
			return withLocalDefault(c), nil
		}
		// `local` is a permanent default even without a clusters.yaml entry.
		if clusterName == "local" {
			return config.ClusterConfig{Name: "local", Endpoint: defaultLocalEndpoint}, nil
		}
		return config.ClusterConfig{}, fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml -- launch the TUI and press A to add one, or L on the row to authorize it", clusterName)
	}

	// No --cluster, no --endpoint: fall back to "local" (clusters.yaml wins,
	// else the localhost:50051 port-forward default).
	clusters, err := config.LoadClusters()
	if err == nil {
		if local, ok := clusters.Get("local"); ok {
			return withLocalDefault(local), nil
		}
	}
	return config.ClusterConfig{Name: "local", Endpoint: defaultLocalEndpoint}, nil
}

func printUsage() {
	fmt.Println("memql-cockpit — memQL Cockpit -- terminal-native IDE and operations console")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql-cockpit [flags]                  Launch the TUI")
	fmt.Println("  memql-cockpit cluster <subcommand>     List or remove saved clusters")
	fmt.Println("  memql-cockpit login <cluster>          Re-authenticate an existing cluster")
	fmt.Println("  memql-cockpit logout <cluster>         Remove cached credentials")
	fmt.Println("  memql-cockpit worker <subcommand>      Run as a memql worker (computer-use)")
	fmt.Println("  memql-cockpit creds <subcommand>       Inspect / migrate the credential store")
	fmt.Println("  memql-cockpit lint [path]              Validate a .memql file or DSL tree")
	fmt.Println("  memql-cockpit setup project [flags]    Stamp a new product workspace from the template")
	fmt.Println("  memql-cockpit deploy --env=<env>       Deploy the engine cluster via the embedded runtime")
	fmt.Println("  memql-cockpit run <automation>         Run a named automation via the embedded runtime")
	fmt.Println("  memql-cockpit harness trace <planId>   Print a harness plan's execution timeline")
	fmt.Println("")
	fmt.Println("FIRST-TIME SETUP")
	fmt.Println("  Launching the TUI on a machine with no ~/.memql/genesis.znas runs")
	fmt.Println("  the setup wizard: pick a .env, validate it, generate the master key,")
	fmt.Println("  seal the envelope. No CLI dance required.")
	fmt.Println("")
	fmt.Println("TUI FLAGS")
	fmt.Println("  --cluster <name>    Connect to a named cluster")
	fmt.Println("  --endpoint <addr>   Connect to a specific gRPC endpoint")
	fmt.Println("  --version           Print version and exit")
	fmt.Println("")
	fmt.Println("ADDING / AUTHORIZING CLUSTERS")
	fmt.Println("  Both flows live in the TUI: press A to add a new cluster,")
	fmt.Println("  or L on a row to (re-)authorize it. Type a discovery URL")
	fmt.Println("  in the form -- cockpit reads /.well-known/memql-config.json")
	fmt.Println("  and runs the OAuth login in the same keypress.")
	fmt.Println("")
	fmt.Println("EXAMPLES")
	fmt.Println("  memql-cockpit                                       # open the TUI")
	fmt.Println("  memql-cockpit login staging                         # re-auth a saved cluster")
	fmt.Println("  memql-cockpit --cluster staging                     # launch into staging")
}

func printClusterUsage() {
	fmt.Println("Usage: memql-cockpit cluster <subcommand>")
	fmt.Println("")
	fmt.Println("Subcommands:")
	fmt.Println("  list                                List clusters (local is always present)")
	fmt.Println("  remove <name>                       Remove a cluster (local cannot be removed)")
	fmt.Println("")
	fmt.Println("To add or authorize a cluster, launch the TUI and press A or L.")
}
