// Package discovery resolves a cluster's connection metadata from
// the well-known endpoint identity services expose at
// /.well-known/memql-config.json. Cockpit hits that URL to discover
// the gRPC endpoint, OIDC client_id, and human cluster name without
// asking the operator to type each value separately.
//
// The package is a CLIENT of identity's discovery handler; it
// declares its own JSON schema so importing it doesn't drag in the
// entire identity binary's dependency tree. Stays in sync with
// memql/component/identity/discovery.go.DiscoveryDocument by
// convention.
package discovery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Document mirrors the schema served at /.well-known/memql-config.json.
type Document struct {
	IdentityURL  string `json:"identityUrl"`
	GRPCEndpoint string `json:"grpcEndpoint"`
	ClientId     string `json:"clientId"`
	ClusterName  string `json:"clusterName"`
}

// Fetch hits <rawURL>/.well-known/memql-config.json and parses the
// response. rawURL may be a full https URL or a bare host; missing
// scheme defaults to https (user-facing URLs in 2026 are TLS).
func Fetch(rawURL string) (*Document, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "https"
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/.well-known/memql-config.json"

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", parsed.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", parsed.String(), resp.StatusCode)
	}
	var doc Document
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &doc, nil
}

// HostFromURL extracts the host portion of a URL for use as a
// cluster name. https://copresent.acme.com/foo -> copresent.acme.com.
// Returns the empty string when raw is empty.
func HostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	stripped := strings.TrimPrefix(raw, "https://")
	stripped = strings.TrimPrefix(stripped, "http://")
	if i := strings.IndexByte(stripped, '/'); i >= 0 {
		stripped = stripped[:i]
	}
	if i := strings.IndexByte(stripped, ':'); i >= 0 {
		stripped = stripped[:i]
	}
	return stripped
}
