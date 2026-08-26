// Package models knows which local model runtimes this machine has, which
// models they can actually serve, and what to claim about each one.
//
// It exists for the same reason the apps package does: the engine cannot
// discover any of this. The cockpit dials out from behind NAT, and the
// engine reads what the cockpit reports on Register -- deriving the
// `model:<id>` routing labels the fleet router selects on (memql#4678).
// Nothing here decides whether a call happens; everything here decides
// whether this machine is ELIGIBLE to be picked.
//
// Three rules run through the whole package:
//
//   - EVERY CAPABILITY DEFAULTS TO ABSENT. The engine's attribute parser
//     is fail-closed: a model that says nothing about structured output is
//     never selected for a structured prompt. So a probe that cannot
//     establish a capability must say nothing rather than guess. The
//     failure a guess produces is not local -- a model that quietly answers
//     prose to a conductor turn produces a parse error three layers away,
//     naming nothing here.
//
//   - THE FLOOR GATES THE INVENTORY, IT DOES NOT ANNOTATE IT. A machine
//     below the hardware floor advertises no models at all. It stays a full
//     worker for everything else and nothing about it is degraded; it
//     simply is not an inference machine, and the reason is a sentence
//     rather than an absence.
//
//   - `models.allow` IS DEFAULT-DENY. Serving a call spends this machine's
//     own GPU on somebody else's prompt, so it gets the posture apps.allow
//     has: nothing is offered until the owner says which model may be. A
//     model that is present but unlisted is REPORTED as blocked rather than
//     omitted -- "present, blocked" is a state an operator can fix, and
//     rendering it identically to "not installed" sends them hunting the
//     wrong problem.
package models

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The wire contract, mirrored from the engine.
//
// These are MIRRORED rather than imported for the reason apps.go states
// about the app id set: cockpit resolves four memql packages, none of them
// `component/worker`, and the attribute encoding itself lives behind the
// engine's `agent` build tag where this module cannot reach it at all.
// The definitions are in memql `component/worker/modelcall.go` and
// `integrations/agent/worker/model_routing.go`; TestWireContract pins this
// copy against the exact strings those files parse.
const (
	// Capability is what a machine must advertise before the router will
	// consider it for a model call at all.
	Capability = "MODEL"
	// LabelPrefix is how a machine advertises a model it will serve.
	LabelPrefix = "model:"
	// RuntimeLabelPrefix names a runtime present on the machine. It
	// steers no selection -- two machines serving the same model through
	// different runtimes are interchangeable to a caller -- and exists
	// for the operator reading the Fleet page.
	RuntimeLabelPrefix = "runtime:"
)

// The runtime kinds this cockpit serves. Closed set (spec D1): Ollama
// discovered natively, and any OpenAI-compatible endpoint the machine's
// policy.yaml declares -- which covers LM Studio, vLLM and llamafile
// without a per-vendor integration for each.
const (
	KindOllama           = "ollama"
	KindOpenAICompatible = "openai-compatible"
)

// Attribute keys inside a `model:<id>` label value.
const (
	attrContext    = "ctx"
	attrStructured = "structured"
	attrEmbeddings = "embeddings"
	attrMax        = "max"
)

// Attributes is what this machine claims about ONE model.
//
// The encoding is a flat `k=v` list rather than JSON, and that is the
// engine's choice rather than ours to improve on: labels are
// map[string]string end to end -- the concept, the wire, the Fleet page --
// and a JSON blob inside one of them is unreadable in every surface that
// renders labels as text.
type Attributes struct {
	// ContextWindow in tokens. Zero means this machine did not say, which
	// meets no floor -- the fail-closed direction.
	ContextWindow int
	// StructuredOutput reports that the runtime can honour a response
	// schema for this model.
	StructuredOutput bool
	// Embeddings reports that the model produces vectors.
	Embeddings bool
	// MaxConcurrent is the per-model ceiling. Zero means none declared,
	// which the engine's load ordering reads as unlimited.
	MaxConcurrent int
}

// String renders the label value the engine parses. Keys are emitted in a
// fixed order and a zero-valued attribute is omitted entirely, so the same
// inventory always produces byte-identical labels -- an unstable rendering
// would rewrite the registration row on every reconnect for no change.
func (a Attributes) String() string {
	parts := make([]string, 0, 4)
	if a.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrContext, a.ContextWindow))
	}
	if a.StructuredOutput {
		parts = append(parts, attrStructured+"=1")
	}
	if a.Embeddings {
		parts = append(parts, attrEmbeddings+"=1")
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrMax, a.MaxConcurrent))
	}
	return strings.Join(parts, ",")
}

