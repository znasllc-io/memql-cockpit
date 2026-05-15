package genesis

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseEnvFile_Basic(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.env")
	content := `# this is a comment

KEY1=value1
KEY2="quoted value"
KEY3='single quoted'
export KEY4=bash-style
KEY5=
KEY6=value=with=equals
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	entries, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	want := []EnvEntry{
		{Name: "KEY1", Value: "value1", Line: 3},
		{Name: "KEY2", Value: "quoted value", Line: 4},
		{Name: "KEY3", Value: "single quoted", Line: 5},
		{Name: "KEY4", Value: "bash-style", Line: 6},
		{Name: "KEY5", Value: "", Line: 7},
		{Name: "KEY6", Value: "value=with=equals", Line: 8},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries mismatch\n got: %+v\nwant: %+v", entries, want)
	}
}

func TestParseEnvFile_MalformedFails(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.env")
	if err := os.WriteFile(path, []byte("BAD LINE WITHOUT EQUALS\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ParseEnvFile(path); err == nil {
		t.Fatal("expected error for malformed line")
	}
}

func TestParseEnvFile_EmptyKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty-key.env")
	if err := os.WriteFile(path, []byte("=value\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ParseEnvFile(path); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestSerializeEntries(t *testing.T) {
	entries := []EnvEntry{
		{Name: "A", Value: "1"},
		{Name: "B", Value: "two words"},
		{Name: "C", Value: ""},
	}
	got := string(SerializeEntries(entries))
	want := "A=1\nB=two words\nC=\n"
	if got != want {
		t.Fatalf("serialize: got %q want %q", got, want)
	}
}

func TestSerializeEntries_RoundTrip(t *testing.T) {
	in := []EnvEntry{
		{Name: "OPENAI_API_KEY", Value: "sk-test-1234"},
		{Name: "MULTI_WORD", Value: "hello world"},
	}
	tmp := t.TempDir()
	path := filepath.Join(tmp, "round.env")
	if err := os.WriteFile(path, SerializeEntries(in), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Name != in[i].Name || out[i].Value != in[i].Value {
			t.Fatalf("round-trip mismatch at %d: got %+v want name=%s value=%s",
				i, out[i], in[i].Name, in[i].Value)
		}
	}
}

func TestLoadManifest_Embedded(t *testing.T) {
	// Ensure no override env vars influence the test.
	t.Setenv("MEMQL_MANIFEST_PATH", "")
	t.Setenv("MEMQL_REPO", "")

	m, err := LoadManifest("")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Secrets) == 0 || len(m.Variables) == 0 {
		t.Fatalf("embedded manifest looks empty: secrets=%d variables=%d", len(m.Secrets), len(m.Variables))
	}
	if m.Source != "embedded snapshot" {
		t.Fatalf("source: got %q want %q", m.Source, "embedded snapshot")
	}
	// Spot-check at least one well-known name is present.
	names := m.Names()
	found := false
	for _, n := range names {
		if n == "OPENAI_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("OPENAI_API_KEY not present in embedded manifest names: %v", names)
	}
}

func TestLoadManifest_FromFlagPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "mini-manifest.yaml")
	content := `
secrets:
  - name: FOO
    scope: global
variables:
  - name: BAR
    scope: global
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Secrets) != 1 || m.Secrets[0].Name != "FOO" {
		t.Fatalf("unexpected secrets: %+v", m.Secrets)
	}
	if len(m.Variables) != 1 || m.Variables[0].Name != "BAR" {
		t.Fatalf("unexpected variables: %+v", m.Variables)
	}
}

func TestFindMissing(t *testing.T) {
	entries := []EnvEntry{{Name: "A"}, {Name: "B"}}
	required := []string{"A", "C", "D"}
	got := findMissing(entries, required)
	want := []string{"C", "D"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findMissing: got %v want %v", got, want)
	}
}

func TestFindMissing_AllPresent(t *testing.T) {
	entries := []EnvEntry{{Name: "A"}, {Name: "B"}, {Name: "C"}}
	required := []string{"A", "B"}
	if got := findMissing(entries, required); len(got) != 0 {
		t.Fatalf("expected no missing, got %v", got)
	}
}

func TestWriteGenesisAtomic_Fresh(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "genesis.znas")
	payload := []byte("hello-genesis")
	if err := writeGenesisAtomic(out, payload); err != nil {
		t.Fatalf("writeGenesisAtomic: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %q want %q", got, payload)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm: got %o want 0600", st.Mode().Perm())
	}
	// No tmp leftovers in the directory.
	matches, err := filepath.Glob(filepath.Join(tmp, "genesis.znas.tmp.*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files leaked: %v", matches)
	}
}

func TestWriteGenesisAtomic_ReplacesExisting(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "genesis.znas")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := writeGenesisAtomic(out, []byte("new")); err != nil {
		t.Fatalf("writeGenesisAtomic: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("content: got %q want %q", got, "new")
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "genesis.znas.tmp.*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files leaked: %v", matches)
	}
}

func TestWriteGenesisAtomic_MissingDirFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nope", "genesis.znas")
	if err := writeGenesisAtomic(out, []byte("x")); err == nil {
		t.Fatalf("expected error when parent dir is missing")
	}
}
