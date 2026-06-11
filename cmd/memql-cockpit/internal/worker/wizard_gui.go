//go:build gui

package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/canvas"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// runTUIWizard renders the worker setup wizard in a single-panel
// TUI built on the same cli/ui + cli/canvas primitives the
// operations console uses. See cli/CLAUDE.md for the canonical-TUI
// rule.
//
// Layout (mirrors cli.App.draw):
//
//	row 0:    header chrome (memQL Cockpit | "Worker Setup" hint)
//	row 1:    dark spacer band
//	rows 2..h-3: content area -- one bordered panel centered, with
//	              a pixel vignette at the top, step indicator, and
//	              live probe results.
//	row h-2:  dark spacer band
//	row h-1:  hint footer (mirrors cli/ui/tabs.go style)
func runTUIWizard() error {
	screen, err := ui.NewScreen()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	defer screen.Fini()

	wiz := newWizard(screen)
	if err := wiz.run(); err != nil {
		return err
	}
	return nil
}

// stepId tags the wizard's three sequential steps.
type stepId int

const (
	stepAccessibility stepId = iota
	stepScreenRecording
	stepValidate
	stepDone
)

// stepStatus is per-step probe state.
type stepStatus struct {
	probed  bool
	granted bool
	detail  string
	tcc     string
}

// wizard holds the wizard's mutable state. All UI work happens on
// the main event-loop goroutine; probe goroutines deliver results
// by stuffing them into result channels and posting an EventInterrupt
// so the main loop redraws.
type wizard struct {
	screen *ui.Screen
	theme  ui.Theme
	header *ui.Header

	binPath string

	mu        sync.Mutex
	step      stepId
	status    map[stepId]stepStatus
	probing   atomic.Bool
	statusMsg string

	quit bool
}

func newWizard(screen *ui.Screen) *wizard {
	theme := ui.DefaultTheme()
	header := ui.NewHeader(theme, nil)
	header.NavHint = "Worker Setup"

	binPath, _ := os.Executable()
	if binPath != "" {
		if abs, err := filepath.Abs(binPath); err == nil {
			binPath = abs
		}
	}

	return &wizard{
		screen:  screen,
		theme:   theme,
		header:  header,
		binPath: binPath,
		step:    stepAccessibility,
		status:  map[stepId]stepStatus{},
	}
}

func (w *wizard) run() error {
	w.draw()
	w.screen.EnableInteraction()

	// Force a second draw -- tcell's first Sync() after Init() can
	// produce incomplete output. Same workaround as cli/app.go.
	go func() {
		time.Sleep(80 * time.Millisecond)
		w.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}()

	// Kick off the first probe immediately so the user sees the
	// wizard already working when it appears.
	w.kickoffProbe(stepAccessibility)

	for !w.quit {
		ev := w.screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			w.draw()
		case *tcell.EventResize:
			w.screen.Sync()
			w.draw()
		case *tcell.EventKey:
			if w.handleKey(ev) {
				w.draw()
			}
		}
	}
	return nil
}

// handleKey routes input. Returns true if the screen should redraw.
func (w *wizard) handleKey(ev *tcell.EventKey) bool {
	if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
		w.quit = true
		return false
	}
	if ev.Key() == tcell.KeyEscape {
		// Esc on a Done state exits; otherwise it's a no-op so the
		// user doesn't accidentally quit during a probe.
		if w.step == stepDone {
			w.quit = true
		}
		return false
	}
	if ev.Key() == tcell.KeyEnter {
		return w.advance()
	}
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case 'r', 'R':
			if w.step != stepDone && !w.probing.Load() {
				w.kickoffProbe(w.step)
				return true
			}
		case 'o', 'O':
			w.openSystemSettings()
			return true
		}
	}
	return false
}

// advance moves to the next step (or exits when on stepDone). Re-
// triggers a probe when entering a new probing step. Pressing Enter
// on a denied step still advances -- the user might want to skip
// past Accessibility (for a HEADLESS-only cockpit) and the wizard
// won't block them.
func (w *wizard) advance() bool {
	if w.probing.Load() {
		return false
	}
	switch w.step {
	case stepAccessibility:
		w.step = stepScreenRecording
		w.kickoffProbe(stepScreenRecording)
		return true
	case stepScreenRecording:
		w.step = stepValidate
		w.kickoffProbe(stepValidate)
		return true
	case stepValidate:
		w.step = stepDone
		return true
	case stepDone:
		w.quit = true
	}
	return false
}

