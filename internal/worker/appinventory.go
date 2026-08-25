package worker

import (
	"context"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// AppInventory reports which local apps this machine has, for Register
// and for every Heartbeat (memql-cockpit#346).
//
// An interface rather than a concrete detector so the runner's tests can
// drive the wire shape without a claude or codex on PATH, and so a build
// that wants no app reporting at all can pass nil.
type AppInventory interface {
	Apps(ctx context.Context) []apps.Info
}

// policyInventory pairs the detector with the policy that gates it. The
// policy is read on every call rather than captured, so a SIGHUP that
// adds an app to apps.allow takes effect on the next beat -- the same
// cadence signing into the app itself gets.
type policyInventory struct {
	detector *apps.Detector
	policy   *tools.Policy
}

// NewAppInventory builds the inventory reporter the worker runs with.
func NewAppInventory(policy *tools.Policy) AppInventory {
	return &policyInventory{detector: &apps.Detector{}, policy: policy}
}

func (p *policyInventory) Apps(ctx context.Context) []apps.Info {
	if p == nil || p.detector == nil {
		return nil
	}
	return p.detector.Detect(ctx, p.policy.AppsAllow())
}

// appsToProto converts the inventory to the wire shape.
//
// Order is preserved from the detector, which sorts by id. The engine
// sorts too, but an unstable order here would rewrite the registration
// row on every beat for no actual change.
func appsToProto(inventory []apps.Info) []*memqlv1.AppInfo {
	if len(inventory) == 0 {
		return nil
	}
	out := make([]*memqlv1.AppInfo, 0, len(inventory))
	for _, a := range inventory {
		out = append(out, &memqlv1.AppInfo{
			Id:           apps.Truncate(a.Id),
			Version:      apps.Truncate(a.Version),
			SignedIn:     a.SignedIn,
			Subscription: apps.NormalizeSubscription(a.Subscription),
			Allowed:      a.Allowed,
		})
	}
	return out
}
