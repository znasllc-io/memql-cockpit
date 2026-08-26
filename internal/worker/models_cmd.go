package worker

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// `memql worker models` (memql-cockpit#363).
//
// It answers ONE question, and the question is the reason the command
// exists: *why is my laptop not in the model list?* From the portal that
// has five plausible answers -- below the hardware floor, no runtime
// installed, not in models.allow, not signed in, simply asleep -- and all
// five render identically, as an absence. Only the machine can tell them
// apart, so the answer is printed here.
//
// It runs the SAME discovery the worker runs, so what it prints is what
// the cluster would be told. A second implementation could disagree with
// the first, and the operator would have no way to know which one lied.

const modelsProbeTimeout = 15 * time.Second

func handleModels(args []string) {
	fs := flag.NewFlagSet("worker models", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml")
	_ = fs.Parse(args)

	policyPath := filepath.Join(filepath.Dir(*configPath), "policy.yaml")
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsProbeTimeout)
	defer cancel()

	// Probe rather than Discover: an operator below the floor still wants
	// to see that they have three models pulled, because "you have models
	// but this machine cannot serve them" and "you have no models" send
	// them to entirely different places.
	inv := (&models.Discoverer{}).Probe(ctx, models.Request{
		Allow:    policy.ModelsAllow(),
		Runtimes: policy.ModelRuntimes(),
	})
	printModelInventory(os.Stdout, inv, policyPath)
}

// printModelInventory writes the report. Split out so the shape is
// assertable without a machine that has Ollama on it.
func printModelInventory(w io.Writer, inv models.Inventory, policyPath string) {
	fmt.Fprintln(w, "Local models on this machine")
	fmt.Fprintln(w, "")

	fmt.Fprintf(w, "Hardware floor: %s\n", floorLine(inv.Floor))
	if inv.Floor.Detail != "" {
		fmt.Fprintf(w, "                %s\n", inv.Floor.Detail)
	}
	fmt.Fprintln(w, "")

	if len(inv.Models) == 0 {
		fmt.Fprintln(w, "Runtimes: none found.")
	} else {
		fmt.Fprintln(w, "Models found:")
		for _, m := range inv.Models {
			fmt.Fprintf(w, "  %-32s %s\n", m.ID, modelVerdict(m, inv.Floor))
			fmt.Fprintf(w, "  %-32s runtime %s (%s) at %s\n", "", m.Runtime, m.Kind, m.BaseURL)
			fmt.Fprintf(w, "  %-32s %s\n", "", attributeLine(m.Attributes))
		}
	}

	if len(inv.ProbeNotes) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Notes:")
		for _, n := range inv.ProbeNotes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}

	advertised := inv.Advertised()
	fmt.Fprintln(w, "")
	if len(advertised) == 0 {
		fmt.Fprintf(w, "This machine offers no models. %s\n", whyNothing(inv, policyPath))
		return
	}

	fmt.Fprintf(w, "This machine offers %d model(s). Registration labels:\n", len(advertised))
	labels := inv.Labels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := labels[k]; v != "" {
			fmt.Fprintf(w, "  %s=%s\n", k, v)
		} else {
			fmt.Fprintf(w, "  %s\n", k)
		}
	}
	fmt.Fprintf(w, "  capability %s, machine concurrency %d\n",
		models.Capability, modelRegistrationFor(inv).Concurrency)
}

func floorLine(v models.FloorVerdict) string {
	if v.Met {
		return "met"
	}
	return "NOT met -- " + v.Reason
}

func modelVerdict(m models.Info, floor models.FloorVerdict) string {
	switch {
	case !floor.Met:
		return "not offered (machine is below the hardware floor)"
	case !m.Allowed:
		return "present, BLOCKED (not in policy.yaml models.allow)"
	default:
		return "offered"
	}
}

// attributeLine renders what would be claimed, naming the ABSENCES too.
// A capability this machine does not advertise is why a model is passed
// over for a structured prompt, and "structured output: not advertised"
// is the sentence that explains a machine which is in the catalog and
// still never picked.
func attributeLine(a models.Attributes) string {
	parts := []string{}
	if a.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("context %d", a.ContextWindow))
	} else {
		parts = append(parts, "context not advertised")
	}
	if a.StructuredOutput {
		parts = append(parts, "structured output")
	} else {
		parts = append(parts, "structured output: not advertised")
	}
	if a.Embeddings {
		parts = append(parts, "embeddings")
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("max %d concurrent", a.MaxConcurrent))
	}
	return strings.Join(parts, ", ")
}

// whyNothing names the actual reason rather than leaving the operator to
// infer one, which is the whole point of the command.
func whyNothing(inv models.Inventory, policyPath string) string {
	if !inv.Floor.Met {
		return inv.Floor.Reason
	}
	if len(inv.Models) == 0 {
		return "No model runtime answered. Install Ollama, or declare an OpenAI-compatible endpoint under models.runtimes in " + policyPath + "."
	}
	return "Every model found is blocked. Add the ones this machine should serve to models.allow in " + policyPath + ", then send the worker a SIGHUP."
}
