package config

import "testing"

func TestClustersFileGet(t *testing.T) {
	file := &ClustersFile{
		Clusters: []ClusterConfig{
			{Name: "local", Endpoint: "localhost:50051"},
			{Name: "staging", Endpoint: "staging.example.com:50051"},
		},
	}

	t.Run("found", func(t *testing.T) {
		c, ok := file.Get("local")
		if !ok {
			t.Fatal("expected to find 'local'")
		}
		if c.Endpoint != "localhost:50051" {
			t.Errorf("expected localhost:50051, got %s", c.Endpoint)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := file.Get("production")
		if ok {
			t.Error("expected 'production' not to be found")
		}
	})
}

func TestStoredTokenExpiry(t *testing.T) {
	t.Run("zero time is expired", func(t *testing.T) {
		token := &StoredToken{}
		if !token.IsExpired() {
			t.Error("zero expiry should be expired")
		}
	})
}
