package deploy

import (
	"fmt"
	"os"
	"strings"
)

// surface.go wires the `cockpit/runner` deployment surface to its runtime
// credentials (I17 / memql#2228). A deploy initiated from an operator
// machine or a CI runner needs three things to actually reach a target
// cluster, all sourced from the environment (CI secrets / Key Vault), never
// committed:
//
//   - a kubeconfig (cluster API access),
//   - ArgoCD server + auth token (GitOps sync), and
//   - the sealed genesis envelope (the cluster's env/secret bundle).
//
// This file resolves those from a documented env-var contract and produces a
// REDACTED readiness summary. It deliberately holds no secret material beyond
// the process environment and never logs secret values.
//
// The credentials are CONSUMED by the deployEngineCluster automation's
// capability actions, which resolve to the runner surface that lands fully
// with I13 (memql#2220). Until then, ResolveSurface proves the wiring (the
// env contract + presence checks + redacted reporting) without itself
// dialing a cluster. See the TODO(I13) in Apply.
//
// Naming follows the env-config registry convention (MEMQL_<COMPONENT>_*,
// component = COCKPIT). The kubeconfig var falls back to the standard
// KUBECONFIG so an operator's existing shell config is honored.

const (
	// EnvKubeconfig is the path to the kubeconfig granting cluster API
	// access for the target environment. Falls back to KUBECONFIG.
	EnvKubeconfig = "MEMQL_COCKPIT_KUBECONFIG"

	// EnvArgoCDServer is the ArgoCD API server URL (e.g.
	// https://argocd.staging.internal). Non-secret.
	EnvArgoCDServer = "MEMQL_COCKPIT_ARGOCD_SERVER"

	// EnvArgoCDAuthToken is the ArgoCD auth token. SECRET — never logged.
	EnvArgoCDAuthToken = "MEMQL_COCKPIT_ARGOCD_AUTH_TOKEN" //nolint:gosec // env var name, not a credential

	// EnvGenesisEnvelope carries the sealed genesis envelope inline
	// (base64). SECRET — never logged.
	EnvGenesisEnvelope = "MEMQL_COCKPIT_GENESIS_ENVELOPE"

	// EnvGenesisEnvelopeFile is a path to a file holding the sealed genesis
	// envelope, used when mounting a secret as a file is preferred over an
	// inline env value. Mutually exclusive with EnvGenesisEnvelope; the
	// inline form wins when both are set.
	EnvGenesisEnvelopeFile = "MEMQL_COCKPIT_GENESIS_ENVELOPE_FILE" //nolint:gosec // env var name, not a credential
)

// Surface is the resolved runner-surface credential set for one deploy. It
// records ONLY what is needed to report readiness and (later) hand off to the
// runner capability actions; secret values are held but never serialized by
// Summary.
type Surface struct {
	KubeconfigPath  string // resolved kubeconfig path (may be empty)
	ArgoCDServer    string // ArgoCD API server URL (non-secret)
	argoCDAuthToken string // SECRET
	genesisEnvelope string // SECRET (inline base64)
	GenesisFile     string // path form, when used instead of the inline value
}

// ResolveSurface reads the documented env-var contract into a Surface. It is
// pure (no I/O beyond reading env + an optional envelope-file existence
// check) and never errors: presence/validation is reported via Summary and
// MissingRequired so the no-op / dry-run paths stay fully exercisable on a
// runner with no real creds.
func ResolveSurface(env func(string) string) Surface {
	if env == nil {
		env = os.Getenv
	}
	s := Surface{
		KubeconfigPath:  strings.TrimSpace(env(EnvKubeconfig)),
		ArgoCDServer:    strings.TrimSpace(env(EnvArgoCDServer)),
		argoCDAuthToken: strings.TrimSpace(env(EnvArgoCDAuthToken)),
		genesisEnvelope: strings.TrimSpace(env(EnvGenesisEnvelope)),
		GenesisFile:     strings.TrimSpace(env(EnvGenesisEnvelopeFile)),
	}
	if s.KubeconfigPath == "" {
		s.KubeconfigPath = strings.TrimSpace(env("KUBECONFIG"))
	}
	return s
}

// HasKubeconfig reports whether a kubeconfig path is configured.
func (s Surface) HasKubeconfig() bool { return s.KubeconfigPath != "" }

// HasArgoCD reports whether both the ArgoCD server and auth token are set.
func (s Surface) HasArgoCD() bool { return s.ArgoCDServer != "" && s.argoCDAuthToken != "" }

// HasGenesis reports whether the genesis envelope is available, inline or by
// file path.
func (s Surface) HasGenesis() bool { return s.genesisEnvelope != "" || s.GenesisFile != "" }

// MissingRequired returns the human-readable names of the credentials a LIVE
// (non-dry-run) deploy needs but that are absent. An empty slice means the
// surface is fully provisioned. Dry-run / no-op deploys ignore this.
func (s Surface) MissingRequired() []string {
	var missing []string
	if !s.HasKubeconfig() {
		missing = append(missing, EnvKubeconfig+" (or KUBECONFIG)")
	}
	if !s.HasArgoCD() {
		missing = append(missing, EnvArgoCDServer+" + "+EnvArgoCDAuthToken)
	}
	if !s.HasGenesis() {
		missing = append(missing, EnvGenesisEnvelope+" (or "+EnvGenesisEnvelopeFile+")")
	}
	return missing
}

// Summary renders a single-line, REDACTED readiness report safe to print to
// stdout and capture in CI logs. Secret values are reduced to present/absent;
// only the non-secret ArgoCD server URL and kubeconfig PATH are shown.
func (s Surface) Summary() string {
	yn := func(ok bool) string {
		if ok {
			return "ready"
		}
		return "absent"
	}
	kube := "absent"
	if s.HasKubeconfig() {
		kube = "ready(" + s.KubeconfigPath + ")"
	}
	argo := "absent"
	if s.HasArgoCD() {
		argo = "ready(" + s.ArgoCDServer + ")"
	}
	return fmt.Sprintf("runner surface: kubeconfig=%s argocd=%s genesis=%s",
		kube, argo, yn(s.HasGenesis()))
}

// Apply is the hand-off point: it would inject the resolved credentials into
// the environment/context the runner capability actions consume. Today it is
// a no-op that returns the redacted Summary, because the runner surface that
// consumes these (I13 / memql#2220) is not wired yet.
//
// TODO(I13, memql#2220): once the deployEngineCluster capability actions
// resolve to the runner surface, materialize the genesis-envelope file (when
// only the inline form is set), export the kubeconfig/ArgoCD context for the
// child capability scripts, and return a typed handle instead of a string.
func (s Surface) Apply() string {
	return s.Summary()
}
