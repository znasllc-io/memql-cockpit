package models

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds one runtime probe. Discovery runs on the Register
// path and again whenever the worker re-checks its inventory; a runtime
// that accepts a connection and then says nothing must not hold the
// worker's registration behind it.
const probeTimeout = 5 * time.Second

// DeclaredRuntime is an OpenAI-compatible endpoint the machine's
// policy.yaml declares (spec D1: LM Studio, vLLM, llamafile, and Ollama's
// own /v1 surface all reach the cockpit this way).
type DeclaredRuntime struct {
	Name      string          `yaml:"name"`
	BaseURL   string          `yaml:"base_url"`
	APIKeyEnv string          `yaml:"api_key_env"`
	Models    []DeclaredModel `yaml:"models"`
}

// DeclaredModel is one model on a declared runtime, with the attributes
// the operator states.
//
// THEY ARE STATED RATHER THAN PROBED, and that is not laziness. An
// OpenAI-compatible `/v1/models` returns ids and nothing else -- no
// context length, no capability flag. A capability the machine cannot
// observe has to be declared or left absent; inferring one from a model
// name would be a guess that fails at parse time, three layers from here.
type DeclaredModel struct {
	ID               string `yaml:"id"`
	ContextWindow    int    `yaml:"context_window"`
	StructuredOutput bool   `yaml:"structured_output"`
	Embeddings       bool   `yaml:"embeddings"`
	MaxConcurrent    int    `yaml:"max_concurrent"`
}

// Request is what discovery is asked to consider.
type Request struct {
	// Allow is policy.yaml models.allow. Empty offers nothing.
	Allow []string
	// Runtimes are the declared OpenAI-compatible endpoints.
	Runtimes []DeclaredRuntime
}

// Discoverer probes this machine. Every external fact is a seam, so the
// whole package is exercisable without Ollama, without a GPU and without
// a network -- which matters because CI has none of the three, and a
// discovery that can only be driven by hand is one nobody drives.
type Discoverer struct {
	// HTTPClient defaults to a client with probeTimeout.
	HTTPClient *http.Client
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// OllamaBaseURL overrides where Ollama is looked for. Empty resolves
	// OLLAMA_HOST, then the documented default.
	OllamaBaseURL string
	// FloorFn defaults to Floor.
	FloorFn func() FloorVerdict
}

// DefaultOllamaBaseURL is where Ollama listens unless told otherwise.
const DefaultOllamaBaseURL = "http://127.0.0.1:11434"

func (d *Discoverer) getenv(k string) string {
	if d != nil && d.Getenv != nil {
		return d.Getenv(k)
	}
	return os.Getenv(k)
}

func (d *Discoverer) client() *http.Client {
	if d != nil && d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: probeTimeout}
}

func (d *Discoverer) floor() FloorVerdict {
	if d != nil && d.FloorFn != nil {
		return d.FloorFn()
	}
	return Floor()
}

// Discover returns what this machine offers.
//
// Below the hardware floor it probes NOTHING and returns the verdict
// alone. That is the cheap direction as well as the correct one: this
// runs on every registration, and a machine that can never serve a model
// has no reason to reach for a runtime socket to prove it.
func (d *Discoverer) Discover(ctx context.Context, req Request) Inventory {
	floor := d.floor()
	if !floor.Met {
		return Inventory{Floor: floor}
	}
	return d.Probe(ctx, req)
}

