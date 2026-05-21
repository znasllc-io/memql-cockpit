package workers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/worker/consent"
)

// fakeClient is the test double for consent IPC. Each method records
// invocations and returns the next queued response.
type fakeClient struct {
	mu sync.Mutex

	statusResp consent.Response
	statusErr  error

	grantResp consent.Response
	grantErr  error

	revokeResp consent.Response
	revokeErr  error

	grantCalls  []grantCall
	revokeCalls int32

	// watch hooks. lines are delivered in order to the callback;
	// the call blocks until ctx is canceled (mirroring the real
	// long-lived stream).
	watchLines [][]byte
	watchErr   error
}

type grantCall struct {
	window time.Duration
	strict bool
}

func (f *fakeClient) Status() (consent.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusResp, f.statusErr
}

func (f *fakeClient) Grant(window time.Duration, strict bool) (consent.Response, error) {
	f.mu.Lock()
	f.grantCalls = append(f.grantCalls, grantCall{window: window, strict: strict})
	resp := f.grantResp
	err := f.grantErr
	f.mu.Unlock()
	return resp, err
}

func (f *fakeClient) Revoke() (consent.Response, error) {
	atomic.AddInt32(&f.revokeCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokeResp, f.revokeErr
}

func (f *fakeClient) Watch(ctx context.Context, onEvent func([]byte)) error {
	f.mu.Lock()
	lines := f.watchLines
	err := f.watchErr
	f.mu.Unlock()
	for _, ln := range lines {
		onEvent(ln)
	}
	<-ctx.Done()
	return err
}

// boundsForView is the rect every test paints into. Wide and tall
// enough that the chrome contract assertions don't have to deal
// with truncation.
const (
	viewWidth  = 100
	viewHeight = 30
)

func makeView(t *testing.T) (*View, *ui.Screen, tcell.SimulationScreen, ui.Rect) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := NewView(ui.DefaultTheme())
	// Override default real-socket client so the test never hits a
	// real Unix socket. Subscribing a fake client also keeps the
	// watch goroutine inert in tests that don't explicitly Start().
	v.SetClient(&fakeClient{})

	return v, screen, sim, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight}
}

func drawAndSnapshot(v *View, screen *ui.Screen, sim tcell.SimulationScreen, bounds ui.Rect) []string {
	sim.Clear()
	v.Draw(screen, bounds)
	sim.Show()
	cells, w, h := sim.GetContents()
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		rows[y] = b.String()
	}
	return rows
}

// TestEmptyState pins the initial render: title says WORKERS, the
// status block reports OFFLINE (the fake client never connects),
// and the audit list shows the empty-state message.
func TestEmptyState(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	if !strings.Contains(rows[0], "WORKERS") {
		t.Errorf("title row missing 'WORKERS': %q", rows[0])
	}
	if !strings.Contains(rows[0], "worker offline") {
		t.Errorf("title counter should report worker offline: %q", rows[0])
	}
	// State row.
	stateRow := ""
	for _, r := range rows {
		if strings.Contains(r, "State") {
			stateRow = r
			break
		}
	}
	if !strings.Contains(stateRow, "OFFLINE") {
		t.Errorf("State row should say OFFLINE: %q", stateRow)
	}
	// Audit empty message lands somewhere in the rendered grid.
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "No worker activity yet") {
		t.Errorf("audit pane should advertise empty-state message")
	}
}

// TestGrantedFromEvent: applying an EventGranted via the Watch
// pipeline flips the status block to GRANTED with the right
// expiry + strict flag.
func TestGrantedFromEvent(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	expires := time.Now().UTC().Add(30 * time.Minute)
	v.applyEvent(consent.Event{
		At:        time.Now().UTC(),
		Kind:      consent.EventGranted,
		ExpiresAt: expires,
		Window:    1 * time.Hour,
		Strict:    true,
	})

	rows := drawAndSnapshot(v, screen, sim, bounds)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "GRANTED") {
		t.Errorf("state should be GRANTED after EventGranted")
	}
	if !strings.Contains(joined, "granted (strict)") {
		t.Errorf("title counter should report granted (strict): %q", rows[0])
	}
}

