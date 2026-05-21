package consent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultSocketPath returns the canonical socket file path for the
// running user. The cockpit worker LISTENS at this path; the
// `memql-cockpit worker consent ...` subcommand DIALS it.
//
// The socket lives under ~/.memql/ because that's the existing
// per-user cockpit state surface; the parent dir is created at
// mode 0700 and the socket file at mode 0600 -- only the running
// user can connect, matching the security boundary the worker
// token shares.
func DefaultSocketPath() string {
	if h := os.Getenv("MEMQL_WORKER_CONSENT_SOCKET"); h != "" {
		return h
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".memql", "worker.sock")
	}
	return filepath.Join(os.TempDir(), "memql-worker.sock")
}

// Request is the line-oriented JSON command shape callers POST to
// the socket. One JSON object per line; the server replies with a
// single JSON response (Response). The WATCH op replies with a
// stream of Events instead -- one per line, until the client
// disconnects.
type Request struct {
	Op            string `json:"op"`
	WindowSeconds int    `json:"window_seconds,omitempty"`
	Strict        bool   `json:"strict,omitempty"`

	// ApprovalId carries the per-action approval handle on
	// "approve" / "deny" ops. The TUI receives the id on an
	// EventApprovalRequested broadcast and echoes it back on the
	// response op. Required for both approve and deny; ignored on
	// other ops.
	ApprovalId string `json:"approval_id,omitempty"`
}

// Response is the unified reply shape for non-WATCH ops.
type Response struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Status Status `json:"status,omitempty"`

	// Pending is populated on the initial WATCH response so a
	// reconnecting TUI can re-render its approval queue without
	// waiting for the next EventApprovalRequested broadcast.
	// Empty on every other op.
	Pending []PendingApprovalInfo `json:"pending,omitempty"`
}

// Server wraps a Manager with a Unix-socket interface.
type Server struct {
	mgr    *Manager
	logger *slog.Logger
	path   string

	mu       sync.Mutex
	listener net.Listener
	stopped  bool
}

// NewServer builds a Server bound to the given Manager.
func NewServer(mgr *Manager, path string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if mgr == nil {
		mgr = NewManager()
	}
	return &Server{mgr: mgr, logger: logger, path: path}
}

// Path returns the socket file path the server listens at.
func (s *Server) Path() string { return s.path }

// Listen opens the Unix socket and starts accepting connections in
// a goroutine. Returns once the listener is bound; ctx cancellation
// closes the listener and the accept loop exits.
//
// Refuses to bind if another process already holds the socket --
// the operator can manually remove ~/.memql/worker.sock if a stale
// file is left behind by an unclean shutdown. We do NOT silently
// unlink because that would let a malicious local process hijack
// the socket between our Stat and Bind.
func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("consent socket: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	// A stale socket file left by an unclean shutdown can't be bound
	// over -- net.Listen returns EADDRINUSE. We only unlink when the
	// peer is verifiably gone (no live connect). This is a coarse
	// probe (dial + 200ms timeout) but it catches the common case.
	if info, err := os.Stat(s.path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if c, derr := net.DialTimeout("unix", s.path, 200*time.Millisecond); derr != nil {
			// Looks dead -- safe to remove.
			_ = os.Remove(s.path)
		} else {
			_ = c.Close()
			return fmt.Errorf("consent socket: %s is already in use by another process", s.path)
		}
	}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("consent socket: listen %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("consent socket: chmod %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()
	s.logger.Info("worker consent socket listening", "path", s.path)

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	go s.acceptLoop(l)
	return nil
}

// Close stops accepting connections and removes the socket file.
// Safe to call repeatedly.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.path)
}

func (s *Server) acceptLoop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("consent socket: accept error", "error", err)
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// One request per connection -- keep the protocol simple. WATCH
	// holds the connection open and streams events until the client
	// disconnects.
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		s.logger.Warn("consent socket: read error", "error", err)
		return
	}
	if len(line) == 0 {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("decode request: %v", err)})
		return
	}
	switch req.Op {
	case "grant":
		s.handleGrant(c, req)
	case "revoke":
		s.handleRevoke(c)
	case "status":
		s.handleStatus(c)
	case "watch":
		s.handleWatch(c)
	case "approve":
		s.handleApprove(c, req)
	case "deny":
		s.handleDeny(c, req)
	default:
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("unknown op %q", req.Op)})
	}
}

func (s *Server) handleGrant(c net.Conn, req Request) {
	if req.WindowSeconds <= 0 {
		s.writeResp(c, Response{OK: false, Error: "window_seconds must be > 0"})
		return
	}
	if _, err := s.mgr.Grant(time.Duration(req.WindowSeconds)*time.Second, req.Strict); err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error()})
		return
	}
	s.writeResp(c, Response{OK: true, Status: s.mgr.Snapshot()})
}

