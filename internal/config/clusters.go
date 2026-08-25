// Package config manages cluster registry and credential storage for the CLI.
// Cluster configuration is stored at ~/.memql/clusters.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterConfig describes a single memQL cluster connection.
//
// Name is the slot key used in clusters.yaml lookups (e.g. "local",
// "staging"). DisplayName is what the row list renders -- typically
// a human-friendly cluster name like "local.znas.io" derived from
// IDENTITY_BOOTSTRAP_DOMAIN, or the `clusterName` returned by a
// discovery doc. Falls back to Name when empty.
type ClusterConfig struct {
	Name        string `yaml:"name"`
	DisplayName string `yaml:"display_name,omitempty"` // Human-friendly name; falls back to Name when empty
	// Domain is the single value the Add/Edit form collects (e.g.
	// "staging.copresent.ai"). Endpoint / Issuer / ClientId below are
	// composed from it by convention (cockpit.<domain> / identity.<domain>
	// / client_id "cockpit"). Stored so the Edit form can round-trip
	// the domain instead of reverse-engineering it from Endpoint.
	// Empty for hand-edited / legacy rows that set the URLs directly.
	Domain   string `yaml:"domain,omitempty"`
	Endpoint string `yaml:"endpoint"`            // gRPC address (host:port)
	Issuer   string `yaml:"issuer,omitempty"`    // OIDC issuer URL
	ClientId string `yaml:"client_id,omitempty"` // OAuth2 client ID
	// PAT is an optional Personal Access Token (mql_pat_<...>) the
	// CLI sends as `Authorization: Bearer <pat>` on every gRPC
	// request. When set, it short-circuits the OIDC browser-login
	// flow -- the token IS the credential. Generate one at
	// /me/tokens on the identity binary.
	PAT string `yaml:"pat,omitempty"`
	// Local marks a cluster as running on the operator's own machine.
	// clusters.yaml is SHARED with the memQL VS Code extension, which
	// gates its "you are about to run a mutation against a non-local
	// cluster" confirmation on this flag. The cockpit does not act on
	// the value itself; it models it so both tools agree on what a
	// cluster is and a cockpit-written entry carries the field instead
	// of silently omitting it (znasllc-io/memql#3313).
	Local bool `yaml:"local,omitempty"`
	// Version is the release that cluster is believed to be running
	// ("v0.18.0"). The memQL VS Code plugin owns the value: it records the
	// version at install and refreshes it opportunistically, because no
	// installed cluster can state its own release -- ServerHello.version is
	// the literal "v1" (the wire protocol) and the engine's release stamp
	// only reaches clusters cut after znasllc-io/memql#3998.
	//
	// The cockpit does not act on the value; it models the key for the same
	// reason it models Local. SaveClusters marshals this struct over the
	// file, so a key that is not a field here is DROPPED on the cockpit's
	// next write -- asserted by TestClusterVersionFieldRoundTrip below. A
	// plugin-recorded version silently vanishing the first time an operator
	// edits a cluster in the cockpit is exactly the failure this field
	// prevents (znasllc-io/memql#3994).
	//
	// Unlike Local there is no absent-versus-false rule to negotiate: a
	// string has no third state, omitempty drops only "", and neither tool
	// writes an empty version. Contract:
	// memql/docs/public/operate/cluster-version-record.md.
	Version string `yaml:"version,omitempty"`

	// Extra carries every key in the entry this Go version does not
	// model, so a cockpit read-modify-write hands them back unchanged
	// (memql-cockpit#333).
	//
	// WHY THIS EXISTS AT ALL. clusters.yaml is SHARED with the memQL VS
	// Code extension, which writes through the yaml Document API and so
	// preserves keys it does not know. The cockpit marshals a struct, and
	// yaml.v3 drops what the struct does not name. Without this field the
	// contract is "every shared key must be modelled in BOTH tools, in
	// lockstep, forever" -- and the failure when that slips is invisible:
	// an operator edits a cluster here and loses a setting they configured
	// in the editor, with no error on either side. `local` and `version`
	// are the two that already had to be added retroactively for exactly
	// that reason; the ones above are the evidence, not the exception.
	//
	// The two tools stay symmetric now: both preserve, neither deletes.
	//
	// THE COST, PAID DELIBERATELY: a map field makes ClusterConfig
	// non-comparable, so `a == b` no longer compiles. That is a feature
	// rather than a wart -- struct equality on a config row was always
	// comparing the modelled subset and silently ignoring the rest, which
	// is the same blind spot in a different costume. Use reflect.DeepEqual
	// where a whole-value comparison is genuinely wanted. The sites the
	// issue named (cli/cluster/cluster_form_test.go) went out with the
	// 2026-08-25 slim-down, so there was nothing left to migrate.
	//
	// A key here must NOT duplicate one of the fields above: yaml.v3
	// errors on a marshal whose inline map collides with a modelled key.
	// Unmarshal cannot produce that -- it routes a known key to its field
	// -- so it only arises if something constructs Extra by hand.
	Extra map[string]any `yaml:",inline"`
}