// kickoffProbe runs the probe for the supplied step in a goroutine
// and posts an EventInterrupt when done so the main loop redraws.
func (w *wizard) kickoffProbe(step stepId) {
	w.mu.Lock()
	w.statusMsg = "Probing..."
	w.mu.Unlock()
	if w.probing.Load() {
		return
	}
	w.probing.Store(true)

	go func() {
		defer w.probing.Store(false)
		defer w.screen.PostEvent(tcell.NewEventInterrupt(nil))

		var st stepStatus
		st.probed = true

		switch step {
		case stepAccessibility:
			ok, detail := probeAccessibility()
			st.granted = ok
			st.detail = detail
			st.tcc = tccCheck("Accessibility", w.binPath)
		case stepScreenRecording:
			ok, detail := probeScreenRecording()
			st.granted = ok
			st.detail = detail
			st.tcc = tccCheck("ScreenCapture", w.binPath)
		case stepValidate:
			// "Validate" reads the previously-probed Accessibility +
			// Screen Recording status. We don't re-probe here; the
			// summary is the validation.
			a := w.statusOf(stepAccessibility)
			s := w.statusOf(stepScreenRecording)
			st.granted = a.granted && s.granted
			if st.granted {
				st.detail = "Both permissions granted in this process tree."
			} else {
				st.detail = "One or more permissions are denied. The worker can still register HEADLESS-only; GUI tools will return gui_unavailable."
			}
		}

		w.mu.Lock()
		w.status[step] = st
		w.statusMsg = ""
		w.mu.Unlock()
	}()
}

func (w *wizard) statusOf(step stepId) stepStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status[step]
}

func (w *wizard) currentStatusMsg() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statusMsg
}

// openSystemSettings fires up the right macOS Privacy & Security
// pane for the active step. macOS 13+ honors the legacy
// x-apple.systempreferences anchors via `open`; earlier OSes
// silently land on the top-level Privacy pane, which is fine.
func (w *wizard) openSystemSettings() {
	if runtime.GOOS != "darwin" {
		return
	}
	var url string
	switch w.step {
	case stepAccessibility:
		url = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
	case stepScreenRecording:
		url = "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
	default:
		url = "x-apple.systempreferences:com.apple.preference.security?Privacy"
	}
	// Fire-and-forget; `open` exits immediately after handing off
	// the URL to System Settings, so no timeout context is needed.
	_ = exec.Command("open", url).Start()
}

// draw renders the entire wizard frame.
func (w *wizard) draw() {
	scr := w.screen
	width, height := scr.Size()

	// Background.
	scr.Clear(w.theme.BaseStyle())

	// Top + bottom chrome bands (dark, matches the operations console).
	barBG := tcell.NewRGBColor(18, 18, 22)
	chromeStyle := tcell.StyleDefault.Background(barBG)

	// Header row (0) + spacer (1).
	w.header.Draw(scr, 0, width)
	scr.FillRect(0, 1, width, 1, chromeStyle)

	// Bottom spacer + hint footer.
	scr.FillRect(0, height-2, width, 1, chromeStyle)
	w.drawHintFooter(height-1, width)

	// Content area: rows 2..height-3.
	contentY := 2
	contentH := height - 4
	if contentH < 1 {
		return
	}

	w.drawPanel(ui.Rect{X: 0, Y: contentY, Width: width, Height: contentH})

	// Hide stray cursor (matches cli/app.go convention).
	scr.Inner().ShowCursor(0, 0)
	scr.Inner().HideCursor()
	scr.Sync()
}

// drawPanel paints the centered single panel + every piece of
// content inside it.
func (w *wizard) drawPanel(bounds ui.Rect) {
	// Panel size: capped so the wizard doesn't sprawl on huge
	// terminals. Roughly the proportions of the help overlay.
	panelW := 78
	if panelW > bounds.Width-4 {
		panelW = bounds.Width - 4
	}
	if panelW < 40 {
		panelW = bounds.Width
	}
	panelH := bounds.Height
	panelX := bounds.X + (bounds.Width-panelW)/2
	panelY := bounds.Y

	panelBG := tcell.NewRGBColor(30, 33, 39)
	panelBorder := tcell.StyleDefault.Foreground(w.theme.Accent).Background(panelBG).Bold(true)
	panelFill := tcell.StyleDefault.Foreground(w.theme.FG).Background(panelBG)

	w.screen.FillRect(panelX, panelY, panelW, panelH, panelFill)
	w.screen.DrawBox(panelX, panelY, panelW, panelH, panelBorder)

	// Panel title in the top border, centered.
	title := " Computer-Use Permissions Setup "
	titleX := panelX + (panelW-len(title))/2
	w.screen.DrawText(titleX, panelY, len(title), title, panelBorder)

	innerX := panelX + 2
	innerY := panelY + 2
	innerW := panelW - 4

	cursor := innerY

	// Pixel vignette at the top.
	cursor = w.drawVignette(innerX, cursor, innerW, panelBG)
	cursor++

	// Step indicator.
	cursor = w.drawSteps(innerX, cursor, innerW, panelBG)
	cursor++

	// Active-step detail block.
	w.drawStepDetail(innerX, cursor, innerW, panelY+panelH-2, panelBG)
}

