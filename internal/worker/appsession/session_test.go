//go:build linux || darwin

package appsession

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

// --- harness ---------------------------------------------------------

type recordedChunk struct {
	stream string
	data   string
	seq    uint64
}

// fakeSender stands in for the worker's stream.
type fakeSender struct {
	mu       sync.Mutex
	chunks   []recordedChunk
	end      *memqlv1.AppSessionEnd
	done     chan struct{}
	failNext int
	attempts int
}

func newFakeSender() *fakeSender {
	return &fakeSender{done: make(chan struct{})}
}

func (f *fakeSender) SendAppSessionChunk(_, stream string, data []byte, seq uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failNext > 0 {
		f.failNext--
		return fmt.Errorf("simulated transient send failure")
	}
	f.chunks = append(f.chunks, recordedChunk{stream: stream, data: string(data), seq: seq})
	return nil
}

func (f *fakeSender) SendAppSessionEnd(end *memqlv1.AppSessionEnd) error {
	f.mu.Lock()
	if f.end == nil {
		f.end = end
		close(f.done)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) wait(t *testing.T) *memqlv1.AppSessionEnd {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(30 * time.Second):
		t.Fatal("session never ended")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.end
}

func (f *fakeSender) recorded() []recordedChunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedChunk(nil), f.chunks...)
}

func (f *fakeSender) transcript() string {
	var b strings.Builder
	for _, c := range f.recorded() {
		b.WriteString(c.data)
	}
	return b.String()
}

// fakeApp installs a shell script on PATH under the app's binary name.
func fakeApp(t *testing.T, binary, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", binary, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// fakeLibrary serves the two byte-bearing Library routes.
type fakeLibrary struct {
	server   *httptest.Server
	mu       sync.Mutex
	uploads  map[string][]byte
	inputs   map[string][]byte
	pullCode map[string]int
	bearers  []string
	nextID   int
}

func newFakeLibrary(t *testing.T) *fakeLibrary {
	t.Helper()
	l := &fakeLibrary{
		uploads:  map[string][]byte{},
		inputs:   map[string][]byte{},
		pullCode: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/artifacts/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/artifacts/"), "/content")
		l.mu.Lock()
		l.bearers = append(l.bearers, r.Header.Get("Authorization"))
		body, ok := l.inputs[id]
		code := l.pullCode[id]
		l.mu.Unlock()
		if code != 0 {
			http.Error(w, "refused", code)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.txt"`)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/artifacts", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.bearers = append(l.bearers, r.Header.Get("Authorization"))
		l.mu.Unlock()
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		l.mu.Lock()
		l.nextID++
		id := fmt.Sprintf("artifact-%d:%s", l.nextID, header.Filename)
		l.uploads[id] = data
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PushResult{ArtifactId: id, FileId: "file-" + id})
	})
	l.server = httptest.NewServer(mux)
	t.Cleanup(l.server.Close)
	return l
}

func (l *fakeLibrary) uploaded() map[string][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range l.uploads {
		out[k] = v
	}
	return out
}

func (l *fakeLibrary) seenBearers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.bearers...)
}

type harness struct {
	manager   *Manager
	sender    *fakeSender
	library   *fakeLibrary
	workspace string
	state     string
}

func newHarness(t *testing.T, allow ...string) *harness {
	t.Helper()
	lib := newFakeLibrary(t)
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	if len(allow) == 0 {
		allowed[apps.IDClaudeCode] = true
	}
	h := &harness{
		sender:    newFakeSender(),
		library:   lib,
		workspace: t.TempDir(),
		state:     t.TempDir(),
	}
	h.manager = NewManager(Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:    h.state,
		LibraryBase: lib.server.URL,
		HTTPClient:  lib.server.Client(),
		Allowed:     func(id string) bool { return allowed[id] },
	})
	return h
}

func (h *harness) start(t *testing.T, mutate func(*memqlv1.AppSessionStart)) *memqlv1.AppSessionEnd {
	t.Helper()
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-test",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "do the thing",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	if mutate != nil {
		mutate(start)
	}
	h.manager.Start(context.Background(), h.sender, start)
	return h.sender.wait(t)
}

// --- tests -----------------------------------------------------------

