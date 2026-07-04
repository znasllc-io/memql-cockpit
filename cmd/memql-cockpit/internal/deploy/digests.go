package deploy

// D5 (memql#2381): emitter tag->digest resolution for the staging/production
// path. The deployEngineCluster bundle's GitOps placement (pinOverlayDigests +
// argoSync) needs payload.digests (nodeType -> image@sha256); images for those
// environments are built ONLY on the GitHub build server and pushed to ACR
// (the hard build rule), so digests resolve from ACR by tag -- the same source
// scripts/release/assemble-lockfile.sh uses. Development never needs digests
// (local build + k3d import by tag).

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const defaultACRRegistry = "acrmemql"

// acrDigestLookup resolves one image tag to its manifest digest. Swappable
// for tests; the default shells to `az acr repository show` (the operator /
// CI machine already holds the OIDC/az session -- the assemble-lockfile
// precedent).
var acrDigestLookup = func(registry, repository, tag string) (string, error) {
	out, err := exec.Command("az", "acr", "repository", "show",
		"--name", registry,
		"--image", fmt.Sprintf("%s:%s", repository, tag),
		"--query", "digest", "-o", "tsv").Output()
	if err != nil {
		return "", fmt.Errorf("az acr repository show %s:%s: %w", repository, tag, err)
	}
	digest := strings.TrimSpace(string(out))
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("unexpected digest for %s:%s: %q", repository, tag, digest)
	}
	return digest, nil
}

// resolveStagingDigests populates inv.input["digests"] (nodeType ->
// image@sha256) and the overlayPath default for a staging/production deploy.
// No-ops when: the env is development (never needs digests), the operator
// already supplied digests via --input, or the invocation stops at resolution
// (--dry-run). A resolution failure is a hard error -- a GitOps deploy
// without pinned digests must never proceed silently.
func resolveStagingDigests(inv *invocation) error {
	if inv.env == "development" || inv.dryRun {
		return nil
	}
	registry := os.Getenv("MEMQL_DEPLOY_ACR")
	if registry == "" {
		registry = defaultACRRegistry
	}
	if _, ok := inv.input["overlayPath"]; !ok {
		inv.input["overlayPath"] = "deploy/k8s/overlays/" + inv.env
	}
	if _, ok := inv.input["digests"]; ok {
		return nil
	}
	version, _ := inv.input["version"].(string)
	if version == "" {
		return fmt.Errorf("digest resolution needs a version tag: pass --ref or --input version")
	}
	nodeTypes := engineNodeTypeStrings(inv.input["engineNodeTypes"])
	if len(nodeTypes) == 0 {
		return fmt.Errorf("digest resolution needs engineNodeTypes")
	}
	digests := map[string]any{}
	for _, nt := range nodeTypes {
		repo := "memql-" + nt
		digest, err := acrDigestLookup(registry, repo, version)
		if err != nil {
			return fmt.Errorf("resolve digest for %s (registry %s): %w -- staging/production images build on the GitHub build server; is the tag pushed?", repo, registry, err)
		}
		digests[nt] = fmt.Sprintf("%s.azurecr.io/%s@%s", registry, repo, digest)
	}
	inv.input["digests"] = digests
	fmt.Fprintf(os.Stderr, "INFO: resolved %d image digests from %s for %s (version %s):\n", len(digests), registry, inv.env, version)
	for _, nt := range nodeTypes {
		fmt.Fprintf(os.Stderr, "INFO:   %s -> %s\n", nt, digests[nt])
	}
	return nil
}

func engineNodeTypeStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
