// Package appsession runs the engine's app sessions on this machine:
// AppSessionStart / Chunk / Control / End over the worker stream, with
// the run, open and attach kinds (memql-cockpit#347).
//
// WHY A SESSION AND NOT A ToolDispatch. A dispatch carries ONE timeout
// and returns ONE result. A headless `claude -p` can run for an hour and
// emits output the whole way, so the shape has to be a stream: start,
// chunks while it works, control from the server, one end.
//
// Three invariants run through the file, each because getting it wrong is
// silently destructive rather than loudly broken:
//
//   - `seq` is monotonic per session and is NEVER renumbered, including
//     on a retried send. The engine drops out-of-order and duplicate
//     chunks rather than appending them, because a transcript is a record
//     and interleaving a replayed chunk corrupts it in a way no later
//     reader can detect.
//
//   - `usage.known=false` when the app reported nothing. The engine
//     records that as billing "unknown", which is the honest answer; an
//     estimate would be recorded as measured, in a ledger somebody bills
//     from.
//
//   - the MCP configuration file is deleted on EVERY exit path. It holds
//     a bearer that cannot be revoked, so deletion is the control rather
//     than the tidy-up. See mcpconfig.go.
package appsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

// Session kinds, from AppSessionStart.kind.
const (
	KindRun    = "run"
	KindOpen   = "open"
	KindAttach = "attach"
)

// Chunk streams, from AppSessionChunk.stream.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamEvent  = "event"
)

// Control actions, from AppSessionControl.action.
const (
	ActionCancel          = "cancel"
	ActionRenewCredential = "renew_credential"
)

// transcriptRel is where the full transcript accumulates during a run,
// inside the session scaffolding so it is removed with it.
const transcriptRel = ".memql-session/transcript.log"

// Sender is the worker's side of the stream, as this package needs it.
type Sender interface {
	SendAppSessionChunk(sessionID, stream string, data []byte, seq uint64) error
	SendAppSessionEnd(end *memqlv1.AppSessionEnd) error
}

// Options configures a Manager.
type Options struct {
	Logger *slog.Logger
	// StateDir is the worker's state directory. The MCP write ledger
	// lives under it, which is what lets a restart sweep what a crash
	// left behind.
	StateDir string
	// ClusterURL is the worker's cluster_url; the Library origin is
	// derived from it.
	ClusterURL string
	// LibraryBase overrides that derivation. Tests set it; nothing else
	// should need to.
	LibraryBase string
	// HTTPClient is the client the Library calls use.
	HTTPClient *http.Client
	// Allowed reports whether policy.yaml apps.allow lists an app.
	// Nil means nothing is allowed, which is the default-deny posture
	// the rest of the worker has.
	Allowed func(appID string) bool
	// CheckWorkspace vetoes a workspace path. The delegation policy
	// picks the workspace root, but the cockpit still gets to refuse a
	// path outside its own -- the engine is naming a directory on
	// somebody else's machine.
	CheckWorkspace func(path string) error
}

// Manager owns every live session on this machine.
type Manager struct {
	opts   Options
	logger *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

// NewManager builds a Manager and sweeps whatever a previous process
// left behind.
func NewManager(opts Options) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: libraryTimeout}
	}
	m := &Manager{opts: opts, logger: logger, sessions: map[string]*session{}}
	if swept := Sweep(opts.StateDir); swept > 0 {
		// Worth a line at boot: it means a previous process died with a
		// live session, and a bearer sat on disk until now.
		logger.Warn("swept MCP configuration files left by a previous cockpit process",
			"files", swept)
	}
	return m
}

// Start opens a session. It returns immediately; the session runs on its
// own goroutine and reports itself through sender.
func (m *Manager) Start(ctx context.Context, sender Sender, start *memqlv1.AppSessionStart) {
	if start == nil || sender == nil {
		return
	}
	id := start.GetSessionId()
	if strings.TrimSpace(id) == "" {
		m.logger.Warn("app session start with no session id; ignoring")
		return
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{
		id:      id,
		start:   start,
		sender:  sender,
		manager: m,
		logger:  m.logger.With("session_id", id, "app", start.GetApp(), "kind", start.GetKind()),
		cancel:  cancel,
	}

	m.mu.Lock()
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		cancel()
		// A duplicate start is the server retrying an envelope it thinks
		// was lost. Running it twice would give one session id two
		// processes and two transcripts.
		m.logger.Warn("duplicate app session start ignored")
		return
	}
	m.sessions[id] = s
	m.mu.Unlock()

	go s.run(runCtx)
}