// TestSession_RunStreamsAndEnds is the happy path for kind=run, and it
// pins the four things a reader of the transcript depends on: ordering,
// classification, the app's own usage, and the app's own session id.
func TestSession_RunStreamsAndEnds(t *testing.T) {
	fakeApp(t, "claude", `
echo '{"type":"system","subtype":"init","session_id":"app-run-42"}'
echo 'plain narration from the agent'
echo '{"type":"result","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20},"session_id":"app-run-42"}'
exit 0
`)
	h := newHarness(t)
	end := h.start(t, nil)

	if end.GetExitCode() != 0 {
		t.Errorf("exit_code = %d, want 0", end.GetExitCode())
	}
	if end.GetError() != "" {
		t.Errorf("error = %q, want empty on a clean run", end.GetError())
	}

	chunks := h.sender.recorded()
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want at least 3: %+v", len(chunks), chunks)
	}

	// seq is monotonic per session, starting at 1. The engine DROPS
	// out-of-order and duplicate chunks rather than appending them, so a
	// gap or a repeat is not a cosmetic problem -- it is a transcript
	// that no longer matches what the app printed, in a way no later
	// reader can detect.
	for i, c := range chunks {
		if c.seq != uint64(i+1) {
			t.Fatalf("chunk %d has seq %d, want %d (monotonic from 1): %+v", i, c.seq, i+1, chunks)
		}
	}

	// The app's own structured JSON goes out as `event`; prose goes out
	// as stdout narration. Nothing synthesises an event out of parsed
	// prose -- a live view that is confidently wrong about what the
	// agent did is worse than a plain one.
	var events, stdout int
	for _, c := range chunks {
		switch c.stream {
		case StreamEvent:
			events++
			if !strings.HasPrefix(strings.TrimSpace(c.data), "{") {
				t.Errorf("event chunk is not JSON: %q", c.data)
			}
		case StreamStdout:
			stdout++
			if strings.HasPrefix(strings.TrimSpace(c.data), "{") {
				t.Errorf("JSON was classified as narration: %q", c.data)
			}
		}
	}
	if events < 2 {
		t.Errorf("event chunks = %d, want the app's two JSON lines", events)
	}
	if stdout < 1 {
		t.Errorf("stdout chunks = %d, want the plain line", stdout)
	}

	usage := end.GetUsage()
	if !usage.GetKnown() {
		t.Fatal("the app reported usage; known must be true")
	}
	if usage.GetInputTokens() != 10 || usage.GetOutputTokens() != 20 || usage.GetCostUsd() != 0.25 {
		t.Errorf("usage = %+v, want what the app reported", usage)
	}
	// The app's own session id comes back so a later kind=attach can
	// resume this run.
	if end.GetAppSessionRef() != "app-run-42" {
		t.Errorf("app_session_ref = %q, want the app's own session id", end.GetAppSessionRef())
	}
}

// TestSession_UsageUnknownWhenTheAppSaysNothing. The engine records
// known=false as billing "unknown", which is the honest answer. An
// estimate would be recorded as measured, in a ledger somebody bills
// from.
func TestSession_UsageUnknownWhenTheAppSaysNothing(t *testing.T) {
	fakeApp(t, "claude", "echo 'did some work'\nexit 0\n")
	h := newHarness(t)
	end := h.start(t, nil)

	usage := end.GetUsage()
	if usage == nil {
		t.Fatal("usage must be present with known=false, not omitted")
	}
	if usage.GetKnown() {
		t.Error("the app reported nothing; known must be false")
	}
	if usage.GetInputTokens() != 0 || usage.GetOutputTokens() != 0 || usage.GetCostUsd() != 0 {
		t.Errorf("usage = %+v, want zeroes alongside known=false", usage)
	}
}

// TestSession_RealExitCodeIsPassedThrough. The engine reads a non-zero
// exit as a FAILED run rather than an ended one, so normalising the code
// misfiles the outcome in a record people read back later.
func TestSession_RealExitCodeIsPassedThrough(t *testing.T) {
	for _, code := range []int{1, 2, 42} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			fakeApp(t, "claude", fmt.Sprintf("echo working\nexit %d\n", code))
			h := newHarness(t)
			end := h.start(t, nil)
			if int(end.GetExitCode()) != code {
				t.Errorf("exit_code = %d, want %d verbatim", end.GetExitCode(), code)
			}
		})
	}
}

