package modelcall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The Ollama client. Chat streams newline-delimited JSON from /api/chat;
// embeddings come back whole from /api/embed.

type ollamaClient struct {
	baseURL string
	http    *http.Client
}

// ollamaChatChunk is one NDJSON line from /api/chat. The same shape
// carries both the incremental content and, on the final line, the
// outcome and the counts.
type ollamaChatChunk struct {
	Model   string `json:"model"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
}

func (c *ollamaClient) Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": ollamaMessages(req.Messages),
		"stream":   true,
	}
	if opts := ollamaOptions(req.Params); len(opts) > 0 {
		body["options"] = opts
	}
	if len(req.Schema) > 0 {
		// Ollama takes the JSON Schema directly as `format`. It is
		// passed through verbatim rather than re-marshalled: the engine
		// validated it, and a round trip through a map would reorder
		// keys the model was shown.
		body["format"] = json.RawMessage(req.Schema)
	}

	resp, err := c.post(ctx, "/api/chat", body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	out := Result{FinishReason: FinishStop}
	scanner := bufio.NewScanner(resp.Body)
	// A single token is small, but a non-streaming fallback response can
	// be the whole generation on one line. 4 MB is well past any answer a
	// local model produces and still bounded.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			// A line this side cannot read is skipped rather than
			// failing the call: the generation so far is real output
			// the caller already has, and discarding it over one
			// malformed frame would be a worse answer than a short one.
			continue
		}
		if chunk.Message.Content != "" {
			if err := emit(chunk.Message.Content); err != nil {
				return out, err
			}
		}
		if chunk.Done {
			out.FinishReason = ollamaFinishReason(chunk.DoneReason)
			out.Usage = Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
				// Ollama reports counts on the final frame. Absent
				// counts stay absent rather than becoming a confident
				// zero.
				Known: chunk.PromptEvalCount > 0 || chunk.EvalCount > 0,
				Model: chunk.Model,
			}
			return out, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	// The stream ended without a done frame. That is the runtime being
	// killed mid-generation, and it is reported as such rather than as a
	// clean stop -- a caller that parses a truncated structured answer
	// would otherwise blame the model.
	return out, fmt.Errorf("ollama: stream ended without a completion frame")
}

// ollamaEmbedResponse is /api/embed.
type ollamaEmbedResponse struct {
	Model           string      `json:"model"`
	Embeddings      [][]float32 `json:"embeddings"`
	PromptEvalCount int64       `json:"prompt_eval_count"`
}

func (c *ollamaClient) Embed(ctx context.Context, req EmbedRequest) (Result, error) {
	resp, err := c.post(ctx, "/api/embed", map[string]any{
		"model": req.Model,
		"input": req.Input,
	})
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	var parsed ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, err
	}
	return Result{
		FinishReason: FinishStop,
		Embeddings:   parsed.Embeddings,
		Usage: Usage{
			InputTokens: parsed.PromptEvalCount,
			Known:       parsed.PromptEvalCount > 0,
			Model:       parsed.Model,
		},
	}, nil
}

func (c *ollamaClient) post(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: %s returned %d%s", path, resp.StatusCode, detail)
	}
	return resp, nil
}

func ollamaMessages(in []Message) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, m := range in {
		out = append(out, map[string]string{"role": m.Role, "content": m.Content})
	}
	return out
}

// ollamaOptions maps the envelope's params onto Ollama's option names.
// A knob the caller did not set is OMITTED rather than sent as zero --
// sending temperature 0 for "no preference" would pin every unspecified
// call to greedy decoding.
func ollamaOptions(p Params) map[string]any {
	out := map[string]any{}
	if p.TemperatureSet {
		out["temperature"] = p.Temperature
	}
	if p.TopPSet {
		out["top_p"] = p.TopP
	}
	if p.MaxOutputTokens > 0 {
		out["num_predict"] = p.MaxOutputTokens
	}
	if len(p.Stop) > 0 {
		out["stop"] = p.Stop
	}
	if p.SeedSet {
		out["seed"] = p.Seed
	}
	return out
}

// ollamaFinishReason maps Ollama's done_reason onto the envelope's closed
// set. Anything unrecognised is "stop", because the generation did in
// fact finish and inventing an error class for a new Ollama spelling
// would fail calls that succeeded.
func ollamaFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return FinishLength
	default:
		return FinishStop
	}
}