// Probe runs discovery WITHOUT the floor gate, reporting the verdict
// alongside what it found.
//
// `memql worker models` uses this: an operator below the floor still
// wants to see that they have Ollama with three models pulled, because
// "you have models but this machine cannot serve them" and "you have no
// models" send them to entirely different places. Inventory.Advertised
// still returns nothing, so nothing here can leak into a registration.
func (d *Discoverer) Probe(ctx context.Context, req Request) Inventory {
	inv := Inventory{Floor: d.floor()}

	allow := make(map[string]bool, len(req.Allow))
	for _, a := range req.Allow {
		if id := strings.TrimSpace(a); id != "" {
			allow[id] = true
		}
	}

	found, note := d.probeOllama(ctx)
	if note != "" {
		inv.ProbeNotes = append(inv.ProbeNotes, note)
	}
	inv.Models = append(inv.Models, found...)

	for _, rt := range req.Runtimes {
		got, n := d.probeDeclared(ctx, rt)
		if n != "" {
			inv.ProbeNotes = append(inv.ProbeNotes, n)
		}
		inv.Models = append(inv.Models, got...)
	}

	// The allow verdict is applied last and to everything, so a model
	// present on two runtimes gets the same answer on both.
	for i := range inv.Models {
		inv.Models[i].Allowed = allow[inv.Models[i].ID]
	}

	// Sorted by id, then runtime, so the same machine produces the same
	// labels every time. An unstable order would rewrite the registration
	// row on every reconnect for no actual change.
	sort.Slice(inv.Models, func(i, j int) bool {
		if inv.Models[i].ID != inv.Models[j].ID {
			return inv.Models[i].ID < inv.Models[j].ID
		}
		return inv.Models[i].Runtime < inv.Models[j].Runtime
	})
	inv.Models, inv.ProbeNotes = resolveDuplicates(inv.Models, inv.ProbeNotes)
	return inv
}

// resolveDuplicates keeps ONE entry per model id.
//
// A model id is a label KEY -- `model:<id>` -- so two runtimes offering
// the same id cannot both be advertised: the second silently overwrites
// the first's attributes, and Find would then hand a call to whichever
// entry sorted first. The machine would be telling the cluster one
// context window and serving from a runtime with another, and nothing
// anywhere would report a conflict.
//
// A DECLARED runtime wins over the native Ollama probe. The operator went
// out of their way to write that entry down, and it is the documented way
// to correct what the probe inferred -- declaring a model against Ollama's
// own /v1 surface with structured_output: true is how you overrule the
// `tools` heuristic. If the auto-discovered entry shadowed it, that escape
// hatch would quietly not work.
func resolveDuplicates(in []Info, notes []string) ([]Info, []string) {
	out := make([]Info, 0, len(in))
	for i := 0; i < len(in); {
		j := i
		for j < len(in) && in[j].ID == in[i].ID {
			j++
		}
		group := in[i:j]
		i = j
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		pick := 0
		for k, m := range group {
			if m.Kind == KindOpenAICompatible {
				pick = k
				break
			}
		}
		out = append(out, group[pick])
		for k, m := range group {
			if k == pick {
				continue
			}
			notes = append(notes, fmt.Sprintf(
				"model %s is offered by both %s and %s; serving it from %s, because a model id can be advertised once",
				m.ID, group[pick].Runtime, m.Runtime, group[pick].Runtime))
		}
	}
	return out, notes
}

// ollamaBaseURL resolves where to look for Ollama.
//
// OLLAMA_HOST is what Ollama's own client reads, and it is documented in
// three shapes -- "host:port", "http://host:port" and a bare host -- so
// all three are accepted here rather than the one this code would prefer.
func (d *Discoverer) ollamaBaseURL() string {
	if d != nil && d.OllamaBaseURL != "" {
		return strings.TrimRight(d.OllamaBaseURL, "/")
	}
	host := strings.TrimSpace(d.getenv("OLLAMA_HOST"))
	if host == "" {
		return DefaultOllamaBaseURL
	}
	if !strings.Contains(host, "://") {
		if !strings.Contains(host, ":") {
			host += ":11434"
		}
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// ollamaParallelism is the per-model concurrency this machine will claim.
//
// ZERO IS NOT AN OPTION HERE, and this is the one place in the package
// where absent is the PERMISSIVE direction rather than the safe one: the
// engine reads a missing `max=` as unlimited. Left unset, a laptop with a
// single 8B model would be handed as many concurrent generations as the
// router had work for. So a ceiling is always declared -- OLLAMA_NUM_PARALLEL
// when the operator set one, and otherwise one, which is what Ollama itself
// falls back to when it cannot fit more.
func (d *Discoverer) ollamaParallelism() int {
	if v := strings.TrimSpace(d.getenv("OLLAMA_NUM_PARALLEL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}
