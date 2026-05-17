package crash_test

// Real-screen integration test for the crash recovery system. Drives
// the production primitives -- crash.Catch + crash.DrawInline +
// crash.WriteLog -- against a real ui.Screen backed by
// tcell.NewSimulationScreen. SimulationScreen doesn't need a TTY,
// so this test can run in any environment (CI, headless build
// servers, the developer's machine while another cockpit instance
// is running) and assert on the rendered cell contents directly.
//
// What this test proves that the unit tests don't:
//   - DrawInline actually paints recognizable cells (not just
//     "returns without panic")
//   - The error code visible in the rendered output is the same
//     code that lands in the crash log filename
//   - The crash log file lands at LogDir() with mode 0600
//   - The full pipeline (Catch -> WriteLog -> DrawInline) works
//     against real ui.Screen primitives, not the test simulator

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/crash"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestCrashIntegration_EndToEnd is the headline test: simulates a
// panicking tab Draw, runs it through crash.Catch the same way
// app.go does, renders the inline placeholder to a simulated
// tcell screen, then verifies (a) the rendered cells contain the
// expected user-facing text + error code + log path, (b) the
// crash log file exists at LogDir() with mode 0600 + the right
// filename, and (c) the file's contents include the panic value
// and the stack trace.
//
// If any future refactor breaks one of those guarantees, this
// test fails loudly. It's the closest we can get to "real
// cockpit panicked and the user saw the placeholder" without
// scripting a live TTY.
func TestCrashIntegration_EndToEnd(t *testing.T) {
	tmpHomeForIntegration(t)

	// SimulationScreen is tcell's headless screen. Init + size it
	// to match a typical cockpit launch.
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	defer sim.Fini()
	const w, h = 120, 40
	sim.SetSize(w, h)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	theme := ui.DefaultTheme()

	// Step 1: simulate the panicking tab. Same shape as app.go's
	// per-tab dispatch: wrap the tab's Draw in crash.Catch, on
	// panic stash the report + render the inline placeholder.
	bounds := ui.Rect{X: 0, Y: 0, Width: w, Height: h}
	report := crash.Catch("draw:concepts", func() {
		// This is the exact crash shape the user hit on the real
		// cockpit before the locks landed: index-out-of-range on
		// a length-0 slice during Draw.
		var empty []int
		_ = empty[2]
	})
	if report == nil {
		t.Fatal("Catch returned nil; expected a Report for the deliberately-panicking fn")
	}
	if report.LogPath == "" {
		t.Fatal("Report.LogPath empty; WriteLog must have failed")
	}

	// Step 2: render the inline placeholder, exactly as app.go does
	// when a tab's draw panics.
	crash.DrawInline(screen, bounds, theme, report)
	sim.Show()

	// Step 3: assert the rendered cells contain the user-facing text.
	rendered := flattenSimCells(sim)
	wantInScreen := []string{
		"SOMETHING WENT WRONG",
		"This tab encountered an unexpected error",
		"F1 / F2 / F3 to switch tabs",
		"Error code:",
		report.Code,
		"contact support",
	}
	for _, want := range wantInScreen {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered screen missing %q; full screen text:\n%s",
				want, indent(rendered, "    "))
		}
	}

	// Step 4: assert the crash log file is real, at the right path,
	// with the right mode, and contains the panic value + stack.
	if !strings.Contains(report.LogPath, crash.LogDir()) {
		t.Errorf("LogPath %q not under LogDir %q", report.LogPath, crash.LogDir())
	}
	info, err := os.Stat(report.LogPath)
	if err != nil {
		t.Fatalf("crash log not on disk: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("crash log mode = %o; want 0600 (sensitive stack contents)", mode)
	}
	body, err := os.ReadFile(report.LogPath)
	if err != nil {
		t.Fatalf("read crash log: %v", err)
	}
	logText := string(body)
	for _, want := range []string{report.Code, "index out of range", "draw:concepts", "goroutine"} {
		if !strings.Contains(logText, want) {
			t.Errorf("crash log missing %q; full log:\n%s", want, indent(logText, "    "))
		}
	}

	// Step 5: the filename pattern should be <timestamp>-<code>.log so
	// support's "find the log for code X" workflow doesn't need a
	// grep into every file.
	base := filepath.Base(report.LogPath)
	if !strings.Contains(base, report.Code) {
		t.Errorf("crash log filename %q does not contain the error code %q", base, report.Code)
	}
	if !strings.HasSuffix(base, ".log") {
		t.Errorf("crash log filename %q missing .log suffix", base)
	}
}