// TestAuditTailNewestFirst: events render with newest at the top.
func TestAuditTailNewestFirst(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	base := time.Date(2026, 5, 21, 17, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		v.applyEvent(consent.Event{
			At:      base.Add(time.Duration(i) * time.Second),
			Kind:    consent.EventDispatch,
			Tool:    "workerHost",
			Action:  "fs_read",
			Class:   consent.ClassObserve,
			Allowed: true,
			Reason:  "consent granted",
		})
	}
	rows := drawAndSnapshot(v, screen, sim, bounds)
	// The first audit row (under the AUDIT TAIL title) should be
	// the LAST event we applied (newest).
	auditIdx := -1
	for i, r := range rows {
		if strings.Contains(r, "AUDIT TAIL") {
			auditIdx = i + 1
			break
		}
	}
	if auditIdx < 0 {
		t.Fatalf("audit title row not found")
	}
	first := rows[auditIdx]
	if !strings.Contains(first, "workerHost.fs_read") {
		t.Errorf("first audit row should show workerHost.fs_read, got %q", first)
	}
	// Local time formatting strips the date; confirm we render a
	// timestamp that matches the newest-applied event's local time.
	want := base.Add(2 * time.Second).Local().Format("15:04:05")
	if !strings.Contains(first, want) {
		t.Errorf("first audit row should carry timestamp %s (newest), got %q", want, first)
	}
}

// TestAuditRingBufferCap ensures the ring keeps at most maxAuditEvents
// entries; older ones drop off the bottom.
func TestAuditRingBufferCap(t *testing.T) {
	v, _, _, _ := makeView(t)
	for i := 0; i < maxAuditEvents+50; i++ {
		v.applyEvent(consent.Event{
			At:   time.Now(),
			Kind: consent.EventDispatch,
			Tool: "workerHost",
		})
	}
	v.Mu.RLock()
	got := len(v.events)
	v.Mu.RUnlock()
	if got != maxAuditEvents {
		t.Errorf("ring size: want %d, got %d", maxAuditEvents, got)
	}
}

// TestGrantModalKeyFlow: pressing G opens the modal, ↓ moves the
// preset cursor, S toggles strict, Enter calls Client.Grant with
// the picked duration + strict flag, and Esc would have cancelled.
func TestGrantModalKeyFlow(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	fc := &fakeClient{
		grantResp: consent.Response{
			OK: true,
			Status: consent.Status{
				Granted:   true,
				ExpiresAt: time.Now().Add(1 * time.Hour),
				Window:    1 * time.Hour,
				Strict:    true,
			},
		},
	}
	v.SetClient(fc)

	// Press G -> modal opens.
	consumed := v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModNone))
	if !consumed {
		t.Fatalf("G should be consumed")
	}
	v.Mu.RLock()
	modalOpen := v.grantModal != nil
	v.Mu.RUnlock()
	if !modalOpen {
		t.Fatalf("G should open the grant modal")
	}

	// ↓ moves to the second preset (1 hour).
	v.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	// S toggles strict on.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModNone))

	// Snapshot the modal frame so the chrome chips assertion below
	// can fire while the modal is still open.
	modalRows := drawAndSnapshot(v, screen, sim, bounds)
	joined := strings.Join(modalRows, "\n")
	if !strings.Contains(joined, "GRANT CONSENT") {
		t.Errorf("modal title GRANT CONSENT missing: %q", joined)
	}
	if !strings.Contains(joined, "Strict mode: ON") {
		t.Errorf("strict toggle should report ON: %q", joined)
	}

	// Enter submits.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	// doGrant runs in a goroutine; give it a moment to land.
	waitForGrantCall(t, fc, 1)

	calls := snapshotGrantCalls(fc)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 Grant call, got %d", len(calls))
	}
	if calls[0].window != 1*time.Hour {
		t.Errorf("expected 1h window, got %s", calls[0].window)
	}
	if !calls[0].strict {
		t.Errorf("expected strict=true")
	}

	// Modal should be closed.
	v.Mu.RLock()
	modalOpen = v.grantModal != nil
	v.Mu.RUnlock()
	if modalOpen {
		t.Errorf("modal should close after Enter")
	}
}

// TestGrantModalEscape: pressing Esc inside the modal closes it
// without calling Grant.
func TestGrantModalEscape(t *testing.T) {
	v, _, _, _ := makeView(t)
	fc := &fakeClient{}
	v.SetClient(fc)
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModNone))
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))

	v.Mu.RLock()
	modalOpen := v.grantModal != nil
	v.Mu.RUnlock()
	if modalOpen {
		t.Errorf("Esc should close modal")
	}
	if len(snapshotGrantCalls(fc)) != 0 {
		t.Errorf("Esc must not trigger a Grant call")
	}
}

// TestRevokeKey: pressing R from the audit-list focus calls
// Client.Revoke once.
func TestRevokeKey(t *testing.T) {
	v, _, _, _ := makeView(t)
	fc := &fakeClient{
		revokeResp: consent.Response{OK: true, Status: consent.Status{Granted: false}},
	}
	v.SetClient(fc)
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone))
	waitForRevokeCount(t, fc, 1)
}

