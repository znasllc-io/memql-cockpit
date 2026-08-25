package appsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

const testBearer = "eyJhbGciOiJSUzI1NiJ9.test-session-bearer.signature"

func TestWriteMCPConfig_ClaudeCodeShapeAndMode(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()

	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-1", state)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	path := filepath.Join(ws, ".mcp.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The bearer is in this file. 0600 is not decoration.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}
	entry, ok := doc.MCPServers[mcpServerName]
	if !ok {
		t.Fatalf("no %q server in the config: %s", mcpServerName, data)
	}
	if entry.Type != "http" || entry.URL != "https://mcp.example.com/mcp" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Headers["Authorization"] != "Bearer "+testBearer {
		t.Errorf("Authorization header = %q", entry.Headers["Authorization"])
	}
}

func TestWriteMCPConfig_CodexIsWorkspaceScoped(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDCodex, ws, "https://mcp.example.com/mcp", testBearer, "sess-2", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	home := filepath.Join(ws, codexHomeRel)
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Fatalf("codex config.toml: %v", err)
	}
	// CODEX_HOME must point INSIDE the workspace. Codex's home defaults
	// to the user profile, which is exactly the global write a per-run
	// credential must never make: it outlives the session, survives a
	// crash, and is shared with every other project on the machine.
	env := m.Env()
	if len(env) != 1 || env[0] != "CODEX_HOME="+home {
		t.Fatalf("env = %v, want CODEX_HOME inside the workspace", env)
	}
	if !strings.HasPrefix(home, ws) {
		t.Errorf("codex home %q escaped the workspace %q", home, ws)
	}
}

// TestWriteMCPConfig_RefusesAnEmptyCredential. An app with no credential
// reaches nothing over MCP and reports that as "MemQL's tools are
// broken". Refusing here names the real cause.
func TestWriteMCPConfig_RefusesAnEmptyCredential(t *testing.T) {
	if _, err := writeMCPConfig(apps.IDClaudeCode, t.TempDir(), "https://mcp.example.com/mcp", "", "s", ""); err == nil {
		t.Fatal("an empty credential must be refused, not written as a blank bearer")
	}
}

// TestRenew_RewritesInPlace: AppSessionControl{renew_credential} arrives
// before the current bearer expires, so no single bearer is ever
// long-lived. The old one must not survive the rewrite.
func TestRenew_RewritesInPlace(t *testing.T) {
	ws := t.TempDir()
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-3", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	const next = "eyJhbGciOiJSUzI1NiJ9.the-renewed-bearer.sig"
	if err := m.Renew(next); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), next) {
		t.Error("the renewed bearer is not in the file")
	}
	if strings.Contains(string(data), testBearer) {
		t.Error("the superseded bearer survived the rewrite")
	}
	info, _ := os.Stat(filepath.Join(ws, ".mcp.json"))
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after renewal = %o, want 600", perm)
	}
}

// TestRemove_DeletesEverything is the security control, asserted
// directly. The bearer cannot be revoked -- the engine's verify path is
// JWKS-only and DB-free, so there is no row to strike -- which makes this
// deletion one of the three things standing in for revocation.
func TestRemove_DeletesEverything(t *testing.T) {
	for _, app := range []string{apps.IDClaudeCode, apps.IDCodex} {
		t.Run(app, func(t *testing.T) {
			ws := t.TempDir()
			state := t.TempDir()
			m, err := writeMCPConfig(app, ws, "https://mcp.example.com/mcp", testBearer, "sess-"+app, state)
			if err != nil {
				t.Fatalf("writeMCPConfig: %v", err)
			}
			m.Remove()

			if found := grepTree(t, ws, testBearer); len(found) > 0 {
				t.Errorf("the bearer survived cleanup in: %v", found)
			}
			if found := grepTree(t, state, testBearer); len(found) > 0 {
				t.Errorf("the bearer reached the state directory: %v", found)
			}
			// A second Remove must be harmless: the run finishing and
			// the stream dying are independent events, and either one
			// has to be sufficient on its own.
			m.Remove()
		})
	}
}