// Display returns DisplayName if set, otherwise Name. Use this for
// any UI surface that shows the cluster's human-readable label.
func (c ClusterConfig) Display() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// ClustersFile is the top-level structure of ~/.memql/clusters.yaml.
//
// SelectedCluster persists which cluster the user had picked as their
// "working cluster" (via Enter). The CLI restores this selection on
// next launch so Explorer / Agents resume where they left off,
// rather than defaulting to whichever cluster happened to connect
// first. Absent on first run, when it defaults to "local".
type ClustersFile struct {
	Clusters        []ClusterConfig `yaml:"clusters"`
	SelectedCluster string          `yaml:"selected_cluster,omitempty"`

	// Extra is ClusterConfig.Extra's twin for the TOP level of the file.
	// The same tool writes both levels, so a preserved `local:` inside an
	// entry beside a dropped top-level key would be a half-kept promise
	// (memql-cockpit#333).
	Extra map[string]any `yaml:",inline"`
}

// NeedsAuth reports whether the cluster lacks enough credentials to
// dial. Callers use this to short-circuit the connection lifecycle
// (no point retrying for 90 seconds against a server we have no
// bearer for) and to drive the "not configured" state in the TUI.
//
// A cluster is "configured" when it has BOTH an endpoint AND one of:
//   - a PAT (the token IS the credential, no OIDC dance needed)
//   - an OIDC issuer + client_id pair (cockpit can run the auth-code
//     flow against them and produce a token)
//
// An empty endpoint also counts as not-configured: even with auth
// fields set, there's nowhere to dial.
func (c ClusterConfig) NeedsAuth() bool {
	if c.Endpoint == "" {
		return true
	}
	if c.PAT != "" {
		return false
	}
	return c.Issuer == "" || c.ClientId == ""
}

// Get returns the cluster config with the given name.
func (f *ClustersFile) Get(name string) (ClusterConfig, bool) {
	for _, c := range f.Clusters {
		if c.Name == name {
			return c, true
		}
	}
	return ClusterConfig{}, false
}

// DefaultLocalEndpoint is where the `local` cluster is reached when
// clusters.yaml left the endpoint blank (memql#4133). The engine overlay
// advertises https://api.<domain>; locally that is api.memql.localhost.
const DefaultLocalEndpoint = "https://api.memql.localhost"

// WithLocalDefault fills in DefaultLocalEndpoint for the `local` cluster
// when the yaml left it blank. Other names and explicit endpoints pass through.
func WithLocalDefault(c ClusterConfig) ClusterConfig {
	if c.Name == "local" && strings.TrimSpace(c.Endpoint) == "" {
		c.Endpoint = DefaultLocalEndpoint
	}
	return c
}

// ConfigDir returns the memql config directory (~/.memql/).
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".memql"
	}
	return filepath.Join(home, ".memql")
}

// LoadClusters reads the cluster registry from ~/.memql/clusters.yaml.
// Returns an empty registry if the file doesn't exist.
//
// The cluster registry stores PAT bearer tokens in plaintext; the
// file MUST be 0600. VerifyCredentialFileMode rejects loads from
// group- or world-readable files so a drift in permissions surfaces
// loudly instead of silently exposing tokens to other local users.
func LoadClusters() (*ClustersFile, error) {
	path := filepath.Join(ConfigDir(), "clusters.yaml")

	if _, statErr := os.Stat(path); statErr == nil {
		if err := VerifyCredentialFileMode(path); err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ClustersFile{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var file ClustersFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &file, nil
}

// SaveClusters writes the cluster registry to ~/.memql/clusters.yaml.
func SaveClusters(file *ClustersFile) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(file)
	if err != nil {
		return fmt.Errorf("marshal clusters: %w", err)
	}

	path := filepath.Join(dir, "clusters.yaml")
	return os.WriteFile(path, data, 0600)
}
