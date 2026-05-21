//go:build !gui

package tools

import (
	"context"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// dispatchComputer on the default (non-gui) build returns
// Unimplemented for every workerComputer action. The cockpit-gui
// build (//go:build gui) provides a real implementation backed by
// RobotGo.
func (d *Dispatcher) dispatchComputer(_ context.Context, action string, _ map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	_ = action
	return nil, &memqlv1.Failure{
		ErrorCode:    "gui_unavailable",
		ErrorMessage: "this cockpit binary was built without the GUI tag; install memql-cockpit-gui to enable workerComputer.* actions",
	}
}

// cursorLocation on the default (non-gui) build always reports an
// unknown cursor. workerComputer.mouse_click can't reach a headless
// worker anyway (dispatchComputer above rejects it), so the consent
// gate never gets a real position here -- the strict-mode region
// exemption (memql-cockpit#131) simply never fires on headless
// builds. The cockpit-gui build supplies the RobotGo-backed reader.
func cursorLocation() (x, y int, known bool) {
	return 0, 0, false
}
