package deploy

import (
	"fmt"
	"strings"
)

// Role is a cluster authorization role, matching the values the engine's
// auth layer assigns (reader / writer / developer / admin / owner).
type Role string

const (
	RoleReader    Role = "reader"
	RoleWriter    Role = "writer"
	RoleDeveloper Role = "developer"
	RoleAdmin     Role = "admin"
	RoleOwner     Role = "owner"
)

// Gate names a deployment role gate. The values mirror the spec names in
// dsl/deployment/specs.memql verbatim so the cockpit's pre-flight decision
// and the engine's in-process spec evaluation cannot drift.
//
//   - GateForward  == spec("requiresDeveloperOrAbove"): cut a version + deploy.
//   - GateRollback == spec("requiresOwner"):            owner-only rollback.
type Gate string

const (
	GateForward  Gate = "requiresDeveloperOrAbove"
	GateRollback Gate = "requiresOwner"
)

// atLeastDeveloper mirrors auth.AtLeastDeveloper (developer / admin / owner).
func (r Role) atLeastDeveloper() bool {
	switch r {
	case RoleDeveloper, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

// isOwner mirrors auth.IsOwner.
func (r Role) isOwner() bool { return r == RoleOwner }

// ParseRole normalizes a free-form role string. Unknown / empty roles
// resolve to RoleReader (read-only) so an unconfigured caller is denied
// by default rather than silently granted.
func ParseRole(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleWriter:
		return RoleWriter
	case RoleDeveloper:
		return RoleDeveloper
	case RoleAdmin:
		return RoleAdmin
	case RoleOwner:
		return RoleOwner
	default:
		return RoleReader
	}
}

// Authorize returns nil when role satisfies gate, or a descriptive error
// (suitable for the audit "denied" reason + a stderr message) otherwise.
//
// The decision is the DSL-side mirror of dsl/deployment/specs.memql; the
// cockpit enforces it BEFORE invoking the automation so a forbidden caller
// never reaches the embedded runtime (handoff "Make → cockpit" decision).
func Authorize(gate Gate, role Role) error {
	switch gate {
	case GateForward:
		if role.atLeastDeveloper() {
			return nil
		}
		return fmt.Errorf("forward deploy requires developer, admin, or owner role (spec requiresDeveloperOrAbove); caller role is %q", role)
	case GateRollback:
		if role.isOwner() {
			return nil
		}
		return fmt.Errorf("rollback requires the owner role (spec requiresOwner); caller role is %q", role)
	default:
		return fmt.Errorf("unknown deployment gate %q", gate)
	}
}

// GateForAutomation classifies a named automation onto its role gate. The
// forward-deploy gate is the default; rollback-shaped automations escalate
// to the owner-only gate, matching the role matrix in dsl/deployment/specs.memql
// (#1876): even admin may not roll back.
func GateForAutomation(name string) Gate {
	if strings.Contains(strings.ToLower(name), "rollback") {
		return GateRollback
	}
	return GateForward
}