// drawVignette renders a small pixel illustration -- a stylized
// shield with a display + cursor inside it -- via cli/canvas.
// Returns the screen row immediately after the vignette so the
// caller can continue layout.
func (w *wizard) drawVignette(x, y, width int, bg tcell.Color) int {
	// Canvas dims: width in cells * 1 pixel per column; height in
	// rows * 2 pixels per row (half-block rendering). We aim for a
	// 60x16 pixel vignette = 60 cells x 8 rows.
	cellsW := 60
	if cellsW > width {
		cellsW = width
	}
	cellsH := 8
	pxW := cellsW
	pxH := cellsH * 2

	bgColor := canvas.Color{R: 30, G: 33, B: 39}
	accent := canvas.Color{R: 189, G: 147, B: 249}
	subtle := canvas.Color{R: 110, G: 110, B: 130}
	success := canvas.Color{R: 152, G: 195, B: 121}

	cv := canvas.New(pxW, pxH, bgColor)

	// Shield outline -- centered, 40px wide x 14px tall.
	sx := (pxW - 40) / 2
	sy := 1
	// Top edge.
	cv.DrawLine(sx+5, sy, sx+34, sy, accent)
	// Top corners (tiny diagonal).
	cv.DrawLine(sx, sy+1, sx+5, sy, accent)
	cv.DrawLine(sx+34, sy, sx+39, sy+1, accent)
	// Sides.
	cv.DrawLine(sx, sy+1, sx, sy+8, accent)
	cv.DrawLine(sx+39, sy+1, sx+39, sy+8, accent)
	// Bottom diagonals to a point.
	cv.DrawLine(sx, sy+8, sx+19, sy+13, accent)
	cv.DrawLine(sx+39, sy+8, sx+20, sy+13, accent)

	// Inner display rectangle.
	cv.DrawRectOutline(sx+9, sy+3, 22, 7, subtle)

	// Cursor arrow inside the display, drawn in success green to
	// hint at "permission granted" energy.
	cursorOriginX := sx + 14
	cursorOriginY := sy + 5
	cv.DrawLine(cursorOriginX, cursorOriginY, cursorOriginX, cursorOriginY+3, success)
	cv.DrawLine(cursorOriginX, cursorOriginY, cursorOriginX+3, cursorOriginY+3, success)
	cv.DrawLine(cursorOriginX, cursorOriginY, cursorOriginX+2, cursorOriginY+1, success)

	// Render the canvas into the panel. Zoom=1.0 means one world
	// pixel per screen pixel column / per half-row -- our 60x16
	// canvas lands as 60 cells wide x 8 rows tall.
	cam := canvas.NewCamera()
	cam.Zoom = 1.0
	r := canvas.NewRenderer(cam)
	bounds := ui.Rect{X: x + (width-cellsW)/2, Y: y, Width: cellsW, Height: cellsH}
	r.DrawCanvas(w.screen, bounds, cv)

	_ = bg
	return y + cellsH
}

