//go:build darwin

package tools

// applyMemoryLimit on darwin is a no-op: macOS does not provide a
// portable RLIMIT_AS equivalent that survives setrlimit
// reliably across all hardware/kernel versions. Operators who
// need hard memory caps should run the worker under a launchd
// configuration that sets `<key>SoftResourceLimits</key>` /
// `<key>HardResourceLimits</key>` instead.
func applyMemoryLimit(_ uint64) {}