// Control applies a server-side steer to a live session.
func (m *Manager) Control(ctl *memqlv1.AppSessionControl) {
	if ctl == nil {
		return
	}
	m.mu.Lock()
	s := m.sessions[ctl.GetSessionId()]
	m.mu.Unlock()
	if s == nil {
		// A control for a session that already ended is normal: the end
		// and the cancel crossed on the wire.
		return
	}
	switch ctl.GetAction() {
	case ActionCancel:
		s.logger.Info("app session cancelled by the server", "reason", ctl.GetReason())
		s.cancelReason(ctl.GetReason())
	case ActionRenewCredential:
		s.renew(ctl.GetCredential())
	default:
		s.logger.Warn("unknown app session control action", "action", ctl.GetAction())
	}
}

// StopAll cancels every live session.
//
// Called when the stream dies and when the cockpit shuts down. A session
// whose stream is gone has nowhere to send chunks and nobody waiting for
// its end, and leaving the process running would be an agent working on
// somebody's machine with nothing watching it -- the same thing cancel
// exists to prevent.
func (m *Manager) StopAll(reason string) {
	m.mu.Lock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.cancelReason(reason)
	}
}

// Live reports how many sessions are running.
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// session is one live app session.
type session struct {
	id      string
	start   *memqlv1.AppSessionStart
	sender  Sender
	manager *Manager
	logger  *slog.Logger
	cancel  context.CancelFunc

	seqMu sync.Mutex
	seq   uint64

	// streamed is how many transcript bytes have been SENT, against
	// limits.max_transcript_bytes.
	streamed int64
	capped   bool

	redact *redactor

	mu            sync.Mutex
	mcp           *mcpConfig
	library       *Library
	child         *child
	cancelReason_ string

	transcript *os.File
	// before is the workspace as it stood when the run started, so the
	// push at the end carries what the run PRODUCED.
	before map[string]fileStamp

	// usage is what the app reported about itself, if anything.
	usageMu sync.Mutex
	usage   *memqlv1.AppSessionUsage
	appRef  string
}

// run drives the whole session and is the only place End is sent.
func (s *session) run(ctx context.Context) {
	defer s.manager.forget(s.id)
	defer func() {
		if rec := recover(); rec != nil {
			// A panic here must still delete the MCP config and still
			// close the session, or the caller waits forever on a run
			// that is already gone.
			s.logger.Error("app session panicked", "panic", rec)
			s.teardown()
			s.sendEnd(-1, fmt.Sprintf("cockpit panic during session: %v", rec), nil)
		}
	}()

	code, err := s.execute(ctx)
	s.teardown()

	artifacts, pushErr := s.pushOutputs(ctx)
	if err == nil && pushErr != nil {
		err = pushErr
	} else if pushErr != nil {
		err = fmt.Errorf("%w; additionally: %v", err, pushErr)
	}

	message := ""
	if err != nil {
		message = err.Error()
	}
	s.sendEnd(code, message, artifacts)
}

// execute resolves the session and runs it, returning the app's real exit
// code. A code of -1 means no process ran.
func (s *session) execute(ctx context.Context) (int, error) {
	spec, err := s.resolveApp()
	if err != nil {
		return -1, err
	}
	workspace, err := s.resolveWorkspace()
	if err != nil {
		return -1, err
	}

	s.redact = newRedactor(s.start.GetCredential())

	// The MCP configuration first: an app that starts without it reaches
	// nothing over MCP and reports that as "MemQL's tools are broken".
	mcp, err := writeMCPConfig(spec.ID, workspace, s.start.GetMcpEndpoint(),
		s.start.GetCredential(), s.id, s.manager.opts.StateDir)
	if err != nil {
		return -1, err
	}
	s.mu.Lock()
	s.mcp = mcp
	s.mu.Unlock()

	base := s.manager.opts.LibraryBase
	if strings.TrimSpace(base) == "" {
		base, err = LibraryBaseURL(s.manager.opts.ClusterURL)
		if err != nil {
			return -1, err
		}
	}
	library := NewLibrary(base, s.start.GetCredential(), s.manager.opts.HTTPClient)
	s.mu.Lock()
	s.library = library
	s.mu.Unlock()

	// Inputs land BEFORE the app starts. An agent that begins work and
	// finds its inputs half-arrived produces confidently wrong output
	// rather than an error.
	if err := s.pullInputs(ctx, workspace); err != nil {
		return -1, err
	}

	if err := s.openTranscript(workspace); err != nil {
		return -1, err
	}

	// The wall-clock ceiling. 0 means none.
	if max := s.start.GetLimits().GetMaxDurationSeconds(); max > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(max)*time.Second)
		defer stop()
	}

	// Snapshot before the run so the push at the end carries what the
	// run PRODUCED, not everything already in the directory.
	before := snapshotWorkspace(workspace)
	s.before = before

	switch s.start.GetKind() {
	case KindOpen:
		return s.runOpen(ctx, spec, workspace)
	case KindAttach:
		return s.runHeadless(ctx, spec, workspace, spec.AttachArgs(s.start.GetAppSessionRef()), true)
	case KindRun, "":
		return s.runHeadless(ctx, spec, workspace, spec.RunArgs(s.start.GetPrompt()), false)
	default:
		return -1, fmt.Errorf("app session: unknown kind %q", s.start.GetKind())
	}
}

