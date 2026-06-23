// Package healing surfaces the self-healing healed-pack flow in the Cockpit
// (Epic 4 / memql#2144, E4.6): list the healed overrides for a base construct
// and drive the validate-proposed-patch flow (accept -> a validated, versioned
// overlay override the engine's two-tier resolver prefers over base; reject ->
// a recorded rejection).
//
// This file is the UI-agnostic CONTROLLER: it talks to the active cluster's
// QueryClient via the same provider pattern the Concepts tab uses
// (cli/concepts/view.go), so it is unit-testable against a fake client with no
// live gRPC stream. The TUI view (view.go) renders the overrides this
// controller lists and dispatches the user's accept/reject through it.
//
// Multi-node aware: every read/write goes through the engine's named
// query/mutation surface (overridesForConstruct / validateOverride /
// rejectOverride), which the engine resolves against the shared store, so the
// Cockpit sees the same overrides and the same resolution outcome regardless
// of which replica serves the request. The blast-radius role gate is enforced
// server-side (validateHealingValidationRankBound); the Cockpit surfaces the
// resulting error verbatim when the operator's rank is insufficient.

package healing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Override is the Cockpit's view of a v1:healing:healedOverride row, projected
// from the healedOverrideFull shape.
type Override struct {
	ID               string
	BaseConstructId  string
	Tier             string
	Valid            bool
	Version          int
	BlastRadius      string
	ValidationStatus string
	ValidatedBy      string
	Reason           string
	RejectionReason  string
}

// IsProposed reports whether the override is awaiting human validation -- the
// rows the validate-proposed-patch flow acts on.
func (o Override) IsProposed() bool {
	return strings.EqualFold(o.ValidationStatus, "proposed")
}

// Client is the narrow slice of the memql SDK QueryClient the controller
// needs. Declared as an interface so tests inject a fake with no gRPC stream.
type Client interface {
	OverridesForConstruct(ctx context.Context, args client.OverridesForConstructArgs) (*client.Result, error)
	ValidateOverride(ctx context.Context, args client.ValidateOverrideArgs) (*client.Result, error)
	RejectOverride(ctx context.Context, args client.RejectOverrideArgs) (*client.Result, error)
}

// Controller drives the healed-pack surface. Construct with a client provider
// (the active cluster's QueryClient) so a cluster switch is picked up on the
// next call, exactly like the Concepts tab.
type Controller struct {
	client func() Client
}

// NewController wires a controller to a client provider. A nil provider (or a
// provider returning nil) makes every call a no-op error -- the Cockpit shows
// the gated message rather than panicking when no cluster is connected.
func NewController(provider func() Client) *Controller {
	return &Controller{client: provider}
}

func (c *Controller) active() (Client, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("healing: no cluster connected")
	}
	cl := c.client()
	if cl == nil {
		return nil, fmt.Errorf("healing: no cluster connected")
	}
	return cl, nil
}

// ListOverrides returns the healed overrides for a base construct, newest
// version first (the override history for the cockpit healed-pack view).
func (c *Controller) ListOverrides(ctx context.Context, baseConstructId string) ([]Override, error) {
	cl, err := c.active()
	if err != nil {
		return nil, err
	}
	res, err := cl.OverridesForConstruct(ctx, client.OverridesForConstructArgs{BaseConstructId: baseConstructId})
	if err != nil {
		return nil, fmt.Errorf("healing: list overrides for %q: %w", baseConstructId, err)
	}
	out := overridesFromResult(res)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

// Validate ACCEPTS a proposed override: it becomes a validated, versioned
// overlay the resolver prefers over base. version is the new captured version
// (E4.5 capture-as-version). The server-side blast-radius role gate may reject
// the call when the operator's rank is insufficient; that error is returned
// verbatim for the Cockpit to surface.
func (c *Controller) Validate(ctx context.Context, overrideId string, version int) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	if strings.TrimSpace(overrideId) == "" {
		return fmt.Errorf("healing: validate requires an override id")
	}
	if _, err := cl.ValidateOverride(ctx, client.ValidateOverrideArgs{OverrideId: overrideId, Version: version}); err != nil {
		return fmt.Errorf("healing: validate %q: %w", overrideId, err)
	}
	return nil
}

// Reject DECLINES a proposed override: it is recorded as rejected (never
// resolution-eligible) with the operator's reason for audit.
func (c *Controller) Reject(ctx context.Context, overrideId, reason string) error {
	cl, err := c.active()
	if err != nil {
		return err
	}
	if strings.TrimSpace(overrideId) == "" {
		return fmt.Errorf("healing: reject requires an override id")
	}
	if _, err := cl.RejectOverride(ctx, client.RejectOverrideArgs{OverrideId: overrideId, RejectionReason: reason}); err != nil {
		return fmt.Errorf("healing: reject %q: %w", overrideId, err)
	}
	return nil
}

// overridesFromResult projects the SDK result rows into Override values.
func overridesFromResult(res *client.Result) []Override {
	if res == nil {
		return nil
	}
	rows := res.Rows()
	out := make([]Override, 0, len(rows))
	for _, row := range rows {
		out = append(out, Override{
			ID:               rowString(row, "id"),
			BaseConstructId:  rowString(row, "baseConstructId"),
			Tier:             rowString(row, "tier"),
			Valid:            rowBool(row, "valid"),
			Version:          rowInt(row, "version"),
			BlastRadius:      rowString(row, "blastRadius"),
			ValidationStatus: rowString(row, "validationStatus"),
			ValidatedBy:      rowString(row, "validatedBy"),
			Reason:           rowString(row, "reason"),
			RejectionReason:  rowString(row, "rejectionReason"),
		})
	}
	return out
}

func rowString(row client.Row, key string) string {
	s, _ := row[key].(string)
	return s
}

func rowBool(row client.Row, key string) bool {
	b, _ := row[key].(bool)
	return b
}

func rowInt(row client.Row, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
