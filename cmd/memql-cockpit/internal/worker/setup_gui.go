//go:build gui

package worker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-vgo/robotgo"
)

// runSetupWizard is the macOS / Linux X11 permissions pre-flight.
//
// Dispatches between two implementations:
//
//   - Single-panel TUI (cli/ui + cli/canvas, see wizard_gui.go).
//     The default path on every interactive terminal session.
//   - Plain printf, with the same probe logic. Used when stdin or
//     stdout is not a TTY (CI, install scripts, piped output) so
//     scripted callers don't choke on tcell escape codes.
//
// Per cli/CLAUDE.md's canonical-TUI rule, every interactive surface
// of memql-cockpit goes through the TUI primitives -- the printf
// path here is strictly a non-TTY fallback, not an alternative UX.
//
// macOS notes:
//
//   - TCC gates Accessibility + Screen Recording per signed-binary
//     identity. A command-line binary launched from Terminal
//     INHERITS Terminal's grants -- the wizard might say "ok" even
//     when the binary itself would be denied if launched as a
//     LaunchAgent at login. Both the TUI + printf paths report the
//     per-binary `tccutil check` result so the user can tell the
//     cases apart.
//   - Probes the actual gated operations (not just reads):
//     Accessibility    -> robotgo.MoveRelative(2,0) + back
//     (CGEventPost; denied -> silent no-op)
//     Screen Recording -> robotgo.SaveCapture; denied capture
//     comes back tiny / empty
func runSetupWizard() error {
	if isInteractiveTTY() && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") {
		if err := runTUIWizard(); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: TUI wizard failed (%v); falling back to printf wizard.\n\n", err)
			return runSetupPrintf()
		}
		return nil
	}
	return runSetupPrintf()
}

// runSetupPrintf is the non-TTY fallback. Same probe logic, just
// rendered as scrolling stdout text instead of a tcell single
// panel.
func runSetupPrintf() error {
	fmt.Println("memql-cockpit-gui worker setup")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	switch runtime.GOOS {
	case "darwin":
		return runSetupMacOS()
	case "linux":
		return runSetupLinux()
	default:
		fmt.Printf("INFO: setup wizard not implemented for %s; the worker will probe permissions at first run.\n", runtime.GOOS)
		return nil
	}
}

