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

// --- the test rig ----------------------------------------------------

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

// readArgv reads the argv a fake app recorded, one argument per line.
//
// The argv is worth asserting rather than the app's behaviour because
// every way it goes wrong is SILENT: an app started without
// `--mcp-config` runs perfectly and reaches no MemQL tool, and one
// started without `--resume` holds a fresh conversation that looks
// exactly like a continued one.
func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the app recorded no argv at %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// argvValue returns the argument following flag, or "".
func argvValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
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

type rig struct {
	manager   *Manager
	sender    *fakeSender
	library   *fakeLibrary
	workspace string
	state     string
}

func newRig(t *testing.T, allow ...string) *rig {
	t.Helper()
	lib := newFakeLibrary(t)
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	if len(allow) == 0 {
		allowed[apps.IDClaudeCode] = true
	}
	h := &rig{
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

func (h *rig) start(t *testing.T, mutate func(*memqlv1.AppSessionStart)) *memqlv1.AppSessionEnd {
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
	h := newRig(t)
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
	h := newRig(t)
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
			h := newRig(t)
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
			h := newRig(t)
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

	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t)

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
		h := newRig(t)
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

	// A 401 used to be reported as "the cockpit's side: the bearer is
	// expired or malformed", and this test asserted that wording. Both were
	// wrong: before memql#4863 the bearer was fine and the Library simply
	// did not admit its class, and since memql#4863 the class IS admitted,
	// so what is left is an expiry the engine can fix in place. The
	// assertion moved with the sentence -- see memql-cockpit#371.
	t.Run("401 points at renewal, not at a broken cockpit", func(t *testing.T) {
		h := newRig(t)
		h.library.mu.Lock()
		h.library.pullCode["stale"] = http.StatusUnauthorized
		h.library.mu.Unlock()
		end := h.start(t, func(s *memqlv1.AppSessionStart) {
			s.SessionId = "sess-401"
			s.Inputs = []string{"stale"}
		})
		if !strings.Contains(end.GetError(), "renew_credential") {
			t.Errorf("error = %q, want it to name the renewal the engine can send", end.GetError())
		}
		if strings.Contains(end.GetError(), "malformed") {
			t.Errorf("error = %q, must not resurrect the malformed-bearer diagnosis", end.GetError())
		}
	})
}

// TestSession_ProducedFilesAndTranscriptArePushed.
func TestSession_ProducedFilesAndTranscriptArePushed(t *testing.T) {
	fakeApp(t, "claude", "echo 'result content' > output.txt\nmkdir -p sub && echo nested > sub/deep.txt\nexit 0\n")
	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t, apps.IDClaudeCode)
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
	h := newRig(t)
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
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach-noref"
		s.Kind = KindAttach
	})
	if !strings.Contains(end.GetError(), "app_session_ref") {
		t.Errorf("error = %q, want it to name the missing ref", end.GetError())
	}
}

// TestSession_AttachResumesByRef, and the argv the harness builds for it.
//
// The three assertions are the three ways this goes silently wrong.
// Without `--resume <ref>` the "resume" is a brand new conversation that
// looks identical from the outside. Without `--mcp-config <path>` the
// app starts perfectly and cannot reach a single MemQL tool. And the
// prompt has to come after `--`: `--mcp-config` is VARIADIC, so a prompt
// following it directly is swallowed as a second config path.
func TestSession_AttachResumesByRef(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	fakeApp(t, "claude", fmt.Sprintf(`
printf '%%s\n' "$@" > %q
echo '{"type":"result","total_cost_usd":0.1,"session_id":"app-earlier-run"}'
exit 0
`, argvFile))
	h := newRig(t)
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

	argv := readArgv(t, argvFile)
	if got := argvValue(argv, "--resume"); got != "app-earlier-run" {
		t.Errorf("--resume = %q, want the ref the engine named; argv: %v", got, argv)
	}
	if got := argvValue(argv, "--mcp-config"); got != filepath.Join(h.workspace, ".mcp.json") {
		t.Errorf("--mcp-config = %q, want the config this session wrote; argv: %v", got, argv)
	}
	if got := argvValue(argv, "--"); got != "do the thing" {
		t.Errorf("the prompt after -- = %q, want it a positional the option parser cannot eat; argv: %v", got, argv)
	}
}

