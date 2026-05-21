// Package workers renders the Workers tab: the cockpit's window
// into the local worker daemon's computer-use consent state.
//
// The tab is built on top of the consent IPC layer that landed in
// memql-cockpit#64 MVP (PR #123): a Unix-socket server at
// ~/.memql/worker.sock that exposes grant / revoke / status / watch
// ops. The Workers view is a long-running Watch client; it renders
// the current consent window (state, expiry, strict flag) and a
// live tail of every worker tool dispatch (allowed + denied).
//
// Operator affordances surfaced here:
//   - G:Grant   opens the duration picker (5m / 1h / Session).
//   - R:Revoke  closes the active window immediately.
//   - Up/Down + PgUp/PgDn  scroll the audit tail.
//
// A separate global kill switch (Ctrl+E, wired in cli/app.go)
// revokes from any tab without needing to switch here first.
//
// Strict-mode per-action approval (typed-text + non-region-confined
// mouse_click) lands as a follow-up under #64 -- this tab will host
// that approval modal once the dispatcher gate's strict path is
// enforcement-enabled.
package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/worker/consent"
)

// maxAuditEvents is the ring-buffer size for the live audit tail.
// Older events fall off the bottom. The buffer is intentionally
// bounded -- the audit pane is a live tail, not a durable log;
// a persistent record would live on the worker side and ship
// separately.
const maxAuditEvents = 256

// reconnectBackoff is how long the Watch loop waits between
// dial attempts when the worker daemon socket isn't reachable.
// Short enough that the pane wakes up quickly after the user
// starts the worker; long enough not to busy-loop dial() when
// they don't.
const reconnectBackoff = 2 * time.Second

// grantPreset names one selectable window length in the Grant modal.
// Defined as a typed constant so the test can drive the modal
// through specific selections without hardcoding indices in two places.
type grantPreset struct {
	Label    string
	Duration time.Duration
}

// grantPresets is the menu the Grant modal shows. Order matters --
// 5m is the safest default and lives at the top so a blind Enter
// picks the most conservative window.
var grantPresets = []grantPreset{
	{Label: "5 minutes", Duration: 5 * time.Minute},
	{Label: "1 hour", Duration: 1 * time.Hour},
	{Label: "Rest of session (8 h)", Duration: 8 * time.Hour},
}

// Client is the slice of consent.Client our View depends on,
// expressed as an interface so tests can inject a fake without
// standing up a real socket.
type Client interface {
	Status() (consent.Response, error)
	Grant(window time.Duration, strict bool) (consent.Response, error)
	Revoke() (consent.Response, error)
	Watch(ctx context.Context, onEvent func([]byte)) error
}

// View renders the Workers tab. Single-pane (no internal focus
// switching) -- the chrome-contract band lives at the bottom of
// the same rect that paints the status block + audit list.
//
// Thread-safety. Three writer sources:
//   - the tcell event-loop goroutine (HandleEvent),
//   - the Watch goroutine (started by Start, pushes incoming events),
//   - the modal-action goroutines (grant / revoke responses).
//
// Draw runs from the event-loop goroutine and takes a read lock;
// mutators take a write lock. Mutex lives in the embedded BaseView.
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	// status mirrors the most recent Status reply the worker has
	// sent (either via the initial Watch response or a fresh
	// status RPC). Empty when no connection has ever succeeded.
	status consent.Status

	// connected reports whether the long-lived Watch connection
	// is currently alive. The Draw path uses it to color the
	// state line and to drive a "worker offline" banner.
	connected bool

	// lastErr is the most recent connection or RPC error string,
	// surfaced in the status block when the worker is offline so
	// the operator can tell whether the daemon is down or whether
	// there's a different problem.
	lastErr string

	// events is the audit ring buffer. New events append; once
	// len exceeds maxAuditEvents, the oldest is dropped. Render
	// order is newest-first.
	events []consent.Event

	// grantModal is non-nil while the Grant modal is open.
	grantModal *grantModalState

	// auditList drives scroll/cursor over the audit ring.
	auditList ui.ListPane

	// client is the consent IPC client we Watch / Grant / Revoke
	// against. Injectable; nil means "use the default Unix socket".
	client Client

	// lifecycle plumbing. cancel terminates the Watch loop on Stop.
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
}

