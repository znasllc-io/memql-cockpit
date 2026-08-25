package worker

import (
	"errors"
	"fmt"
)

// Setup exit codes. `worker setup` is run by install scripts as often as
// by a person, and the two callers need different things from it: a
// person reads the prose, a script reads `$?`.
//
// Distinguishing "a permission this machine has not granted" from "the
// probe itself blew up" is the whole job. The first is a to-do list for
// the operator; the second is a bug report. A single non-zero code makes
// them one thing, which is what `--non-interactive` was doing --
// znasllc-io/memql#4552 asked for HONEST exit codes and the flag was
// exiting 1 for everything.
//
// The vocabulary is the capability-script contract's
// (memql/docs/internal/design/capability-script-contract.md), minus the
// codes with no meaning here: 0 ok, 2 bad usage, 4 prerequisite missing,
// 5 operation failed.
const (
	SetupExitOK       = 0
	SetupExitUsage    = 2
	SetupExitPrereq   = 4
	SetupExitOpFailed = 5
)

// SetupError is a setup failure that names its own exit code, so the
// dispatcher never has to re-derive one from the message text.
type SetupError struct {
	Code int
	Msg  string
}

func (e *SetupError) Error() string { return e.Msg }

// setupPrereq: the machine is fine, something has not been granted or
// installed yet. The operator has an action; a script should report it.
func setupPrereq(format string, args ...any) error {
	return &SetupError{Code: SetupExitPrereq, Msg: fmt.Sprintf(format, args...)}
}

// setupFailed: the probe itself could not run. Nobody has an action but
// whoever maintains this.
func setupFailed(format string, args ...any) error {
	return &SetupError{Code: SetupExitOpFailed, Msg: fmt.Sprintf(format, args...)}
}

// SetupExitCode maps a runSetupWizard result onto a process exit code. A
// nil error is 0; a SetupError states its own; anything else is an
// operation failure, which is the honest reading of an error nobody
// classified.
func SetupExitCode(err error) int {
	if err == nil {
		return SetupExitOK
	}
	var se *SetupError
	if errors.As(err, &se) {
		return se.Code
	}
	return SetupExitOpFailed
}