// TestSession_NoConfigSurvivesAnyExitPath is #348's acceptance criterion
// driven through the real session runner rather than the writer alone.
func TestSession_NoConfigSurvivesAnyExitPath(t *testing.T) {
	cases := map[string]string{
		"clean exit":    "echo done\nexit 0\n",
		"non-zero exit": "echo failing\nexit 7\n",
		"crash":         "kill -9 $$\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			fakeApp(t, "claude", script)
			h := newHarness(t)
			h.start(t, nil)

			if found := grepTree(t, h.workspace, testBearer); len(found) > 0 {
				t.Errorf("the bearer survived in: %v", found)
			}
			if _, err := os.Stat(filepath.Join(h.workspace, ".mcp.json")); !os.IsNotExist(err) {
				t.Errorf(".mcp.json survived (%v)", err)
			}
		})
	}
}

// TestSession_CancelKillsTheProcessGroup. A `claude` that survives its
// cancel is an agent running on somebody's machine with nothing watching
// it. The engine sends cancel when the user asks, when the calling plan's
// context dies, and when the kill switch flips -- all three mean STOP.
func TestSession_CancelKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-alive")
	// The app forks a grandchild that outlives it and keeps touching a
	// file. Killing only the direct child would leave it running.
	fakeApp(t, "claude", fmt.Sprintf(`
( while true; do touch %q; sleep 0.1; done ) &
echo started
sleep 60
`, marker))

	h := newHarness(t)
	sender := h.sender
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-cancel",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "long job",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	h.manager.Start(context.Background(), sender, start)

	// Wait for the app to be up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(sender.recorded()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-cancel",
		Action:    ActionCancel,
		Reason:    "the user cancelled the plan",
	})
	end := sender.wait(t)

	if !strings.Contains(end.GetError(), "cancelled") {
		t.Errorf("error = %q, want it to name the cancel", end.GetError())
	}
	if !strings.Contains(end.GetError(), "the user cancelled the plan") {
		t.Errorf("error = %q, want the server's reason carried through", end.GetError())
	}

	// The grandchild must be gone. Give the group kill a moment, then
	// confirm the marker stops advancing.
	time.Sleep(1500 * time.Millisecond)
	before, err := os.Stat(marker)
	if err != nil {
		// Never touched at all is also a pass.
		return
	}
	time.Sleep(1500 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil {
		return
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a grandchild survived the cancel: killing the process group is the point")
	}
}

// TestSession_MaxDurationEndsTheRun.
func TestSession_MaxDurationEndsTheRun(t *testing.T) {
	fakeApp(t, "claude", "echo started\nsleep 60\n")
	h := newHarness(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-duration"
		s.Limits = &memqlv1.AppSessionLimits{MaxDurationSeconds: 1}
	})
	if !strings.Contains(end.GetError(), "max_duration_seconds") {
		t.Errorf("error = %q, want it to name the limit that bit", end.GetError())
	}
}

// TestSession_TranscriptCapBoundsTheStreamNotTheArtifact. The limit
// bounds what the ENGINE keeps on the session row; the complete
// transcript is expected to be an artifact. Stopping silently would leave
// a reader believing the run went quiet, so the cap announces itself.
func TestSession_TranscriptCapBoundsTheStreamNotTheArtifact(t *testing.T) {
	fakeApp(t, "claude", `
i=0
while [ $i -lt 200 ]; do
  echo "line $i xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  i=$((i+1))
done
exit 0
`)
	h := newHarness(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-cap"
		s.Limits = &memqlv1.AppSessionLimits{MaxTranscriptBytes: 500}
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}

	streamed := h.sender.transcript()
	if !strings.Contains(streamed, "live transcript truncated") {
		t.Error("the cap must announce itself; going silent reads as the run going quiet")
	}

	// The artifact carries the WHOLE thing.
	var transcript []byte
	for name, body := range h.library.uploaded() {
		if strings.Contains(name, "transcript") {
			transcript = body
		}
	}
	if transcript == nil {
		t.Fatal("no transcript artifact was pushed")
	}
	if !strings.Contains(string(transcript), "line 199") {
		t.Error("the transcript artifact was truncated too; the engine's row cap is not the artifact's cap")
	}
	if int64(len(transcript)) <= 500 {
		t.Errorf("transcript artifact is %d bytes, which is inside the row cap", len(transcript))
	}
}