// grantModalState is the transient state for the duration picker.
type grantModalState struct {
	selected int  // index into grantPresets
	strict   bool // toggled with 's' before submitting
}

// NewView creates an empty Workers view. The Client defaults to
// consent.DefaultClient() (Unix socket at ~/.memql/worker.sock,
// 2s dial timeout); tests inject a fake via SetClient.
func NewView(theme ui.Theme) *View {
	v := &View{}
	v.Theme = theme
	v.auditList.RowsPerItem = 2
	v.auditList.Render = v.renderEventRow
	v.auditList.EmptyMessage = "No worker activity yet."
	v.auditList.Focused = true
	v.client = consent.DefaultClient()
	return v
}

// SetClient replaces the consent IPC client. Used by tests; in
// production the default client wired in NewView is what the user
// sees.
func (v *View) SetClient(c Client) {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.client = c
}

// Start kicks off the background Watch loop. Safe to call multiple
// times -- only the first call wires the goroutine.
func (v *View) Start(ctx context.Context) {
	v.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		v.cancel = cancel
		go v.watchLoop(ctx)
	})
}

// Stop cancels the Watch loop. Safe to call multiple times.
func (v *View) Stop() {
	v.stopOnce.Do(func() {
		if v.cancel != nil {
			v.cancel()
		}
	})
}

// watchLoop maintains a long-lived consent.Watch subscription with
// reconnect. Every Watch call delivers an initial Response (current
// Status) followed by a stream of Event lines. The loop dispatches
// them via onLine + reconnects on disconnect.
func (v *View) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		client := v.snapshotClient()
		if client == nil {
			// No client configured (test paths can set client=nil
			// to disable the background loop entirely).
			return
		}

		err := client.Watch(ctx, v.onWatchLine)
		// Watch returns on either a clean ctx-cancel or a socket
		// disconnect / dial failure. Mark disconnected and try
		// again after a short backoff.
		v.markDisconnected(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectBackoff):
		}
	}
}

// snapshotClient returns the current client under the read lock so
// SetClient calls from tests aren't racy with the Watch goroutine.
func (v *View) snapshotClient() Client {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	return v.client
}

