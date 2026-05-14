// Package authorize implements `memql-cockpit authorize <url>` --
// the one-shot onboarding command that registers a cluster + walks
// the user through OAuth + sets the new cluster as active. Wraps
// the existing cluster-add + login flows so the user paste-types
// one URL instead of several flags.
package authorize

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/visionarys-io/memql-cockpit/cli/auth"
	"github.com/visionarys-io/memql-cockpit/cli/config"
)

// discoveryDocument mirrors the schema served by the identity
// service at /.well-known/memql-config.json. The cockpit is a
// CLIENT of that endpoint, so it carries its own JSON schema --
// importing component/identity from cmd/memql-cockpit would drag
// in the entire identity binary's deps. Stays in sync with
// component/identity/discovery.go.DiscoveryDocument by convention.
type discoveryDocument struct {
	IdentityURL  string `json:"identityUrl"`
	GRPCEndpoint string `json:"grpcEndpoint"`
	ClientId     string `json:"clientId"`
	ClusterName  string `json:"clusterName"`
}

// HandleCommand routes `memql-cockpit authorize ...` invocations.
//
// Two shapes:
//
//	memql-cockpit authorize https://copresent.acme.com
//	  Discovery-driven. Hits <url>/.well-known/memql-config.json
//	  for the gRPC endpoint + client_id, then runs the OAuth flow.
//
//	memql-cockpit authorize \
//	    --grpc <host:port> --identity <url> --client-id <id> \
//	    [--name acme]
//	  Explicit-flag fallback for environments without the discovery
//	  endpoint, or for power users / scripted setups.
func HandleCommand(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage()
		return
	}

	fs := flag.NewFlagSet("authorize", flag.ExitOnError)
	identityURL := fs.String("identity", "", "explicit OIDC issuer URL (skip discovery)")
	grpcEndpoint := fs.String("grpc", "", "explicit gRPC host:port (skip discovery)")
	clientId := fs.String("client-id", "", "explicit OAuth2 client_id (skip discovery)")
	clusterName := fs.String("name", "", "local cluster name (default: derived from URL)")
	patToken := fs.String("pat", "", "use a Personal Access Token instead of OAuth")
	force := fs.Bool("force", false, "overwrite an existing cluster registration with the same name")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	// First positional arg (after fs.Parse has consumed flags + their
	// values) is the discovery URL. Anything beyond is ignored --
	// authorize takes at most one URL.
	var discoveryURL string
	if fs.NArg() > 0 {
		discoveryURL = fs.Arg(0)
	}

	cfg, err := resolveCluster(discoveryURL, *identityURL, *grpcEndpoint, *clientId, *clusterName, *patToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	if err := saveAndLogin(cfg, *force); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Authorized. Cluster %q is now active.\n", cfg.Name)
	fmt.Printf("  endpoint: %s\n", cfg.Endpoint)
	if cfg.Issuer != "" {
		fmt.Printf("  identity: %s\n", cfg.Issuer)
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  - memql-cockpit                     Open the TUI on this cluster.")
	fmt.Println("  - memql-cockpit-gui worker pair X   Pair a computer using a code from CoPresent.")
	fmt.Println("  - memql-cockpit cluster list        See all configured clusters.")
}

// resolveCluster builds a ClusterConfig either from a discovery URL
// or from explicit flags. Either path produces the same shape; the
// rest of the flow doesn't care which was used.
func resolveCluster(discoveryURL, identityURL, grpcEndpoint, clientId, clusterName, pat string) (config.ClusterConfig, error) {
	identityURL = strings.TrimRight(strings.TrimSpace(identityURL), "/")
	grpcEndpoint = strings.TrimSpace(grpcEndpoint)
	clientId = strings.TrimSpace(clientId)
	clusterName = strings.TrimSpace(clusterName)
	pat = strings.TrimSpace(pat)
	discoveryURL = strings.TrimRight(strings.TrimSpace(discoveryURL), "/")

	// Explicit-flag mode: enough information without discovery.
	if identityURL != "" && grpcEndpoint != "" {
		if clusterName == "" {
			clusterName = hostFromURL(identityURL)
		}
		return config.ClusterConfig{
			Name:     clusterName,
			Endpoint: grpcEndpoint,
			Issuer:   identityURL,
			ClientId: clientId,
			PAT:      pat,
		}, nil
	}

	// Discovery mode: need a URL to dial.
	if discoveryURL == "" {
		return config.ClusterConfig{}, errors.New("authorize requires either a discovery URL (e.g. https://copresent.acme.com) or --identity + --grpc flags")
	}
	doc, err := fetchDiscovery(discoveryURL)
	if err != nil {
		return config.ClusterConfig{}, fmt.Errorf("discovery failed (%w). Re-run with explicit flags:\n    memql-cockpit authorize --identity <url> --grpc <host:port> [--client-id <id>] [--name <name>]", err)
	}

	// Allow per-flag overrides to win over discovery values.
	if identityURL != "" {
		doc.IdentityURL = identityURL
	}
	if grpcEndpoint != "" {
		doc.GRPCEndpoint = grpcEndpoint
	}
	if clientId != "" {
		doc.ClientId = clientId
	}
	if clusterName != "" {
		doc.ClusterName = clusterName
	}

	if doc.IdentityURL == "" || doc.GRPCEndpoint == "" {
		return config.ClusterConfig{}, fmt.Errorf("discovery returned an incomplete config (identityUrl=%q, grpcEndpoint=%q). Re-run with explicit flags.", doc.IdentityURL, doc.GRPCEndpoint)
	}
	if doc.ClusterName == "" {
		doc.ClusterName = hostFromURL(doc.IdentityURL)
	}
	return config.ClusterConfig{
		Name:     doc.ClusterName,
		Endpoint: doc.GRPCEndpoint,
		Issuer:   doc.IdentityURL,
		ClientId: doc.ClientId,
		PAT:      pat,
	}, nil
}

// saveAndLogin runs OAuth (if needed), then persists the cluster on
// success. The login-before-save order means a failed authorize
// leaves clusters.yaml exactly as it was -- a re-run doesn't trip
// the "cluster already exists" guard with a half-broken row.
func saveAndLogin(cfg config.ClusterConfig, force bool) error {
	clusters, err := config.LoadClusters()
	if err != nil {
		return fmt.Errorf("load clusters config: %w", err)
	}
	if existing, ok := clusters.Get(cfg.Name); ok && !force {
		return fmt.Errorf("cluster %q already exists (use --force to overwrite, or --name to register under a different name; existing endpoint is %s)", cfg.Name, existing.Endpoint)
	}

	// Run the auth-flow side-effects first (browser OAuth or PAT
	// validation). On success we know the credential is good; only
	// then do we mutate ~/.memql/clusters.yaml.
	if cfg.PAT == "" {
		fmt.Printf("Opening browser to authenticate with %q (%s)...\n", cfg.Name, cfg.Issuer)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := auth.EnsureValidToken(ctx, cfg); err != nil {
			return fmt.Errorf("OAuth login failed: %w", err)
		}
	}

	// Auth succeeded -- safe to commit the cluster row now. With
	// --force, replace any existing entry in place; otherwise just
	// append (the precondition above already rejected the duplicate
	// case).
	if _, ok := clusters.Get(cfg.Name); ok {
		filtered := clusters.Clusters[:0]
		for _, c := range clusters.Clusters {
			if c.Name == cfg.Name {
				continue
			}
			filtered = append(filtered, c)
		}
		clusters.Clusters = filtered
	}
	clusters.Clusters = append(clusters.Clusters, cfg)
	clusters.SelectedCluster = cfg.Name

	if err := config.SaveClusters(clusters); err != nil {
		return fmt.Errorf("save clusters config: %w", err)
	}

	if cfg.PAT != "" {
		fmt.Printf("Cluster %q registered (auth: PAT).\n", cfg.Name)
	}
	return nil
}

// fetchDiscovery hits <url>/.well-known/memql-config.json and
// parses the response.
func fetchDiscovery(rawURL string) (*discoveryDocument, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme == "" {
		// Default to https; user-facing URLs in 2026 should be TLS.
		parsed.Scheme = "https"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/.well-known/memql-config.json"

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", parsed.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", parsed.String(), resp.StatusCode)
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &doc, nil
}

// hostFromURL extracts the host portion of a URL for use as a
// cluster name. https://copresent.acme.com/foo -> copresent.acme.com
func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	stripped := strings.TrimPrefix(raw, "https://")
	stripped = strings.TrimPrefix(stripped, "http://")
	if i := strings.IndexByte(stripped, '/'); i >= 0 {
		stripped = stripped[:i]
	}
	if i := strings.IndexByte(stripped, ':'); i >= 0 {
		stripped = stripped[:i]
	}
	return stripped
}