// resolveApp checks the app is one this machine will run.
func (s *session) resolveApp() (apps.Spec, error) {
	id := strings.TrimSpace(s.start.GetApp())
	spec, ok := apps.SpecFor(id)
	if !ok {
		return apps.Spec{}, fmt.Errorf("app session: this cockpit has no runner for app %q", id)
	}
	allowed := s.manager.opts.Allowed
	if allowed == nil || !allowed(id) {
		// The engine should never route here -- it derives the routing
		// label from the allowed flag this machine reported. Enforcing
		// it again is the point: policy.yaml is the machine owner's
		// word, and it is checked where it is enforced rather than
		// trusted from a round trip.
		return apps.Spec{}, fmt.Errorf("app session: %q is not in this machine's policy.yaml apps.allow", id)
	}
	return spec, nil
}

// resolveWorkspace validates the directory the engine named.
func (s *session) resolveWorkspace() (string, error) {
	workspace := strings.TrimSpace(s.start.GetWorkspace())
	if workspace == "" {
		return "", errors.New("app session: no workspace in AppSessionStart")
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("app session: workspace %q is not absolute", workspace)
	}
	if check := s.manager.opts.CheckWorkspace; check != nil {
		if err := check(workspace); err != nil {
			return "", fmt.Errorf("app session: workspace refused by this machine's policy: %w", err)
		}
	}
	if err := os.MkdirAll(workspace, configDirMode); err != nil {
		return "", fmt.Errorf("app session: workspace: %w", err)
	}
	return workspace, nil
}

// pullInputs fetches every named artifact before the run starts.
func (s *session) pullInputs(ctx context.Context, workspace string) error {
	inputs := s.start.GetInputs()
	if len(inputs) == 0 {
		return nil
	}
	s.mu.Lock()
	library := s.library
	s.mu.Unlock()

	for _, id := range inputs {
		if _, err := library.Pull(ctx, id, workspace); err != nil {
			// Name the id that failed. "an input could not be fetched"
			// sends whoever reads this to check all of them.
			return err
		}
	}
	s.logger.Info("app session inputs landed", "count", len(inputs))
	return nil
}

// runHeadless runs the app with no human attached -- the run and attach
// kinds.
func (s *session) runHeadless(ctx context.Context, spec apps.Spec, workspace string, argv []string, isAttach bool) (int, error) {
	if len(argv) == 0 {
		if isAttach {
			// Say which app, and say it is the app's limitation rather
			// than a cockpit bug -- and do NOT quietly start a fresh
			// run, which would look like a resume and be a new session.
			return -1, fmt.Errorf("app session: %s has no resume mechanism this cockpit can drive, "+
				"so kind=attach cannot be honoured for app_session_ref %q", spec.ID, s.start.GetAppSessionRef())
		}
		return -1, fmt.Errorf("app session: no run command for app %q", spec.ID)
	}
	if isAttach && strings.TrimSpace(s.start.GetAppSessionRef()) == "" {
		return -1, errors.New("app session: kind=attach with no app_session_ref names no run to resume")
	}

	s.mu.Lock()
	env := s.mcp.Env()
	s.mu.Unlock()

	full := append([]string{spec.Binary}, argv...)
	c, err := startChild(workspace, full, env)
	if err != nil {
		return -1, err
	}
	s.mu.Lock()
	s.child = c
	s.mu.Unlock()

	readersDone := s.pump(c, s.emitStdout(spec), s.emitChunk)

	// The readers finish BEFORE the process is reaped, and that ordering
	// is not incidental: os/exec closes the pipes it handed out from
	// inside Wait, so reaping while a reader is still draining truncates
	// the transcript at whatever byte the race landed on. A record that
	// is silently short is worse than one that is late.
	select {
	case <-readersDone:
	case <-ctx.Done():
		// Cancel, the wall-clock limit, or the stream dying. Killing the
		// PROCESS GROUP is the point -- see process.go. It runs on its
		// own goroutine so the escalation timer never delays the drain:
		// a process that takes the SIGTERM EOFs its pipes immediately,
		// and one that ignores it gets SIGKILLed while this waits.
		go c.terminate()
		<-readersDone
	}
	return exitCode(c.wait()), s.contextFailure(ctx)
}

