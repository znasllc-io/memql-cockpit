package worker

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// `memql worker apps` (memql-cockpit#438).
//
// It answers the question an owner has the moment they write apps.levels --
// *what will this machine actually run, at each level, for each app?* -- and
// the one the portal cannot answer for them: why an app on this machine is
// not being picked. "Not installed", "installed but not in apps.allow" and
// "allowed but not signed in" all render identically from the cluster, as an
// absence; only the machine can tell them apart.
//
// It runs the SAME detector and reads the SAME policy the worker does, so
// what it prints is what a session would get. A second implementation of
// "which knobs does strong mean here" would be a second answer, and the
// owner would have no way to know which one lied.

const appsProbeTimeout = 15 * time.Second

func handleApps(args []string) {
	fs := flag.NewFlagSet("worker apps", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	_ = fs.Parse(args)

	// The policy.yaml the worker itself reads: beside workers.yaml for a
	// fleet, beside worker.yaml otherwise -- the order `memql worker
	// backup` already follows. A report read from the other file would be
	// a confident answer about a policy the worker never loads.
	policyPath := filepath.Join(filepath.Dir(*workersPath), "policy.yaml")
	if _, err := os.Stat(policyPath); err != nil {
		policyPath = filepath.Join(filepath.Dir(*configPath), "policy.yaml")
	}
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		// Unlike the worker, which runs on defaults when the file does not
		// parse, this command exists to say what the file means -- and a
		// table printed from defaults would be a confident answer about a
		// file nobody read.
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), appsProbeTimeout)
	defer cancel()
	inventory := (&apps.Detector{}).Detect(ctx, policy.AppsAllow())
	printAppInventory(os.Stdout, inventory, policy, policyPath)
}

// printAppInventory writes the report. Split out so the shape is assertable
// without a claude or a codex on the machine running the tests.
func printAppInventory(w io.Writer, inventory []apps.Info, policy *tools.Policy, policyPath string) {
	fmt.Fprintln(w, "Apps on this machine")
	fmt.Fprintln(w, "(as this shell's PATH finds them; the worker's service can run with a different PATH)")
	fmt.Fprintln(w)

	found := make(map[string]apps.Info, len(inventory))
	for _, a := range inventory {
		found[a.Id] = a
	}
	for _, spec := range apps.Specs() {
		a, ok := found[spec.ID]
		if !ok {
			fmt.Fprintf(w, "%-12s not installed (no %s on PATH)\n\n", spec.ID, spec.Binary)
			continue
		}
		printAppRows(w, a, policy, policyPath)
	}

	if problems := policy.AppLevelProblems(); len(problems) > 0 {
		noun := "problems"
		if len(problems) == 1 {
			noun = "problem"
		}
		fmt.Fprintf(w, "apps.levels in %s has %d %s:\n", policyPath, len(problems), noun)
		for _, p := range problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "A level is how much intelligence a step needs. This machine turns it into each")
	fmt.Fprintln(w, "app's own model and effort; the model and effort a session actually ran on come")
	fmt.Fprintln(w, "back from the app itself. To change a row, edit apps.levels in")
	fmt.Fprintln(w, "  "+policyPath)
	fmt.Fprintln(w, "and send the worker a SIGHUP.")
}

// printAppRows writes one app: its state, its harness, and every level.
func printAppRows(w io.Writer, a apps.Info, policy *tools.Policy, policyPath string) {
	version := strings.TrimSpace(a.Version)
	if version == "" {
		version = "version unknown"
	}
	fmt.Fprintf(w, "%-12s %s\n", a.Id, version)
	fmt.Fprintf(w, "  %-10s %s\n", "state", appVerdict(a, policyPath))
	fmt.Fprintf(w, "  %-10s %s\n", "harness", harnessLine(a))

	// The table a session would resolve through: the built-in one for the
	// harness this machine drives the app with, under the owner's entries.
	override, refused := policy.AppLevels(a.Id)
	table := harness.MergeLevels(harness.BuiltinLevels(a.Harness), override)

	levels := harness.AppLevels()
	cells := make([]string, len(levels))
	width := 0
	for i, level := range levels {
		if _, bad := refused[level]; bad {
			cells[i] = "REFUSED (see the problem below)"
		} else {
			cells[i] = harness.DescribeKnobs(a.Harness, table[level])
		}
		width = max(width, len(cells[i]))
	}
	for i, level := range levels {
		label := ""
		if i == 0 {
			label = "levels"
		}
		source := ""
		if _, bad := refused[level]; !bad {
			source = "built-in"
			if _, mine := override[level]; mine {
				source = "policy.yaml"
			}
		}
		row := fmt.Sprintf("  %-10s %-11s %-*s  %s", label, level, width, cells[i], source)
		fmt.Fprintln(w, strings.TrimRight(row, " "))
	}
	fmt.Fprintf(w, "  %-10s %-11s %s\n", "", "embeddings", "never through an app")
	fmt.Fprintln(w)
}

// appVerdict says whether the cluster can send this app sessions here, and
// when it cannot, what the owner does about it. A label needs BOTH allowed
// and signed in, so each missing half is named separately.
func appVerdict(a apps.Info, policyPath string) string {
	switch {
	case a.Allowed && a.SignedIn:
		return "allowed and signed in -- the cluster can send it sessions"
	case !a.Allowed && a.SignedIn:
		return "present but BLOCKED -- add it to apps.allow in " + policyPath
	case a.Allowed && !a.SignedIn:
		return "allowed but NOT signed in -- sign in to the app itself; until then the cluster will not pick this machine"
	}
	return "present, BLOCKED and NOT signed in -- add it to apps.allow in " + policyPath + " and sign in to the app"
}

// harnessLine names the protocol this machine drives the app through and
// what that protocol can do. A harness that cannot constrain an answer says
// so, because it is the reason the engine keeps structured calls away from
// this machine.
func harnessLine(a apps.Info) string {
	var can []string
	if a.StructuredResult {
		can = append(can, "structured answers")
	} else {
		can = append(can, "no structured answers")
	}
	if a.FollowUps {
		can = append(can, "follow-up turns")
	} else {
		can = append(can, "no follow-up turns")
	}
	word := a.Harness
	if strings.TrimSpace(word) == "" {
		word = "no harness"
	}
	return word + ": " + strings.Join(can, ", ")
}