// TestSession_OpenFailsFastWhenTheAppIsMissing. An `open` that cannot
// launch must end immediately with a reason -- not return success and
// wait for a window that will never appear, and not fall back to a
// headless run, because the user asked to drive it themselves.
func TestSession_OpenFailsFastWhenTheAppIsMissing(t *testing.T) {
	// Nothing named `claude` on PATH.
	t.Setenv("PATH", t.TempDir())
	h := newRig(t)

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
	h := newRig(t)
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
	h := newRig(t)
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
	h := newRig(t)
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

// --- turns -----------------------------------------------------------

// waitForChunk blocks until the session has streamed something, which is
// how a test knows the app is actually up.
func (h *rig) waitForChunk(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.sender.recorded()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the app never streamed anything; it did not start")
}

// twoTurnApp writes a fake `claude` that behaves differently on each
// turn, records each turn's argv, and holds the FIRST turn open until
// the test releases a gate file.
//
// The gate is what makes the follow-up arrive MID-TURN, which is the
// case that matters: a control cannot be answered, so the engine has no
// way to know a turn is in flight, and a cockpit that refused a
// follow-up for that reason would drop a prompt a person typed.
func twoTurnApp(t *testing.T, dir string) {
	t.Helper()
	fakeApp(t, "claude", fmt.Sprintf(`
d=%q
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
printf '%%s' "$n" > "$d/count"
printf '%%s\n' "$@" > "$d/argv.$n"
cat .mcp.json > "$d/config.$n" 2>/dev/null
if [ "$n" = "1" ]; then
  echo '{"type":"system","subtype":"init","session_id":"app-turn-1"}'
  i=0
  while [ ! -f "$d/gate" ] && [ $i -lt 400 ]; do i=$((i+1)); sleep 0.05; done
  echo '{"type":"result","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":20},"session_id":"app-turn-1"}'
  exit 0
fi
echo '{"type":"assistant","session_id":"app-turn-1","message":{"content":[{"type":"text","text":"the follow-up answer"},{"type":"tool_use","id":"t1","name":"Read"}]}}'
echo '{"type":"result","total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":2},"session_id":"app-turn-1"}'
exit 0
`, dir))
}

// TestSession_FollowUpRunsAsTheNextTurn is the two-turn session: a
// `message` control arriving while turn one is still running starts turn
// two in the SAME conversation, and the session ends when the queue
// drains rather than when the first process exits.
//
// It also pins the three things that only a second turn can show: the
// resume ref carried across processes, usage SUMMED over the session
// rather than taken from the last turn, and the typed chunks the harness
// produces (`text` for prose a person reads, `tool` for the envelope
// that carries the tool call).
func TestSession_FollowUpRunsAsTheNextTurn(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-followup",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	// Queued while turn one is in flight, then the gate is released so
	// turn one can finish. The order is the point: the follow-up is
	// accepted by a session that is busy, and it is delivered anyway.
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-followup",
		Action:    ActionMessage,
		Prompt:    "and now the follow-up",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}

	end := h.sender.wait(t)
	if end.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d (%s); transcript: %s", end.GetExitCode(), end.GetError(), h.sender.transcript())
	}

	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "2" {
		t.Fatalf("the app ran %q times, want 2 -- the follow-up did not become a turn (%v)", count, err)
	}

	// Turn two resumes the conversation turn one opened. Without this
	// the follow-up is a stranger with no context, which looks identical
	// from the outside.
	second := readArgv(t, filepath.Join(dir, "argv.2"))
	if got := argvValue(second, "--resume"); got != "app-turn-1" {
		t.Errorf("second turn --resume = %q, want the app's own session id; argv: %v", got, second)
	}
	if got := argvValue(second, "--"); got != "and now the follow-up" {
		t.Errorf("second turn prompt = %q, want the follow-up's text; argv: %v", got, second)
	}
	first := readArgv(t, filepath.Join(dir, "argv.1"))
	if got := argvValue(first, "--resume"); got != "" {
		t.Errorf("the FIRST turn resumed %q; a run with no app_session_ref starts fresh", got)
	}

	// Usage is the SESSION's, summed across its turns. Taking only the
	// last would under-report every conversation in a ledger somebody
	// bills from.
	usage := end.GetUsage()
	if !usage.GetKnown() {
		t.Fatal("both turns reported usage; known must be true")
	}
	if usage.GetInputTokens() != 11 || usage.GetOutputTokens() != 22 || usage.GetCostUsd() != 0.75 {
		t.Errorf("usage = %+v, want both turns summed (11/22/0.75)", usage)
	}
	if end.GetAppSessionRef() != "app-turn-1" {
		t.Errorf("app_session_ref = %q, want the conversation both turns ran in", end.GetAppSessionRef())
	}

	// The typed chunks the harness produced: prose as `text`, the
	// envelope carrying the tool call as `tool`. The old runner had
	// neither -- everything that parsed as JSON was an `event` and
	// everything else was narration.
	var sawText, sawTool bool
	for _, c := range h.sender.recorded() {
		switch c.stream {
		case StreamText:
			if strings.Contains(c.data, "the follow-up answer") {
				sawText = true
			}
		case StreamTool:
			if strings.Contains(c.data, "tool_use") {
				sawTool = true
			}
		}
	}
	if !sawText {
		t.Errorf("no `text` chunk carried the assistant's prose: %+v", h.sender.recorded())
	}
	if !sawTool {
		t.Errorf("no `tool` chunk carried the tool call: %+v", h.sender.recorded())
	}
}