// drawSteps draws the three-step indicator under the vignette.
func (w *wizard) drawSteps(x, y, width int, panelBG tcell.Color) int {
	stepDefs := []struct {
		id    stepId
		label string
	}{
		{stepAccessibility, "Accessibility"},
		{stepScreenRecording, "Screen Recording"},
		{stepValidate, "Validate"},
	}

	rowStyle := tcell.StyleDefault.Foreground(w.theme.FG).Background(panelBG)
	subtle := tcell.StyleDefault.Foreground(w.theme.Subtle).Background(panelBG)
	accent := tcell.StyleDefault.Foreground(w.theme.Accent).Background(panelBG).Bold(true)
	success := tcell.StyleDefault.Foreground(w.theme.Success).Background(panelBG)
	errorStyle := tcell.StyleDefault.Foreground(w.theme.Error).Background(panelBG)

	for i, step := range stepDefs {
		st := w.statusOf(step.id)
		var glyph rune
		var glyphStyle tcell.Style
		var labelStyle tcell.Style
		var note string
		var noteStyle tcell.Style

		switch {
		case w.step == step.id && w.probing.Load():
			glyph = '▶'
			glyphStyle = accent
			labelStyle = accent
			note = "(probing...)"
			noteStyle = subtle
		case w.step == step.id:
			glyph = '▶'
			glyphStyle = accent
			labelStyle = accent
			if st.probed {
				if st.granted {
					note = "granted"
					noteStyle = success
				} else {
					note = "denied"
					noteStyle = errorStyle
				}
			}
		case w.stepIsComplete(step.id):
			if st.granted {
				glyph = '●'
				glyphStyle = success
				note = "granted"
				noteStyle = success
			} else {
				glyph = '×'
				glyphStyle = errorStyle
				note = "denied"
				noteStyle = errorStyle
			}
			labelStyle = rowStyle
		default:
			glyph = '○'
			glyphStyle = subtle
			labelStyle = subtle
		}

		row := y + i
		w.screen.SetCell(x+1, row, glyph, glyphStyle)
		w.screen.DrawText(x+3, row, width-3, fmt.Sprintf("Step %d: %s", i+1, step.label), labelStyle)
		if note != "" {
			noteX := x + 3 + len(fmt.Sprintf("Step %d: %s", i+1, step.label)) + 2
			if noteX < x+width {
				w.screen.DrawText(noteX, row, x+width-noteX, "("+note+")", noteStyle)
			}
		}
	}
	return y + len(stepDefs)
}

// stepIsComplete reports whether the supplied step has been probed
// and the wizard has moved past it.
func (w *wizard) stepIsComplete(s stepId) bool {
	if w.step == s {
		return false
	}
	if w.step == stepDone {
		return true
	}
	return int(s) < int(w.step)
}

// drawStepDetail renders the active-step detail block (status,
// per-binary tcc grant, hint text).
func (w *wizard) drawStepDetail(x, y, width, maxY int, panelBG tcell.Color) {
	body := tcell.StyleDefault.Foreground(w.theme.FG).Background(panelBG)
	subtle := tcell.StyleDefault.Foreground(w.theme.Subtle).Background(panelBG)
	success := tcell.StyleDefault.Foreground(w.theme.Success).Background(panelBG)
	errorStyle := tcell.StyleDefault.Foreground(w.theme.Error).Background(panelBG)
	warning := tcell.StyleDefault.Foreground(w.theme.Warning).Background(panelBG).Bold(true)

	innerW := width
	cursor := y

	// Header line for the active step.
	switch w.step {
	case stepAccessibility:
		w.screen.DrawText(x, cursor, innerW, "Accessibility -- required for mouse + keyboard", warning)
	case stepScreenRecording:
		w.screen.DrawText(x, cursor, innerW, "Screen Recording -- required for screenshots", warning)
	case stepValidate:
		w.screen.DrawText(x, cursor, innerW, "Validation summary", warning)
	case stepDone:
		w.screen.DrawText(x, cursor, innerW, "Setup complete", warning)
	}
	cursor += 2
	if cursor >= maxY {
		return
	}

	st := w.statusOf(w.step)
	probing := w.probing.Load() && w.step != stepDone
	currentMsg := w.currentStatusMsg()

	// Live status / detail text.
	switch {
	case probing && currentMsg != "":
		w.screen.DrawText(x, cursor, innerW, "  "+currentMsg, subtle)
		cursor++
	case st.probed:
		statusGlyph := "  ✓ "
		statusStyle := success
		if !st.granted {
			statusGlyph = "  ✗ "
			statusStyle = errorStyle
		}
		statusLine := statusGlyph + st.detail
		for _, line := range ui.WrapText(statusLine, innerW) {
			if cursor >= maxY {
				break
			}
			w.screen.DrawText(x, cursor, innerW, line, statusStyle)
			cursor++
		}
		if st.tcc != "" && cursor < maxY {
			w.screen.DrawText(x, cursor, innerW, "    Per-binary TCC: "+st.tcc, subtle)
			cursor++
		}
	}

	cursor++
	if cursor >= maxY {
		return
	}

	// Body messaging per step.
	for _, line := range w.bodyText() {
		if cursor >= maxY {
			break
		}
		for _, wrapped := range ui.WrapText(line, innerW) {
			if cursor >= maxY {
				break
			}
			w.screen.DrawText(x, cursor, innerW, wrapped, body)
			cursor++
		}
	}

	// Binary path footer at the bottom of the panel content area.
	if w.binPath != "" && cursor < maxY-1 {
		footerY := maxY - 1
		w.screen.DrawText(x, footerY, innerW, "binary: "+w.binPath, subtle)
	}
}

