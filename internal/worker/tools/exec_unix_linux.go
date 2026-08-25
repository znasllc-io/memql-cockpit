//go:build linux

package tools

import "syscall"

// applyMemoryLimit sets RLIMIT_AS on Linux. Best-effort.
func applyMemoryLimit(bytes uint64) {
	_ = syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: bytes, Max: bytes})
}
