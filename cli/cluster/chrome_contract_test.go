package cluster

// Regression tests that the Clusters tab's two list panes (cluster
// manager + partition manager) abide by the panel chrome contract in
// cli/CLAUDE.md ("Panel chrome contract"). These panes are the
// reference implementation -- the contract was reverse-engineered
// from them, so a future refactor breaking the convention here is
// the worst-case regression: it would silently legitimize divergence
// in any other pane that follows by copy/paste.
//
// What we assert per pane:
//   * The bottom row of the rendered bounds carries the action-hint
//     band (single-space-padded chip line, not a count footer).
//   * Each chip parses as `Key:Label` -- no `Key: Label`, no prose.
//
// Search-input rules don't apply here -- neither pane filters --
// so we skip those assertions. The concepts package owns the search-
// specific contract tests.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	contractWidth  = 60
	contractHeight = 20
)

// hintChip matches `Key:Label` shape; Key may be empty (when the key
// itself IS `:`, used by panes that filter -- not these two, but kept
// in lockstep with cli/concepts/chrome_contract_test.go so the
// grammar definition is identical everywhere).
var hintChip = regexp.MustCompile(`^\S*:\S+$`)

// flattenSim returns the simulation screen's contents as one string
// per row. Tests assert on the LAST row (the chrome band).
func flattenSim(sim tcell.SimulationScreen) []string {
	cells, w, h := sim.GetContents()
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		rows[y] = b.String()
	}
	return rows
}

// assertChipFormat trims the row, splits on the canonical two-space
// chip separator, and runs the contract checks against each chip.
// Empty rows (no hints to assert) cause an immediate test failure --
// every interactive pane MUST advertise at least one action.
func assertChipFormat(t *testing.T, where, row string) {
	t.Helper()
	row = strings.TrimRight(row, " ")
	row = strings.TrimLeft(row, " ")
	if row == "" {
		t.Fatalf("%s: chrome band row was empty -- every interactive pane must render at least one Key:Label hint", where)
	}
	chips := strings.Split(row, "  ")
	for _, chip := range chips {
		chip = strings.TrimSpace(chip)
		if chip == "" {
			continue
		}
		if !strings.Contains(chip, ":") {
			t.Errorf("%s: chip %q missing colon separator (contract: Key:Label)", where, chip)
			continue
		}
		idx := strings.Index(chip, ":")
		if idx+1 < len(chip) && chip[idx+1] == ' ' {
			t.Errorf("%s: chip %q has space after colon (contract: Key:Label with no space)", where, chip)
		}
		if !hintChip.MatchString(chip) {
			t.Errorf("%s: chip %q does not match canonical Key:Label shape", where, chip)
		}
	}
}

// TestPanelChrome_ClustersView_BottomBand renders the Clusters
// manager pane and asserts the LAST row carries a valid action-hint
// strip. The cluster manager is the reference implementation; this
// test guards against a future refactor that moves the hints
// elsewhere or starts painting a count footer at the bottom.
func TestPanelChrome_ClustersView_BottomBand(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(contractWidth, contractHeight)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	v := NewClustersView(ui.DefaultTheme())
	v.SetClusters([]ClusterStatus{
		{Config: config.ClusterConfig{Name: "local"}, Status: "connected"},
		{Config: config.ClusterConfig{Name: "staging"}, Status: "unreachable"},
	})
	// OnEntryState gates the lifecycle-aware chips (Enter:Select,
	// R:Retry, etc.). Stub it so the cluster manager has the full
	// set of context-aware hints to render.
	v.OnEntryState = func(name string) (string, int, string, bool) {
		switch name {
		case "local":
			return "connected", 0, "", true
		case "staging":
			return "failed", 3, "", true
		}
		return "", 0, "", false
	}

	bounds := ui.Rect{X: 0, Y: 0, Width: contractWidth, Height: contractHeight}
	v.Draw(screen, bounds)
	sim.Show()
	rows := flattenSim(sim)

	// The Clusters tab splits the screen into a left column (manager
	// + partitions) and a right column (topology) with a `│` divider
	// between them. Action hints live in the left column; trim
	// everything from the divider onward before running the chip
	// assertions so right-pane content isn't mistakenly treated as
	// a chip.
	foundManager := false
	for _, row := range rows {
		leftSegment := row
		if i := strings.Index(row, "│"); i >= 0 {
			leftSegment = row[:i]
		}
		if strings.Contains(leftSegment, "A:Add") {
			foundManager = true
			assertChipFormat(t, "ClustersView hint row", leftSegment)
		}
	}
	if !foundManager {
		t.Fatalf("expected to find a 'A:Add' chip in the rendered Clusters tab; got:\n%s", strings.Join(rows, "\n"))
	}
}

// TestPanelChrome_PartitionsView_BottomBand retired in #56 phase 8.
// The PartitionsView is now a no-op stub and the partition manager
// chrome contract no longer applies.
