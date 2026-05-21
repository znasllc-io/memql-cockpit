package concepts

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestSerializeRow_ShapeAndBlocks pins the pseudo-MemQL output for a
// representative row. The exact text matters for two reasons:
//
//  1. Sense's tokenizer is keyed on it. If we drift away from valid
//     MemQL-shaped declarations the syntax highlighting silently
//     degrades.
//  2. The block ids drive the Viewer's background tinting. The header
//     / closing-brace rows belong to blockNone; field rows under
//     payload / provenance map to blockPayload / blockProvenance.
func TestSerializeRow_ShapeAndBlocks(t *testing.T) {
	row := map[string]any{
		"id":        "v1:cognition:space:abc123",
		"concept":   "v1:cognition:space",
		"type":      "default",
		"createdBy": "user-xyz",
		"createdAt": "2026-05-20T10:00:00Z",
		"payload": map[string]any{
			"name":   "Daily Standup",
			"status": "active",
		},
		"provenance": map[string]any{
			"kind": "system",
		},
	}

	got := serializeRow(row)
	lines := strings.Split(got.text, "\n")

	if len(lines) != len(got.blocks) {
		t.Fatalf("text/blocks length mismatch: %d lines, %d blocks", len(lines), len(got.blocks))
	}

	// First line is the concept header in blockNone; final line is the
	// closing brace, also blockNone.
	if !strings.HasPrefix(lines[0], "concept v1:cognition:space ") {
		t.Errorf("first line: %q, want concept header", lines[0])
	}
	if got.blocks[0] != blockNone {
		t.Errorf("header block: %d, want blockNone", got.blocks[0])
	}
	if lines[len(lines)-1] != "}" {
		t.Errorf("last line: %q, want closing brace", lines[len(lines)-1])
	}
	if got.blocks[len(got.blocks)-1] != blockNone {
		t.Errorf("closing-brace block: %d, want blockNone", got.blocks[len(got.blocks)-1])
	}

	// Intrinsic rows carry blockIntrinsics.
	var intrinsicSeen bool
	for i, ln := range lines {
		if strings.HasPrefix(ln, "  id: ") {
			intrinsicSeen = true
			if got.blocks[i] != blockIntrinsics {
				t.Errorf("intrinsic line %d block: %d, want blockIntrinsics", i, got.blocks[i])
			}
		}
	}
	if !intrinsicSeen {
		t.Error("no intrinsic line emitted for id")
	}

	// Payload field rows carry blockPayload.
	var payloadFieldSeen bool
	for i, ln := range lines {
		if strings.HasPrefix(ln, "    name: ") {
			payloadFieldSeen = true
			if got.blocks[i] != blockPayload {
				t.Errorf("payload line %d block: %d, want blockPayload", i, got.blocks[i])
			}
		}
	}
	if !payloadFieldSeen {
		t.Error("no payload line emitted for name")
	}

	// Provenance field rows carry blockProvenance.
	var provFieldSeen bool
	for i, ln := range lines {
		if strings.HasPrefix(ln, "    kind: ") {
			provFieldSeen = true
			if got.blocks[i] != blockProvenance {
				t.Errorf("provenance line %d block: %d, want blockProvenance", i, got.blocks[i])
			}
		}
	}
	if !provFieldSeen {
		t.Error("no provenance line emitted for kind")
	}
}

// TestSerializeRow_LongValueGetsOwnLine confirms a really long
// payload field value still emits ONE line in the serialized output;
// the Viewer's hard-wrap takes over from there. This is the
// boundary between "serializer's job" and "viewer's job."
func TestSerializeRow_LongValueGetsOwnLine(t *testing.T) {
	long := strings.Repeat("X", 500)
	row := map[string]any{
		"id":      "v1:test:thing:abc",
		"concept": "v1:test:thing",
		"payload": map[string]any{
			"description": long,
		},
	}
	got := serializeRow(row)
	var hit bool
	for _, ln := range strings.Split(got.text, "\n") {
		if strings.Contains(ln, long) {
			hit = true
			break
		}
	}
	if !hit {
		t.Error("long value missing from serialized output -- serializer must not pre-wrap")
	}
}

// TestSerializeRow_ProvenanceUnderMetadata covers the alternative
// location for provenance: row.metadata.provenance. Mirrors the
// previous renderer's both-paths fallback.
func TestSerializeRow_ProvenanceUnderMetadata(t *testing.T) {
	row := map[string]any{
		"id":      "v1:test:thing:abc",
		"concept": "v1:test:thing",
		"metadata": map[string]any{
			"provenance": map[string]any{
				"kind": "metadata-nested",
			},
		},
	}
	got := serializeRow(row)
	if !strings.Contains(got.text, `kind: "metadata-nested"`) {
		t.Errorf("metadata-nested provenance missing; got:\n%s", got.text)
	}
}

// TestSerializeRow_NilInputDoesNotPanic: a nil row maps to an empty
// serializedRow.
func TestSerializeRow_NilInputDoesNotPanic(t *testing.T) {
	got := serializeRow(nil)
	if got.text != "" || len(got.blocks) != 0 {
		t.Errorf("nil row: got non-empty serialized row %+v", got)
	}
}

// TestRowToViewerLines_NoSpansWithoutTokens confirms the row renders
// as plain text (no Spans) when Sense returned no tokens. The viewer
// still gets blocks + numbered lines, just no per-token coloring.
func TestRowToViewerLines_NoSpansWithoutTokens(t *testing.T) {
	row := map[string]any{
		"id":      "v1:test:thing:abc",
		"concept": "v1:test:thing",
		"payload": map[string]any{"name": "alpha"},
	}
	out := rowToViewerLines(row, nil, ui.DefaultTheme())
	if len(out) == 0 {
		t.Fatal("rowToViewerLines: no lines produced")
	}
	for i, ln := range out {
		if len(ln.Spans) != 0 {
			t.Errorf("line %d: spans=%v expected empty (no tokens)", i, ln.Spans)
		}
	}
}

// TestRowToViewerLines_SpansFromTokens verifies a Sense token at
// (line=1, col=1..7) lands as a HighlightSpan on the first ViewerLine
// covering rune indices 0..6 -- i.e. exactly the bytes "concept".
func TestRowToViewerLines_SpansFromTokens(t *testing.T) {
	row := map[string]any{
		"id":      "v1:test:thing:abc",
		"concept": "v1:test:thing",
	}
	tokens := []sense.Token{{
		Type:    "keyword",
		Literal: "concept",
		Range: sense.Range{
			Start: sense.Position{Line: 1, Column: 1},
			End:   sense.Position{Line: 1, Column: 8}, // exclusive
		},
	}}
	out := rowToViewerLines(row, tokens, ui.DefaultTheme())
	if len(out) == 0 {
		t.Fatal("no lines produced")
	}
	if len(out[0].Spans) != 1 {
		t.Fatalf("line 0 spans: %d, want 1", len(out[0].Spans))
	}
	if out[0].Spans[0].Start != 0 || out[0].Spans[0].End != 7 {
		t.Errorf("span range: [%d, %d), want [0, 7)", out[0].Spans[0].Start, out[0].Spans[0].End)
	}
}