// pump starts the stdout and stderr readers and returns a channel closed
// when both have drained to EOF.
func (s *session) pump(c *child, onStdout, onStderr func(string, []byte) error) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = (&streamReader{name: StreamStdout, src: c.stdout, emit: onStdout, holdBack: s.redact.holdBack()}).run()
	}()
	go func() {
		defer wg.Done()
		_ = (&streamReader{name: StreamStderr, src: c.stderr, emit: onStderr, holdBack: s.redact.holdBack()}).run()
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// runOpen hands the app to the human -- the open kind.
func (s *session) runOpen(ctx context.Context, spec apps.Spec, workspace string) (int, error) {
	s.mu.Lock()
	env := s.mcp.Env()
	s.mu.Unlock()

	c, note, err := launchOpen(ctx, spec, workspace, s.start.GetPrompt(), env)
	if err != nil {
		// Immediately, with a reason, and with no fallback to headless:
		// the user asked to drive it themselves.
		return -1, err
	}
	s.mu.Lock()
	s.child = c
	s.mu.Unlock()

	_ = s.emitChunk(StreamStderr, []byte(note+"\n"))

	readersDone := s.pump(c, s.emitChunk, s.emitChunk)
	code := waitForOpen(ctx, c, filepath.Join(workspace, openSessionExitFile), readersDone)
	// Usage on this path is normally unknown, and that is correct: a
	// human-driven session does not report tokens back to the cockpit.
	// The engine records it as billing "unknown" rather than as free.
	return code, s.contextFailure(ctx)
}

// contextFailure turns a cancelled or expired context into the named
// error the End should carry, or nil when the run simply finished.
func (s *session) contextFailure(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	s.mu.Lock()
	reason := s.cancelReason_
	s.mu.Unlock()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("app session: exceeded limits.max_duration_seconds (%d)",
			s.start.GetLimits().GetMaxDurationSeconds())
	}
	if reason != "" {
		return fmt.Errorf("app session cancelled: %s", reason)
	}
	return errors.New("app session cancelled")
}

// cancelReason cancels the session, recording why for the End.
func (s *session) cancelReason(reason string) {
	s.mu.Lock()
	if s.cancelReason_ == "" {
		s.cancelReason_ = reason
	}
	c := s.child
	s.mu.Unlock()
	s.cancel()
	if c != nil {
		c.terminate()
	}
}

// renew swaps the bearer in the MCP configuration file and in the
// Library client.
func (s *session) renew(credential string) {
	if strings.TrimSpace(credential) == "" {
		s.logger.Warn("renew_credential carried no credential; ignoring")
		return
	}
	s.mu.Lock()
	mcp, library := s.mcp, s.library
	s.mu.Unlock()

	// Redact the NEW one too, and keep redacting the old: a chunk
	// already buffered may still carry it.
	s.redact.add(credential)
	if library != nil {
		library.SetCredential(credential)
	}
	if mcp != nil {
		if err := mcp.Renew(credential); err != nil {
			// Never log the credential, only the failure.
			s.logger.Error("app session credential renewal failed", "error", err)
			return
		}
	}
	s.logger.Info("app session credential renewed in place")
}

// teardown deletes the MCP configuration and stops the process. Every
// exit path passes through here, including the panic recovery.
func (s *session) teardown() {
	s.mu.Lock()
	mcp, c, transcript := s.mcp, s.child, s.transcript
	s.mcp = nil
	s.mu.Unlock()

	if c != nil {
		c.terminate()
	}
	if mcp != nil {
		mcp.Remove()
	}
	if transcript != nil {
		_ = transcript.Sync()
	}
}
