// memql-cockpit is a memQL Cockpit -- terminal-native IDE and operations console.
// It provides a multi-tab TUI with an editor (Sense integration), cluster
// topology viewer (pixel canvas), and automation flow diagrams.
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
// The built-in "local" cluster points at https://bff.local.znas.io -- the
// nginx :443 entry point in docker-compose.full.yml. nginx terminates TLS
// (mkcert-issued cert; root CA in the system trust store) and forwards
// gRPC to bff:50051 inside the docker network.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	coregenesis "github.com/visionarys-io/memql/component/genesis"

	"github.com/visionarys-io/memql-cockpit/cli"
	"github.com/visionarys-io/memql-cockpit/cli/auth"
	"github.com/visionarys-io/memql-cockpit/cli/config"
	"github.com/visionarys-io/memql-cockpit/cmd/memql-cockpit/internal/authorize"
	"github.com/visionarys-io/memql-cockpit/cmd/memql-cockpit/internal/genesis"
	"github.com/visionarys-io/memql-cockpit/cmd/memql-cockpit/internal/lint"
	"github.com/visionarys-io/memql-cockpit/cmd/memql-cockpit/internal/worker"
)

const version = "0.1.0"

func main() {
	// Repo-root .env override sits above whatever genesis sealed in,
	// matching the contract documented in memql/component/genesis/
	// localenv.go. Errors here are warnings: the cockpit can run
	// without a local .env (production case) and shouldn't refuse to
	// launch just because the file has a stray syntax error.
	if overridden, err := coregenesis.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "memql-cockpit: warning: local .env override failed: %v\n", err)
	} else if len(overridden) > 0 {
		fmt.Fprintf(os.Stderr, "memql-cockpit: local .env override applied: %v\n", overridden)
	}

	// Check for subcommands before parsing flags.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "authorize":
			authorize.HandleCommand(os.Args[2:])
			return
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
		case "genesis":
			os.Exit(genesis.HandleCommand(os.Args[2:]))
		case "lint":
			os.Exit(lint.HandleCommand(os.Args[2:]))
		case "help":
			printUsage()
			return
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	app := cli.NewApp(cli.AppConfig{
		Cluster: cluster,
		Logger:  logger,
		Version: version,
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Run()
	}()

	select {
	case sig := <-sigCh:
		logger.Info("signal received, shutting down", "signal", sig.String())
		app.Quit()
	case err := <-errCh:
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
	case "add":
		handleClusterAdd(args[1:])
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

func handleClusterAdd(args []string) {
	// Extract the name (first non-flag arg) before parsing flags.
	// This allows: cluster add local --endpoint ... OR cluster add --endpoint ... local
	var name string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else if name == "" {
			name = a
		} else {
			flagArgs = append(flagArgs, a)
		}
	}

	fs := flag.NewFlagSet("cluster add", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "gRPC endpoint (host:port) [required]")
	issuer := fs.String("issuer", "", "OIDC issuer URL")
	clientId := fs.String("client-id", "", "OAuth2 client ID")
	patToken := fs.String("pat", "", "Personal Access Token (mql_pat_...) -- bypasses OIDC")
	fs.Parse(flagArgs)

	if name == "" {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Usage: memql-cockpit cluster add <name> --endpoint <host:port> --issuer <url> --client-id <id> [--pat <token>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "TIP: prefer `memql-cockpit authorize <url>` -- it discovers")
		fmt.Fprintln(os.Stderr, "issuer + client_id from the cluster's well-known endpoint and")
		fmt.Fprintln(os.Stderr, "runs the OAuth login in one step.")
		os.Exit(1)
	}

	if *endpoint == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --endpoint is required")
		os.Exit(1)
	}

	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// Check for duplicate.
	if _, exists := clusters.Get(name); exists {
		fmt.Fprintf(os.Stderr, "ERROR: cluster %q already exists (use 'cluster remove' first)\n", name)
		os.Exit(1)
	}

	clusters.Clusters = append(clusters.Clusters, config.ClusterConfig{
		Name:     name,
		Endpoint: *endpoint,
		Issuer:   *issuer,
		ClientId: *clientId,
		PAT:      *patToken,
	})

	if err := config.SaveClusters(clusters); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Cluster %q added (%s)\n", name, *endpoint)
}

