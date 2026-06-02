// Package harness implements the `memql-cockpit harness trace <planId>`
// subcommand -- a one-shot CLI that fetches a harness plan's full
// execution timeline over gRPC and prints it.
//
// It consumes the engine's `harnessTrace` builtin (memql-cockpit#142,
// the memql-side contract is merged on znasllc-io/memql main) via the
// generated SDK method QueryClient.HarnessTrace. The builtin returns
// one synthetic node (concept v1:harness:trace) whose payload carries
// {planId, timeline, complete, stepCount}; this command prints the
// rendered `timeline` string.
//
// The command reuses the cockpit's existing cluster + credential
// machinery: it resolves the active (or --cluster / --endpoint)
// cluster from ~/.memql/clusters.yaml, resolves the cached bearer via
// auth.EnsureValidToken, and dials with client.Connect -- the same
// path login / creds take. No bespoke wire layer, no hardcoded
// endpoints.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/cli/auth"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// dialTimeout bounds the connect + query round-trip. A trace is a
// single synthetic-node read; 30s is generous headroom for a cold
// remote cluster.
const dialTimeout = 30 * time.Second

// HandleCommand is the entry point the cockpit binary calls for
// `memql-cockpit harness ...`. Returns the process exit code for the
// caller to forward to os.Exit.
func HandleCommand(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 1
	}
	switch args[0] {
	case "trace":
		return handleTrace(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown harness subcommand %q\n\n", args[0])
		printUsage()
		return 1
	}
}

// handleTrace parses `trace <planId> [--cluster X | --endpoint Y]`,
// resolves the cluster + creds, runs the harnessTrace builtin, and
// prints the rendered timeline.
func handleTrace(args []string) int {
	var (
		planId      string
		clusterName string
		endpoint    string
	)

	// Tiny argv parser: --cluster / --endpoint accept either
	// `--flag=value` or `--flag value`; first bare positional is the
	// planId. Matches the lightweight style of the lint subcommand
	// rather than pulling in flag.NewFlagSet for three tokens.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help", a == "-h":
			printUsage()
			return 0
		case a == "--cluster" || a == "--endpoint":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "ERROR: %s requires a value\n", a)
				return 1
			}
			i++
			if a == "--cluster" {
				clusterName = args[i]
			} else {
				endpoint = args[i]
			}
		case strings.HasPrefix(a, "--cluster="):
			clusterName = strings.TrimPrefix(a, "--cluster=")
		case strings.HasPrefix(a, "--endpoint="):
			endpoint = strings.TrimPrefix(a, "--endpoint=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "ERROR: unknown flag %q\n", a)
			printUsage()
			return 1
		default:
			if planId != "" {
				fmt.Fprintf(os.Stderr, "ERROR: too many positional arguments (got %q after %q)\n", a, planId)
				return 1
			}
			planId = a
		}
	}

	if planId == "" {
		fmt.Fprintln(os.Stderr, "ERROR: a planId is required")
		printUsage()
		return 1
	}

	timeline, err := runTrace(planId, clusterName, endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 1
	}
	fmt.Println(timeline)
	return 0
}

// runTrace resolves the cluster + token, dials, calls HarnessTrace,
// and returns the rendered timeline string (or the raw payload when
// the timeline field is empty).
func runTrace(planId, clusterName, endpoint string) (string, error) {
	cluster, err := resolveCluster(clusterName, endpoint)
	if err != nil {
		return "", err
	}

	// Install the active credential store (keyring or file, same
	// auto-probe the TUI and `creds` use) so EnsureValidToken reads
	// from whichever backend won resolution rather than always
	// defaulting to the file store.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if store, resolveErr := config.Resolve(config.ResolveOptions{Logger: logger}); resolveErr == nil {
		config.SetActiveStore(store)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	token, err := auth.EnsureValidToken(ctx, cluster)
	if err != nil {
		return "", fmt.Errorf("authenticate with cluster %q: %w", cluster.Name, err)
	}

	conn, err := client.Connect(ctx, client.ConnectConfig{
		Endpoint: cluster.Endpoint,
		Token:    token,
		Logger:   logger,
	})
	if err != nil {
		return "", fmt.Errorf("connect to cluster %q (%s): %w", cluster.Name, cluster.Endpoint, err)
	}
	defer conn.Close()

	qc := client.NewQueryClient(conn.Dispatcher())
	result, err := qc.HarnessTrace(ctx, client.HarnessTraceArgs{PlanId: planId})
	if err != nil {
		return "", fmt.Errorf("harnessTrace(%s): %w", planId, err)
	}

	row := result.Single()
	if row == nil {
		return "", fmt.Errorf("no trace returned for plan %q (unknown plan id, or not owned by the authenticated user)", planId)
	}

	payload, err := json.Marshal(row)
	if err != nil {
		return "", fmt.Errorf("encode trace payload: %w", err)
	}
	return extractTimeline(payload)
}