// ParseAttributes reads a label value back. It exists so the round-trip
// against the engine's reading is assertable here rather than discovered
// in production, and it is deliberately the same shape as the engine's
// parser: unrecognised input costs eligibility rather than granting it.
func ParseAttributes(value string) Attributes {
	var a Attributes
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case attrContext:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.ContextWindow = n
			}
		case attrStructured:
			a.StructuredOutput = parseAdvertisedBool(v)
		case attrEmbeddings:
			a.Embeddings = parseAdvertisedBool(v)
		case attrMax:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.MaxConcurrent = n
			}
		}
	}
	return a
}

// parseAdvertisedBool mirrors the engine's permissiveness, which runs in
// exactly one direction: anything unrecognised is false, so a novel
// spelling costs eligibility rather than granting it.
func parseAdvertisedBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// Label renders the advertisement label for a model id.
func Label(modelId string) string { return LabelPrefix + strings.TrimSpace(modelId) }

// RuntimeLabel renders the advertisement label for a runtime kind.
func RuntimeLabel(kind string) string { return RuntimeLabelPrefix + strings.TrimSpace(kind) }

// Info is one model this machine found, and everything needed to serve a
// call against it.
//
// Allowed is this machine's own policy.yaml verdict and is separate from
// the attributes on purpose: a blocked model is still reported, so the
// portal can distinguish it from one that is not installed.
type Info struct {
	// ID is the runtime-facing model id -- the exact string the router
	// will select on and send back in ModelCallStart.model, so it is
	// never normalised or prettified here.
	ID string
	// Kind is the runtime family: KindOllama or KindOpenAICompatible.
	Kind string
	// Runtime is the operator-facing runtime name. "ollama" for the
	// native probe; whatever policy.yaml called it for a declared one.
	// It is NOT what goes in the runtime label -- the engine's set is
	// the kinds -- and exists so `memql worker models` can tell two
	// declared endpoints apart.
	Runtime string
	// BaseURL is where a call to this model goes.
	BaseURL string
	// APIKeyEnv names the environment variable holding this runtime's
	// bearer, when it needs one. The VALUE is never stored on this
	// struct: Info is printed by the CLI and logged, and a struct that
	// holds a secret eventually prints one.
	APIKeyEnv string
	// Allowed mirrors policy.yaml models.allow.
	Allowed bool

	Attributes
}

// Inventory is the whole answer to "what does this machine serve".
type Inventory struct {
	// Floor is the hardware verdict. When it is not met, Advertised is
	// empty no matter what Models holds.
	Floor FloorVerdict
	// Models is everything found, allowed or not, sorted by id. It is
	// what the diagnostic prints.
	Models []Info
	// ProbeNotes records why a runtime contributed nothing -- "no
	// response from Ollama at ...", "declared runtime lmstudio refused
	// the connection". Absence of a runtime is not an error and is not
	// logged as one; it is a sentence an operator can read when they ask.
	ProbeNotes []string
}

// Advertised is the set this machine actually offers: allowed models, and
// only when the hardware floor is met.
func (inv Inventory) Advertised() []Info {
	if !inv.Floor.Met {
		return nil
	}
	out := make([]Info, 0, len(inv.Models))
	for _, m := range inv.Models {
		if m.Allowed {
			out = append(out, m)
		}
	}
	return out
}

// Labels renders the registration labels for the advertised set: one
// `model:<id>` per model and one `runtime:<kind>` per kind present.
//
// Returns nil when nothing is advertised, so a machine that offers no
// models sends no model labels at all rather than an empty marker.
func (inv Inventory) Labels() map[string]string {
	advertised := inv.Advertised()
	if len(advertised) == 0 {
		return nil
	}
	out := make(map[string]string, len(advertised)+2)
	for _, m := range advertised {
		out[Label(m.ID)] = m.Attributes.String()
		out[RuntimeLabel(m.Kind)] = ""
	}
	return out
}

// RuntimeKinds lists the kinds behind the advertised set, sorted.
func (inv Inventory) RuntimeKinds() []string {
	seen := map[string]bool{}
	for _, m := range inv.Advertised() {
		seen[m.Kind] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Find returns the advertised model with this id. A call for a model this
// machine does not currently offer is refused rather than served: the
// advertisement is the promise, and honouring a call outside it would let
// a model removed from models.allow keep serving until the next reconnect.
func (inv Inventory) Find(modelId string) (Info, bool) {
	for _, m := range inv.Advertised() {
		if m.ID == modelId {
			return m, true
		}
	}
	return Info{}, false
}
