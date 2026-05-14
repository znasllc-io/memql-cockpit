package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StoredToken holds a cached OAuth2 token for a cluster.
type StoredToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// IsExpired returns true if the token has expired or will expire within 5 minutes.
func (t *StoredToken) IsExpired() bool {
	return time.Now().Add(5 * time.Minute).After(t.Expiry)
}

// credentialsDir returns the path to ~/.memql/credentials/.
func credentialsDir() string {
	return filepath.Join(ConfigDir(), "credentials")
}

// LoadToken reads a cached token for the given cluster.
func LoadToken(clusterName string) (*StoredToken, error) {
	path := filepath.Join(credentialsDir(), clusterName+".json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token: %w", err)
	}

	var token StoredToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	return &token, nil
}

// SaveToken writes a token to ~/.memql/credentials/<cluster>.json.
func SaveToken(clusterName string, token *StoredToken) error {
	dir := credentialsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}

	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}

	path := filepath.Join(dir, clusterName+".json")
	return os.WriteFile(path, data, 0600)
}

// DeleteToken removes the cached token for a cluster.
func DeleteToken(clusterName string) error {
	path := filepath.Join(credentialsDir(), clusterName+".json")
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
