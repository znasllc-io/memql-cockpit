package models

import "testing"

// TestWireContract pins the mirrored constants as LITERALS.
//
// They are not asserted against an import of the engine's own
// definitions, and that is a deliberate trade rather than an oversight.
// component/worker is a nested module of its own in the memql split
// (memql#3228); requiring it here for three strings would put cockpit's
// build graph behind a module that is still moving, which is the exact
// fragility .github/memql-pin exists to contain. So this repo does what
// apps.go already does with the engine's app id set: mirror, and name the
// source.
//
// Sources: `ModelCapability`, `ModelLabelPrefix` and `RuntimeLabelPrefix`
// in memql component/worker/modelcall.go.
func TestWireContract(t *testing.T) {
	if Capability != "MODEL" {
		t.Errorf("Capability = %q, engine says %q", Capability, "MODEL")
	}
	if LabelPrefix != "model:" {
		t.Errorf("LabelPrefix = %q, engine says %q", LabelPrefix, "model:")
	}
	if RuntimeLabelPrefix != "runtime:" {
		t.Errorf("RuntimeLabelPrefix = %q, engine says %q", RuntimeLabelPrefix, "runtime:")
	}
	if got := Label("llama3.1:8b"); got != "model:llama3.1:8b" {
		t.Errorf("Label = %q, want %q", got, "model:llama3.1:8b")
	}
	if got := RuntimeLabel(KindOpenAICompatible); got != "runtime:openai-compatible" {
		t.Errorf("RuntimeLabel = %q, want %q", got, "runtime:openai-compatible")
	}
}

// TestAttributesString pins the exact label VALUE the engine parses.
//
// The encoding lives in the engine's integrations/agent/worker/
// model_routing.go, which is behind its `agent` build tag and therefore
// out of reach of an import. These literals are that file's
// ModelAttributes.String, transcribed -- so a change there fails here
// with a diff an operator can read rather than with a machine that is in
// the catalog and never picked.
func TestAttributesString(t *testing.T) {
	tests := []struct {
		name string
		in   Attributes
		want string
	}{
		{"everything", Attributes{ContextWindow: 131072, StructuredOutput: true, Embeddings: true, MaxConcurrent: 2},
			"ctx=131072,structured=1,embeddings=1,max=2"},
		{"context only", Attributes{ContextWindow: 8192}, "ctx=8192"},
		{"embeddings model", Attributes{ContextWindow: 512, Embeddings: true, MaxConcurrent: 4},
			"ctx=512,embeddings=1,max=4"},
		// The fail-closed direction, rendered: a model this machine can
		// say nothing about produces an EMPTY value, not a value full of
		// zeroes and falses. The engine reads both the same way; the
		// empty one is what says so honestly.
		{"nothing known", Attributes{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := ParseAttributes(tt.want); got != tt.in {
				t.Errorf("round trip = %+v, want %+v", got, tt.in)
			}
		})
	}
}

// TestParseAttributes_FailsClosed. Every unreadable input costs
// eligibility rather than granting it -- the direction that turns a
// garbled label into "this machine is not picked for structured prompts"
// instead of "this machine answers prose to a conductor turn".
func TestParseAttributes_FailsClosed(t *testing.T) {
	for _, in := range []string{
		"ctx=notanumber,structured=maybe,embeddings=perhaps,max=lots",
		"ctx=-1,max=0",
		"garbage",
		"",
		"structured", // no '=' at all
	} {
		if got := ParseAttributes(in); got != (Attributes{}) {
			t.Errorf("ParseAttributes(%q) = %+v, want the zero value", in, got)
		}
	}
	// The permissive spellings the engine accepts, and only those.
	for _, yes := range []string{"1", "true", "TRUE", "yes", "y", "Y"} {
		if !ParseAttributes("structured=" + yes).StructuredOutput {
			t.Errorf("structured=%s should parse true", yes)
		}
	}
	for _, no := range []string{"0", "false", "no", "on", "enabled", "sure"} {
		if ParseAttributes("structured=" + no).StructuredOutput {
			t.Errorf("structured=%s must NOT parse true", no)
		}
	}
}

func offered(id string, attrs Attributes) Info {
	return Info{ID: id, Kind: KindOllama, Runtime: KindOllama, BaseURL: "http://x", Allowed: true, Attributes: attrs}
}

// TestAdvertised_FloorGatesEverything. Below the floor a machine offers
// nothing, however much it has installed and however much of it the owner
// allowed. The floor gates the inventory; it does not annotate it.
func TestAdvertised_FloorGatesEverything(t *testing.T) {
	inv := Inventory{
		Floor:  FloorVerdict{Met: false, Reason: "an Intel Mac is not supported as an inference machine."},
		Models: []Info{offered("llama3.1:8b", Attributes{ContextWindow: 8192})},
	}
	if got := inv.Advertised(); len(got) != 0 {
		t.Errorf("Advertised() = %v below the floor, want none", got)
	}
	if got := inv.Labels(); got != nil {
		t.Errorf("Labels() = %v below the floor, want nil", got)
	}
	if _, ok := inv.Find("llama3.1:8b"); ok {
		t.Error("Find() resolved a model below the floor")
	}
}

// TestAdvertised_BlockedIsReportedButNotOffered. models.allow is
// default-deny, and a blocked model stays VISIBLE -- that is what lets
// the portal say "present, blocked" rather than rendering it identically
// to "not installed".
func TestAdvertised_BlockedIsReportedButNotOffered(t *testing.T) {
	blocked := offered("qwen2.5:7b", Attributes{ContextWindow: 32768})
	blocked.Allowed = false
	inv := Inventory{
		Floor:  FloorVerdict{Met: true},
		Models: []Info{offered("llama3.1:8b", Attributes{ContextWindow: 8192, MaxConcurrent: 1}), blocked},
	}
	if len(inv.Models) != 2 {
		t.Fatalf("the blocked model must still be reported: %v", inv.Models)
	}
	adv := inv.Advertised()
	if len(adv) != 1 || adv[0].ID != "llama3.1:8b" {
		t.Fatalf("Advertised() = %v, want only the allowed model", adv)
	}
	if _, ok := inv.Find("qwen2.5:7b"); ok {
		t.Error("Find() resolved a blocked model; a call for it must be refused")
	}
}

// TestLabels_ShapeAndRuntimes.
func TestLabels_ShapeAndRuntimes(t *testing.T) {
	inv := Inventory{
		Floor: FloorVerdict{Met: true},
		Models: []Info{
			offered("llama3.1:8b", Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 1}),
			{ID: "qwen2.5-7b", Kind: KindOpenAICompatible, Runtime: "lmstudio", Allowed: true,
				Attributes: Attributes{ContextWindow: 32768, StructuredOutput: true, MaxConcurrent: 1}},
		},
	}
	labels := inv.Labels()
	if got := labels["model:llama3.1:8b"]; got != "ctx=131072,structured=1,max=1" {
		t.Errorf("model label = %q", got)
	}
	if _, ok := labels["runtime:ollama"]; !ok {
		t.Error("missing runtime:ollama")
	}
	if _, ok := labels["runtime:openai-compatible"]; !ok {
		t.Error("missing runtime:openai-compatible")
	}
	if got := inv.RuntimeKinds(); len(got) != 2 || got[0] != KindOllama || got[1] != KindOpenAICompatible {
		t.Errorf("RuntimeKinds() = %v", got)
	}
}
