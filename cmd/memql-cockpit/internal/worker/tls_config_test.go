package worker

import (
	"crypto/tls"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildTLSConfig_PinsServerName confirms that the configured host
// shows up as ServerName so the TLS handshake validates the
// certificate against the expected FQDN (not whatever DNS resolved
// to).
func TestBuildTLSConfig_PinsServerName(t *testing.T) {
	t.Setenv(envWorkerCACertFile, "")
	t.Setenv(envWorkerClientCertFile, "")
	t.Setenv(envWorkerClientKeyFile, "")
	t.Setenv(envWorkerTLSInsecureSkipVerify, "")

	cfg, err := buildTLSConfig("bff.example.com:443", discardLogger())
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg.ServerName != "bff.example.com" {
		t.Fatalf("ServerName = %q, want %q", cfg.ServerName, "bff.example.com")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify defaulted to true; must be false")
	}
}

func TestBuildTLSConfig_RejectsEmptyHost(t *testing.T) {
	_, err := buildTLSConfig("", discardLogger())
	if err == nil {
		t.Fatalf("expected error for empty host")
	}
}

// TestBuildTLSConfig_InsecureSkipVerifyEscapeHatch verifies the dev
// opt-in but also asserts no production drift: the env var must be
// set to the literal "1" (not "true" / "yes" / etc.) and must emit a
// WARN log.
func TestBuildTLSConfig_InsecureSkipVerifyEscapeHatch(t *testing.T) {
	t.Setenv(envWorkerTLSInsecureSkipVerify, "1")

	cfg, err := buildTLSConfig("bff.example.com", discardLogger())
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify=true under the escape hatch")
	}
}

// TestBuildTLSConfig_RejectsHalfConfiguredClientCerts confirms that
// passing only one of the cert/key file env vars produces an error
// at load time rather than silently dropping mTLS.
func TestBuildTLSConfig_RejectsHalfConfiguredClientCerts(t *testing.T) {
	t.Setenv(envWorkerClientCertFile, "/tmp/cert.pem")
	t.Setenv(envWorkerClientKeyFile, "")
	t.Setenv(envWorkerTLSInsecureSkipVerify, "")

	_, err := buildTLSConfig("bff.example.com", discardLogger())
	if err == nil {
		t.Fatalf("expected error when only the cert env var is set")
	}
	if !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("error %q does not mention the must-set-together constraint", err.Error())
	}
}

// TestVerifyPrivateKeyFileMode_RejectsPermissive writes a fake key
// file at mode 0644 and asserts the validator rejects it.
func TestVerifyPrivateKeyFileMode_RejectsPermissive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.key")
	if err := os.WriteFile(path, []byte("not a key, just a file"), 0o644); err != nil {
		t.Fatalf("write fake key: %v", err)
	}
	if err := verifyPrivateKeyFileMode(path); err == nil {
		t.Fatalf("expected rejection for 0644 key file")
	} else if !strings.Contains(err.Error(), "must be 0600") {
		t.Fatalf("error %q does not name the required mode", err.Error())
	}
}

// TestVerifyPrivateKeyFileMode_AcceptsSecure writes a fake key file
// at mode 0600 and asserts the validator accepts it.
func TestVerifyPrivateKeyFileMode_AcceptsSecure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.key")
	if err := os.WriteFile(path, []byte("not a key, just a file"), 0o600); err != nil {
		t.Fatalf("write fake key: %v", err)
	}
	if err := verifyPrivateKeyFileMode(path); err != nil {
		t.Fatalf("expected 0600 key file to pass, got: %v", err)
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"host":          "host",
		"host:443":      "host",
		"bff.local:50050": "bff.local",
		"":            "",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}
