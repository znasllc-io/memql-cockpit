//go:build darwin && gui

package worker

import (
	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/internal/worker/tools"
)

// init wires the Screen Recording preflight check into the tools
// package. The hook fires before every screenshot dispatch so a
// permission that was approved at setup but later revoked from
// System Settings -> Privacy -> Screen Recording surfaces as a
// clean `permission_denied` failure rather than a confusing
// silent-or-empty screenshot.
//
// macOS-only, GUI-only. On every other platform / build
// configuration the hook stays nil and the dispatcher skips the
// check.
func init() {
	tools.ScreenCapturePreflightHook = preflightScreenCaptureAccess
}
