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

// expiryBuffer is the slack window IsExpired enforces ahead of the
// actual Expiry timestamp. Treating tokens as "expired" a bit early
// keeps us from handing out an access token that will be rejected by
// the server seconds later (clock skew + in-flight latency). 60s is
// generous enough to absorb both while remaining small enough to be
// shorter than the smallest TTL operators are likely to set: the
// identity service caps the access TTL at 60s on the low end
// (MinAccessTokenTTLSeconds in memql core), so anything tighter would
// make every cached token look expired the moment it's saved.
//
// History: this used to be 5 minutes, which silently broke the
// refresh path under short-TTL test configs (3-min access tokens
// would always look expired, and pre-refresh-wiring that meant the
// browser flow fired on every dial). When we ship the refresh path,
// the buffer needs to be < TTL or refresh never runs proactively.
const expiryBuffer = 60 * time.Second

// IsExpired returns true if the token has expired or will expire
// within expiryBuffer.
func (t *StoredToken) IsExpired() bool {
	return time.Now().Add(expiryBuffer).After(t.Expiry)
}

// credentialsDir returns the path to ~/.memql/credentials/.
func credentialsDir() string {
	return filepath.Join(ConfigDir(), "credentials")
}

// LoadToken reads a cached token for the given cluster.
//
// The token file stores OAuth access + refresh tokens in plaintext;
// it MUST be 0600. VerifyCredentialFileMode rejects loads from a
// group- or world-readable file.
func LoadToken(clusterName string) (*StoredToken, error) {
	path := filepath.Join(credentialsDir(), clusterName+".json")

	if _, statErr := os.Stat(path); statErr == nil {
		if err := VerifyCredentialFileMode(path); err != nil {
			return nil, err
		}
	}

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
