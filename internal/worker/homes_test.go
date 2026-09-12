package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomeIDFromURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://api.memql.znas.io":        "api.memql.znas.io",
		"https://api.memql.znas.io/":       "api.memql.znas.io",
		"https://API.Example.COM:443/path": "api.example.com",
		"http://localhost:8080":            "localhost",
		"https://api.memql.localhost":      "api.memql.localhost",
		"":                                 "default",
	}
	for in, want := range cases {
		if got := HomeIDFromURL(in); got != want {
			t.Errorf("HomeIDFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadWorkersMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "worker.yaml")
	workers := filepath.Join(dir, "workers.yaml")
	body := []byte(`cluster_url: https://api.memql.znas.io
token: mql_wkr_testtoken1234567890
name: znas-mac
capabilities:
  - HEADLESS
`)
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWorkers(workers, legacy)
	if err != nil {
		t.Fatalf("LoadWorkers: %v", err)
	}
	if _, err := os.Stat(workers); err != nil {
		t.Fatalf("expected workers.yaml to be written: %v", err)
	}
	// Legacy must remain (never wipe tokens).
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy worker.yaml must remain after migrate: %v", err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d, want 1", len(w.Homes))
	}
	if w.Homes[0].ID != "api.memql.znas.io" {
		t.Errorf("migrated id = %q, want api.memql.znas.io", w.Homes[0].ID)
	}
	if w.Homes[0].Token != "mql_wkr_testtoken1234567890" {
		t.Errorf("token not preserved")
	}
	if w.WorkerName != "znas-mac" {
		t.Errorf("worker_name = %q", w.WorkerName)
	}
	// Second load reads workers.yaml, does not re-migrate.
	w2, err := LoadWorkers(workers, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(w2.Homes) != 1 || w2.Homes[0].ID != w.Homes[0].ID {
		t.Errorf("second load drifted: %+v", w2.Homes)
	}
}

func TestUpsertHomeAdditive(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")

	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "production",
		ClusterURL:  "https://api.memql.znas.io",
		Token:       "mql_wkr_prod_aaaaaaaaaaaa",
		Name:        "znas-mac",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d", len(w.Homes))
	}

	w, err = UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "local",
		ClusterURL:  "https://api.memql.localhost",
		Token:       "mql_wkr_local_bbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 2 {
		t.Fatalf("expected 2 homes after additive upsert, got %d", len(w.Homes))
	}
	ids := map[string]bool{}
	for _, h := range w.Homes {
		ids[h.ID] = true
	}
	if !ids["production"] || !ids["local"] {
		t.Fatalf("missing sibling: %+v", w.Homes)
	}
	// Force replaces THAT home only.
	w, err = UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "local",
		ClusterURL:  "https://api.memql.localhost",
		Token:       "mql_wkr_local_cccccccccccc",
		Force:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 2 {
		t.Fatalf("force must not drop siblings, got %d", len(w.Homes))
	}
	for _, h := range w.Homes {
		if h.ID == "local" && h.Token != "mql_wkr_local_cccccccccccc" {
			t.Errorf("local token not replaced")
		}
		if h.ID == "production" && h.Token != "mql_wkr_prod_aaaaaaaaaaaa" {
			t.Errorf("production token clobbered")
		}
	}
}