// TestSession_RenewLandsOnTheNextTurn. Claude Code reads its MCP
// configuration at STARTUP, so a renewal mid-run cannot reach the
// process already running -- that is the known limitation of
// mcpconfig.go. One process per turn is what makes it honest: the
// replacement bearer is in place before the next turn's process starts,
// and this proves the app actually reads it there.
func TestSession_RenewLandsOnTheNextTurn(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-renew-turn",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	const next = "eyJhbGciOiJSUzI1NiJ9.renewed-between-turns.sig"
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId:  "sess-renew-turn",
		Action:     ActionRenewCredential,
		Credential: next,
	})
	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-renew-turn",
		Action:    ActionMessage,
		Prompt:    "carry on",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	h.sender.wait(t)

	// What the SECOND turn's process read off disk, captured by the fake
	// before it answered. Reading the file after the session would prove
	// nothing: it is deleted on the way out.
	second, err := os.ReadFile(filepath.Join(dir, "config.2"))
	if err != nil {
		t.Fatalf("the second turn read no config: %v", err)
	}
	if !strings.Contains(string(second), next) {
		t.Error("the second turn started with the superseded bearer; renewal is supposed to land on the next turn")
	}
	if strings.Contains(string(second), testBearer) {
		t.Error("the superseded bearer survived into the next turn's config")
	}
}

// TestSession_FollowUpQueueRefusesRatherThanSwallows.
//
// Both refusals exist because the alternative is silent. A follow-up
// queued into a session whose turn loop has already exited waits forever
// on a drain that will never come, and one appended to an unbounded
// queue is a memory hole fed by the network and drained by an app that
// takes minutes per turn. The queue is opened only by a running loop and
// latched shut by the same critical section that finds it empty.
func TestSession_FollowUpQueueRefusesRatherThanSwallows(t *testing.T) {
	s := &session{turnsClosed: true}

	if err := s.queueFollowUp("too late"); err == nil {
		t.Fatal("a follow-up for a session that is not taking turns must be refused, not queued")
	}

	s.openFollowUps()
	for i := range maxQueuedFollowUps {
		if err := s.queueFollowUp(fmt.Sprintf("prompt %d", i)); err != nil {
			t.Fatalf("follow-up %d was refused below the bound: %v", i, err)
		}
	}
	err := s.queueFollowUp("one too many")
	if err == nil {
		t.Fatal("the queue is unbounded; it is fed by the network")
	}
	if !strings.Contains(err.Error(), "not delivered") {
		t.Errorf("refusal = %q, want it to say the follow-up did not arrive", err)
	}

	for i := range maxQueuedFollowUps {
		got, ok := s.nextFollowUp()
		if !ok {
			t.Fatalf("the queue ran dry after %d of %d", i, maxQueuedFollowUps)
		}
		if want := fmt.Sprintf("prompt %d", i); got != want {
			t.Errorf("popped %q, want %q -- turns are a sequence, in order", got, want)
		}
	}
	if _, ok := s.nextFollowUp(); ok {
		t.Fatal("an empty queue must end the session, not invent a turn")
	}
	// Draining latched it shut, in the same critical section that found
	// it empty: anything less leaves a window where a follow-up is
	// accepted by a loop that has already decided to exit.
	if err := s.queueFollowUp("after the drain"); err == nil {
		t.Fatal("the queue accepted a follow-up after the turn loop had finished with it")
	}
}

