//go:build linux || darwin

package appsession

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// record_test.go holds the content policy to what it promises: a file the
// app names is read only when it is inside the workspace, outside the
// session's scaffolding, and a regular file -- and the bytes that travel
// are the file's own, bounded.

func writeFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func policyFor(ws string) *contentPolicy {
	return newContentPolicy(ws,
		filepath.Join(ws, sessionScaffoldDir),
		filepath.Join(ws, ".mcp.json"),
		filepath.Join(ws, ".mcp.json"+backupSuffix))
}

func readOne(p *contentPolicy, op, path string) harness.Content {
	c := []harness.Content{{Op: op, Path: path}}
	p.fill(c)
	return c[0]
}

func TestContentPolicyReadsOnlyInsideTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	notes := writeFile(t, ws, "notes.txt", "alpha\nbeta\n")
	secret := writeFile(t, outside, "secret.txt", "not the session's")
	writeFile(t, ws, ".mcp.json", `{"mcpServers":{"memql":{"headers":{"Authorization":"Bearer `+testBearer+`"}}}}`)
	writeFile(t, ws, sessionScaffoldDir+"/codex/config.toml", "bearer = \""+testBearer+"\"\n")
	writeFile(t, ws, ".mcp.json"+backupSuffix, "the person's own config")
	if err := os.Symlink(secret, filepath.Join(ws, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(notes, filepath.Join(ws, "link-in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := policyFor(ws)

	cases := []struct {
		name, path, omitted, data string
	}{
		{"a file in the workspace", notes, "", "alpha\nbeta\n"},
		{"a relative path, under the workspace", "notes.txt", "", "alpha\nbeta\n"},
		{"a link that stays inside", filepath.Join(ws, "link-in"), "", "alpha\nbeta\n"},
		{"a file elsewhere", secret, harness.OmittedOutsideWorkspace, ""},
		{"a link out of the workspace", filepath.Join(ws, "link-out"), harness.OmittedOutsideWorkspace, ""},
		{"a climb out with ..", filepath.Join(ws, "..", filepath.Base(outside), "secret.txt"), harness.OmittedOutsideWorkspace, ""},
		{"the MCP configuration with the bearer", filepath.Join(ws, ".mcp.json"), harness.OmittedScaffolding, ""},
		{"the session directory", filepath.Join(ws, sessionScaffoldDir, "codex", "config.toml"), harness.OmittedScaffolding, ""},
		{"a configuration moved aside", filepath.Join(ws, ".mcp.json"+backupSuffix), harness.OmittedScaffolding, ""},
		{"nothing there", filepath.Join(ws, "missing.txt"), harness.OmittedNotFound, ""},
		{"a directory", filepath.Join(ws, "dir"), harness.OmittedNotRegular, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readOne(p, harness.ContentRead, tc.path)
			if got.Omitted != tc.omitted {
				t.Fatalf("omitted = %q, want %q (%+v)", got.Omitted, tc.omitted, got)
			}
			if tc.omitted != "" {
				if got.Data != nil || got.Encoding != "" {
					t.Errorf("an omitted file still carried bytes: %+v", got)
				}
				if tc.omitted != harness.OmittedNotRegular && got.Digest != "" {
					t.Errorf("a file that was not read has a digest: %+v", got)
				}
				return
			}
			if got.Data == nil || *got.Data != tc.data || got.Encoding != harness.EncodingUTF8 {
				t.Errorf("data = %+v, want %q", got, tc.data)
			}
			if got.Digest != harness.Digest([]byte(tc.data)) || got.Bytes == nil || *got.Bytes != int64(len(tc.data)) {
				t.Errorf("digest/bytes = %s %v", got.Digest, got.Bytes)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want the path the app named", got.Path)
			}
		})
	}
}

// TestContentPolicyNeverWaitsOnAPipe: a named pipe opened for reading
// blocks until a writer appears, which here would be never. The open is
// non-blocking and the pipe is refused on its descriptor.
func TestContentPolicyNeverWaitsOnAPipe(t *testing.T) {
	ws := t.TempDir()
	fifo := filepath.Join(ws, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan harness.Content, 1)
	go func() { done <- readOne(policyFor(ws), harness.ContentRead, fifo) }()
	select {
	case got := <-done:
		if got.Omitted != harness.OmittedNotRegular {
			t.Errorf("a pipe = %+v, want not_regular", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a named pipe hung the recording")
	}
}

func TestContentPolicyBoundsWhatTravels(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)

	big := writeFile(t, ws, "big.bin", strings.Repeat("x", int(maxInlineFileBytes)+1))
	got := readOne(p, harness.ContentWrite, big)
	if got.Omitted != harness.OmittedOverCeiling || got.Data != nil {
		t.Errorf("a file over the inline ceiling = %+v", got)
	}
	if got.Digest != harness.Digest([]byte(strings.Repeat("x", int(maxInlineFileBytes)+1))) {
		t.Error("a file over the inline ceiling lost its digest; it can still be compared")
	}

	// Five files of the per-file ceiling in one action: four fit the event
	// budget, and the fifth is digested but not carried.
	var batch []harness.Content
	for i := 0; i < 5; i++ {
		path := writeFile(t, ws, "part"+string(rune('a'+i)), strings.Repeat("y", int(maxInlineFileBytes)))
		batch = append(batch, harness.Content{Op: harness.ContentWrite, Path: path})
	}
	p.fill(batch)
	for i, c := range batch[:4] {
		if c.Data == nil || c.Omitted != "" {
			t.Errorf("file %d = %+v, want it inline", i, c)
		}
	}
	if batch[4].Omitted != harness.OmittedOverBudget || batch[4].Data != nil || batch[4].Digest == "" {
		t.Errorf("the fifth file = %+v, want over_budget with its digest", batch[4])
	}

	// Past the digest ceiling nothing is read at all; the size still says
	// how big it was. A sparse file makes this cheap to build.
	huge := filepath.Join(ws, "huge.bin")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxDigestFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got = readOne(p, harness.ContentRead, huge)
	if got.Omitted != harness.OmittedTooLarge || got.Digest != "" || got.Bytes == nil || *got.Bytes != maxDigestFileBytes+1 {
		t.Errorf("a file past the digest ceiling = %+v", got)
	}
}

func TestContentPolicyCarriesBinaryAsBase64AndEmptyAsEmpty(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	raw := []byte{0xff, 0xfe, 0x00, 0x41}
	path := filepath.Join(ws, "blob.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := readOne(p, harness.ContentRead, path)
	if got.Encoding != harness.EncodingBase64 || got.Data == nil || *got.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("binary = %+v", got)
	}
	if got.Digest != harness.Digest(raw) {
		t.Error("the digest is over the bytes, not over their encoding")
	}

	empty := writeFile(t, ws, "empty.txt", "")
	got = readOne(p, harness.ContentWrite, empty)
	if got.Data == nil || *got.Data != "" || got.Bytes == nil || *got.Bytes != 0 || got.Omitted != "" {
		t.Errorf("an empty file = %+v, want present, empty and inline", got)
	}
}

func TestContentPolicyLeavesADeleteAlone(t *testing.T) {
	ws := t.TempDir()
	path := writeFile(t, ws, "still-here.txt", "x")
	got := readOne(policyFor(ws), harness.ContentDelete, path)
	if got.Digest != "" || got.Data != nil || got.Omitted != "" || got.Bytes != nil {
		t.Errorf("a delete read the file it deleted: %+v", got)
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w", "/w/..foo", true},
		{"/w", "/wx", false},
		{"/w", "/", false},
		{"/w", "/other/w", false},
	} {
		if got := within(tc.root, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}
