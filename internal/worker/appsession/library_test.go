package appsession

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLibraryBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":     "https://api.example.com",
		"https://api.example.com:443": "https://api.example.com",
		"grpcs://api.example.com":     "https://api.example.com",
		"api.example.com:443":         "https://api.example.com",
		"api.example.com":             "https://api.example.com",
		"http://localhost:8080":       "http://localhost:8080",
		"grpc://localhost:50051":      "http://localhost:50051",
	}
	for in, want := range cases {
		got, err := LibraryBaseURL(in)
		if err != nil {
			t.Errorf("LibraryBaseURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("LibraryBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := LibraryBaseURL("   "); err == nil {
		t.Error("an empty cluster_url must be an error")
	}
}

// TestPull_WritesUnderTheWorkspaceOnly. A Content-Disposition filename is
// server-supplied and lands inside somebody's workspace; a traversal in
// it would put a file wherever the header said.
func TestPull_WritesUnderTheWorkspaceOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../escaped.txt"`)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	l := NewLibrary(srv.URL, testBearer, srv.Client())
	path, err := l.Pull(context.Background(), "artifact-1", dir)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("pulled to %q, which is outside the workspace %q", path, dir)
	}
	if strings.Contains(path, "..") {
		t.Errorf("path %q still carries a traversal", path)
	}
}

// TestPull_SendsTheSessionBearer, in both directions.
func TestPull_SendsTheSessionBearer(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	if _, err := l.Pull(context.Background(), "a", t.TempDir()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if seen != "Bearer "+testBearer {
		t.Errorf("Authorization = %q", seen)
	}
}

// TestPull_404SaysBothThingsItCouldMean. The engine's download route
// answers 404 on deny by design, so a link cannot be used to probe which
// ids exist -- which means a denied read and a missing artifact are
// genuinely indistinguishable here. Asserting one of them would send
// somebody to the wrong place half the time.
func TestPull_404SaysBothThingsItCouldMean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	_, err := l.Pull(context.Background(), "ghost", t.TempDir())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error must name the id: %q", msg)
	}
	if !strings.Contains(msg, "does not exist") || !strings.Contains(msg, "cannot reach it") {
		t.Errorf("error must state both readings: %q", msg)
	}
}

// TestPush_ReturnsTheArtifactId.
func TestPush_ReturnsTheArtifactId(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "no file part", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"artifactId":"art-9","fileId":"file-9"}`)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	id, err := l.PushBytes(context.Background(), []byte("hello"), "out.txt")
	if err != nil {
		t.Fatalf("PushBytes: %v", err)
	}
	if id != "art-9" {
		t.Errorf("id = %q", id)
	}
}

// TestPush_ForbiddenNamesTheUser: the credential acts as the owning user,
// so a 403 is a statement about that user's access.
func TestPush_ForbiddenNamesTheUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()

	l := NewLibrary(srv.URL, testBearer, srv.Client())
	_, err := l.PushBytes(context.Background(), []byte("x"), "out.txt")
	if err == nil || !strings.Contains(err.Error(), "owning user") {
		t.Errorf("error = %v, want it to name the user's access", err)
	}
}

// TestCollectProduced_OnlyWhatTheRunChanged.
func TestCollectProduced_OnlyWhatTheRunChanged(t *testing.T) {
	ws := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("untouched.txt", "before")
	write("node_modules/huge.js", "dependency")
	write(".git/config", "vcs")
	before := snapshotWorkspace(ws)

	write("new.txt", "produced")
	write("sub/nested.txt", "produced too")
	write("untouched.txt", "before")            // same content, rewritten
	write("node_modules/another.js", "ignored") // excluded tree

	produced, skipped := collectProduced(ws, before)
	names := map[string]bool{}
	for _, f := range produced {
		names[filepath.ToSlash(f.rel)] = true
	}
	if !names["new.txt"] || !names["sub/nested.txt"] {
		t.Errorf("produced = %v, want the two new files", names)
	}
	for _, unwanted := range []string{"node_modules/huge.js", "node_modules/another.js", ".git/config"} {
		if names[unwanted] {
			t.Errorf("%s should never be pushed: it is reproducible and enormous", unwanted)
		}
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none at this size", skipped)
	}

	// A rewritten file with a new mtime IS produced -- the run touched
	// it, and the reader wants to see what it wrote.
	touched := filepath.Join(ws, "untouched.txt")
	if err := os.WriteFile(touched, []byte("after"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	produced, _ = collectProduced(ws, before)
	found := false
	for _, f := range produced {
		if f.rel == "untouched.txt" {
			found = true
		}
	}
	if !found {
		t.Error("a file the run rewrote was not reported as produced")
	}
}

// TestCollectProduced_NeverTruncatesSilently. A bounded list that reads
// as complete is worse than a shorter one that says what it dropped:
// downstream, produced_artifact_ids is taken as the whole answer.
func TestCollectProduced_NeverTruncatesSilently(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	for i := range maxPushFiles + 5 {
		path := filepath.Join(ws, fmt.Sprintf("out-%03d.txt", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	produced, skipped := collectProduced(ws, before)
	if len(produced) != maxPushFiles {
		t.Errorf("produced = %d, want the cap of %d", len(produced), maxPushFiles)
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped = %d entries, want the 5 that were dropped named", len(skipped))
	}
	for _, note := range skipped {
		if !strings.Contains(note, "cap") {
			t.Errorf("skip note does not say why: %q", note)
		}
	}
}

// TestCollectProduced_IgnoresSessionScaffolding: the MCP config, the
// launcher and the transcript are not the run's output.
func TestCollectProduced_IgnoresSessionScaffolding(t *testing.T) {
	ws := t.TempDir()
	before := snapshotWorkspace(ws)
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"+backupSuffix), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".memql-session"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".memql-session", "transcript.log"), []byte("log"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	produced, _ := collectProduced(ws, before)
	if len(produced) != 0 {
		t.Errorf("produced = %+v, want none: none of that is the run's output", produced)
	}
}

// TestArtifactName_KeepsTheLocation. The Library stores under the last
// path segment, so two files with the same basename in different
// directories would arrive indistinguishable.
func TestArtifactName_KeepsTheLocation(t *testing.T) {
	if got := artifactName("api/schema.json"); got != "api__schema.json" {
		t.Errorf("artifactName = %q", got)
	}
	if got := artifactName("output.txt"); got != "output.txt" {
		t.Errorf("a root file was altered: %q", got)
	}
	if artifactName("api/schema.json") == artifactName("web/schema.json") {
		t.Error("two files with the same basename collapsed to one artifact name")
	}
}