// TestLedger_NeverHoldsTheBearer. The ledger exists so a restart can
// delete the file that holds the credential. Writing the credential into
// it would mean two files to clean up instead of one, and the second one
// outside the workspace.
func TestLedger_NeverHoldsTheBearer(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-4", state)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer m.Remove()

	if found := grepTree(t, state, testBearer); len(found) > 0 {
		t.Fatalf("the ledger carries the bearer: %v", found)
	}
	entries, err := os.ReadDir(filepath.Join(state, ledgerDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ledger entries = %v (%v), want exactly one", entries, err)
	}
}

// TestSweep_CleansUpAfterAKilledCockpit is the test #348 asks for by
// name: no configuration file survives a session that was killed.
//
// A `defer` cannot provide this. A SIGKILLed worker, an OOM, a machine
// that lost power mid-run -- each leaves a bearer on disk and the service
// manager restarts the worker without anything else noticing. Sweeping at
// startup is what bounds that exposure to the downtime rather than to
// forever.
func TestSweep_CleansUpAfterAKilledCockpit(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()

	// A session that was live when the process died: the config is
	// written and Remove is never called, exactly as a SIGKILL leaves it.
	if _, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "killed-1", state); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".mcp.json")); err != nil {
		t.Fatalf("precondition: the config should exist: %v", err)
	}

	// The next cockpit process starts.
	if removed := Sweep(state); removed != 1 {
		t.Errorf("Sweep removed %d files, want 1", removed)
	}
	if found := grepTree(t, ws, testBearer); len(found) > 0 {
		t.Errorf("a killed session left the bearer behind in: %v", found)
	}
	if _, err := os.Stat(filepath.Join(ws, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf(".mcp.json survived the sweep (%v)", err)
	}
	// The ledger entry goes with it, so the next boot has nothing to do.
	if removed := Sweep(state); removed != 0 {
		t.Errorf("a second sweep removed %d, want 0", removed)
	}
}

// TestSweep_RestoresAPreexistingConfig: if the crash happened while the
// session's config was in place over the user's own, the sweep puts
// theirs back.
func TestSweep_RestoresAPreexistingConfig(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	const mine = `{"mcpServers":{"my-own-server":{}}}`
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(mine), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "killed-2", state); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	Sweep(state)

	got, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("the user's own config was not restored: %v", err)
	}
	if string(got) != mine {
		t.Errorf("restored config = %s, want %s", got, mine)
	}
}

// TestRemove_RestoresAPreexistingConfig is the same guarantee on the
// clean path. An `open` or `attach` session can point at a real project,
// and destroying a config the user wrote is a worse outcome than
// anything this feature is worth.
func TestRemove_RestoresAPreexistingConfig(t *testing.T) {
	ws := t.TempDir()
	const mine = `{"mcpServers":{"my-own-server":{}}}`
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(mine), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := writeMCPConfig(apps.IDClaudeCode, ws, "https://mcp.example.com/mcp", testBearer, "sess-5", t.TempDir())
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	// While the session runs, ours is the one in place.
	live, _ := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if !strings.Contains(string(live), mcpServerName) {
		t.Fatalf("the session config is not in place: %s", live)
	}
	m.Remove()

	got, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("the user's own config was not restored: %v", err)
	}
	if string(got) != mine {
		t.Errorf("restored config = %s, want %s", got, mine)
	}
}

// TestSanitizeLedgerName keeps a server-minted session id from naming a
// path outside the ledger directory.
func TestSanitizeLedgerName(t *testing.T) {
	cases := map[string]string{
		"sess-abc123":          "sess-abc123",
		"../../etc/passwd":     "______etc_passwd",
		"a/b":                  "a_b",
		"":                     "unnamed",
		"with space and:colon": "with_space_and_colon",
	}
	for in, want := range cases {
		if got := sanitizeLedgerName(in); got != want {
			t.Errorf("sanitizeLedgerName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeLedgerName(strings.Repeat("x", 500)); len(got) != 128 {
		t.Errorf("length = %d, want 128", len(got))
	}
}

// grepTree returns every file under root whose contents contain needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(data), needle) {
			found = append(found, path)
		}
		return nil
	})
	return found
}
