package worker

import (
	"errors"
	"testing"
)

// The exit code is the whole point of --non-interactive. It shipped
// exiting 1 for everything, which is what znasllc-io/memql#4552 meant by
// asking for HONEST exit codes: an install script reading `$?` has to be
// able to tell "a permission has not been granted yet" (the operator has
// an action) from "the probe itself failed" (nobody does but us).
func TestSetupExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, SetupExitOK},
		{"prerequisite missing", setupPrereq("Accessibility is not granted"), SetupExitPrereq},
		{"probe failed", setupFailed("temp file: no space"), SetupExitOpFailed},
		// An error nobody classified is an operation failure -- not a
		// success, and not a prerequisite. Reading it as either would be
		// an invention.
		{"unclassified", errors.New("something else"), SetupExitOpFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SetupExitCode(tc.err); got != tc.want {
				t.Errorf("SetupExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// The codes must stay distinct, or the mapping above is decorative --
// which is exactly the state it replaces.
func TestSetupExitCodesAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, code := range map[string]int{
		"OK": SetupExitOK, "Usage": SetupExitUsage,
		"Prereq": SetupExitPrereq, "OpFailed": SetupExitOpFailed,
	} {
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s share exit code %d", name, other, code)
		}
		seen[code] = name
	}
	if SetupExitOK != 0 {
		t.Errorf("SetupExitOK = %d, want 0", SetupExitOK)
	}
}

func TestSetupErrorCarriesItsMessageAndCode(t *testing.T) {
	err := setupPrereq("wayland session: %s", "RobotGo drives X11 only")
	if err.Error() != "wayland session: RobotGo drives X11 only" {
		t.Errorf("Error() = %q", err.Error())
	}
	var se *SetupError
	if !errors.As(err, &se) {
		t.Fatal("setupPrereq did not produce a *SetupError")
	}
	if se.Code != SetupExitPrereq {
		t.Errorf("Code = %d, want %d", se.Code, SetupExitPrereq)
	}
}

// The headless build's setup must SUCCEED and prompt for nothing: it is
// reached by `worker pair` on every non-computeruse machine, and an
// error there would abort an enrollment with nothing wrong with it.
func TestHeadlessSetupSucceeds(t *testing.T) {
	if BuildHasComputerUse() {
		t.Skip("computeruse build: the real pre-flight runs instead")
	}
	if err := runSetupWizard(); err != nil {
		t.Errorf("headless runSetupWizard returned %v, want nil", err)
	}
}