func handleClusterList() {
	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-15s %-35s %-8s %s\n", "NAME", "ENDPOINT", "AUTH", "ISSUER")
	fmt.Printf("%-15s %-35s %-8s %s\n", "----", "--------", "----", "------")

	// Always show local first.
	fmt.Printf("%-15s %-35s %-8s %s\n", "local", "https://bff.local.znas.io", "none", "-")

	for _, c := range clusters.Clusters {
		if c.Name == "local" {
			continue // Already shown above.
		}
		authStr := "OIDC"
		if c.PAT != "" {
			authStr = "PAT"
		}
		issuer := c.Issuer
		if issuer == "" {
			issuer = "-"
		}
		fmt.Printf("%-15s %-35s %-8s %s\n", c.Name, c.Endpoint, authStr, issuer)
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
		c, ok := clusters.Get(clusterName)
		if !ok {
			return config.ClusterConfig{}, fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml -- run `memql-cockpit authorize <url>` first", clusterName)
		}
		return c, nil
	}

	return config.ClusterConfig{
		Name:     "local",
		Endpoint: "https://bff.local.znas.io",
	}, nil
}

func printUsage() {
	fmt.Println("memql-cockpit — memQL Cockpit -- terminal-native IDE and operations console")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql-cockpit authorize <url>          Register + log into a cluster (one-shot)")
	fmt.Println("  memql-cockpit [flags]                  Launch the TUI")
	fmt.Println("  memql-cockpit cluster <subcommand>     Manage cluster configurations (advanced)")
	fmt.Println("  memql-cockpit login <cluster>          Re-authenticate an existing cluster")
	fmt.Println("  memql-cockpit logout <cluster>         Remove cached credentials")
	fmt.Println("  memql-cockpit worker <subcommand>      Run as a memql worker (computer-use)")
	fmt.Println("  memql-cockpit genesis init [--from .env]  Create / update ~/.memql/genesis.znas")
	fmt.Println("  memql-cockpit lint [path]              Validate a .memql file or DSL tree")
	fmt.Println("")
	fmt.Println("TUI FLAGS")
	fmt.Println("  --cluster <name>    Connect to a named cluster")
	fmt.Println("  --endpoint <addr>   Connect to a specific gRPC endpoint")
	fmt.Println("  --version           Print version and exit")
	fmt.Println("")
	fmt.Println("CLUSTER MANAGEMENT")
	fmt.Println("  authorize <url>                           One-shot register + OAuth login (preferred)")
	fmt.Println("  cluster add <name> --endpoint <host:port> --issuer <url> --client-id <id>")
	fmt.Println("  cluster list                              List clusters (\"local\" is always present)")
	fmt.Println("  cluster remove <name>                     Remove a cluster (\"local\" cannot be removed)")
	fmt.Println("")
	fmt.Println("EXAMPLES")
	fmt.Println("  memql-cockpit authorize http://localhost:8081      # local-dev (full identity flow)")
	fmt.Println("  memql-cockpit authorize https://copresent.acme.com  # production")
	fmt.Println("  memql-cockpit login staging")
	fmt.Println("  memql-cockpit --cluster staging")
}

func printClusterUsage() {
	fmt.Println("Usage: memql-cockpit cluster <subcommand>")
	fmt.Println("")
	fmt.Println("Subcommands:")
	fmt.Println("  add <name> --endpoint <host:port>   Add a cluster configuration")
	fmt.Println("  list                                List clusters (local is always present)")
	fmt.Println("  remove <name>                       Remove a cluster (local cannot be removed)")
}
