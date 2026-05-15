package cluster

import "testing"

func TestSplitJoinEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort string
	}{
		{"bare host:port", "localhost:50050", "localhost", "50050"},
		{"bare host no port", "localhost", "localhost", ""},
		{"https full URL", "https://bff.local.znas.io", "https://bff.local.znas.io", ""},
		{"https URL with explicit port", "https://bff.local.znas.io:50443", "https://bff.local.znas.io", "50443"},
		{"grpc plaintext URL", "grpc://localhost:50050", "grpc://localhost", "50050"},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := splitEndpoint(tc.endpoint)
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("split(%q) = (%q, %q), want (%q, %q)", tc.endpoint, host, port, tc.wantHost, tc.wantPort)
			}
			rejoined := joinEndpoint(host, port)
			if tc.endpoint != "" && rejoined != tc.endpoint {
				t.Errorf("join(split(%q)) = %q, want %q", tc.endpoint, rejoined, tc.endpoint)
			}
		})
	}
}
