// Package config manages cluster registry and credential storage for the CLI.
// Cluster configuration is stored at ~/.memql/clusters.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ClusterConfig describes a single memQL cluster connection.
//
// Name is the slot key used in clusters.yaml lookups (e.g. "local",
// "staging"). DisplayName is what the row list renders -- typically
// a human-friendly cluster name like "local.znas.io" derived from
// IDENTITY_BOOTSTRAP_DOMAIN, or the `clusterName` returned by a
// discovery doc. Falls back to Name when empty.
//
// SelectedPartition persists which partition the user picked the last
// time they used this cluster. When empty (first-time or never set),
// the CLI defaults to "default". Saved per-cluster so SaaS engineers
// working on staging/acme and prod/acme don't fight the tooling.
type ClusterConfig struct {
	Name              string `yaml:"name"`
	DisplayName       string `yaml:"display_name,omitempty"`        // Human-friendly name; falls back to Name when empty
	Endpoint          string `yaml:"endpoint"`                      // gRPC address (host:port)
	Issuer            string `yaml:"issuer,omitempty"`              // OIDC issuer URL
	ClientId          string `yaml:"client_id,omitempty"`           // OAuth2 client ID
	SelectedPartition string `yaml:"selected_partition,omitempty"`  // Per-cluster sticky partition
	// PAT is an optional Personal Access Token (mql_pat_<...>) the
	// CLI sends as `Authorization: Bearer <pat>` on every gRPC
	// request. When set, it short-circuits the OIDC browser-login
	// flow -- the token IS the credential. Generate one at
	// /me/tokens on the identity binary.
	PAT string `yaml:"pat,omitempty"`
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
func LoadClusters() (*ClustersFile, error) {
	path := filepath.Join(ConfigDir(), "clusters.yaml")

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
