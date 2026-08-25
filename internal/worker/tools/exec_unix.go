//go:build linux || darwin

package tools

import (
	"fmt"
	"math"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// applyShellSysProcAttr sets the SysProcAttr on the child exec
// command. Always sets Setpgid=true so context-cancellation kills
// the whole process tree (defeats grandchild orphaning). When
// limits.RunAsUser is set, additionally configures setuid via
// Credential -- requires the worker process to be running as root,
// silently a no-op otherwise (the syscall will fail and bubble up
// when the child runs).
func applyShellSysProcAttr(cmd *exec.Cmd, limits ShellLimits) error {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if limits.RunAsUser != "" {
		uid, gid, err := lookupUIDGID(limits.RunAsUser)
		if err != nil {
			return fmt.Errorf("run_as_user %q: %w", limits.RunAsUser, err)
		}
		// Bound-check before narrowing to uint32: a uid/gid outside the
		// unsigned 32-bit range would wrap silently
		// (CodeQL go/incorrect-integer-conversion).
		if uid < 0 || uid > math.MaxUint32 || gid < 0 || gid > math.MaxUint32 {
			return fmt.Errorf("run_as_user %q: uid/gid %d/%d out of uint32 range", limits.RunAsUser, uid, gid)
		}
		attr.Credential = &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		}
	}
	cmd.SysProcAttr = attr
	return nil
}

// applyResourceLimits applies the configured rlimits to the
// CURRENT process before exec.Run. The child inherits these via
// fork+exec semantics (copy-on-fork). This is the per-process
// equivalent of putting the worker under a systemd MemoryMax or
// macOS launchd ResourceLimits stanza, except it runs once per
// dispatch -- the Set persists for the lifetime of the parent so
// subsequent dispatches see tighter limits if the operator dials
// the policy down via SIGHUP.
func applyResourceLimits(limits ShellLimits) {
	if limits.MaxCPUSeconds > 0 {
		_ = syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{
			Cur: uint64(limits.MaxCPUSeconds),
			Max: uint64(limits.MaxCPUSeconds),
		})
	}
	if limits.MaxOpenFiles > 0 {
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{
			Cur: uint64(limits.MaxOpenFiles),
			Max: uint64(limits.MaxOpenFiles),
		})
	}
	// RLIMIT_AS is the address-space cap; macOS lacks it as a
	// portable name. Linux honours it; macOS silently no-ops.
	if limits.MaxMemoryMB > 0 {
		applyMemoryLimit(uint64(limits.MaxMemoryMB) * 1024 * 1024)
	}
}

func lookupUIDGID(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid: %w", err)
	}
	return uid, gid, nil
}