func (s *Server) handleRevoke(c net.Conn) {
	s.mgr.Revoke()
	s.writeResp(c, Response{OK: true, Status: s.mgr.Snapshot()})
}

func (s *Server) handleStatus(c net.Conn) {
	s.writeResp(c, Response{OK: true, Status: s.mgr.Snapshot()})
}

func (s *Server) handleApprove(c net.Conn, req Request) {
	if req.ApprovalId == "" {
		s.writeResp(c, Response{OK: false, Error: "approval_id is required"})
		return
	}
	if err := s.mgr.Approve(req.ApprovalId); err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error()})
		return
	}
	s.writeResp(c, Response{OK: true, Status: s.mgr.Snapshot()})
}

func (s *Server) handleDeny(c net.Conn, req Request) {
	if req.ApprovalId == "" {
		s.writeResp(c, Response{OK: false, Error: "approval_id is required"})
		return
	}
	if err := s.mgr.Deny(req.ApprovalId); err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error()})
		return
	}
	s.writeResp(c, Response{OK: true, Status: s.mgr.Snapshot()})
}

func (s *Server) handleWatch(c net.Conn) {
	ch, cancel := s.mgr.Subscribe()
	defer cancel()
	// Send the initial state first so the client can render its
	// dashboard before any events arrive. Includes any pending
	// approvals already in flight so a reconnecting TUI doesn't
	// drop on-screen modals.
	if err := s.writeResp(c, Response{
		OK:      true,
		Status:  s.mgr.Snapshot(),
		Pending: s.mgr.PendingApprovals(),
	}); err != nil {
		return
	}
	enc := json.NewEncoder(c)
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
}

func (s *Server) writeResp(c net.Conn, r Response) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = c.Write(body)
	return err
}

// ---------------------------------------------------------------------------
// Client helpers
// ---------------------------------------------------------------------------

// Client is the thin sync-RPC client the `worker consent ...`
// subcommand uses to talk to a running worker. Each call opens a
// fresh connection (the protocol is one-shot for non-WATCH ops).
type Client struct {
	Path    string
	Timeout time.Duration
}

// DefaultClient returns a Client wired to DefaultSocketPath() with
// a 2-second dial timeout.
func DefaultClient() *Client {
	return &Client{Path: DefaultSocketPath(), Timeout: 2 * time.Second}
}

// Grant opens a consent window of the requested duration. Strict
// enables the per-action approval gate on the high-risk subset
// (workerComputer.key_type + workerComputer.mouse_click); see
// Manager.Allows.
func (c *Client) Grant(window time.Duration, strict bool) (Response, error) {
	if window <= 0 {
		return Response{}, errors.New("client: window must be positive")
	}
	return c.exec(Request{Op: "grant", WindowSeconds: int(window.Seconds()), Strict: strict})
}

// Revoke closes any active window.
func (c *Client) Revoke() (Response, error) { return c.exec(Request{Op: "revoke"}) }

// Approve resolves a strict-mode pending approval as ALLOW. The id
// comes from an EventApprovalRequested broadcast on the Watch
// stream. The worker side unblocks the dispatcher and the call
// proceeds.
func (c *Client) Approve(id string) (Response, error) {
	if id == "" {
		return Response{}, errors.New("client: approval id is required")
	}
	return c.exec(Request{Op: "approve", ApprovalId: id})
}

// Deny resolves a strict-mode pending approval as DENY. The
// dispatcher's blocked call returns with a typed `consent_required`
// failure carrying the deny reason.
func (c *Client) Deny(id string) (Response, error) {
	if id == "" {
		return Response{}, errors.New("client: approval id is required")
	}
	return c.exec(Request{Op: "deny", ApprovalId: id})
}

// Status returns the current Manager snapshot.
func (c *Client) Status() (Response, error) { return c.exec(Request{Op: "status"}) }

// Watch opens a long-lived connection and yields each event /
// status update to the supplied callback. Returns when the
// connection is closed (either side).
func (c *Client) Watch(ctx context.Context, onEvent func([]byte)) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	// Close the connection when ctx fires so the read loop exits.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	body, err := json.Marshal(Request{Op: "watch"})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 16*1024), 1<<20)
	for scanner.Scan() {
		onEvent(scanner.Bytes())
	}
	return scanner.Err()
}

func (c *Client) exec(req Request) (Response, error) {
	conn, err := c.dial()
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return Response{}, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("decode response: %w (raw=%q)", err, string(line))
	}
	return resp, nil
}

func (c *Client) dial() (net.Conn, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := net.DialTimeout("unix", c.Path, timeout)
	if err != nil {
		return nil, fmt.Errorf("consent client: dial %s: %w (is the worker running?)", c.Path, err)
	}
	return conn, nil
}
