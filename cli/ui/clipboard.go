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
	if out, err := c.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(cmd + ": " + msg)
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
