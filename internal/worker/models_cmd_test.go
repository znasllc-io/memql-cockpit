package worker

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

func render(t *testing.T, inv models.Inventory) string {
	t.Helper()
	var b strings.Builder
	printModelInventory(&b, inv, "/home/op/.memql/policy.yaml")
	return b.String()
}

// The command exists to answer ONE question -- "why is my laptop not in
// the model list?" -- which from the portal has five plausible answers
// that all render identically, as an absence. Each test below is one of
// those five, and asserts the report NAMES it rather than leaving the
// operator to infer.

func TestModelsReport_BelowTheFloor(t *testing.T) {
	out := render(t, models.Inventory{
		Floor: models.FloorVerdict{
			Reason: "this machine has 8 GB of unified memory; the floor is 16 GB.",
			Detail: "apple silicon, 8 GB, macOS 15",
		},
		Models: []models.Info{offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1})},
	})
	if !strings.Contains(out, "NOT met") || !strings.Contains(out, "the floor is 16 GB") {
		t.Errorf("the floor verdict must be stated:\n%s", out)
	}
	// The installed model is STILL shown. "You have models but this
	// machine cannot serve them" and "you have no models" send an
	// operator to entirely different places.
	if !strings.Contains(out, "llama3.1:8b") {
		t.Errorf("an installed model must still be listed below the floor:\n%s", out)
	}
	if !strings.Contains(out, "below the hardware floor") {
		t.Errorf("the model's own line must say why it is not offered:\n%s", out)
	}
	if strings.Contains(out, "model:llama3.1:8b=") {
		t.Errorf("nothing may be advertised below the floor:\n%s", out)
	}
}

func TestModelsReport_NoRuntime(t *testing.T) {
	out := render(t, models.Inventory{
		Floor:      models.FloorVerdict{Met: true, Detail: "apple silicon, 32 GB, macOS 15"},
		ProbeNotes: []string{"no Ollama at http://127.0.0.1:11434 (connection refused)"},
	})
	if !strings.Contains(out, "Install Ollama") {
		t.Errorf("with no runtime the report must say what to install:\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the probe note must be shown:\n%s", out)
	}
}

func TestModelsReport_EverythingBlocked(t *testing.T) {
	blocked := offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1})
	blocked.Allowed = false
	out := render(t, models.Inventory{
		Floor:  models.FloorVerdict{Met: true},
		Models: []models.Info{blocked},
	})
	if !strings.Contains(out, "present, BLOCKED") {
		t.Errorf("a blocked model must read as blocked, not missing:\n%s", out)
	}
	if !strings.Contains(out, "/home/op/.memql/policy.yaml") {
		t.Errorf("the fix must name the file to edit:\n%s", out)
	}
	if !strings.Contains(out, "SIGHUP") {
		t.Errorf("the fix must say how to apply it without a restart:\n%s", out)
	}
}

// TestModelsReport_OfferedPrintsTheExactLabels. What the command prints
// is what the cluster is told -- so an operator comparing this against
// the Fleet page is comparing the same strings, not two renderings that
// could disagree.
func TestModelsReport_OfferedPrintsTheExactLabels(t *testing.T) {
	inv := servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 2}),
	)
	out := render(t, inv)
	if !strings.Contains(out, "model:llama3.1:8b=ctx=131072,structured=1,max=2") {
		t.Errorf("the exact label must be printed:\n%s", out)
	}
	if !strings.Contains(out, "capability MODEL, machine concurrency 2") {
		t.Errorf("the capability and ceiling must be printed:\n%s", out)
	}
	if !strings.Contains(out, "offers 1 model") {
		t.Errorf("the count must be stated:\n%s", out)
	}
}

// TestModelsReport_NamesTheAbsentCapabilities. A model that is in the
// catalog and never picked is explained by an ABSENCE, so the absences
// have to be printed too.
func TestModelsReport_NamesTheAbsentCapabilities(t *testing.T) {
	out := render(t, servingInventory(
		offeredModel("plain:7b", models.Attributes{MaxConcurrent: 1}),
	))
	if !strings.Contains(out, "structured output: not advertised") {
		t.Errorf("an absent capability must be named:\n%s", out)
	}
	if !strings.Contains(out, "context not advertised") {
		t.Errorf("an absent context window must be named:\n%s", out)
	}
}
