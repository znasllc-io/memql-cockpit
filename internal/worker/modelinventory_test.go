package worker

import (
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

func offeredModel(id string, attrs models.Attributes) models.Info {
	return models.Info{
		ID: id, Kind: models.KindOllama, Runtime: models.KindOllama,
		BaseURL: "http://127.0.0.1:11434", Allowed: true, Attributes: attrs,
	}
}

func servingInventory(infos ...models.Info) models.Inventory {
	return models.Inventory{Floor: models.FloorVerdict{Met: true}, Models: infos}
}

// TestBuildRegister_CarriesTheModelInventory. The engine derives every
// `model:<id>` routing label from this message and has no other way to
// learn any of it -- it cannot dial this machine.
func TestBuildRegister_CarriesTheModelInventory(t *testing.T) {
	inv := servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 2}),
		offeredModel("nomic-embed-text", models.Attributes{ContextWindow: 2048, Embeddings: true, MaxConcurrent: 4}),
	)
	register := buildRegister(Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 8},
	}, nil, inv)

	var hasModel bool
	for _, c := range register.GetCapabilities() {
		if c == models.Capability {
			hasModel = true
		}
	}
	if !hasModel {
		t.Fatalf("capabilities = %v, want MODEL among them", register.GetCapabilities())
	}
	labels := register.GetLabels()
	if got := labels["model:llama3.1:8b"]; got != "ctx=131072,structured=1,max=2" {
		t.Errorf("model:llama3.1:8b = %q", got)
	}
	if got := labels["model:nomic-embed-text"]; got != "ctx=2048,embeddings=1,max=4" {
		t.Errorf("model:nomic-embed-text = %q", got)
	}
	if _, ok := labels["runtime:ollama"]; !ok {
		t.Errorf("labels = %v, want a runtime label", labels)
	}
	// The machine-wide ceiling is the sum of the per-model ones -- the
	// most this machine could be running if every model ran at its own
	// limit -- and the serving side enforces the same number.
	if got := register.GetConcurrency()[models.Capability]; got != 6 {
		t.Errorf("MODEL concurrency = %d, want 6", got)
	}
	if got := register.GetConcurrency()["HEADLESS"]; got != 8 {
		t.Errorf("the existing HEADLESS concurrency must survive: %d", got)
	}
}

// TestBuildRegister_NoModelsContributesNothing. A machine that advertises
// MODEL with no model labels is selected by the capability-level plan and
// then ruled out by every narrowing, which reads in the refusal report as
// a machine that ALMOST worked.
func TestBuildRegister_NoModelsContributesNothing(t *testing.T) {
	for name, inv := range map[string]models.Inventory{
		"nothing found": servingInventory(),
		"below the floor": {
			Floor:  models.FloorVerdict{Reason: "an Intel Mac is not supported as an inference machine."},
			Models: []models.Info{offeredModel("llama3.1:8b", models.Attributes{MaxConcurrent: 1})},
		},
		"everything blocked": {
			Floor: models.FloorVerdict{Met: true},
			Models: []models.Info{{
				ID: "llama3.1:8b", Kind: models.KindOllama, Allowed: false,
				Attributes: models.Attributes{MaxConcurrent: 1},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			register := buildRegister(Config{
				Name:         "test-worker",
				Capabilities: []string{"HEADLESS"},
				Labels:       map[string]string{"team": "platform"},
				Concurrency:  map[string]uint32{"HEADLESS": 8},
			}, nil, inv)

			for _, c := range register.GetCapabilities() {
				if c == models.Capability {
					t.Error("a machine offering no models must not advertise MODEL")
				}
			}
			for k := range register.GetLabels() {
				if k != "team" {
					t.Errorf("unexpected label %q", k)
				}
			}
			if _, ok := register.GetConcurrency()[models.Capability]; ok {
				t.Error("no models means no MODEL concurrency entry")
			}
		})
	}
}

// TestModelRegistration_NeverDerivesSharedInference.
//
// `sharedInference` is the OWNER's grant, read by the engine from
// operatorLabels alone -- deliberately not from the merged map, because
// `labels` is overwritten from Register on every reconnect. A cockpit
// that derived one would be claiming a permission it has no standing to
// give itself, and revoking it whenever the lid closed.
func TestModelRegistration_NeverDerivesSharedInference(t *testing.T) {
	reg := modelRegistrationFor(servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}),
	))
	for k := range reg.Labels {
		if k == "sharedInference" {
			t.Fatal("the cockpit derived a sharedInference label")
		}
	}
}

