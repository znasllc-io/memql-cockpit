package modelcall

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The OpenAI-compatible client: LM Studio, vLLM, llamafile, and Ollama's
// own /v1 surface. Chat streams server-sent events; embeddings come back
// whole.

type openAIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type openAIChatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *openAIClient) Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": openAIMessages(req.Messages),
		"stream":   true,
		// Without this, a streaming response carries no usage at all and
		// every local call would be billed "unknown" -- servers that do
		// not implement the option ignore it rather than refusing.
		"stream_options": map[string]any{"include_usage": true},
	}
	applyOpenAIParams(body, req.Params)
	if len(req.Schema) > 0 {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": json.RawMessage(req.Schema),
				"strict": true,
			},
		}
	}

	resp, err := c.post(ctx, "/chat/completions", body)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	out := Result{FinishReason: FinishStop}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawTerminator := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if string(payload) == "[DONE]" {
			sawTerminator = true
			break
		}
		var chunk openAIChatChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			out.Usage.Model = chunk.Model
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := emit(choice.Delta.Content); err != nil {
					return out, err
				}
			}
			if choice.FinishReason != "" {
				out.FinishReason = openAIFinishReason(choice.FinishReason)
			}
		}
		// Usage arrives on its own final frame when include_usage was
		// honoured. Absent, it stays absent.
		if chunk.Usage != nil {
			out.Usage.InputTokens = chunk.Usage.PromptTokens
			out.Usage.OutputTokens = chunk.Usage.CompletionTokens
			out.Usage.Known = true
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if !sawTerminator {
		return out, fmt.Errorf("openai-compatible: stream ended without [DONE]")
	}
	return out, nil
}

type openAIEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int64 `json:"prompt_tokens"`
	} `json:"usage"`
}

func (c *openAIClient) Embed(ctx context.Context, req EmbedRequest) (Result, error) {
	resp, err := c.post(ctx, "/embeddings", map[string]any{
		"model": req.Model,
		"input": req.Input,
	})
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	var parsed openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, err
	}
	// The envelope says "one vector per input, in input order". The API
	// carries an explicit index precisely because the array order is not
	// promised, so the vectors are placed by index rather than appended.
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		at := d.Index
		if at < 0 || at >= len(out) {
			at = i
		}
		out[at] = d.Embedding
	}
	res := Result{FinishReason: FinishStop, Embeddings: out, Usage: Usage{Model: parsed.Model}}
	if parsed.Usage != nil {
		res.Usage.InputTokens = parsed.Usage.PromptTokens
		res.Usage.Known = true
	}
	return res, nil
}

func (c *openAIClient) post(ctx context.Context, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail := readErrorBody(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-compatible: %s returned %d%s", path, resp.StatusCode, detail)
	}
	return resp, nil
}

func openAIMessages(in []Message) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, m := range in {
		out = append(out, map[string]string{"role": m.Role, "content": m.Content})
	}
	return out
}

func applyOpenAIParams(body map[string]any, p Params) {
	if p.TemperatureSet {
		body["temperature"] = p.Temperature
	}
	if p.TopPSet {
		body["top_p"] = p.TopP
	}
	if p.MaxOutputTokens > 0 {
		body["max_tokens"] = p.MaxOutputTokens
	}
	if len(p.Stop) > 0 {
		body["stop"] = p.Stop
	}
	if p.SeedSet {
		body["seed"] = p.Seed
	}
}

func openAIFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return FinishLength
	default:
		return FinishStop
	}
}

// readErrorBody returns a short, quoted fragment of a failed response for
// the error message.
//
// It is BOUNDED and it is the only place a runtime's body reaches a log.
// A local runtime echoes the prompt back in some error shapes, and a
// prompt is the user's data -- so what lands in a log line is a fragment
// sized for a status explanation, never a body this code streamed whole.
func readErrorBody(resp *http.Response) string {
	const maxErrorBody = 256
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(raw))
}