func runSetupMacOS() error {
	binPath, _ := os.Executable()
	if binPath != "" {
		if abs, err := filepath.Abs(binPath); err == nil {
			binPath = abs
		}
	}

	fmt.Println("Binary:", binPath)
	fmt.Println()
	fmt.Println("Note: macOS grants Accessibility + Screen Recording PER")
	fmt.Println("BINARY. Running this wizard from Terminal probes whether")
	fmt.Println("the call SUCCEEDS, which can be true because Terminal")
	fmt.Println("already has the permission and child processes inherit.")
	fmt.Println("When the LaunchAgent runs cockpit-gui detached at login,")
	fmt.Println("the binary needs ITS OWN entry under System Settings ->")
	fmt.Println("Privacy & Security. The wizard now also reports the per-")
	fmt.Println("binary status from `tccutil` so you can tell the two")
	fmt.Println("cases apart.")
	fmt.Println()

	fmt.Println("Step 1/3: Accessibility")
	fmt.Println(strings.Repeat("-", 60))
	accessOK, accessDetail := probeAccessibility()
	fmt.Printf("  active probe: %s\n", accessDetail)
	bundleHasAccess := tccCheck("Accessibility", binPath)
	fmt.Printf("  per-binary grant: %s\n", bundleHasAccess)
	if !accessOK {
		fmt.Println()
		fmt.Println("  Accessibility is DENIED in this process tree.")
		fmt.Println("  Open System Settings -> Privacy & Security -> Accessibility")
		fmt.Println("  and approve EITHER memql-cockpit-gui OR your terminal app.")
		fmt.Println("  If you plan to run as a LaunchAgent later, you must")
		fmt.Println("  approve the cockpit-gui binary directly.")
		fmt.Println()
		if !waitForKey("Press Enter after approving (or Ctrl-C to abort)...") {
			return errors.New("setup cancelled")
		}
		// Re-probe after approval.
		accessOK, accessDetail = probeAccessibility()
		fmt.Printf("  re-probe: %s\n", accessDetail)
		if !accessOK {
			return errors.New("Accessibility still denied after approval -- restart the binary or check System Settings")
		}
	}
	fmt.Println("  Accessibility: OK")
	fmt.Println()

	fmt.Println("Step 2/3: Screen Recording")
	fmt.Println(strings.Repeat("-", 60))
	srOK, srDetail := probeScreenRecording()
	fmt.Printf("  active probe: %s\n", srDetail)
	bundleHasSR := tccCheck("ScreenCapture", binPath)
	fmt.Printf("  per-binary grant: %s\n", bundleHasSR)
	if !srOK {
		fmt.Println()
		fmt.Println("  Screen Recording is DENIED in this process tree.")
		fmt.Println("  Open System Settings -> Privacy & Security -> Screen Recording")
		fmt.Println("  and approve EITHER memql-cockpit-gui OR your terminal app.")
		fmt.Println("  If you plan to run as a LaunchAgent later, you must")
		fmt.Println("  approve the cockpit-gui binary directly.")
		fmt.Println()
		if !waitForKey("Press Enter after approving (or Ctrl-C to abort)...") {
			return errors.New("setup cancelled")
		}
		srOK, srDetail = probeScreenRecording()
		fmt.Printf("  re-probe: %s\n", srDetail)
		if !srOK {
			return errors.New("Screen Recording still denied after approval -- macOS sometimes requires a binary restart; re-run setup")
		}
	}
	fmt.Println("  Screen Recording: OK")
	fmt.Println()

	fmt.Println("Step 3/3: Validate")
	fmt.Println(strings.Repeat("-", 60))
	width, height := robotgo.GetScreenSize()
	x, y := robotgo.Location()
	fmt.Printf("  display:   %dx%d\n", width, height)
	fmt.Printf("  cursor:    (%d,%d)\n", x, y)
	fmt.Println()
	fmt.Println("SUCCESS: cockpit-gui worker permissions look good.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Configure ~/.memql/worker.yaml (cluster_url + token).")
	fmt.Println("  2. Run `memql-cockpit-gui worker run` to start serving.")
	fmt.Println()
	fmt.Println("If you'll run this as a LaunchAgent: install via")
	fmt.Println("`scripts/install/install-mac.sh` -- it loads the agent and")
	fmt.Println("the FIRST time the LaunchAgent process tries Accessibility")
	fmt.Println("or Screen Recording, macOS will prompt you separately for")
	fmt.Println("the cockpit-gui binary itself (a different TCC entry from")
	fmt.Println("Terminal). Approve that prompt to make the agent work at login.")
	return nil
}

// probeAccessibility issues a real CGEventPost (mouse-relative
// move) and verifies the cursor actually moved. Reading
// robotgo.Location() doesn't require Accessibility on macOS so a
// pure read isn't a meaningful probe.
//
// Wrapped in a 5-second timeout for the same reason
// probeScreenRecording is: the underlying CGO calls can block
// indefinitely on a TCC-state mismatch and we'd rather time out
// with a clear message than freeze the wizard.
//
// Returns (granted, human-readable detail).
func probeAccessibility() (bool, string) {
	type result struct {
		ok     bool
		detail string
	}
	resCh := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resCh <- result{false, fmt.Sprintf("panic during probe: %v", r)}
			}
		}()
		startX, startY := robotgo.Location()
		robotgo.MoveRelative(2, 0)
		robotgo.MilliSleep(40)
		midX, midY := robotgo.Location()
		robotgo.Move(startX, startY)
		robotgo.MilliSleep(40)
		if midX != startX || midY != startY {
			resCh <- result{true, fmt.Sprintf("cursor moved (%d,%d)->(%d,%d)->back", startX, startY, midX, midY)}
			return
		}
		resCh <- result{false, fmt.Sprintf("cursor stayed at (%d,%d) after MoveRelative -- Accessibility denied", startX, startY)}
	}()
	select {
	case r := <-resCh:
		return r.ok, r.detail
	case <-time.After(5 * time.Second):
		return false, "probe timed out after 5s -- press R to re-probe after confirming Accessibility entry"
	}
}