// extractTimeline pulls the `timeline` string out of the
// harnessTrace synthetic row's JSON payload. When the timeline field
// is empty or absent it falls back to returning the raw payload so a
// caller still sees something actionable rather than a blank line.
//
// Kept pure (bytes in, string out, no network) so the parsing
// contract is unit-testable without a live cluster.
func extractTimeline(payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("empty trace payload")
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		return "", fmt.Errorf("decode trace payload: %w", err)
	}
	if timeline, ok := fields["timeline"].(string); ok && strings.TrimSpace(timeline) != "" {
		return timeline, nil
	}
	// Fall back to the raw payload -- the row came back but carried no
	// rendered timeline (e.g. an older engine, or a degenerate plan).
	return string(payload), nil
}

// resolveCluster mirrors the precedence the TUI launcher uses:
// --endpoint wins (direct dial), then --cluster (named lookup in
// clusters.yaml), then the saved "local" entry as the active
// default.
func resolveCluster(clusterName, endpoint string) (config.ClusterConfig, error) {
	if endpoint != "" {
		return config.ClusterConfig{
			Name:     "direct",
			Endpoint: strings.TrimSpace(endpoint),
		}, nil
	}

	clusters, err := config.LoadClusters()
	if err != nil {
		return config.ClusterConfig{}, fmt.Errorf("load clusters config: %w", err)
	}

	if clusterName != "" {
		c, ok := clusters.Get(clusterName)
		if !ok {
			return config.ClusterConfig{}, fmt.Errorf("cluster %q not found in ~/.memql/clusters.yaml -- run `memql-cockpit cluster list` to see configured clusters", clusterName)
		}
		return c, nil
	}

	// No --cluster, no --endpoint: fall back to the saved "local"
	// entry (seeded by `genesis init`). If it has no endpoint yet the
	// connect path surfaces a clear dial error.
	local, ok := clusters.Get("local")
	if !ok || local.Endpoint == "" {
		return config.ClusterConfig{}, fmt.Errorf("no cluster specified and the \"local\" cluster has no endpoint configured -- pass --cluster <name> or --endpoint <addr>, or launch the TUI to authorize one")
	}
	return local, nil
}

func printUsage() {
	fmt.Println("Usage: memql-cockpit harness trace <planId> [flags]")
	fmt.Println()
	fmt.Println("Fetch a harness plan's full execution timeline (every plan/step")
	fmt.Println("version transition + observations, ordered by createdAt) and print")
	fmt.Println("the rendered timeline. Owner-scoped to the authenticated user's own plan.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --cluster <name>   Cluster from ~/.memql/clusters.yaml (default: local)")
	fmt.Println("  --endpoint <addr>  gRPC endpoint (overrides --cluster)")
	fmt.Println("  --help, -h         Show this help")
	fmt.Println()
	fmt.Println("Exit codes:")
	fmt.Println("  0  success")
	fmt.Println("  1  usage error / not authenticated / connect failure / empty result")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  memql-cockpit harness trace v1:planner:plan:a9f3b7c2")
	fmt.Println("  memql-cockpit harness trace v1:planner:plan:a9f3b7c2 --cluster staging")
}