// TestSession_StructuredResultRidesTheEnd pins AppSessionEnd.result_json
// (memql-cockpit#444), which the pin has carried since 2026-09-08 and this
// runner never set -- the answer left as a namespaced `event` chunk that
// nothing in the engine reads, so a structured answer never arrived.
//
// Two properties. The answer is on the End whatever the transcript cap has
// done, because the cap bounds NARRATION and an answer is not narration.
// And no chunk carries it any more: pre-release, a field that has landed
// replaces the stand-in rather than running beside it.
func TestSession_StructuredResultRidesTheEnd(t *testing.T) {
	newSession := func(sender *fakeSender, result []byte) *session {
		return &session{
			id:     "sess-result",
			start:  &memqlv1.AppSessionStart{SessionId: "sess-result"},
			sender: sender,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			redact: newRedactor(testBearer),
			result: result,
			// The transcript cap has ALREADY bitten: every ordinary
			// chunk from here on is dropped.
			capped: true,
		}
	}

	t.Run("the answer is on the end", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, []byte(`{"answer":42}`))
		s.sendEnd(0, "", nil)

		end := sender.wait(t)
		if end.GetResultJson() != `{"answer":42}` {
			t.Errorf("result_json = %q, want the app's own answer verbatim", end.GetResultJson())
		}
		for _, c := range sender.recorded() {
			if strings.Contains(c.data, "memql.app_session.result") {
				t.Errorf("the answer still left as a chunk as well: %q", c.data)
			}
		}
	})

	// IT DOES NOT IMPLY SUCCESS, which is the proto's own rule: a harness
	// can answer the schema and still exit non-zero, and dropping the
	// answer because the run failed would lose the one part we can read.
	t.Run("an answer rides a failed end too", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, []byte(`{"answer":42}`))
		s.sendEnd(2, "the app exited 2", nil)

		end := sender.wait(t)
		if end.GetResultJson() != `{"answer":42}` || end.GetExitCode() != 2 || end.GetError() == "" {
			t.Errorf("end = %+v, want the answer beside the failure, neither folded into the other", end)
		}
	})

	t.Run("no result means an empty field, never an empty object", func(t *testing.T) {
		sender := newFakeSender()
		s := newSession(sender, nil)
		s.sendEnd(0, "", nil)

		if got := sender.wait(t).GetResultJson(); got != "" {
			t.Errorf("result_json = %q, want empty: an empty object reads as \"the app answered nothing\"", got)
		}
		if chunks := sender.recorded(); len(chunks) != 0 {
			t.Errorf("chunks = %+v, want none", chunks)
		}
	})
}

// TestSession_ResponseSchemaReachesTheHarness pins
// AppSessionStart.response_schema_json (memql-cockpit#444). The engine has
// been sending it since the pin carried it; this runner answered "the
// engine asked for nothing" to every session, so no app was ever asked for
// a structured answer.
func TestSession_ResponseSchemaReachesTheHarness(t *testing.T) {
	dir := t.TempDir()
	fakeApp(t, "claude", fmt.Sprintf(`
printf '%%s\n' "$@" > %q
echo '{"type":"system","subtype":"init","session_id":"app-schema"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-schema","total_cost_usd":0.1,"usage":{"input_tokens":1,"output_tokens":2},"structured_output":{"city":"Paris"},"result":"{\"city\":\"Paris\"}"}'
`, filepath.Join(dir, "argv")))
	const schema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.ResponseSchemaJson = schema
	})

	if end.GetError() != "" {
		t.Fatalf("error = %q; transcript: %s", end.GetError(), h.sender.transcript())
	}
	if got := argvValue(readArgv(t, filepath.Join(dir, "argv")), "--json-schema"); got != schema {
		t.Errorf("--json-schema = %q, want the engine's schema verbatim", got)
	}
	if end.GetResultJson() != `{"city":"Paris"}` {
		t.Errorf("result_json = %q, want the structured answer the app produced", end.GetResultJson())
	}
}

