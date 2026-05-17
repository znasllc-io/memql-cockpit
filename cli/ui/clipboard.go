package ui

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// CopyToClipboard writes text to the system clipboard by shelling out
// to the platform's native copy tool. No third-party library required
// -- the binary cost of a CLI that can survive without cgo + X libs is
// worth more than the one-invocation fork cost of "pbcopy".
//
// Returns nil on success, or an error describing which tool was tried
// and why it didn't work. Callers should surface the error to the
// notifications feed so the user sees why copy failed (missing tool,
// no display, etc.) rather than silently no-op.
//
// CRITICAL: do NOT capture stdout / stderr via exec.Cmd's pipe
// mechanism (CombinedOutput, Output, c.Stdout = &buf). On Linux/X11,
// xclip's input mode forks a background "selection owner" child so
// the X clipboard remains populated after the parent process exits.
// That forked child inherits any pipe FD we set up to capture
// stdout/stderr; cmd.Wait() then blocks until ALL writers close the
// pipe, which only happens when another X client takes the selection
// (i.e. potentially never). The cockpit's Ctrl+Y handler runs in the
// tcell event loop -- a blocked Wait() freezes the whole TUI, no
// keys respond (Ctrl+Q included), and the user has to kill -9 the
// process. Leaving Stdout/Stderr nil routes them to /dev/null; the
// forked child inherits the null FDs, has nothing to keep open, and
// cmd.Wait() returns as soon as the parent xclip exits. Trade-off:
// we lose detailed stderr capture, so on failure we surface only
// the exit-code error -- worth it; a hung TUI is much worse than
// a less-detailed error message. The LookPath pre-check above
// distinguishes "tool not installed" from "tool failed at runtime",
// which covers the two cases that actually need different remediation.
func CopyToClipboard(text string) error {
	cmd, arg, env := clipboardCommand()
	if cmd == "" {
		return errors.New("no clipboard tool detected on this platform")
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return errors.New("clipboard tool " + cmd + " not found on PATH")
	}

	c := exec.Command(cmd, arg...)
	c.Env = append(c.Env, env...)
	c.Stdin = strings.NewReader(text)
	// Stdout / Stderr deliberately left nil -- see the comment above
	// for the xclip-fork-child rationale.
	if err := c.Run(); err != nil {
		return errors.New(cmd + ": " + err.Error())
	}
	return nil
}

// clipboardCommand returns the (command, args, env) triple to use on
// the current OS. Linux prefers Wayland's wl-copy when WAYLAND_DISPLAY
// is set, otherwise falls back to X's xclip -- that ordering matches
// what users expect when both are installed.
func clipboardCommand() (string, []string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "pbcopy", nil, nil
	case "windows":
		return "clip.exe", nil, nil
	case "linux":
		// wl-copy is headless-friendly on Wayland; xclip on X11.
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return "wl-copy", nil, nil
		}
		return "xclip", []string{"-selection", "clipboard"}, nil
	default:
		return "", nil, nil
	}
}