// TestSession_InputsLandBeforeTheAppStarts. An agent that begins work and
// finds its inputs half-arrived produces confidently wrong output rather
// than an error, and nothing downstream can tell the difference.
func TestSession_InputsLandBeforeTheAppStarts(t *testing.T) {
	fakeApp(t, "claude", "cat spec-1.txt\nexit 0\n")
	h := newHarness(t)
	h.library.mu.Lock()
	h.library.inputs["spec-1"] = []byte("the specification body")
	h.library.mu.Unlock()

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-inputs"
		s.Inputs = []string{"spec-1"}
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}
	if !strings.Contains(h.sender.transcript(), "the specification body") {
		t.Error("the app did not see its input: it was not on disk before the run started")
	}
}

// TestSession_FailedInputEndsTheSessionNamingTheId, and the app never
// runs.
func TestSession_FailedInputEndsTheSessionNamingTheId(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "the-app-ran")
	fakeApp(t, "claude", fmt.Sprintf("touch %q\nexit 0\n", ran))
	h := newHarness(t)

	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-bad-input"
		s.Inputs = []string{"missing-artifact"}
	})
	if !strings.Contains(end.GetError(), "missing-artifact") {
		t.Errorf("error = %q, want it to name the id that failed", end.GetError())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the app ran despite an input that never arrived")
	}
	if end.GetExitCode() == 0 {
		t.Error("a session that never started must not report a clean exit")
	}
}

// TestSession_PullErrorNamesTheRightParty. The credential acts as the
// OWNING USER, so a refusal is a statement about that user's access, not
// about this machine. Getting it wrong sends an operator to check a
// worker token that is working fine.
func TestSession_PullErrorNamesTheRightParty(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")

	t.Run("403 is the user's access", func(t *testing.T) {
		h := newHarness(t)
		h.library.mu.Lock()
		h.library.pullCode["restricted"] = http.StatusForbidden
		h.library.mu.Unlock()
		end := h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-403"
			s.Inputs = []string{"restricted"}
		})
		if !strings.Contains(end.GetError(), "owning user") {
			t.Errorf("error = %q, want it to name the user's access", end.GetError())
		}
	})

	t.Run("401 is the cockpit's side", func(t *testing.T) {
		h := newHarness(t)
		h.library.mu.Lock()
		h.library.pullCode["stale"] = http.StatusUnauthorized
		h.library.mu.Unlock()
		end := h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-401"
			s.Inputs = []string{"stale"}
		})
		if !strings.Contains(end.GetError(), "cockpit's side") {
			t.Errorf("error = %q, want it to name the cockpit", end.GetError())
		}
	})
}

// TestSession_ProducedFilesAndTranscriptArePushed.
func TestSession_ProducedFilesAndTranscriptArePushed(t *testing.T) {
	fakeApp(t, "claude", "echo 'result content' > output.txt\nmkdir -p sub && echo nested > sub/deep.txt\nexit 0\n")
	h := newHarness(t)
	// A file that predates the run must NOT be reported as produced.
	if err := os.WriteFile(filepath.Join(h.workspace, "preexisting.txt"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	end := h.start(t, nil)

	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s)", end.GetExitCode(), end.GetError())
	}
	names := map[string]bool{}
	for name := range h.library.uploaded() {
		names[name] = true
	}
	var sawOutput, sawNested, sawTranscript, sawPreexisting bool
	for name := range names {
		switch {
		case strings.HasSuffix(name, "output.txt"):
			sawOutput = true
		case strings.HasSuffix(name, "sub__deep.txt"):
			sawNested = true
		case strings.Contains(name, "transcript"):
			sawTranscript = true
		case strings.Contains(name, "preexisting"):
			sawPreexisting = true
		}
	}
	if !sawOutput || !sawNested {
		t.Errorf("produced files not pushed: %v", names)
	}
	if !sawTranscript {
		t.Errorf("the full transcript must be pushed as an artifact: %v", names)
	}
	if sawPreexisting {
		t.Error("a file that predates the run was reported as produced")
	}
	// Every id comes back on the End so the portal can point at them.
	if len(end.GetProducedArtifactIds()) != len(names) {
		t.Errorf("produced_artifact_ids = %d, want %d", len(end.GetProducedArtifactIds()), len(names))
	}
}