func TestRemoveHome(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "a", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "b", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	w, err := RemoveHome(workers, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 || w.Homes[0].ID != "b" {
		t.Fatalf("after remove: %+v", w.Homes)
	}
	w, err = RemoveHome(workers, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 || w.Homes[0].IsEnabled() {
		t.Fatalf("disableOnly left enabled: %+v", w.Homes[0])
	}
	if err := w.ValidateRun(); err == nil {
		t.Fatal("ValidateRun should fail with no enabled homes")
	}
}

func TestWorkersFileValidate(t *testing.T) {
	t.Parallel()
	enabled := true
	w := WorkersFile{
		Version:      1,
		WorkerName:   "mac",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{{
			ID: "x", ClusterURL: "https://x", Token: "not-a-worker-token", Enabled: &enabled,
		}},
	}
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "mql_wkr_") {
		t.Fatalf("want token prefix error, got %v", err)
	}
}

func TestConfigForHome(t *testing.T) {
	t.Parallel()
	enabled := true
	w := WorkersFile{
		Version:      1,
		WorkerName:   "znas-mac",
		LogLevel:     "debug",
		StateDir:     "/tmp/state",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 4},
		Homes: []Home{{
			ID: "production", ClusterURL: "https://api.example", Token: "mql_wkr_zzzzzzzzzzzzzz", Enabled: &enabled,
		}},
	}
	cfg := w.ConfigForHome(w.Homes[0])
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "znas-mac" || cfg.ClusterURL != "https://api.example" {
		t.Fatalf("projection: %+v", cfg)
	}
	wantState := filepath.Join("/tmp/state", "homes", "production")
	if cfg.StateDir != wantState {
		t.Fatalf("StateDir = %q, want namespaced %q", cfg.StateDir, wantState)
	}
}

func TestConfigForHomeNamespacesDistinctHomes(t *testing.T) {
	t.Parallel()
	on := true
	w := WorkersFile{
		Version: 1, WorkerName: "mac", StateDir: "/var/memql/state",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{
			{ID: "a.example", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa", Enabled: &on},
			{ID: "b.example", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb", Enabled: &on},
		},
	}
	ca := w.ConfigForHome(w.Homes[0])
	cb := w.ConfigForHome(w.Homes[1])
	if ca.StateDir == cb.StateDir {
		t.Fatalf("homes share StateDir %q; want distinct namespaces", ca.StateDir)
	}
	if !strings.HasSuffix(ca.StateDir, filepath.Join("homes", "a.example")) {
		t.Fatalf("home a StateDir = %q", ca.StateDir)
	}
	if !strings.HasSuffix(cb.StateDir, filepath.Join("homes", "b.example")) {
		t.Fatalf("home b StateDir = %q", cb.StateDir)
	}
}

func TestSanitizeHomeID(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                "default",
		"api.example.com": "api.example.com",
		"prod/west":       "prod_west",
		"..":              "default",
		"a b":             "a_b",
	}
	for in, want := range cases {
		if got := sanitizeHomeID(in); got != want {
			t.Errorf("sanitizeHomeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveWorkersMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workers.yaml")
	enabled := true
	w := WorkersFile{
		Version: 1, WorkerName: "n", Capabilities: []string{"HEADLESS"},
		Homes: []Home{{ID: "h", ClusterURL: "https://h", Token: "mql_wkr_hhhhhhhhhhhhhh", Enabled: &enabled}},
	}
	if err := SaveWorkers(path, w); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %04o; want 0600", info.Mode().Perm())
	}
}

func TestUpsertHomeSameURLDifferentIDNoForce(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")

	// Pair path: explicit --home-id local for api.memql.localhost.
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.memql.localhost",
		Token: "mql_wkr_local_bbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}

	// Install path: empty ID → HomeIDFromURL = api.memql.localhost.
	// Same cluster_url must refresh WITHOUT --force and keep id "local".
	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ClusterURL: "https://api.memql.localhost",
		Token:      "mql_wkr_local_cccccccccccc",
	})
	if err != nil {
		t.Fatalf("same URL refresh without force: %v", err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d, want 1 (no duplicate)", len(w.Homes))
	}
	if w.Homes[0].ID != "local" {
		t.Fatalf("id = %q, want preserved local", w.Homes[0].ID)
	}
	if w.Homes[0].Token != "mql_wkr_local_cccccccccccc" {
		t.Fatalf("token not refreshed")
	}
}

func TestUpsertHomeForceRequiredForIDRemap(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.memql.localhost",
		Token: "mql_wkr_local_bbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.other.example",
		Token: "mql_wkr_other_dddddddddddd",
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want --force required for id remap, got %v", err)
	}
	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.other.example",
		Token: "mql_wkr_other_dddddddddddd", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 || w.Homes[0].ClusterURL != "https://api.other.example" {
		t.Fatalf("force remap failed: %+v", w.Homes)
	}
}
