package cluster

// Shared deploy helpers used by the concept-driven Deployments section
// (deployments.go / deployments_controls.go). These were extracted from
// the now-removed deployment-v2 "Surface A" files (deploy.go /
// deploy_controls.go / deploy_grpcstatus.go) when that surface was
// deleted in the persistent-split overhaul (memql-cockpit#221). The
// Deployments section's cut/deploy/rollback modal reuses the async-fire
// outcome formatting, the gRPC PermissionDenied mapping, the bounded
// action timeout, the colored-token writer, the wrapped-text writer, and
// the warning-color style -- so they live here, package-scoped, rather
// than in a file tied to either surface.

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
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

// formatActionOutcome renders the terminal result line. On a transport
// error it surfaces the message, mapping a PermissionDenied to the
// owner/admin hint. On success it shows the ActionResult message plus
// the AuditEventId so the operator has the audit handle. Returns the
// line and whether it was a success.
func formatActionOutcome(res client.ActionResult, err error) (string, bool) {
	if err != nil {
		if isPermissionDenied(err) {
			return "ERROR: requires owner/admin", false
		}
		return "ERROR: " + err.Error(), false
	}
	if !res.OK {
		msg := res.Message
		if msg == "" {
			msg = "action rejected"
		}
		return "ERROR: " + msg, false
	}
	msg := res.Message
	if msg == "" {
		msg = "ok"
	}
	if res.AuditEventID != "" {
		return fmt.Sprintf("SUCCESS: %s (audit %s)", msg, res.AuditEventID), true
	}
	return "SUCCESS: " + msg, true
}

// permissionDeniedCode is the gRPC code the server returns for a caller
// whose role doesn't admit a deploy-control action.
const permissionDeniedCode = codes.PermissionDenied

// isPermissionDenied reports whether err is (or wraps) a gRPC
// PermissionDenied. The SDK wraps the status error with %w so the gRPC
// status is still recoverable; we also fall back to a substring match
// for robustness against any non-status wrapper.
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := grpcStatusFromError(err); ok && st == permissionDeniedCode {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permissiondenied") ||
		strings.Contains(err.Error(), "PermissionDenied")
}

// grpcStatusFromError returns the gRPC status code embedded in err (or
// any error it wraps), and whether one was found. status.FromError
// unwraps via the GRPCStatus() / errors.As path.
func grpcStatusFromError(err error) (codes.Code, bool) {
	if err == nil {
		return codes.OK, false
	}
	if st, ok := status.FromError(err); ok {
		return st.Code(), true
	}
	return codes.Unknown, false
}
