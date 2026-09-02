//go:build linux || darwin

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The watched-folder sweeper (memql#4841).
//
// The fake below answers the engine's REAL routes -- /memql/query for the
// graph half and the /artifacts family for the bytes -- because everything
// worth testing here is a contract with those routes. A double that stubbed
// this package's own methods would test that the methods were called and
// nothing about the calls the engine has to accept.

type fakeEngine struct {
	mu sync.Mutex

	watches []map[string]any
	// calls records the MemQL construct text of every graph call, so a test
	// can assert what was SENT rather than what a mock remembered.
	calls []string
	// uploads records every file that arrived, by uploadedFromPath.
	uploads map[string]int64
	// chunks records the chunk numbers a session received.
	chunks map[string][]int
	// refuseUpload makes /artifacts answer a status.
	refuseUpload int
	// oneShotOnly rejects the session route, so a test can prove which route
	// a size took.
	sessionSize int64

	server *httptest.Server
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{uploads: map[string]int64{}, chunks: map[string][]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", f.handleQuery)
	mux.HandleFunc("/artifacts", f.handleOneShot)
	mux.HandleFunc("/artifacts/uploads", f.handleInit)
	mux.HandleFunc("/artifacts/uploads/", f.handleSession)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeEngine) handleQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, body.Query)
	watches := f.watches
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(body.Query, "query libraryWatchedFolders"):
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"data": watches}})
	default:
		// Mutations answer an empty result, which is what the gateway does.
		_, _ = w.Write([]byte(`{"result":{}}`))
	}
}

