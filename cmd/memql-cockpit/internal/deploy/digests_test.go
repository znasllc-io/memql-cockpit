package deploy

// D5 (memql#2381): emitter tag->digest resolution pin tests.

import (
	"fmt"
	"testing"
)

func withFakeACR(t *testing.T, fn func(registry, repo, tag string) (string, error)) {
	t.Helper()
	orig := acrDigestLookup
	acrDigestLookup = fn
	t.Cleanup(func() { acrDigestLookup = orig })
}

func TestResolveStagingDigests(t *testing.T) {
	withFakeACR(t, func(registry, repo, tag string) (string, error) {
		return "sha256:" + repo + "-" + tag, nil
	})

	inv := invocation{env: "staging", input: map[string]any{
		"version": "0.12.0", "engineNodeTypes": []string{"identity", "mcp"},
	}}
	if err := resolveStagingDigests(&inv); err != nil {
		t.Fatal(err)
	}
	d, _ := inv.input["digests"].(map[string]any)
	want := "acrmemql.azurecr.io/memql-identity@sha256:memql-identity-0.12.0"
	if d["identity"] != want {
		t.Fatalf("identity digest = %v, want %v", d["identity"], want)
	}
	if inv.input["overlayPath"] != "deploy/k8s/overlays/staging" {
		t.Fatalf("overlayPath default = %v", inv.input["overlayPath"])
	}

	// development never resolves; --dry-run stops at resolution.
	for _, inv := range []invocation{
		{env: "development", input: map[string]any{"version": "x", "engineNodeTypes": []string{"mcp"}}},
		{env: "staging", dryRun: true, input: map[string]any{"version": "x", "engineNodeTypes": []string{"mcp"}}},
	} {
		if err := resolveStagingDigests(&inv); err != nil {
			t.Fatal(err)
		}
		if _, ok := inv.input["digests"]; ok {
			t.Fatalf("env=%s dryRun=%v must not resolve digests", inv.env, inv.dryRun)
		}
	}

	// operator-supplied digests win.
	inv = invocation{env: "staging", input: map[string]any{
		"version": "0.12.0", "engineNodeTypes": []string{"mcp"}, "digests": map[string]any{"mcp": "pinned"},
	}}
	if err := resolveStagingDigests(&inv); err != nil {
		t.Fatal(err)
	}
	if d, _ := inv.input["digests"].(map[string]any); d["mcp"] != "pinned" {
		t.Fatal("explicit --input digests must win")
	}

	// resolution failure is a hard error.
	withFakeACR(t, func(registry, repo, tag string) (string, error) {
		return "", fmt.Errorf("no such tag")
	})
	inv = invocation{env: "production", input: map[string]any{"version": "ghost", "engineNodeTypes": []string{"mcp"}}}
	if err := resolveStagingDigests(&inv); err == nil {
		t.Fatal("unresolvable digest must fail the deploy loudly")
	}
}
