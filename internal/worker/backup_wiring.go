package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/auth"
	"github.com/znasllc-io/memql-cockpit/internal/config"
)

// How the watched-folder sweeper gets an origin and a credential
// (memql#4841).
//
// ===========================================================================
// THE CREDENTIAL IS THE SIGNED-IN USER'S, AND IT HAS TO BE
// ===========================================================================
// The Library's HTTP routes resolve an actor only for a `class="user"` (or
// classless) bearer -- the engine's own http_access.go pins every machine
// class OFF that surface deliberately, so byte-storing writes cannot be
// reached by a credential whose gRPC pin denies them everywhere else. The
// worker token this process authenticates its STREAM with is one of those:
// `mql_wkr_` is admitted on WorkerService and nowhere else, and no HTTP
// middleware anywhere reads it.
//
// So the sweeper runs as the person, through the cockpit's ordinary OIDC
// session. That is the arrangement that makes row authorization here exactly
// the browser's -- the same reads, the same writes, the same refusals -- with
// no second story to keep in step.
//
// A PAT DOES NOT WORK EITHER, and this is worth knowing before debugging a
// 401: PATs verify only on the identity node (the per-node verifier is built
// with a nil PATVerifier), so a cluster configured with a PAT alone can dial
// gRPC and cannot reach `/artifacts`. Both clients name that case in their 401
// message rather than leaving somebody to find it.

// backupBaseURL derives the cluster's HTTP origin from the worker's own
// cluster_url.
//
// THERE IS NO SECOND HOST TO CONFIGURE. The Library routes and the query
// gateway are served by the same front door the worker already dials, and
// inventing a knob for them would be one an operator has to keep in sync with
// something they cannot see. Same derivation appsession.LibraryBaseURL makes,
// restated here rather than imported: coupling the app-session package and the
// backup package for one string would tie two subsystems that have nothing
// else to say to each other.
func backupBaseURL(clusterURL string) string {
	raw := strings.TrimSpace(clusterURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "https://" + strings.TrimSuffix(raw, ":443")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	switch strings.ToLower(u.Scheme) {
	case "grpc", "http":
		scheme = "http"
	}
	return scheme + "://" + strings.TrimSuffix(u.Host, ":443")
}

// backupBearer resolves the signed-in user's access token for the cluster this
// worker is pointed at, or returns nil when there is no way to get one.
//
// A NIL RESULT DISABLES THE FEATURE, quietly. A machine that is paired but not
// signed in is the ordinary state of a freshly paired worker, and refusing to
// start the worker over it would make backups a prerequisite for computer use.
//
// NON-INTERACTIVE, always. This runs under a LaunchAgent or a systemd unit,
// where the browser step is not slow -- it is a process blocked forever on a
// window that will never open. The refresh path is unaffected and still rolls
// an expired token forward silently; only the last resort differs, and it says
// to run `memql login` here.
func backupBearer(clusterURL string, logger *slog.Logger) func(context.Context) (string, error) {
	cluster, ok := clusterFor(clusterURL)
	if !ok {
		if logger != nil {
			logger.Debug("backup: this worker's cluster is not in clusters.yaml, so nothing is backed up",
				"cluster_url", clusterURL)
		}
		return nil
	}
	return func(ctx context.Context) (string, error) {
		token, err := auth.EnsureValidTokenNonInteractive(ctx, cluster, logger)
		if err != nil {
			return "", fmt.Errorf("backup: no usable sign-in for cluster %q: %w", cluster.Name, err)
		}
		return token, nil
	}
}

// clusterFor finds the registered cluster this worker is pointed at.
//
// MATCHED ON HOST, not on the string. The worker's cluster_url and the
// registry's endpoint describe the same front door and are routinely written
// differently -- "api.example.com:443" against "https://api.example.com" --
// and a string comparison would silently find nothing and disable the feature
// with no error anybody could act on.
func clusterFor(clusterURL string) (config.ClusterConfig, bool) {
	want := hostOf(clusterURL)
	if want == "" {
		return config.ClusterConfig{}, false
	}
	file, err := config.LoadClusters()
	if err != nil || file == nil {
		return config.ClusterConfig{}, false
	}
	for _, entry := range file.Clusters {
		resolved := config.WithLocalDefault(entry)
		if hostOf(resolved.Endpoint) == want {
			return resolved, true
		}
	}
	return config.ClusterConfig{}, false
}

func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Host, ":443"))
}