// TestSession_BearerNeverReachesAChunk. The transcript is persisted on
// the engine side and rendered in the portal, so a chunk carrying the
// credential publishes it everywhere that record reaches.
func TestSession_BearerNeverReachesAChunk(t *testing.T) {
	fakeApp(t, "claude", "cat .mcp.json\ncat .mcp.json 1>&2\nexit 0\n")
	h := newHarness(t)
	end := h.start(t, nil)

	if strings.Contains(h.sender.transcript(), testBearer) {
		t.Error("the session bearer was streamed to the engine in a chunk")
	}
	if !strings.Contains(h.sender.transcript(), string(redactedMarker)) {
		t.Error("the echoed config should show a visible redaction, not silently vanish")
	}
	if strings.Contains(end.GetError(), testBearer) {
		t.Error("the session bearer reached the End's error text")
	}
}

// TestSession_RetryDoesNotRenumber. "If you retry a send, do not
// renumber" -- the engine drops duplicates and out-of-order chunks, so a
// renumbered retry opens a gap rather than closing one.
func TestSession_RetryDoesNotRenumber(t *testing.T) {
	fakeApp(t, "claude", "echo one\necho two\necho three\nexit 0\n")
	h := newHarness(t)
	h.sender.mu.Lock()
	h.sender.failNext = 1 // the first chunk send fails once
	h.sender.mu.Unlock()

	h.start(t, nil)

	chunks := h.sender.recorded()
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	seen := map[uint64]bool{}
	for i, c := range chunks {
		if seen[c.seq] {
			t.Fatalf("duplicate seq %d", c.seq)
		}
		seen[c.seq] = true
		if c.seq != uint64(i+1) {
			t.Fatalf("chunk %d has seq %d, want %d -- a retry renumbered", i, c.seq, i+1)
		}
	}
}

// TestSession_RefusesAnAppNotInPolicy. The engine derives its routing
// label from the allowed flag this machine reported, so it should never
// route here -- enforcing it again is the point. policy.yaml is the
// machine owner's word, checked where it is enforced rather than trusted
// from a round trip.
func TestSession_RefusesAnAppNotInPolicy(t *testing.T) {
	fakeApp(t, "codex", "exit 0\n")
	h := newHarness(t, apps.IDClaudeCode)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-denied"
		s.App = apps.IDCodex
	})
	if !strings.Contains(end.GetError(), "apps.allow") {
		t.Errorf("error = %q, want it to name policy.yaml apps.allow", end.GetError())
	}
}

// TestSession_RefusesAnUnknownApp.
func TestSession_RefusesAnUnknownApp(t *testing.T) {
	h := newHarness(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-unknown"
		s.App = "some-future-app"
	})
	if !strings.Contains(end.GetError(), "no runner") {
		t.Errorf("error = %q", end.GetError())
	}
}

// TestSession_AttachRequiresARef: kind=attach with nothing to resume must
// say so rather than quietly starting a fresh run, which would look like
// a resume and be a new session.
func TestSession_AttachRequiresARef(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")
	h := newHarness(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach-noref"
		s.Kind = KindAttach
	})
	if !strings.Contains(end.GetError(), "app_session_ref") {
		t.Errorf("error = %q, want it to name the missing ref", end.GetError())
	}
}

// TestSession_AttachResumesByRef.
func TestSession_AttachResumesByRef(t *testing.T) {
	fakeApp(t, "claude", `
if [ "$1" = "--resume" ] && [ "$2" = "app-earlier-run" ]; then
  echo '{"type":"result","total_cost_usd":0.1,"session_id":"app-earlier-run"}'
  exit 0
fi
echo "wrong argv: $@" 1>&2
exit 3
`)
	h := newHarness(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach"
		s.Kind = KindAttach
		s.AppSessionRef = "app-earlier-run"
	})
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s); transcript: %s", end.GetExitCode(), end.GetError(), h.sender.transcript())
	}
	if end.GetAppSessionRef() != "app-earlier-run" {
		t.Errorf("app_session_ref = %q", end.GetAppSessionRef())
	}
}

