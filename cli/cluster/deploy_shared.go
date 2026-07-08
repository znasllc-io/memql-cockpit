package cluster

// Shared deploy helpers used by the concept-driven Deployments section
// (deployments.go / deployments_controls.go). These were extracted from
// the now-removed deployment-v2 "Surface A" files when that surface was
// deleted in the persistent-split overhaul (memql-cockpit#221). The
// Deployments section reuses the bounded action timeout (suggest read),
// the colored-token writer, the wrapped-text writer, and the warning-color
// style -- so they live here, package-scoped. (The gRPC action-outcome
// formatting + PermissionDenied mapping went away with #292, when the
// cut/deploy/rollback controls moved off DeployControlService onto the
// deployEngineCluster automation runner.)

import (
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// deployActionTimeout bounds each SDK write action (cut / deploy /
// rollback). Generous relative to a read because the action kicks a
// pipeline server-side; 30s is enough for the synchronous ack without
// hanging the modal forever on a wedged backend.
const deployActionTimeout = 30 * time.Second

// warningStyle is the deploy surface's amber/warning color helper,
// wrapping the raw Theme.Warning palette color over the base background
// the same way the node-health renderer does.
func (v *View) warningStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)
}

// requestRedrawLocked / requestRedraw ask the app to repaint. The
// *Locked variant is safe to call while holding v.mu (it only reads the
// callback pointer, which is set once at wire time); requestRedraw takes
// the read lock for use from an async goroutine.
func (v *View) requestRedrawLocked() {
	if v.OnRedraw != nil {
		v.OnRedraw()
	}
}

func (v *View) requestRedraw() {
	v.mu.RLock()
	cb := v.OnRedraw
	v.mu.RUnlock()
	if cb != nil {
		cb()
	}
}

// drawColoredToken writes text at (cx, y), clipping to limit, and
// returns the x just past what it wrote.
func drawColoredToken(screen *ui.Screen, cx, y, limit int, text string, style tcell.Style) int {
	for _, ch := range text {
		if cx >= limit {
			break
		}
		screen.SetCell(cx, y, ch, style)
		cx++
	}
	return cx
}

// drawWrapped writes text wrapped to w over up to h rows.
func (v *View) drawWrapped(screen *ui.Screen, x, y, w, h int, text string, style tcell.Style) {
	r := []rune(text)
	row := 0
	for len(r) > 0 && row < h {
		n := w
		if n > len(r) {
			n = len(r)
		}
		screen.DrawText(x, y+row, w, string(r[:n]), style)
		r = r[n:]
		row++
	}
}
