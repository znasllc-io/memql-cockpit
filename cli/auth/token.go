package auth

import (
	"context"
	"fmt"

	"github.com/visionarys-io/memql-cockpit/cli/config"
)

// EnsureValidToken loads a cached token for the cluster, checks expiry,
// and triggers re-authentication if needed. Returns a valid access token.
//
// Resolution priority:
//  1. cluster.PAT  -> the literal PAT string (no expiry; revocation
//                     is server-side via /me/tokens or /admin/tokens)
//  2. cached OIDC  -> if not expired, return it
//  3. fresh OIDC   -> open browser, mint a new pair, cache, return
//
// Every path goes through the identity service. There is NO
// no-auth shortcut -- the dev environment must run the identity
// service like production does, the cockpit must authorize against
// it like any other cluster, and any code path that wants to skip
// auth has to deal with that locally and not via the CLI surface.
func EnsureValidToken(ctx context.Context, cluster config.ClusterConfig) (string, error) {
	// PAT path -- short-circuit the OIDC dance entirely. The PAT IS
	// the bearer token; the server unwraps it via the pat package's
	// Verifier (component/identity/pat/verifier.go).
	if cluster.PAT != "" {
		return cluster.PAT, nil
	}

	if cluster.Issuer == "" || cluster.ClientId == "" {
		return "", fmt.Errorf("cluster %q has no issuer / client_id / PAT configured. Re-run `memql-cockpit authorize <url>` to register it against an identity service", cluster.Name)
	}

	// Try cached token first.
	stored, err := config.LoadToken(cluster.Name)
	if err != nil {
		return "", fmt.Errorf("load cached token: %w", err)
	}

	if stored != nil && !stored.IsExpired() {
		return stored.AccessToken, nil
	}

	// No valid cached token — perform browser login.
	result, err := Login(ctx, cluster.Issuer, cluster.ClientId)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}

	// Cache the token.
	if err := config.SaveToken(cluster.Name, &config.StoredToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		Expiry:       result.Expiry,
	}); err != nil {
		// Non-fatal: token works but won't be cached.
		_ = err
	}

	return result.AccessToken, nil
}

// Logout removes cached credentials for a cluster.
func Logout(clusterName string) error {
	return config.DeleteToken(clusterName)
}