// TestMergeModelLabels_OperatorWins. If somebody hand-wrote a `model:`
// label in worker.yaml, discovery must not silently overrule it: the
// machine they were describing is the one they are standing next to.
func TestMergeModelLabels_OperatorWins(t *testing.T) {
	operator := map[string]string{"model:llama3.1:8b": "ctx=4096", "team": "platform"}
	derived := map[string]string{"model:llama3.1:8b": "ctx=131072,max=1", "runtime:ollama": ""}

	got := mergeModelLabels(operator, derived)
	if got["model:llama3.1:8b"] != "ctx=4096" {
		t.Errorf("operator label was overwritten: %q", got["model:llama3.1:8b"])
	}
	if _, ok := got["runtime:ollama"]; !ok {
		t.Error("a derived label with no operator opinion must survive")
	}
	if len(operator) != 2 {
		t.Errorf("the operator's map was mutated: %v", operator)
	}
}

// TestWithModelCapability_NoDuplicateNoMutation.
func TestWithModelCapability_NoDuplicateNoMutation(t *testing.T) {
	base := []string{"HEADLESS", models.Capability}
	got := withModelCapability(base, models.Capability)
	if len(got) != 2 {
		t.Errorf("MODEL was added twice: %v", got)
	}

	base = []string{"HEADLESS"}
	got = withModelCapability(base, models.Capability)
	if len(base) != 1 {
		t.Errorf("the caller's slice was mutated: %v", base)
	}
	if len(got) != 2 || got[1] != models.Capability {
		t.Errorf("got = %v", got)
	}
	if same := withModelCapability(base, ""); len(same) != 1 {
		t.Errorf("an empty capability must add nothing: %v", same)
	}
}

// TestAdvertisedFingerprint. The runner spends a RECONNECT on a change
// here -- the only way to re-advertise, because the engine binds labels
// at the handshake -- so the fingerprint has to be stable across
// re-discovery and sensitive to an actual change.
func TestAdvertisedFingerprint(t *testing.T) {
	a := servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}),
		offeredModel("qwen2.5:7b", models.Attributes{ContextWindow: 32768, MaxConcurrent: 1}),
	)
	// Same set, different order out of discovery.
	b := servingInventory(
		offeredModel("qwen2.5:7b", models.Attributes{ContextWindow: 32768, MaxConcurrent: 1}),
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}),
	)
	if advertisedFingerprint(a.Labels()) != advertisedFingerprint(b.Labels()) {
		t.Error("re-ordering the same models must not read as a change; it would reconnect the worker for nothing")
	}

	// An attribute change IS a change: the router selects on it.
	c := servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, StructuredOutput: true, MaxConcurrent: 1}),
		offeredModel("qwen2.5:7b", models.Attributes{ContextWindow: 32768, MaxConcurrent: 1}),
	)
	if advertisedFingerprint(a.Labels()) == advertisedFingerprint(c.Labels()) {
		t.Error("a changed capability must read as a change")
	}
	// A newly pulled model is a change.
	d := servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}))
	if advertisedFingerprint(a.Labels()) == advertisedFingerprint(d.Labels()) {
		t.Error("a removed model must read as a change")
	}
	if advertisedFingerprint(nil) != "" {
		t.Error("no models must fingerprint as the empty string")
	}
}