// probeScreenRecording asks macOS via CGPreflightScreenCaptureAccess
// whether this process has a Screen Recording grant -- including the
// inherited grant from the parent shell when the binary is launched
// from Terminal. The CG call is synchronous and never blocks; the
// previous robotgo.SaveCapture-based probe was observed to hang
// indefinitely under certain TCC-state mismatches (user just toggled
// a grant on, the binary's hash changed since the last approval,
// inheritance not propagating to the child process). The 5-second
// timeout below is defensive; the cgo call itself returns in
// microseconds.
//
// On Linux / Windows builds (gui-tagged but non-darwin) the wrapper
// always reports true -- there's no system-level TCC ledger to
// preflight against; downstream capture failures surface as runtime
// errors rather than pre-flight denials.
func probeScreenRecording() (bool, string) {
	type result struct {
		ok     bool
		detail string
	}
	resCh := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resCh <- result{false, fmt.Sprintf("panic during preflight: %v", r)}
			}
		}()
		if preflightScreenCaptureAccess() {
			resCh <- result{true, "CGPreflightScreenCaptureAccess returned granted"}
			return
		}
		resCh <- result{false, "CGPreflightScreenCaptureAccess returned denied -- approve cockpit-gui (or its parent Terminal) under System Settings -> Privacy & Security -> Screen Recording"}
	}()
	select {
	case r := <-resCh:
		return r.ok, r.detail
	case <-time.After(5 * time.Second):
		return false, "preflight timed out after 5s -- press R to re-probe"
	}
}

// tccCheck shells out to /usr/bin/tccutil to query the per-binary
// permission status. Best-effort: tccutil's `check` subcommand was
// added in macOS Sonoma 14.4; older OSes report "unsupported".
func tccCheck(service, binaryPath string) string {
	if binaryPath == "" {
		return "unknown (cannot resolve binary path)"
	}
	tcc, err := exec.LookPath("tccutil")
	if err != nil {
		return "unknown (tccutil not found)"
	}
	cmd := exec.Command(tcc, "check", service, binaryPath)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		// Older macOS without `check` reports usage on stderr.
		if strings.Contains(trimmed, "Usage") || strings.Contains(trimmed, "unrecognized") {
			return "unknown (this macOS version's tccutil does not support `check`)"
		}
		// `tccutil check X` exits non-zero when the answer is "denied".
		if trimmed != "" {
			return "denied (" + firstLine(trimmed) + ")"
		}
		return "denied"
	}
	if trimmed == "" {
		return "granted"
	}
	return "granted (" + firstLine(trimmed) + ")"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func runSetupLinux() error {
	display := os.Getenv("DISPLAY")
	wayland := os.Getenv("WAYLAND_DISPLAY")
	if wayland != "" && display == "" {
		fmt.Println("Wayland detected (WAYLAND_DISPLAY set, DISPLAY unset).")
		fmt.Println("RobotGo's open-source build targets X11 only.")
		fmt.Println()
		fmt.Println("Action: register the worker as HEADLESS-only by leaving")
		fmt.Println("the GUI capability out of ~/.memql/worker.yaml. The")
		fmt.Println("install-linux.sh installer detects Wayland and does")
		fmt.Println("this for you.")
		return nil
	}
	if display == "" {
		return errors.New("setup: DISPLAY env var is unset; X11 unreachable")
	}
	fmt.Printf("X11 DISPLAY=%s\n", display)
	startX, startY := robotgo.Location()
	robotgo.MoveRelative(2, 0)
	robotgo.MilliSleep(40)
	midX, midY := robotgo.Location()
	robotgo.Move(startX, startY)
	if midX == startX && midY == startY {
		return fmt.Errorf("X11: cursor MoveRelative did not move; check XTEST + DISPLAY auth (Xauth/cookie)")
	}
	tmp, err := os.CreateTemp("", "worker-setup-probe-*.png")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	if err := robotgo.SaveCapture(tmpPath, 0, 0, 16, 16); err != nil {
		return fmt.Errorf("X11 screenshot probe: %w", err)
	}
	stat, err := os.Stat(tmpPath)
	if err != nil || stat.Size() < 200 {
		return fmt.Errorf("X11 screenshot suspiciously small (%d bytes)", stat.Size())
	}
	fmt.Printf("  cursor: moved + restored\n")
	fmt.Printf("  screenshot: %d bytes\n", stat.Size())
	fmt.Println()
	fmt.Println("SUCCESS: X11 permissions look good.")
	return nil
}

func waitForKey(prompt string) bool {
	fmt.Print(prompt + " ")
	r := bufio.NewReader(os.Stdin)
	_, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	return true
}
