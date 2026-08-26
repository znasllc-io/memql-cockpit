package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The Ollama probe (spec D1: the one runtime discovered natively).
//
// Two calls per model, and both are needed. /api/tags says WHAT IS
// INSTALLED and nothing about what any of it can do; /api/show is where
// the context length and the capability list live. Advertising from tags
// alone would claim a context window this side never read.

// ollamaTagsResponse is the shape of GET /api/tags.
type ollamaTagsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// ollamaShowResponse is the shape of POST /api/show.
//
// model_info is a free-form map whose keys are namespaced by architecture
// ("llama.context_length", "qwen2.context_length", ...), so the context
// length is found by suffix rather than by a key this code could enumerate
// -- a new architecture must not silently lose its context window.
type ollamaShowResponse struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
}

// probeOllama returns the models Ollama has, and a note when it has
// nothing to say.
//
// A REFUSED CONNECTION IS NOT AN ERROR. No Ollama on this machine is the
// ordinary case across most of a fleet, and it means "this machine offers
// no models" -- a fact the worker reports, not a condition it logs at warn
// level on every registration.
func (d *Discoverer) probeOllama(ctx context.Context) ([]Info, string) {
	base := d.ollamaBaseURL()

	var tags ollamaTagsResponse
	if err := d.getJSON(ctx, base+"/api/tags", "", &tags); err != nil {
		return nil, fmt.Sprintf("no Ollama at %s (%v)", base, err)
	}
	if len(tags.Models) == 0 {
		return nil, fmt.Sprintf("Ollama is running at %s but has no models pulled", base)
	}

	parallel := d.ollamaParallelism()
	out := make([]Info, 0, len(tags.Models))
	for _, m := range tags.Models {
		id := strings.TrimSpace(m.Model)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		info := Info{
			ID:      id,
			Kind:    KindOllama,
			Runtime: KindOllama,
			BaseURL: base,
			Attributes: Attributes{
				MaxConcurrent: parallel,
			},
		}
		// A /api/show that fails leaves every capability absent, which
		// costs this model eligibility for structured and embedding
		// prompts and nothing else. That is the right trade: the model
		// is still installed and still servable for free text.
		var show ollamaShowResponse
		if err := d.postJSON(ctx, base+"/api/show", map[string]string{"model": id}, &show); err == nil {
			info.ContextWindow = ollamaContextLength(show.ModelInfo)
			info.Embeddings = hasCapability(show.Capabilities, "embedding")
			info.StructuredOutput = ollamaStructuredOutput(show.Capabilities)
		}
		out = append(out, info)
	}
	return out, ""
}

// ollamaStructuredOutput decides whether to claim schema-honouring output.
//
// Ollama has no "structured output" capability of its own; it accepts a
// `format` schema for any model and how well the model HOLDS to it is a
// property of the model. `tools` is the closest honest proxy -- a model
// trained to emit a constrained tool call is a model trained to emit
// constrained JSON -- and it is the one that covers the operational class
// the spec names (llama3.1:8b, qwen2.5:7b both report it).
//
// It is deliberately conservative, per the package's fail-closed rule. An
// operator who disagrees about a specific model has a stated escape
// hatch rather than an argument with this heuristic: declare it under an
// OpenAI-compatible runtime pointed at Ollama's own /v1 surface, with
// structured_output: true. Their claim, their machine.
func ollamaStructuredOutput(capabilities []string) bool {
	return hasCapability(capabilities, "tools")
}

func hasCapability(list []string, want string) bool {
	for _, c := range list {
		if strings.EqualFold(strings.TrimSpace(c), want) {
			return true
		}
	}
	return false
}

// ollamaContextLength finds the architecture-namespaced context length.
//
// JSON numbers arrive as float64; a context window is an integer count of
// tokens, and a value that does not survive the round trip is treated as
// absent rather than truncated.
func ollamaContextLength(info map[string]any) int {
	for k, v := range info {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n > 0 && float64(int(n)) == n {
				return int(n)
			}
		case json.Number:
			if i, err := n.Int64(); err == nil && i > 0 {
				return int(i)
			}
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------------

func (d *Discoverer) getJSON(ctx context.Context, url, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return d.do(req, out)
}

func (d *Discoverer) postJSON(ctx context.Context, url string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return d.do(req, out)
}

func (d *Discoverer) do(req *http.Request, out any) error {
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