// bodyText returns the per-step explanatory copy, one element per
// paragraph. WrapText handles wrapping inside the panel.
func (w *wizard) bodyText() []string {
	switch w.step {
	case stepAccessibility:
		if runtime.GOOS != "darwin" {
			return []string{"On Linux the wizard checks the display server first -- RobotGo requires X11, so a Wayland or display-less session FAILS here and input actions return display_server_unsupported at dispatch time. On X11 it then probes XTEST mouse movement."}
		}
		return []string{
			"macOS grants Accessibility per BINARY. Running this wizard from Terminal probes whether the underlying CGEvent post succeeds, which can be true because Terminal already has the grant and the cockpit-gui inherits.",
			"When the LaunchAgent runs cockpit-gui detached at login, it needs its OWN entry under System Settings -> Privacy & Security -> Accessibility. Approve it there to make the agent work without a logged-in shell.",
			"Press R to re-probe, O to open System Settings, Enter to continue.",
		}
	case stepScreenRecording:
		if runtime.GOOS != "darwin" {
			return []string{"On Linux the screen-grab requires an X11 session (Wayland FAILS with display_server_unsupported); on X11 the grab uses the X11 SHM extension."}
		}
		return []string{
			"Screen Recording is a separate macOS TCC entry from Accessibility. Same inheritance rules apply -- a probe that succeeds via Terminal does NOT mean cockpit-gui itself is approved.",
			"Press R to re-probe, O to open System Settings, Enter to continue.",
		}
	case stepValidate:
		return []string{
			"The wizard's job is done. Press Enter to view the summary screen.",
		}
	case stepDone:
		st := w.statusOf(stepAccessibility)
		sr := w.statusOf(stepScreenRecording)
		var lines []string
		if st.granted && sr.granted {
			lines = append(lines,
				"Both Accessibility and Screen Recording are granted in this process tree.",
				"Configure ~/.memql/worker.yaml with cluster_url + token, then run: memql-cockpit-gui worker run",
			)
		} else {
			lines = append(lines, "Some permissions are denied; the worker can still register HEADLESS-only.")
			if !st.granted {
				lines = append(lines, "  - Accessibility denied: workerComputer.mouse_* and key_* will fail.")
			}
			if !sr.granted {
				lines = append(lines, "  - Screen Recording denied: workerComputer.screenshot will fail.")
			}
			lines = append(lines, "Press O to open System Settings and approve the missing entries; Enter to exit.")
		}
		return lines
	}
	return nil
}

// drawHintFooter renders the bottom hint bar (mirrors cli/ui/tabs.go
// styling for visual continuity with the operations console).
func (w *wizard) drawHintFooter(y, width int) {
	barBG := tcell.NewRGBColor(18, 18, 22)
	w.screen.FillRect(0, y, width, 1, chromeStyle(barBG))

	subtle := tcell.StyleDefault.Foreground(w.theme.Subtle).Background(barBG)

	hint := w.hintText()
	w.screen.DrawText(1, y, width-2, hint, subtle)

	// Right-justified universal quit hint.
	right := "Ctrl+Q:Quit"
	rightX := width - len(right) - 1
	if rightX > 1+len(hint)+2 {
		w.screen.DrawText(rightX, y, len(right), right, subtle)
	}
}

func (w *wizard) hintText() string {
	if w.probing.Load() {
		return "  probing... (Ctrl+Q to abort)"
	}
	switch w.step {
	case stepAccessibility, stepScreenRecording:
		if runtime.GOOS == "darwin" {
			return "  Enter:Continue   R:Re-probe   O:Open Settings"
		}
		return "  Enter:Continue   R:Re-probe"
	case stepValidate:
		return "  Enter:Continue"
	case stepDone:
		if runtime.GOOS == "darwin" {
			return "  Enter:Exit   O:Open Settings"
		}
		return "  Enter:Exit"
	}
	return "  Ctrl+Q:Quit"
}

func chromeStyle(bg tcell.Color) tcell.Style {
	return tcell.StyleDefault.Background(bg)
}