func printUsage() {
	fmt.Println("memql-cockpit authorize -- one-shot cluster registration + OAuth login")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql-cockpit authorize <url>")
	fmt.Println("    Discovery-driven: reads <url>/.well-known/memql-config.json for the")
	fmt.Println("    gRPC endpoint + OAuth client_id, then runs the browser login.")
	fmt.Println("")
	fmt.Println("  memql-cockpit authorize --identity <url> --grpc <host:port> [--client-id <id>]")
	fmt.Println("    Explicit-flag form. Use when the cluster doesn't expose the")
	fmt.Println("    well-known discovery endpoint, or for scripted setup.")
	fmt.Println("")
	fmt.Println("FLAGS")
	fmt.Println("  --identity <url>      OIDC issuer URL")
	fmt.Println("  --grpc <host:port>    gRPC endpoint")
	fmt.Println("  --client-id <id>      OAuth2 client id")
	fmt.Println("  --name <name>         Local cluster name (default: derived from URL)")
	fmt.Println("  --pat <token>         Use a Personal Access Token instead of OAuth")
	fmt.Println("  --force               Overwrite an existing cluster of the same name")
	fmt.Println("")
	fmt.Println("EXAMPLES")
	fmt.Println("  memql-cockpit authorize http://localhost:8081      # local-dev (full identity flow)")
	fmt.Println("  memql-cockpit authorize https://copresent.acme.com  # production")
}
