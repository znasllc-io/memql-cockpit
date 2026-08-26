package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func metFloor() FloorVerdict { return FloorVerdict{Met: true, Detail: "test"} }

// ollamaStub serves /api/tags and /api/show. show maps a model id to the
// JSON body /api/show returns for it; a model missing from the map gets a
// 404, which is how a real Ollama answers for a model that vanished
// between the two calls.
func ollamaStub(t *testing.T, tags []string, show map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var body struct {
			Models []map[string]string `json:"models"`
		}
		for _, id := range tags {
			body.Models = append(body.Models, map[string]string{"name": id, "model": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, ok := show[req.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(raw))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func discovererFor(url string, env map[string]string) *Discoverer {
	return &Discoverer{
		OllamaBaseURL: url,
		FloorFn:       metFloor,
		Getenv:        func(k string) string { return env[k] },
	}
}

// TestProbeOllama_FullAttributes: the happy path, and the shape the
// engine will parse out of it.
func TestProbeOllama_FullAttributes(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"llama3.1:8b"}, map[string]string{
		"llama3.1:8b": `{"capabilities":["completion","tools"],"model_info":{"llama.context_length":131072}}`,
	})
	d := discovererFor(srv.URL, map[string]string{"OLLAMA_NUM_PARALLEL": "2"})

	inv := d.Probe(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
	got := inv.Models[0]
	if got.ID != "llama3.1:8b" || got.Kind != KindOllama || got.BaseURL != srv.URL {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.ContextWindow != 131072 || !got.StructuredOutput || got.Embeddings || got.MaxConcurrent != 2 {
		t.Errorf("attributes = %+v", got.Attributes)
	}
	if !got.Allowed {
		t.Error("an allowed model must be marked allowed")
	}
	if want := "ctx=131072,structured=1,max=2"; inv.Labels()["model:llama3.1:8b"] != want {
		t.Errorf("label = %q, want %q", inv.Labels()["model:llama3.1:8b"], want)
	}
}

// TestProbeOllama_CapabilitiesFailClosed. A model that does not report
// tools is not claimed for structured output, and an embeddings model is
// claimed only for embeddings. The direction is the point: an
// over-claimed capability fails three layers away with a parse error that
// names nothing here.
func TestProbeOllama_CapabilitiesFailClosed(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"plain:7b", "nomic-embed-text", "mystery:1b"}, map[string]string{
		"plain:7b":         `{"capabilities":["completion"],"model_info":{"llama.context_length":8192}}`,
		"nomic-embed-text": `{"capabilities":["embedding"],"model_info":{"nomic-bert.context_length":2048}}`,
		// mystery is absent from show entirely: /api/show 404s.
	})
	d := discovererFor(srv.URL, nil)
	inv := d.Probe(context.Background(), Request{Allow: []string{"plain:7b", "nomic-embed-text", "mystery:1b"}})

	byID := map[string]Info{}
	for _, m := range inv.Models {
		byID[m.ID] = m
	}
	if got := byID["plain:7b"]; got.StructuredOutput {
		t.Error("a model without the tools capability must not claim structured output")
	}
	if got := byID["nomic-embed-text"]; !got.Embeddings || got.StructuredOutput {
		t.Errorf("embeddings model = %+v", got.Attributes)
	}
	// A show that failed still leaves the model SERVABLE for free text --
	// it is installed, and dropping it would cost more than it saves.
	m, ok := byID["mystery:1b"]
	if !ok {
		t.Fatal("a model whose /api/show failed must still be reported")
	}
	if m.ContextWindow != 0 || m.StructuredOutput || m.Embeddings {
		t.Errorf("an unreadable show must leave every capability absent: %+v", m.Attributes)
	}
	if m.MaxConcurrent == 0 {
		t.Error("max concurrent must always be declared; absent means UNLIMITED to the engine")
	}
}

// TestProbeOllama_AbsentIsNotAnError. No Ollama is the ordinary case
// across most of a fleet.
func TestProbeOllama_AbsentIsNotAnError(t *testing.T) {
	// A port nothing is listening on.
	srv, _ := ollamaStub(t, nil, nil)
	dead := srv.URL
	srv.Close()

	d := discovererFor(dead, nil)
	inv := d.Probe(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 0 {
		t.Errorf("Models = %+v, want none", inv.Models)
	}
	if len(inv.ProbeNotes) != 1 || !strings.Contains(inv.ProbeNotes[0], "no Ollama at") {
		t.Errorf("ProbeNotes = %v, want one note naming the endpoint", inv.ProbeNotes)
	}
}

func TestProbeOllama_RunningButEmpty(t *testing.T) {
	srv, _ := ollamaStub(t, nil, nil)
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{})
	if len(inv.ProbeNotes) != 1 || !strings.Contains(inv.ProbeNotes[0], "no models pulled") {
		t.Errorf("ProbeNotes = %v", inv.ProbeNotes)
	}
}

// TestOllamaBaseURL. All three documented shapes of OLLAMA_HOST, because
// an operator who set it the way Ollama's own docs show must not have
// their machine silently offer nothing.
func TestOllamaBaseURL(t *testing.T) {
	tests := map[string]string{
		"":                       DefaultOllamaBaseURL,
		"127.0.0.1:11434":        "http://127.0.0.1:11434",
		"http://gpu-box:11434":   "http://gpu-box:11434",
		"gpu-box":                "http://gpu-box:11434",
		"https://gpu-box:11434/": "https://gpu-box:11434",
	}
	for host, want := range tests {
		d := &Discoverer{Getenv: func(k string) string {
			if k == "OLLAMA_HOST" {
				return host
			}
			return ""
		}}
		if got := d.ollamaBaseURL(); got != want {
			t.Errorf("OLLAMA_HOST=%q -> %q, want %q", host, got, want)
		}
	}
}

// TestOllamaParallelism_NeverZero. This is the one attribute whose
// absence is PERMISSIVE on the engine side, so it is never left absent.
func TestOllamaParallelism_NeverZero(t *testing.T) {
	for env, want := range map[string]int{"": 1, "0": 1, "-3": 1, "nonsense": 1, "4": 4} {
		d := &Discoverer{Getenv: func(k string) string {
			if k == "OLLAMA_NUM_PARALLEL" {
				return env
			}
			return ""
		}}
		if got := d.ollamaParallelism(); got != want {
			t.Errorf("OLLAMA_NUM_PARALLEL=%q -> %d, want %d", env, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Declared OpenAI-compatible runtimes
// -----------------------------------------------------------------------------

func openAIStub(t *testing.T, served []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data []map[string]string `json:"data"`
		}
		for _, id := range served {
			body.Data = append(body.Data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeDeclared_AttributesRideThrough(t *testing.T) {
	api := openAIStub(t, []string{"qwen2.5-7b-instruct"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	d := discovererFor(dead.URL, nil)
	inv := d.Probe(context.Background(), Request{
		Allow: []string{"qwen2.5-7b-instruct"},
		Runtimes: []DeclaredRuntime{{
			Name:    "lmstudio",
			BaseURL: api.URL,
			Models: []DeclaredModel{{
				ID: "qwen2.5-7b-instruct", ContextWindow: 32768, StructuredOutput: true, MaxConcurrent: 3,
			}},
		}},
	})
	if len(inv.Models) != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
	got := inv.Models[0]
	if got.Kind != KindOpenAICompatible || got.Runtime != "lmstudio" || got.BaseURL != api.URL {
		t.Errorf("identity = %+v", got)
	}
	if got.ContextWindow != 32768 || !got.StructuredOutput || got.MaxConcurrent != 3 {
		t.Errorf("declared attributes did not ride through: %+v", got.Attributes)
	}
}

// TestProbeDeclared_MaxConcurrentDefaultsToOne. Silence must not reach
// the engine as "unlimited".
func TestProbeDeclared_MaxConcurrentDefaultsToOne(t *testing.T) {
	api := openAIStub(t, []string{"m"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()
	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"m"},
		Runtimes: []DeclaredRuntime{{Name: "vllm", BaseURL: api.URL, Models: []DeclaredModel{{ID: "m"}}}},
	})
	if len(inv.Models) != 1 || inv.Models[0].MaxConcurrent != 1 {
		t.Fatalf("Models = %+v", inv.Models)
	}
}

// TestProbeDeclared_DownEndpointOffersNothing. Advertising a model whose
// server is down means the router picks this machine, sends somebody's
// prompt, and the call fails at the last hop.
func TestProbeDeclared_DownEndpointOffersNothing(t *testing.T) {
	api := openAIStub(t, []string{"m"})
	url := api.URL
	api.Close()
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"m"},
		Runtimes: []DeclaredRuntime{{Name: "vllm", BaseURL: url, Models: []DeclaredModel{{ID: "m"}}}},
	})
	if len(inv.Models) != 0 {
		t.Fatalf("Models = %+v, want none", inv.Models)
	}
	if !strings.Contains(strings.Join(inv.ProbeNotes, " "), "did not answer") {
		t.Errorf("ProbeNotes = %v", inv.ProbeNotes)
	}
}

// TestProbeDeclared_NotCurrentlyServed. A model the operator declared but
// the endpoint is not serving right now is held back, and SAID so.
func TestProbeDeclared_NotCurrentlyServed(t *testing.T) {
	api := openAIStub(t, []string{"loaded"})
	dead, _ := ollamaStub(t, nil, nil)
	dead.Close()

	inv := discovererFor(dead.URL, nil).Probe(context.Background(), Request{
		Allow: []string{"loaded", "unloaded"},
		Runtimes: []DeclaredRuntime{{Name: "lmstudio", BaseURL: api.URL, Models: []DeclaredModel{
			{ID: "loaded"}, {ID: "unloaded"},
		}}},
	})
	if len(inv.Models) != 1 || inv.Models[0].ID != "loaded" {
		t.Fatalf("Models = %+v", inv.Models)
	}
	if !strings.Contains(strings.Join(inv.ProbeNotes, " "), "unloaded") {
		t.Errorf("ProbeNotes must name what was held back: %v", inv.ProbeNotes)
	}
}

// TestDiscover_BelowFloorProbesNothing. Not merely "offers nothing" --
// it must not reach for a runtime socket at all. This runs on every
// registration.
func TestDiscover_BelowFloorProbesNothing(t *testing.T) {
	srv, hits := ollamaStub(t, []string{"llama3.1:8b"}, map[string]string{
		"llama3.1:8b": `{"capabilities":["tools"],"model_info":{"llama.context_length":8192}}`,
	})
	d := &Discoverer{
		OllamaBaseURL: srv.URL,
		FloorFn:       func() FloorVerdict { return FloorVerdict{Reason: "below the floor"} },
	}
	inv := d.Discover(context.Background(), Request{Allow: []string{"llama3.1:8b"}})
	if len(inv.Models) != 0 || len(inv.Advertised()) != 0 {
		t.Fatalf("below the floor a machine offers nothing: %+v", inv.Models)
	}
	if n := *hits; n != 0 {
		t.Errorf("the runtime was probed %d times below the floor; want 0", n)
	}
	if inv.Floor.Reason == "" {
		t.Error("the verdict must carry the reason")
	}
}

// TestProbe_StableOrder. An unstable order rewrites the registration row
// on every reconnect for no actual change.
func TestProbe_StableOrder(t *testing.T) {
	ids := []string{"zeta:1b", "alpha:1b", "mid:1b"}
	show := map[string]string{}
	for _, id := range ids {
		show[id] = fmt.Sprintf(`{"capabilities":["tools"],"model_info":{"x.context_length":%d}}`, 4096)
	}
	srv, _ := ollamaStub(t, ids, show)
	d := discovererFor(srv.URL, nil)

	var first []string
	for i := 0; i < 3; i++ {
		inv := d.Probe(context.Background(), Request{Allow: ids})
		var got []string
		for _, m := range inv.Models {
			got = append(got, m.ID)
		}
		if i == 0 {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed between probes: %v then %v", first, got)
		}
	}
	if strings.Join(first, ",") != "alpha:1b,mid:1b,zeta:1b" {
		t.Errorf("order = %v, want sorted by id", first)
	}
}
