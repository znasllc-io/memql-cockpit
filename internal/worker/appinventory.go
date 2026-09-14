package worker

import (
	"context"
	"strings"

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

// appDescriptorsToProto converts the inventory's harness descriptors to
// Register.app_descriptors (memql-cockpit#444): HOW this machine drives
// each app -- the protocol word, and whether that protocol can return a
// structured answer and take a follow-up.
//
// THE ENGINE READS AN ABSENT DESCRIPTOR AS "ASSUME CAPABLE", because absence
// is how a cockpit that predates the field looks, and refusing on silence
// would take structured answers away from every machine that had not
// upgraded. That makes sending one the only way this machine can say no --
// and the codex-mcp fallback, which cannot constrain an answer, needs to.
//
// They ride REGISTER ONLY, which the engine accepts once per stream, so a
// harness this machine gains mid-connection (a Codex upgraded to one with
// the app-server) is advertised on the next reconnect: the same rollout
// cost the model labels carry, and the one the proto documents.
//
// An entry with no harness word is left out rather than sent empty: the
// engine drops a word it does not know, but an empty one is a claim this
// machine never meant to make. The order is the detector's, for
// appsToProto's reason.
func appDescriptorsToProto(inventory []apps.Info) []*memqlv1.AppDescriptor {
	var out []*memqlv1.AppDescriptor
	for _, a := range inventory {
		if strings.TrimSpace(a.Harness) == "" {
			continue
		}
		out = append(out, &memqlv1.AppDescriptor{
			Id:               apps.Truncate(a.Id),
			Harness:          a.Harness,
			StructuredResult: a.StructuredResult,
			FollowUps:        a.FollowUps,
		})
	}
	return out
}