// TestKillSwitchEntryPoint: View.Revoke is the entry point the
// global Ctrl+E binding calls into. Returns the client error
// surface and updates IsGranted to reflect the new state.
func TestKillSwitchEntryPoint(t *testing.T) {
	v, _, _, _ := makeView(t)
	// Seed: prentend a window is open so IsGranted reports true.
	v.applyEvent(consent.Event{
		At:        time.Now().UTC(),
		Kind:      consent.EventGranted,
		ExpiresAt: time.Now().UTC().Add(1 * time.Hour),
		Window:    1 * time.Hour,
	})
	if !v.IsGranted() {
		t.Fatalf("IsGranted should be true after EventGranted")
	}

	fc := &fakeClient{
		revokeResp: consent.Response{OK: true, Status: consent.Status{Granted: false}},
	}
	v.SetClient(fc)
	if err := v.Revoke(); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if atomic.LoadInt32(&fc.revokeCalls) != 1 {
		t.Errorf("expected exactly 1 underlying client Revoke, got %d", fc.revokeCalls)
	}
	if v.IsGranted() {
		t.Errorf("IsGranted should be false after Revoke")
	}
}

// TestChromeContract: title row carries the pane name, hint chips
// follow Key:Label grammar separated by two spaces, and search is
// NOT advertised (the Workers tab doesn't filter -- `:` is a no-op).
func TestChromeContract(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	// Seed an event so the audit chip appears.
	v.applyEvent(consent.Event{
		At:      time.Now().UTC(),
		Kind:    consent.EventDispatch,
		Tool:    "workerHost",
		Action:  "fs_read",
		Allowed: true,
	})
	rows := drawAndSnapshot(v, screen, sim, bounds)

	// Title is left-aligned in the title row.
	if !strings.HasPrefix(strings.TrimSpace(rows[0]), "WORKERS") {
		t.Errorf("title row must start with WORKERS: %q", rows[0])
	}

	// Hint row is the LAST row of the pane.
	hint := strings.TrimSpace(rows[len(rows)-1])
	// No 'Press X to Y' prose, no colons-with-spaces.
	if strings.Contains(hint, "Press ") {
		t.Errorf("hint should not contain 'Press ...' prose: %q", hint)
	}
	// G:Grant is present when no window is open.
	if !strings.Contains(hint, "G:Grant") {
		t.Errorf("hint should advertise G:Grant when no consent: %q", hint)
	}
	// Two-space separator: the chip after G:Grant should be
	// preceded by two spaces, not one.
	if strings.Contains(hint, "G:Grant ") && !strings.Contains(hint, "G:Grant  ") {
		t.Errorf("hint chips must be separated by two spaces: %q", hint)
	}
}

// TestWatchLineDispatch confirms the JSON-line discriminator on
// onWatchLine routes responses vs events correctly.
func TestWatchLineDispatch(t *testing.T) {
	v, _, _, _ := makeView(t)

	// Initial Response (status only).
	v.onWatchLine([]byte(`{"ok":true,"status":{"granted":true,"expires_at":"2026-05-21T18:00:00Z","window_ms":3600000000000,"strict":false}}`))
	if !v.IsGranted() {
		t.Errorf("Response should update IsGranted")
	}

	// An EventDispatch lands in the audit ring.
	v.onWatchLine([]byte(`{"at":"2026-05-21T17:01:00Z","kind":"dispatch","tool":"workerHost","action":"fs_read","class":"observe","allowed":true}`))
	v.Mu.RLock()
	count := len(v.events)
	v.Mu.RUnlock()
	if count != 1 {
		t.Errorf("dispatch event should land in audit ring; have %d entries", count)
	}

	// Garbage line is ignored (no panic, no state change).
	v.onWatchLine([]byte(`not-json`))
	v.Mu.RLock()
	count = len(v.events)
	v.Mu.RUnlock()
	if count != 1 {
		t.Errorf("garbage line should not change ring: have %d", count)
	}
}

// TestFormatDuration covers the compact h/m/s rendering.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{2 * time.Minute, "2m0s"},
		{1*time.Hour + 15*time.Minute, "1h15m"},
		{8 * time.Hour, "8h0m"},
	}
	for _, tc := range cases {
		got := formatDuration(tc.in)
		if got != tc.want {
			t.Errorf("formatDuration(%v): want %q, got %q", tc.in, tc.want, got)
		}
	}
}

// --- helpers ---

func snapshotGrantCalls(fc *fakeClient) []grantCall {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	out := make([]grantCall, len(fc.grantCalls))
	copy(out, fc.grantCalls)
	return out
}

func waitForGrantCall(t *testing.T, fc *fakeClient, want int) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		fc.mu.Lock()
		n := len(fc.grantCalls)
		fc.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d grant calls", want)
}

func waitForRevokeCount(t *testing.T, fc *fakeClient, want int32) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fc.revokeCalls) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d revoke calls", want)
}