// TestSession_AReasonIsNeverAPrompt pins AppSessionControl.prompt
// (memql-cockpit#444). The engine sends a follow-up in `prompt`, and this
// runner read `reason` -- which the proto documents as transcript free-text
// on cancel -- so every follow-up arrived empty and was dropped. The fix
// reads `prompt` and ONLY `prompt`: one field meaning two things cannot be
// read without knowing which branch wrote it.
func TestSession_AReasonIsNeverAPrompt(t *testing.T) {
	dir := t.TempDir()
	twoTurnApp(t, dir)
	h := newRig(t)

	h.manager.Start(context.Background(), h.sender, &memqlv1.AppSessionStart{
		SessionId:   "sess-reason",
		App:         apps.IDClaudeCode,
		Kind:        KindRun,
		Prompt:      "the first question",
		Workspace:   h.workspace,
		Credential:  testBearer,
		McpEndpoint: "https://mcp.example.com/mcp",
	})
	h.waitForChunk(t)

	h.manager.Control(&memqlv1.AppSessionControl{
		SessionId: "sess-reason",
		Action:    ActionMessage,
		Reason:    "free text a cancel would carry",
	})
	if err := os.WriteFile(filepath.Join(dir, "gate"), nil, 0o600); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	h.sender.wait(t)

	if count, _ := os.ReadFile(filepath.Join(dir, "count")); string(count) != "1" {
		t.Errorf("the app ran %q times, want 1: a message control with no prompt is not a turn", count)
	}
}

// TestSession_CodexHarnessComesFromTheMachineNotTheId is the reason the
// runner resolves the app through the Detector rather than through the
// static spec table.
//
// Two harnesses answer to the id `codex`. apps.Specs() carries the FLOOR
// -- `codex mcp-server`, which every Codex has -- so a runner that
// trusted it would drive every Codex in the fleet through the fallback,
// losing usage numbers and structured answers on every machine whose
// binary has the app-server. Only a probe of THIS machine's binary can
// tell, and it is the same probe the registration advertised with.
func TestSession_CodexHarnessComesFromTheMachineNotTheId(t *testing.T) {
	cases := []struct {
		name       string
		probeExits int
		want       string
		notWant    string
	}{
		{name: "a codex with an app-server is driven through it", probeExits: 0, want: "app-server", notWant: "mcp-server"},
		{name: "a codex without one falls back to the mcp-server tools", probeExits: 9, want: "mcp-server", notWant: "app-server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")
			fakeApp(t, "codex", fmt.Sprintf(`
echo "$*" >> %q
if [ "$1" = "app-server" ] && [ "$2" = "--help" ]; then exit %d; fi
exit 9
`, argvFile, tc.probeExits))

			h := newRig(t, apps.IDCodex)
			end := h.start(t, func(s *memqlv1.AppSessionStart) {
				s.SessionId = "sess-codex-" + tc.want
				s.App = apps.IDCodex
			})
			// Every fake dies at once, so the handshake fails; what is
			// under test is WHICH subcommand was launched.
			if end.GetError() == "" {
				t.Fatal("a codex that dies at startup must end the session with a reason")
			}
			// Both Codex harnesses speak JSON-RPC over the child's
			// stdio. A supervisor that handed them the default closed
			// stdin would fail every Codex session on this machine with
			// this sentence, whatever the app did.
			if strings.Contains(end.GetError(), "without a stdin") {
				t.Errorf("the harness got no writable stdin from the supervisor: %s", end.GetError())
			}

			launched := ""
			for _, line := range readArgv(t, argvFile) {
				if line == "app-server" || line == "mcp-server" {
					launched = line
				}
			}
			if launched != tc.want {
				t.Errorf("launched %q, want %q; the recorded invocations were %v",
					launched, tc.want, readArgv(t, argvFile))
			}
			if launched == tc.notWant {
				t.Errorf("this machine's codex was driven through %q", launched)
			}
		})
	}
}

