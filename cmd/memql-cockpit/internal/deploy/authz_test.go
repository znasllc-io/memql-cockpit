package deploy

import "testing"

func TestAuthorize_ForwardGate(t *testing.T) {
	// requiresDeveloperOrAbove: developer / admin / owner pass; reader /
	// writer are denied. Mirrors dsl/deployment/specs.memql.
	cases := map[Role]bool{
		RoleReader:    false,
		RoleWriter:    false,
		RoleDeveloper: true,
		RoleAdmin:     true,
		RoleOwner:     true,
	}
	for role, want := range cases {
		err := Authorize(GateForward, role)
		if (err == nil) != want {
			t.Errorf("Authorize(forward, %s): allowed=%v want=%v (err=%v)", role, err == nil, want, err)
		}
	}
}

func TestAuthorize_RollbackGate(t *testing.T) {
	// requiresOwner: ONLY owner passes -- not even admin may roll back.
	cases := map[Role]bool{
		RoleReader:    false,
		RoleWriter:    false,
		RoleDeveloper: false,
		RoleAdmin:     false,
		RoleOwner:     true,
	}
	for role, want := range cases {
		err := Authorize(GateRollback, role)
		if (err == nil) != want {
			t.Errorf("Authorize(rollback, %s): allowed=%v want=%v (err=%v)", role, err == nil, want, err)
		}
	}
}

func TestAuthorize_UnknownGate(t *testing.T) {
	if err := Authorize(Gate("nope"), RoleOwner); err == nil {
		t.Fatal("expected error for unknown gate")
	}
}

func TestGateForAutomation(t *testing.T) {
	cases := map[string]Gate{
		"deployEngineCluster":   GateForward,
		"cutVersion":            GateForward,
		"rollbackDeployment":    GateRollback,
		"engineClusterRollback": GateRollback,
		"someRollbackThing":     GateRollback,
	}
	for name, want := range cases {
		if got := GateForAutomation(name); got != want {
			t.Errorf("GateForAutomation(%q) = %q want %q", name, got, want)
		}
	}
}

func TestParseRole(t *testing.T) {
	cases := map[string]Role{
		"owner":       RoleOwner,
		"ADMIN":       RoleAdmin,
		" developer ": RoleDeveloper,
		"writer":      RoleWriter,
		"reader":      RoleReader,
		"":            RoleReader, // deny-by-default
		"bogus":       RoleReader,
	}
	for in, want := range cases {
		if got := ParseRole(in); got != want {
			t.Errorf("ParseRole(%q) = %q want %q", in, got, want)
		}
	}
}
