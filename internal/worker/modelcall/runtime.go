// Package modelcall serves the engine's ModelCall envelope from this
// machine's own model runtime (epic memql#4676, task memql-cockpit#362).
//
// The engine fixes the contract and picks the machine; everything here
// runs the call. Start / delta / end / cancel, correlated by request id,
// over the stream the worker already holds open -- the AppSession shape,
// because a generation emits tokens for as long as it runs and the caller
// wants them as they arrive.
//
// Four rules run through the package, and each one fails silently when
// broken:
//
//   - DELTAS CARRY A MONOTONIC seq. The engine drops out-of-order and
//     duplicate deltas rather than corrupting the generation, so the
//     sequence has to be produced correctly here; there is no repair on
//     the other side.
//
//   - THE ENVELOPE OWNS THE DEADLINES. Timeout, idle ceiling and
//     keepalive arrive on ModelCallLimits. A local 8B model on a cold GPU
//     is twenty seconds from start to first token, which is
//     indistinguishable from a wedged machine to anything holding only a
//     wall clock -- the keepalive is what makes the idle ceiling
//     enforceable rather than a guess, so a call with nothing to say
//     still has to say it.
//
//   - USAGE IS REPORTED, NEVER INFERRED. What the runtime said, including
//     which model it actually ran. Silence stays silence, which the
//     engine records as billing "unknown". A count derived from string
//     length would be stored as measured, and the only thing it could
//     corrupt is the loop-cap arithmetic that exists to notice a runaway.
//
//   - A SCHEMA IS HONOURED OR THE CALL FAILS. The router only ever sends
//     a response schema to a machine that advertised the capability, so
//     answering prose instead would defeat the gating that put the call
//     here -- and the parse error would surface three layers away, naming
//     nothing.
package modelcall

import (
	"context"
	"time"
)

// Kinds, mirrored from memql component/worker/modelcall.go.
const (
	KindChat      = "chat"
	KindEmbedding = "embedding"
)

// Finish reasons, mirrored from the same file.
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishCancelled = "cancelled"
	FinishTimeout   = "timeout"
	FinishError     = "error"
)

// Envelope defaults, mirrored from the same file. A ModelCallLimits that
// states nothing gets these rather than "no limit": an unbounded
// generation on somebody's laptop is a resource leak nobody is watching.
const (
	DefaultTimeout     = 10 * time.Minute
	DefaultIdleTimeout = 90 * time.Second
	DefaultKeepalive   = 20 * time.Second
)

// Message is one turn handed to the model.
type Message struct {
	Role    string
	Content string
}

// Params are the generation knobs. Each optional knob carries an explicit
// Set companion because zero is a MEANINGFUL value for both temperature
// and top_p: temperature 0 is what a structured-output prompt actually
// wants, and it is not the same request as "the caller said nothing".
type Params struct {
	Temperature     float64
	TemperatureSet  bool
	TopP            float64
	TopPSet         bool
	MaxOutputTokens int64
	Stop            []string
	Seed            int64
	SeedSet         bool
}

// ChatRequest is one chat generation.
type ChatRequest struct {
	Model    string
	Messages []Message
	Params   Params
	// Schema is a JSON Schema for structured output. Nil means free text.
	Schema []byte
}

// EmbedRequest is one embedding call.
type EmbedRequest struct {
	Model string
	Input []string
}

// Usage is what the RUNTIME REPORTED. Known separates "it told us zero"
// from "it told us nothing", which the engine needs in order to record
// billing "unknown" instead of a confident zero.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
	// Model is what the runtime ACTUALLY ran, which is not always what
	// was asked for -- a quantisation alias, a tag resolving to a digest.
	// A token count without the model it was spent on cannot be read.
	Model string
}

// Result is a finished call.
type Result struct {
	FinishReason string
	// Embeddings is the KindEmbedding result, one vector per input, in
	// input order.
	Embeddings [][]float32
	Usage      Usage
}

// emitFunc receives one piece of generated text. Returning an error stops
// the generation -- it means the stream back to the cluster is gone, and
// continuing would spend this machine's GPU on output nobody will read.
type emitFunc func(content string) error

// client is one runtime family. Both implementations are stateless; the
// per-call state lives on the call.
type client interface {
	Chat(ctx context.Context, req ChatRequest, emit emitFunc) (Result, error)
	Embed(ctx context.Context, req EmbedRequest) (Result, error)
}