// TestSession_HarnessThatCannotStartStillDeletesTheConfig.
//
// A harness that forks in Start -- both Codex clients do -- is a NEW
// exit path out of the session, taken before any turn has run. The
// deletion of the MCP configuration is the security control rather than
// housekeeping (the bearer in it cannot be revoked), so every exit path
// has to pass through the teardown, including one added later by
// somebody who was thinking about something else.
func TestSession_HarnessThatCannotStartStillDeletesTheConfig(t *testing.T) {
	fakeApp(t, "codex", "exit 9\n")
	h := newRig(t, apps.IDCodex)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-codex-dead"
		s.App = apps.IDCodex
	})

	if end.GetError() == "" {
		t.Fatal("a session whose app could not be started must carry a reason")
	}
	if end.GetExitCode() == 0 {
		t.Error("a session that never ran a turn must not report a clean exit")
	}
	if found := grepTree(t, h.workspace, testBearer); len(found) > 0 {
		t.Errorf("the bearer survived a failed harness start in: %v", found)
	}
	if found := grepTree(t, h.state, testBearer); len(found) > 0 {
		t.Errorf("the bearer reached the ledger, which records paths only: %v", found)
	}
}

// TestSession_AttachWithoutAPromptIsRefused.
//
// Attaching is now RESUMING AND SPEAKING, because that is the only thing
// either app's protocol offers -- neither has a "watch the run somebody
// else started" primitive. A turn with nothing to say would spend the
// machine owner's subscription to ask the app nothing, so the refusal
// names what attach actually does instead.
func TestSession_AttachWithoutAPromptIsRefused(t *testing.T) {
	fakeApp(t, "claude", "exit 0\n")
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.SessionId = "sess-attach-noprompt"
		s.Kind = KindAttach
		s.AppSessionRef = "app-earlier-run"
		s.Prompt = ""
	})
	if !strings.Contains(end.GetError(), "prompt") {
		t.Errorf("error = %q, want it to name the missing prompt", end.GetError())
	}
	if !strings.Contains(end.GetError(), "app-earlier-run") {
		t.Errorf("error = %q, want it to name the session it would have resumed", end.GetError())
	}
}

// TestLauncher_IsTheSupervisorWithStdinAsADecision.
//
// The harness clients fork NOTHING: every process they need comes from
// this adapter, so that production keeps exactly one process supervisor.
// What that buys is asserted end to end by
// TestSession_CancelKillsTheProcessGroup, which now runs through this
// path -- a second supervisor would have quietly reaped only the direct
// child and left an agent running on somebody's machine.
//
// What is asserted here is the one thing the adapter had to ADD: stdin.
// The default stays closed so a prompt-on-stdin app fails fast instead
// of hanging on input that will never come; a JSON-RPC harness that owns
// both ends of the pipe opts in, because for `codex app-server` a closed
// stdin is not a safety property but a client with nothing to say.
func TestLauncher_IsTheSupervisorWithStdinAsADecision(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "reader.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nif read line; then echo \"got: $line\"; else echo 'stdin was closed'; fi\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	run := func(t *testing.T, stdin bool, write string) (*session, string) {
		t.Helper()
		s := &session{}
		proc, err := s.launcher()(context.Background(), dir, []string{script}, nil, stdin)
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		if w := proc.Stdin(); w != nil {
			if write != "" {
				if _, err := io.WriteString(w, write); err != nil {
					t.Fatalf("write to the child's stdin: %v", err)
				}
			}
			_ = w.Close()
		}
		out, err := io.ReadAll(proc.Stdout())
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		_ = proc.Wait()
		return s, strings.TrimSpace(string(out))
	}

	t.Run("a harness that asks for stdin can speak its protocol", func(t *testing.T) {
		s, out := run(t, true, "hello\n")
		if out != "got: hello" {
			t.Errorf("the child read %q, want the line written into its stdin", out)
		}
		// Registered on the session, which is what makes cancel and
		// teardown reach it. A process nothing has a handle on is an
		// agent running with nothing watching it.
		s.mu.Lock()
		registered := s.child != nil
		s.mu.Unlock()
		if !registered {
			t.Error("the launched process was not registered on the session; cancel cannot reach it")
		}
	})

	t.Run("the default is closed, and a reader sees EOF at once", func(t *testing.T) {
		_, out := run(t, false, "")
		if out != "stdin was closed" {
			t.Errorf("the child read %q, want an immediate EOF", out)
		}
	})

	t.Run("a session already over starts no further process", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "it-ran")
		s := &session{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.launcher()(ctx, dir, []string{"/bin/sh", "-c", "touch " + marker}, nil, false); err == nil {
			t.Fatal("a cancel that lands between two turns must not start the next one")
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("a process was forked for a session that was already over")
		}
	})
}