// TestCrashIntegration_StickyState_SuppressesRedraw is the
// integration analog of the unit test that says "after a tab Draw
// panics once, subsequent frames render the placeholder instead
// of re-invoking the broken Draw". This time we use the real
// crash subsystem + real ui.Screen instead of the tabSim mock.
//
// Specifically: invoking DrawInline with the SAME report multiple
// times produces a stable visible output (idempotent), AND the
// LogDir doesn't accumulate one new file per draw -- only the
// original Catch wrote a log; re-renders of the placeholder do
// not.
func TestCrashIntegration_StickyState_SuppressesRedraw(t *testing.T) {
	tmpHomeForIntegration(t)

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(120, 40)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	bounds := ui.Rect{X: 0, Y: 0, Width: 120, Height: 40}
	theme := ui.DefaultTheme()

	// One Catch -> one log.
	report := crash.Catch("draw:concepts", func() { panic("first") })
	if report == nil {
		t.Fatal("expected a report from the panic")
	}
	logsBefore := listLogs(t)
	if got := len(logsBefore); got != 1 {
		t.Errorf("expected exactly 1 crash log after Catch; got %d (%v)", got, logsBefore)
	}

	// Render the placeholder N times. None of these should write a
	// new log file; DrawInline is a pure renderer.
	for i := 0; i < 5; i++ {
		crash.DrawInline(screen, bounds, theme, report)
	}
	sim.Show() // flush the back buffer so GetContents sees the paint
	logsAfter := listLogs(t)
	if got := len(logsAfter); got != 1 {
		t.Errorf("expected the log count to stay at 1 across re-renders; got %d", got)
	}

	// The placeholder content should be unchanged -- key strings
	// still present.
	rendered := flattenSimCells(sim)
	if !strings.Contains(rendered, report.Code) {
		t.Errorf("re-rendered placeholder missing error code %q; output:\n%s",
			report.Code, indent(rendered, "    "))
	}
}

// TestCrashIntegration_DrawInline_DegradedBounds covers the
// defensive path where the cockpit is sized so small that the
// inline placeholder can't fully fit. DrawInline must NOT panic;
// it can render a truncated message but must remain a pure
// function call.
func TestCrashIntegration_DrawInline_DegradedBounds(t *testing.T) {
	tmpHomeForIntegration(t)

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(10, 3)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	theme := ui.DefaultTheme()
	report := &crash.Report{
		Code:    "abc12345",
		At:      time.Now(),
		Label:   "test:tiny",
		Value:   "nope",
		LogPath: "/tmp/whatever.log",
	}

	// Bounds 10x3 -- tighter than the body needs. Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DrawInline panicked on tiny bounds: %v", r)
		}
	}()
	crash.DrawInline(screen, ui.Rect{X: 0, Y: 0, Width: 10, Height: 3}, theme, report)

	// Also a zero-sized bounds -- early-out path. Should be a
	// complete no-op, no panic.
	crash.DrawInline(screen, ui.Rect{X: 0, Y: 0, Width: 0, Height: 0}, theme, report)
}

// --- helpers ---------------------------------------------------------

// tmpHomeForIntegration redirects $HOME for the duration of one
// integration test so crash logs land in a sandboxed temp dir and
// get cleaned up automatically. Same pattern as the unit-test
// helper; duplicated here so this file is self-contained.
func tmpHomeForIntegration(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	saved, had := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("HOME", saved)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
}

// flattenSimCells joins the simulation screen's rendered cells into
// a single string, one row per line. Lets tests assert with simple
// strings.Contains instead of cell-by-cell coordinate walks.
func flattenSimCells(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	if w <= 0 || h <= 0 {
		return ""
	}
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// indent prefixes every line of s with prefix. Used to make test
// failure messages with multi-line dumps stand out from the
// surrounding error text.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// listLogs returns every file currently in LogDir(). Used to assert
// that re-rendering the inline placeholder does NOT mint additional
// crash logs.
func listLogs(t *testing.T) []string {
	t.Helper()
	dir := crash.LogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read LogDir: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type()&fs.ModeType == 0 {
			out = append(out, e.Name())
		}
	}
	return out
}
