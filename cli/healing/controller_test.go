package healing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Epic 4 / memql#2144 (E4.6): the Cockpit healed-pack controller. Driven
// against a fake client so the validate-proposed-patch flow is exercisable
// with no live gRPC stream.

type fakeClient struct {
	overrides       *client.Result
	overridesErr    error
	validateErr     error
	rejectErr       error
	lastValidateArg client.ValidateOverrideArgs
	lastRejectArg   client.RejectOverrideArgs
	validateCalls   int
	rejectCalls     int
}

func (f *fakeClient) OverridesForConstruct(_ context.Context, _ client.OverridesForConstructArgs) (*client.Result, error) {
	return f.overrides, f.overridesErr
}
func (f *fakeClient) ValidateOverride(_ context.Context, args client.ValidateOverrideArgs) (*client.Result, error) {
	f.validateCalls++
	f.lastValidateArg = args
	return nil, f.validateErr
}
func (f *fakeClient) RejectOverride(_ context.Context, args client.RejectOverrideArgs) (*client.Result, error) {
	f.rejectCalls++
	f.lastRejectArg = args
	return nil, f.rejectErr
}

// resultWith builds a *client.Result wrapping the given override rows.
func resultWith(rows ...map[string]any) *client.Result {
	out := make([]client.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, client.Row(r))
	}
	return client.ResultFromRows(out)
}

func TestController_ListOverrides_NewestFirst(t *testing.T) {
	fc := &fakeClient{overrides: resultWith(
		map[string]any{"id": "ov-1", "baseConstructId": "deployStaging", "version": float64(1), "validationStatus": "rejected"},
		map[string]any{"id": "ov-2", "baseConstructId": "deployStaging", "version": float64(2), "validationStatus": "proposed", "blastRadius": "shared"},
	)}
	ctrl := NewController(func() Client { return fc })

	overrides, err := ctrl.ListOverrides(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("ListOverrides: %v", err)
	}
	if len(overrides) != 2 {
		t.Fatalf("want 2 overrides, got %d", len(overrides))
	}
	if overrides[0].Version != 2 {
		t.Errorf("overrides not newest-first: got version %d first", overrides[0].Version)
	}
	if !overrides[0].IsProposed() || overrides[0].BlastRadius != "shared" {
		t.Errorf("proposed override projected wrong: %+v", overrides[0])
	}
}

func TestController_Validate_CallsSDK(t *testing.T) {
	fc := &fakeClient{}
	ctrl := NewController(func() Client { return fc })
	if err := ctrl.Validate(context.Background(), "ov-2", 3); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fc.validateCalls != 1 {
		t.Fatalf("expected 1 validate call, got %d", fc.validateCalls)
	}
	if fc.lastValidateArg.OverrideId != "ov-2" || fc.lastValidateArg.Version != 3 {
		t.Errorf("validate args wrong: %+v", fc.lastValidateArg)
	}
}

// The server-side blast-radius role gate rejection is surfaced verbatim.
func TestController_Validate_SurfacesRoleGateError(t *testing.T) {
	fc := &fakeClient{validateErr: errors.New("blast-radius heal requires role rank >= 200")}
	ctrl := NewController(func() Client { return fc })
	err := ctrl.Validate(context.Background(), "ov-2", 3)
	if err == nil || !strings.Contains(err.Error(), "blast-radius") {
		t.Fatalf("expected the role-gate error surfaced, got %v", err)
	}
}

func TestController_Reject_CallsSDK(t *testing.T) {
	fc := &fakeClient{}
	ctrl := NewController(func() Client { return fc })
	if err := ctrl.Reject(context.Background(), "ov-2", "not portable enough"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if fc.rejectCalls != 1 || fc.lastRejectArg.RejectionReason != "not portable enough" {
		t.Errorf("reject args wrong: calls=%d arg=%+v", fc.rejectCalls, fc.lastRejectArg)
	}
}

func TestController_NoCluster_GatedNotPanic(t *testing.T) {
	ctrl := NewController(func() Client { return nil })
	if _, err := ctrl.ListOverrides(context.Background(), "x"); err == nil {
		t.Errorf("expected a no-cluster error, got nil")
	}
	if err := ctrl.Validate(context.Background(), "ov", 1); err == nil {
		t.Errorf("expected a no-cluster error from Validate")
	}
	// nil provider entirely
	ctrl2 := NewController(nil)
	if err := ctrl2.Reject(context.Background(), "ov", "r"); err == nil {
		t.Errorf("expected a no-cluster error from a nil provider")
	}
}

func TestController_RequiresOverrideId(t *testing.T) {
	ctrl := NewController(func() Client { return &fakeClient{} })
	if err := ctrl.Validate(context.Background(), "  ", 1); err == nil {
		t.Errorf("expected validate to require an override id")
	}
	if err := ctrl.Reject(context.Background(), "", "r"); err == nil {
		t.Errorf("expected reject to require an override id")
	}
}
