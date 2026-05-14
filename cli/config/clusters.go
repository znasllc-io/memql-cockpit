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
// SelectedPartition persists which partition the user picked the last
// time they used this cluster. When empty (first-time or never set),
// the CLI defaults to "default". Saved per-cluster so SaaS engineers
// working on staging/acme and prod/acme don't fight the tooling.
type ClusterConfig struct {
	Name              string `yaml:"name"`
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

// ClustersFile is the top-level structure of ~/.memql/clusters.yaml.
//
// SelectedCluster persists which cluster the user had picked as their
// "working cluster" (via Enter). The CLI restores this selection on
// next launch so Explorer / Automations resume where they left off,
// rather than defaulting to whichever cluster happened to connect
// first. Absent on first run, when it defaults to "local".
type ClustersFile struct {
	Clusters        []ClusterConfig `yaml:"clusters"`
	SelectedCluster string          `yaml:"selected_cluster,omitempty"`
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