// onWatchLine is invoked once per raw JSON line from the Watch
// stream. The first line is the initial Response (current Status);
// subsequent lines are Event objects. We discriminate by the
// "kind" field (present only on events).
func (v *View) onWatchLine(line []byte) {
	// Cheap discriminator: events carry "kind", responses carry "ok".
	// Both are tagged unique to their shape so unmarshal-twice without
	// a probe step is the simplest robust path.
	var probe struct {
		Kind string `json:"kind"`
		OK   *bool  `json:"ok"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return
	}
	if probe.Kind != "" {
		var ev consent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return
		}
		v.applyEvent(ev)
		return
	}
	if probe.OK != nil {
		var resp consent.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return
		}
		v.applyStatus(resp.Status, resp.OK, resp.Error)
		return
	}
}

func (v *View) applyEvent(ev consent.Event) {
	v.Mu.Lock()
	v.connected = true
	v.lastErr = ""

	// State-affecting events keep the status block in sync with
	// the audit tail without requiring a separate status RPC.
	switch ev.Kind {
	case consent.EventGranted:
		v.status = consent.Status{
			Granted:   true,
			ExpiresAt: ev.ExpiresAt,
			Window:    ev.Window,
			Strict:    ev.Strict,
		}
	case consent.EventRevoked:
		v.status = consent.Status{Granted: false}
	}

	// Append to the audit ring. Bounded length.
	v.events = append(v.events, ev)
	if len(v.events) > maxAuditEvents {
		v.events = v.events[len(v.events)-maxAuditEvents:]
	}
	v.auditList.Count = len(v.events)
	v.Mu.Unlock()
	v.Redraw()
}

func (v *View) applyStatus(s consent.Status, ok bool, errMsg string) {
	v.Mu.Lock()
	v.connected = ok
	if !ok {
		v.lastErr = errMsg
	} else {
		v.lastErr = ""
		v.status = s
	}
	v.Mu.Unlock()
	v.Redraw()
}

func (v *View) markDisconnected(err error) {
	v.Mu.Lock()
	v.connected = false
	if err != nil {
		v.lastErr = err.Error()
	}
	v.Mu.Unlock()
	v.Redraw()
}

// Draw renders the Workers tab inside bounds. Layout (top -> bottom):
//
//	[title bar]   WORKERS                           <state-summary>
//	[status]      5 lines: socket / state / expires / window / strict
//	[audit title] AUDIT TAIL                                 cursor N/M
//	[audit list]  scrollable, newest first
//	[chrome]      hint bar OR modal overlay
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	// Title row (top).
	ui.PaneTitle{
		Title:   "WORKERS",
		Counter: v.statusSummary(),
		Focused: true,
	}.Draw(screen, bounds, v.Theme)

	// Status block (rows 1..statusHeight).
	statusHeight := 6
	statusBounds := ui.Rect{
		X:      bounds.X,
		Y:      bounds.Y + 1,
		Width:  bounds.Width,
		Height: statusHeight,
	}
	v.drawStatus(screen, statusBounds)

	// Audit title row.
	auditTitleY := bounds.Y + 1 + statusHeight
	ui.PaneTitle{
		Title:   "AUDIT TAIL",
		Counter: ui.FormatCursor(v.auditList.Selected, len(v.events)),
		Focused: true,
	}.Draw(screen, ui.Rect{X: bounds.X, Y: auditTitleY, Width: bounds.Width, Height: 1}, v.Theme)

	// Reserve the last two rows for the hint bar (or modal hint).
	hintRows := 1
	auditTop := auditTitleY + 1
	auditBottom := bounds.Y + bounds.Height - hintRows
	auditBounds := ui.Rect{
		X:      bounds.X + 1,
		Y:      auditTop,
		Width:  bounds.Width - 2,
		Height: auditBottom - auditTop,
	}
	if auditBounds.Height > 0 {
		v.auditList.Draw(screen, auditBounds, v.Theme)
	}

	// Chrome (bottom). If a modal is open, paint it over the audit
	// list AFTER the list so it sits on top.
	v.drawChrome(screen, bounds)
	if v.grantModal != nil {
		v.drawGrantModal(screen, bounds)
	}
}

// statusSummary is the right-aligned counter on the title row.
// Compact -- the audit pane has its own counter below.
func (v *View) statusSummary() string {
	if !v.connected {
		return "worker offline"
	}
	if !v.status.Granted {
		return "no consent"
	}
	if v.status.Strict {
		return "granted (strict)"
	}
	return "granted"
}

func (v *View) drawStatus(screen *ui.Screen, bounds ui.Rect) {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	style := v.Theme.BaseStyle()
	subtle := v.Theme.SubtleStyle()
	accent := v.Theme.AccentStyle().Bold(true)

	rows := []struct {
		label string
		value string
		style tcell.Style
	}{
		{label: "Socket", value: consent.DefaultSocketPath(), style: subtle},
		{label: "State", value: v.stateLabel(), style: v.stateStyle(style, accent)},
		{label: "Expires", value: v.expiresLabel(), style: style},
		{label: "Window", value: v.windowLabel(), style: subtle},
		{label: "Strict", value: v.strictLabel(), style: style},
	}
	if v.lastErr != "" && !v.connected {
		rows = append(rows, struct {
			label string
			value string
			style tcell.Style
		}{label: "Error", value: v.lastErr, style: v.Theme.SubtleStyle()})
	}
	for i, r := range rows {
		y := bounds.Y + i
		if y >= bounds.Y+bounds.Height {
			break
		}
		screen.DrawText(bounds.X+2, y, 8, r.label, subtle)
		screen.DrawText(bounds.X+12, y, bounds.Width-13, r.value, r.style)
	}
}

func (v *View) stateLabel() string {
	if !v.connected {
		return "OFFLINE"
	}
	if !v.status.Granted {
		return "NO CONSENT"
	}
	return "GRANTED"
}

func (v *View) stateStyle(base, accent tcell.Style) tcell.Style {
	if !v.connected {
		return v.Theme.SubtleStyle()
	}
	if v.status.Granted {
		return accent
	}
	return base.Bold(true)
}

func (v *View) expiresLabel() string {
	if !v.status.Granted {
		return "--"
	}
	now := time.Now().UTC()
	remaining := v.status.ExpiresAt.Sub(now)
	if remaining < 0 {
		return v.status.ExpiresAt.Format(time.RFC3339) + " (expired)"
	}
	return fmt.Sprintf("%s (in %s)", v.status.ExpiresAt.Format(time.RFC3339), formatDuration(remaining))
}

func (v *View) windowLabel() string {
	if v.status.Window <= 0 {
		return "--"
	}
	return formatDuration(v.status.Window)
}

func (v *View) strictLabel() string {
	if !v.status.Granted {
		return "--"
	}
	if v.status.Strict {
		return "yes"
	}
	return "no"
}

// formatDuration is a compact "Xh Ym" / "Ym Zs" / "Xs" rendering
// that drops zero-valued higher units so the status line stays
// honest at a glance.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// renderEventRow paints one audit-tail row. RowsPerItem = 2:
//
//	[time]  [tool.action]                [class]  [ALLOW|DENY]
//	  [reason -- dimmed, wrapped to width]
//
// Events are rendered newest-first so the freshest dispatch is at
// the top of the audit pane.
func (v *View) renderEventRow(screen *ui.Screen, bounds ui.Rect, idx int, selected bool, theme ui.Theme) {
	// Newest-first: invert the index.
	srcIdx := len(v.events) - 1 - idx
	if srcIdx < 0 || srcIdx >= len(v.events) {
		return
	}
	e := v.events[srcIdx]

	ts := e.At.Local().Format("15:04:05")
	verdict := "ALLOW"
	verdictStyle := theme.BaseStyle().Bold(true)
	if !e.Allowed {
		verdict = "DENY"
		// Use the subtle style for denied calls so they read as
		// non-default state without dragging in an arbitrary
		// red -- the chrome contract leaves color allocation to
		// the theme.
		verdictStyle = theme.SubtleStyle().Bold(true)
	}
	kind := string(e.Kind)
	tool := e.Tool
	if tool == "" {
		tool = kind
	}
	target := tool
	if e.Action != "" {
		target = fmt.Sprintf("%s.%s", tool, e.Action)
	}
	if e.Kind == consent.EventGranted {
		target = "GRANT"
	} else if e.Kind == consent.EventRevoked {
		target = "REVOKE"
	}
	classText := string(e.Class)
	if classText == "" {
		classText = "-"
	}

	primary := fmt.Sprintf("%s  %s", ts, target)
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-1, primary, theme.BaseStyle())

	classX := bounds.X + bounds.Width - len(verdict) - 1 - len(classText) - 2
	if classX > bounds.X+len(primary)+2 {
		screen.DrawText(classX, bounds.Y, len(classText), classText, theme.SubtleStyle())
	}
	verdictX := bounds.X + bounds.Width - len(verdict) - 1
	if verdictX > bounds.X+len(primary)+2 {
		screen.DrawText(verdictX, bounds.Y, len(verdict), verdict, verdictStyle)
	}

	// Subtitle row: reason. Dimmed and truncated to width.
	if bounds.Height >= 2 && e.Reason != "" {
		screen.DrawText(bounds.X+3, bounds.Y+1, bounds.Width-4, e.Reason, theme.SubtleStyle())
	}
	_ = selected
}

// drawChrome renders the bottom hint bar per the panel chrome contract.
// Chips are context-aware: G:Grant is omitted when a window is already
// open, R:Revoke is omitted when none is.
func (v *View) drawChrome(screen *ui.Screen, bounds ui.Rect) {
	chips := []ui.HintChip{
		{Key: "G", Label: "Grant", Disabled: v.status.Granted},
		{Key: "R", Label: "Revoke", Disabled: !v.status.Granted},
		{Key: "Ctrl+E", Label: "KillSwitch", Disabled: !v.status.Granted},
		{Key: "↑/↓", Label: "Scroll", Disabled: len(v.events) == 0},
	}
	if v.grantModal != nil {
		chips = []ui.HintChip{
			{Key: "↑/↓", Label: "Pick"},
			{Key: "S", Label: v.strictChipLabel()},
			{Key: "Enter", Label: "Grant"},
			{Key: "Esc", Label: "Cancel"},
		}
	}
	bar := ui.HintBar{Chips: chips}
	bar.Draw(screen, bounds, v.Theme)
}

func (v *View) strictChipLabel() string {
	if v.grantModal != nil && v.grantModal.strict {
		return "Strict:On"
	}
	return "Strict:Off"
}

// drawGrantModal overlays a small bordered panel near the bottom of
// the pane listing the duration presets. The selected preset is
// highlighted; pressing Enter calls Client.Grant with that duration.
func (v *View) drawGrantModal(screen *ui.Screen, bounds ui.Rect) {
	if v.grantModal == nil {
		return
	}
	// Geometry: 4 + len(presets) rows tall, ~40 cols wide, centered.
	modalH := len(grantPresets) + 4
	modalW := 44
	if modalW > bounds.Width-4 {
		modalW = bounds.Width - 4
	}
	if modalH > bounds.Height-4 {
		modalH = bounds.Height - 4
	}
	x := bounds.X + (bounds.Width-modalW)/2
	y := bounds.Y + bounds.Height - modalH - 2
	if y < bounds.Y+1 {
		y = bounds.Y + 1
	}

	screen.FillRect(x, y, modalW, modalH, v.Theme.BaseStyle())
	screen.DrawBox(x, y, modalW, modalH, v.Theme.AccentStyle())
	screen.DrawText(x+2, y, 18, " GRANT CONSENT ", v.Theme.AccentStyle().Bold(true))

	for i, p := range grantPresets {
		rowY := y + 1 + i
		style := v.Theme.BaseStyle()
		marker := "  "
		if i == v.grantModal.selected {
			style = v.Theme.SelectionStyle()
			marker = "> "
		}
		screen.FillRect(x+1, rowY, modalW-2, 1, style)
		screen.DrawText(x+2, rowY, modalW-4, marker+p.Label, style)
	}

	strictLine := "Strict mode: off  (S to toggle)"
	if v.grantModal.strict {
		strictLine = "Strict mode: ON   (S to toggle)"
	}
	screen.DrawText(x+2, y+modalH-2, modalW-4, strictLine, v.Theme.SubtleStyle())
}

// HandleEvent routes a tcell event into the view. Returns true when
// the event was consumed.
//
// When the Grant modal is open, the modal consumes all key events
// (Esc closes it, Enter submits, ↑/↓ pick a preset, S toggles strict).
// Otherwise the view consumes G / R for the action keys, and forwards
// ↑/↓/PgUp/PgDn/etc. to the audit ListPane for scrolling.
func (v *View) HandleEvent(ev tcell.Event) bool {
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	v.Mu.Lock()
	modalOpen := v.grantModal != nil
	v.Mu.Unlock()

	if modalOpen {
		return v.handleModalKey(key)
	}
	return v.handleListKey(key)
}

func (v *View) handleListKey(key *tcell.EventKey) bool {
	if key.Key() == tcell.KeyRune {
		switch key.Rune() {
		case 'g', 'G':
			v.openGrantModal()
			return true
		case 'r', 'R':
			go v.doRevoke()
			return true
		}
	}
	// Scroll keys -> audit list.
	v.Mu.Lock()
	defer v.Mu.Unlock()
	return v.auditList.HandleEvent(key)
}

func (v *View) handleModalKey(key *tcell.EventKey) bool {
	v.Mu.Lock()
	m := v.grantModal
	if m == nil {
		v.Mu.Unlock()
		return false
	}
	switch key.Key() {
	case tcell.KeyEscape:
		v.grantModal = nil
		v.Mu.Unlock()
		v.Redraw()
		return true
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.Mu.Unlock()
		v.Redraw()
		return true
	case tcell.KeyDown:
		if m.selected < len(grantPresets)-1 {
			m.selected++
		}
		v.Mu.Unlock()
		v.Redraw()
		return true
	case tcell.KeyEnter:
		picked := grantPresets[m.selected]
		strict := m.strict
		v.grantModal = nil
		v.Mu.Unlock()
		v.Redraw()
		go v.doGrant(picked.Duration, strict)
		return true
	case tcell.KeyRune:
		if r := key.Rune(); r == 's' || r == 'S' {
			m.strict = !m.strict
			v.Mu.Unlock()
			v.Redraw()
			return true
		}
	}
	v.Mu.Unlock()
	return false
}

func (v *View) openGrantModal() {
	v.Mu.Lock()
	v.grantModal = &grantModalState{}
	v.Mu.Unlock()
	v.Redraw()
}

// doGrant is invoked from a goroutine so the event loop never
// blocks on a socket round-trip. Status updates flow back through
// the Watch stream's `granted` event; we still apply the immediate
// Response to the status block so the UI doesn't have to wait a
// roundtrip for the audit event to land.
func (v *View) doGrant(window time.Duration, strict bool) {
	v.Mu.RLock()
	c := v.client
	v.Mu.RUnlock()
	if c == nil {
		return
	}
	resp, err := c.Grant(window, strict)
	if err != nil {
		v.Notify(fmt.Sprintf("Grant failed: %v", err))
		v.markDisconnected(err)
		return
	}
	if !resp.OK {
		v.Notify(fmt.Sprintf("Grant rejected: %s", resp.Error))
		return
	}
	v.applyStatus(resp.Status, true, "")
}

// doRevoke is the worker pane's local Revoke trigger. The global
// kill switch (Ctrl+E) calls into Revoke separately from any tab;
// both paths converge on the same socket op.
func (v *View) doRevoke() {
	v.Mu.RLock()
	c := v.client
	v.Mu.RUnlock()
	if c == nil {
		return
	}
	resp, err := c.Revoke()
	if err != nil {
		v.Notify(fmt.Sprintf("Revoke failed: %v", err))
		v.markDisconnected(err)
		return
	}
	if !resp.OK {
		v.Notify(fmt.Sprintf("Revoke rejected: %s", resp.Error))
		return
	}
	v.applyStatus(resp.Status, true, "")
}

// Revoke is the entry point the global kill switch in app.go calls
// from any tab. Synchronous wrt the response so the notification
// shows immediately. Safe to call concurrently with the in-pane
// Revoke handler.
func (v *View) Revoke() error {
	v.Mu.RLock()
	c := v.client
	v.Mu.RUnlock()
	if c == nil {
		return fmt.Errorf("workers: no consent client configured")
	}
	resp, err := c.Revoke()
	if err != nil {
		v.markDisconnected(err)
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	v.applyStatus(resp.Status, true, "")
	return nil
}

// IsGranted reports whether a window is currently open per the most
// recent status mirrored from the worker. Used by the global
// kill-switch path so the notification can distinguish "revoked"
// from "no-op because nothing was granted in the first place".
func (v *View) IsGranted() bool {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	return v.connected && v.status.Granted
}
