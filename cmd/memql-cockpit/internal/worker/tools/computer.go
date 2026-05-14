//go:build !gui

package tools

import (
	"context"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
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
