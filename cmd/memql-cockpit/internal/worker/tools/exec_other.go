//go:build !linux && !darwin

package tools

import "os/exec"

// applyShellSysProcAttr on non-Unix platforms is a no-op. Setpgid,
// Credential, and rlimits all live under syscall packages that
// only build on linux/darwin in the standard library; Windows
// would need a different code path entirely. Until the worker
// supports Windows, this stub keeps the build green on dev
// machines that aren't linux/darwin.
func applyShellSysProcAttr(_ *exec.Cmd, _ ShellLimits) error {
	return nil
}

func applyResourceLimits(_ ShellLimits) {}