// TestSession_OpenFailsFastWhenTheAppIsMissing. An `open` that cannot
// launch must end immediately with a reason -- not return success and
// wait for a window that will never appear, and not fall back to a
// headless run, because the user asked to drive it themselves.
func TestSession_OpenFailsFastWhenTheAppIsMissing(t *testing.T) {
	// Nothing named `claude` on PATH.
	t.Setenv("PATH", t.TempDir())
	h := newHarness(t)

	done := make(chan *memqlv1.AppSessionEnd, 1)
	go func() {
		done <- h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-open-missing"
			s.Kind = KindOpen
		})
	}()

	select {
	case end := <-done:
		if end.GetError() == "" {
			t.Fatal("an open that could not launch must carry a non-empty error")
		}
		if !strings.Contains(end.GetError(), "PATH") {
			t.Errorf("error = %q, want it to name why it could not launch", end.GetError())
		}
		if end.GetExitCode() == 0 {
			t.Error("a failed open must not report a clean exit")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("open did not fail fast; it waited for a window that will never appear")
	}
}

// TestSession_DuplicateStartIsIgnored: a retried envelope must not give
// one session id two processes and two transcripts.
func TestSession_DuplicateStartIsIgnored(t *testing.T) {
	fakeApp(t, "claude", "echo once\nsleep 0.4\nexit 0\n")
	h := newHarness(t)
	start := &memqlv1.AppSessionStart{
		SessionId:   "sess-dup",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	}
	h.manager.Start(context.Background(), h.sender, start)
	h.manager.Start(context.Background(), h.sender, start)
	h.sender.wait(t)

	var onces int
	for _, c := range h.sender.recorded() {
		if strings.Contains(c.data, "once") {
			onces++
		}
	}
	if onces != 1 {
		t.Errorf("the app ran %d times for one session id", onces)
	}
}

// TestSession_StopAllEndsLiveSessions. A worker disconnect ends every
// live session with a named error rather than leaving callers parked
// until their own deadlines expire -- and rather than leaving an agent
// running with nothing watching it.
func TestSession_StopAllEndsLiveSessions(t *testing.T) {
	fakeApp(t, "claude", "echo started\nsleep 60\n")
	h := newHarness(t)
	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-stopall",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && h.manager.Live() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	h.manager.StopAll("the worker's stream to the cluster was lost")

	end := h.sender.wait(t)
	if !strings.Contains(end.GetError(), "stream") {
		t.Errorf("error = %q, want the reason carried through", end.GetError())
	}
	// The End is sent before the session deregisters, so this is
	// inherently a moment later.
	settled := time.Now().Add(10 * time.Second)
	for time.Now().Before(settled) && h.manager.Live() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if h.manager.Live() != 0 {
		t.Errorf("live sessions = %d after StopAll", h.manager.Live())
	}
}

// TestSession_RenewRewritesTheConfigAndTheLibraryBearer.
func TestSession_RenewRewritesTheConfigAndTheLibraryBearer(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	fakeApp(t, "claude", fmt.Sprintf("touch %q\nsleep 30\n", ready))
	h := newHarness(t)
	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-renew",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	const next = "eyJhbGciOiJSUzI1NiJ9.renewed-mid-run.sig"
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId:  "sess-renew",
		Action:     ActionRenewCredential,
		Credential: next,
	})

	data, err := os.ReadFile(filepath.Join(h.workspace, ".mcp.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), next) {
		t.Error("renew_credential did not rewrite the config in place")
	}
	if strings.Contains(string(data), testBearer) {
		t.Error("the superseded bearer survived the renewal")
	}

	h.manager.Control(&memqlv1.AppSessionControl{SessionId: "sess-renew", Action: ActionCancel})
	h.sender.wait(t)

	// The push at the end used the CURRENT bearer, not the one the
	// session opened with.
	var sawRenewed bool
	for _, b := range h.library.seenBearers() {
		if strings.Contains(b, next) {
			sawRenewed = true
		}
	}
	if !sawRenewed {
		t.Errorf("the Library calls did not pick up the renewed bearer: %v", h.library.seenBearers())
	}
}