func (f *fakeEngine) handleOneShot(w http.ResponseWriter, r *http.Request) {
	if f.refuseUpload != 0 {
		http.Error(w, "the worker registration is not one of your machines", f.refuseUpload)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := r.FormValue("uploadedFromPath")
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	n, _ := io.Copy(io.Discard, file)
	f.mu.Lock()
	f.uploads[path] = n
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(PushResult{ArtifactID: "a-1", FileID: "f-" + filepath.Base(path)})
}

func (f *fakeEngine) handleInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Path string `json:"uploadedFromPath"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.sessionSize = body.Size
	f.uploads[body.Path] = 0
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	// A DIFFERENT chunk size from the client's own constant, deliberately:
	// the session's size is the server's to name, and a client that used its
	// own would commit a block list the server counts differently.
	_ = json.NewEncoder(w).Encode(initResponse{UploadID: "u-1", ChunkSize: 1024})
}

func (f *fakeEngine) handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/artifacts/uploads/")
	parts := strings.Split(rest, "/")
	uploadID := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		f.mu.Lock()
		staged := []stagedChunk{}
		for _, n := range f.chunks[uploadID] {
			staged = append(staged, stagedChunk{N: n, Size: 1024})
		}
		size := f.sessionSize
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inventoryResponse{UploadID: uploadID, Status: "open", Size: size, ChunkSize: 1024, Staged: staged})
	case len(parts) == 3 && parts[1] == "chunks" && r.Method == http.MethodPut:
		n, _ := strconv.Atoi(parts[2])
		written, _ := io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.chunks[uploadID] = append(f.chunks[uploadID], n)
		for path := range f.uploads {
			f.uploads[path] += written
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "complete" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PushResult{ArtifactID: "a-1", FileID: "f-session"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEngine) graphCalls(construct string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		if strings.Contains(call, construct+"(") {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeEngine) uploadedPaths() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for k, v := range f.uploads {
		out[k] = v
	}
	return out
}

func managerFor(t *testing.T, f *fakeEngine, root string, check func(string) error) *Manager {
	t.Helper()
	m := New(Options{
		StateDir:   t.TempDir(),
		BaseURL:    f.server.URL,
		Bearer:     func(context.Context) (string, error) { return "token", nil },
		CheckPath:  check,
		HTTPClient: f.server.Client(),
	})
	if m == nil {
		t.Fatal("New returned nil with a base URL and a bearer -- the manager disabled itself")
	}
	m.opts.WorkerID = "wkr-1"
	_ = root
	return m
}

func watchRow(id, path string, over map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "workerId": "wkr-1", "localPath": path,
		"folderId": "", "status": "active", "includeHidden": false,
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func allow(string) error { return nil }

// ---------------------------------------------------------------------------

// THE VETO IS THE LOAD-BEARING RULE. A watch row is written from a browser, so
// its path is one the cluster names on somebody else's machine; without this
// check, anybody who could write a watch could have ~/.ssh uploaded.
func TestAPathThisMachineHasNotAllowedIsRefusedAndReported(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secret.txt"), 16)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, func(string) error { return fmt.Errorf("not in backup.roots") })
	m.SweepOnce(context.Background())

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Fatalf("a refused folder was uploaded anyway: %v", got)
	}
	reports := f.graphCalls("reportLibraryWatchedFolderSweep")
	if len(reports) != 1 {
		t.Fatalf("want exactly one sweep report, got %d: %v", len(reports), reports)
	}
	// REPORTED, not silent. A machine that quietly ignored a watch would be
	// indistinguishable from one that was offline, and the person would have
	// nothing to act on.
	if !strings.Contains(reports[0], `"refused_by_policy"`) {
		t.Errorf("the refusal was not reported as refused_by_policy: %s", reports[0])
	}
	if !strings.Contains(reports[0], "not in backup.roots") {
		t.Errorf("the machine's own reason was not carried to the cluster: %s", reports[0])
	}
}

// A nil check is a wiring mistake, and it must fail CLOSED.
func TestNoPolicyAtAllBacksUpNothing(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, nil)
	m.SweepOnce(context.Background())

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Fatalf("a manager with no policy uploaded files: %v", got)
	}
}

func TestASweepPushesEveryFileOnceAndThenStopsPushing(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 32)
	writeFile(t, filepath.Join(root, "nested", "b.txt"), 64)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())

	uploaded := f.uploadedPaths()
	if len(uploaded) != 2 {
		t.Fatalf("want both files uploaded, got %v", uploaded)
	}

	// THE SECOND SWEEP IS THE POINT. Every sweep after the first must cost
	// stat calls and no bytes, or a folder of client video would be re-sent
	// every five minutes forever.
	before := len(f.uploadedPaths())
	m.SweepOnce(context.Background())
	if after := len(f.uploadedPaths()); after != before {
		t.Errorf("an unchanged folder was re-uploaded: %d files before, %d after", before, after)
	}
}

func TestAChangedFileIsPushedAgainAndATouchedOneIsNot(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 32)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())
	if f.uploadedPaths()[path] != 32 {
		t.Fatalf("first push did not land: %v", f.uploadedPaths())
	}

	// A TOUCH with identical bytes: the cheap check moves, the digest does
	// not, and nothing is sent. This is the case that would otherwise re-push
	// a whole folder because a sync tool rewrote timestamps.
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	m.SweepOnce(context.Background())
	if got := f.uploadedPaths()[path]; got != 32 {
		t.Errorf("a touched file with unchanged bytes was re-uploaded (%d bytes recorded)", got)
	}

	// Real new bytes DO go.
	writeFile(t, path, 48)
	m.SweepOnce(context.Background())
	if got := f.uploadedPaths()[path]; got != 48 {
		t.Errorf("changed bytes were not re-uploaded: recorded %d, want 48", got)
	}
}

// A big file takes the SESSION route, and the chunking uses the SERVER's size.
func TestAFileOverTheThresholdTakesTheSessionRouteWithTheServersChunkSize(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "big.mov")
	writeFile(t, path, 4096)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	// Lower the threshold rather than writing 32 MiB, so this exercises the
	// ROUTE CHOICE in Push and not just the session code beneath it.
	m.library.oneShotLimit = 1024
	m.SweepOnce(context.Background())

	f.mu.Lock()
	got := append([]int(nil), f.chunks["u-1"]...)
	f.mu.Unlock()
	// 4096 bytes at the SERVER's 1024 = four chunks, 1-based. A client that
	// used its own 16 MiB constant would have sent exactly one.
	if len(got) != 4 {
		t.Fatalf("want 4 chunks at the server's chunk size, got %d (%v)", len(got), got)
	}
	if got[0] != 1 {
		t.Errorf("chunks are 1-based on the wire; first was %d", got[0])
	}
}

// Every push names its machine and its path, which is what makes a re-push a
// new VERSION rather than a duplicate.
func TestEveryPushNamesItsMachineAndPath(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())

	if _, ok := f.uploadedPaths()[path]; !ok {
		t.Fatalf("the upload did not carry uploadedFromPath; recorded: %v", f.uploadedPaths())
	}
}

// A vanished file is FLAGGED, never removed -- the epic's invariant.
func TestAFileDeletedAtTheOriginIsFlaggedAndNothingIsDeleted(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.SweepOnce(context.Background())

	states := f.graphCalls("setLibraryFileLinkState")
	if len(states) == 0 {
		t.Fatal("nothing reported the file as gone from the machine")
	}
	last := states[len(states)-1]
	if !strings.Contains(last, `"origin_gone"`) {
		t.Errorf("want origin_gone, got: %s", last)
	}
	// NOTHING removes the copy. There is no archive, no delete, no supersede
	// anywhere in this package, and this is the assertion that says so.
	for _, call := range f.calls {
		for _, forbidden := range []string{"archive", "delete", "Delete", "Archive"} {
			if strings.Contains(call, forbidden) {
				t.Errorf("the sweeper sent a destructive call, which the one-way invariant forbids: %s", call)
			}
		}
	}
}

// A paused watch is read and left entirely alone.
func TestAPausedWatchIsNotSweptAndIsNotReported(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 8)
	f.watches = []map[string]any{watchRow("w-1", root, map[string]any{"status": "paused"})}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Errorf("a paused watch uploaded files: %v", got)
	}
	if reports := f.graphCalls("reportLibraryWatchedFolderSweep"); len(reports) != 0 {
		t.Errorf("a paused watch reported a sweep it did not run: %v", reports)
	}
}

// A missing folder is `missing`, and it is told apart from unreadable.
func TestAMissingFolderIsReportedAsMissing(t *testing.T) {
	f := newFakeEngine(t)
	f.watches = []map[string]any{watchRow("w-1", filepath.Join(t.TempDir(), "gone"), nil)}

	m := managerFor(t, f, "", allow)
	m.SweepOnce(context.Background())

	reports := f.graphCalls("reportLibraryWatchedFolderSweep")
	if len(reports) != 1 || !strings.Contains(reports[0], `"missing"`) {
		t.Fatalf("want one report of missing, got: %v", reports)
	}
}

// A refusal arrives as HTTP 200 with an `errors` array, and a client that only
// read the status code would treat "you may not see these" as "you are
// watching nothing" -- a backup with nothing to do looks exactly like one that
// is up to date.
func TestA200WithErrorsIsARefusalNotAnEmptyList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"message":"actor rank below the floor"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGraph(srv.URL, srv.Client(), func(context.Context) (string, error) { return "t", nil })
	_, err := g.Watches(context.Background(), "wkr-1")
	if err == nil {
		t.Fatal("a 200 carrying an errors array was read as success")
	}
	if !strings.Contains(err.Error(), "actor rank below the floor") {
		t.Errorf("the server's own sentence was not carried: %v", err)
	}
}

// A 401 names the repair, because it is the single most common reason this
// feature does nothing and "unauthorized" alone is a diagnosis with no
// treatment.
func TestA401SaysWhatToDoAboutIt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGraph(srv.URL, srv.Client(), func(context.Context) (string, error) { return "t", nil })
	_, err := g.Watches(context.Background(), "wkr-1")
	if err == nil {
		t.Fatal("a 401 was not an error")
	}
	if !strings.Contains(err.Error(), "memql login") {
		t.Errorf("the 401 does not name the repair: %v", err)
	}
}

// The sweep must never claim a check time: `lastSweepAt` is server-stamped, so
// a caller cannot say it looked when it did not.
func TestTheSweepReportNeverSendsItsOwnCheckTime(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	f.watches = []map[string]any{watchRow("w-1", root, nil)}
	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background())

	for _, call := range f.graphCalls("reportLibraryWatchedFolderSweep") {
		if strings.Contains(call, "lastSweepAt") {
			t.Errorf("the sweep named its own check time, which the mutation stamps: %s", call)
		}
	}
}
