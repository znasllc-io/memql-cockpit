package deploy

import (
	"strings"
	"testing"
)

// envMap returns an env lookup func backed by a map, for hermetic tests.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveSurface_Empty(t *testing.T) {
	s := ResolveSurface(envMap(nil))
	if s.HasKubeconfig() || s.HasArgoCD() || s.HasGenesis() {
		t.Fatalf("expected an empty surface, got %+v", s)
	}
	missing := s.MissingRequired()
	if len(missing) != 3 {
		t.Fatalf("expected 3 missing creds, got %d: %v", len(missing), missing)
	}
}

func TestResolveSurface_KubeconfigFallback(t *testing.T) {
	// MEMQL_COCKPIT_KUBECONFIG wins.
	s := ResolveSurface(envMap(map[string]string{
		EnvKubeconfig: "/explicit/kubeconfig",
		"KUBECONFIG":  "/fallback/kubeconfig",
	}))
	if s.KubeconfigPath != "/explicit/kubeconfig" {
		t.Fatalf("explicit var should win, got %q", s.KubeconfigPath)
	}
	// Falls back to KUBECONFIG when the explicit var is unset.
	s = ResolveSurface(envMap(map[string]string{"KUBECONFIG": "/fallback/kubeconfig"}))
	if s.KubeconfigPath != "/fallback/kubeconfig" {
		t.Fatalf("expected KUBECONFIG fallback, got %q", s.KubeconfigPath)
	}
}

func TestResolveSurface_FullyProvisioned(t *testing.T) {
	s := ResolveSurface(envMap(map[string]string{
		EnvKubeconfig:      "/home/runner/.kube/config",
		EnvArgoCDServer:    "https://argocd.staging.internal",
		EnvArgoCDAuthToken: "supersecret-token",
		EnvGenesisEnvelope: "c2VhbGVkLWVudmVsb3Bl",
	}))
	if !s.HasKubeconfig() || !s.HasArgoCD() || !s.HasGenesis() {
		t.Fatalf("expected a fully-provisioned surface, got %+v", s)
	}
	if got := s.MissingRequired(); len(got) != 0 {
		t.Fatalf("expected nothing missing, got %v", got)
	}
}

func TestResolveSurface_GenesisFileForm(t *testing.T) {
	s := ResolveSurface(envMap(map[string]string{
		EnvGenesisEnvelopeFile: "/run/secrets/genesis.env",
	}))
	if !s.HasGenesis() {
		t.Fatalf("file form should satisfy HasGenesis")
	}
}

func TestSummary_RedactsSecrets(t *testing.T) {
	token := "supersecret-argocd-token"
	envelope := "c2VhbGVkLWVudmVsb3Bl-secret"
	s := ResolveSurface(envMap(map[string]string{
		EnvKubeconfig:      "/home/runner/.kube/config",
		EnvArgoCDServer:    "https://argocd.staging.internal",
		EnvArgoCDAuthToken: token,
		EnvGenesisEnvelope: envelope,
	}))
	out := s.Summary()
	if strings.Contains(out, token) {
		t.Fatalf("Summary leaked the ArgoCD token: %q", out)
	}
	if strings.Contains(out, envelope) {
		t.Fatalf("Summary leaked the genesis envelope: %q", out)
	}
	// Non-secret context is fine to show.
	if !strings.Contains(out, "https://argocd.staging.internal") {
		t.Fatalf("expected the non-secret ArgoCD server URL in the summary: %q", out)
	}
	if !strings.Contains(out, "genesis=ready") {
		t.Fatalf("expected genesis=ready, got %q", out)
	}
}

func TestApply_IsRedactedNoOp(t *testing.T) {
	s := ResolveSurface(envMap(map[string]string{EnvArgoCDAuthToken: "secret"}))
	if got := s.Apply(); got != s.Summary() {
		t.Fatalf("Apply should return the redacted Summary, got %q", got)
	}
}
