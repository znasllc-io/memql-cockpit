package concepts

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// Block ids for the viewer's background-tinted bands. Zero means
// "no block" (e.g. header / separator rows); intrinsics / payload /
// provenance each get their own id so consecutive rows form a band.
const (
	blockNone       = 0
	blockIntrinsics = 1
	blockPayload    = 2
	blockProvenance = 3
)

// serializedRow is the intermediate representation produced by
// serializeRow: a MemQL-style multi-line text block plus a parallel
// slice of block ids (one per source line).
type serializedRow struct {
	text   string
	blocks []int // len == count('\n') + 1 in `text`
}

// serializeRow renders a row map as a pseudo-MemQL declaration block.
// The output is shaped to make MemQLSense's tokenizer produce
// meaningful tokens (concept ids, strings, numbers, braces) so the
// viewer's syntax highlighting reads naturally.
//
// Example:
//
//	concept v1:cognition:space {
//	  id: "v1:cognition:space:abc"
//	  type: "default"
//	  createdAt: "2026-05-20T10:00:00Z"
//	  payload {
//	    name: "Daily Standup"
//	    status: "active"
//	  }
//	  provenance {
//	    kind: "system"
//	  }
//	}
//
// The pseudo-declaration is read-only and never round-tripped back to
// the engine; the only consumer is the viewer.
func serializeRow(row map[string]any) serializedRow {
	if row == nil {
		return serializedRow{}
	}
	var b strings.Builder
	var blocks []int

	addLine := func(text string, block int) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		blocks = append(blocks, block)
	}

	conceptId := getString(row, "concept")
	rowId := getString(row, "id")
	header := "concept"
	if conceptId != "" {
		header += " " + conceptId
	}
	header += " {"
	addLine(header, blockNone)

	// Intrinsics block. Sorted in canonical order so the same row
	// always renders identically (helps with snapshot comparisons in
	// future tests and with user-side muscle memory).
	intrinsicKeys := []string{"id", "type", "schema", "createdBy", "createdAt"}
	hasIntrinsics := false
	for _, k := range intrinsicKeys {
		v := getString(row, k)
		if v == "" {
			continue
		}
		addLine(fmt.Sprintf("  %s: %s", k, quoteString(v)), blockIntrinsics)
		hasIntrinsics = true
	}
	_ = rowId
	if !hasIntrinsics {
		// Still emit at least the id so the viewer's first block isn't
		// missing entirely. rowId could be empty for synthetic rows
		// (unlikely but defensive).
		if rowId != "" {
			addLine(fmt.Sprintf("  id: %s", quoteString(rowId)), blockIntrinsics)
		}
	}

	if payload := getMap(row, "payload"); payload != nil {
		addLine("", blockNone)
		addLine("  payload {", blockNone)
		for _, ln := range serializeObject(payload, 2) {
			addLine(ln, blockPayload)
		}
		addLine("  }", blockNone)
	}

	// Provenance can live at either `row.provenance` or
	// `row.metadata.provenance` depending on which writer produced
	// the row. Mirror the previous renderer's both-paths logic.
	if prov := getMap(row, "provenance"); prov != nil {
		addLine("", blockNone)
		addLine("  provenance {", blockNone)
		for _, ln := range serializeObject(prov, 2) {
			addLine(ln, blockProvenance)
		}
		addLine("  }", blockNone)
	} else if meta := getMap(row, "metadata"); meta != nil {
		if prov := getMap(meta, "provenance"); prov != nil {
			addLine("", blockNone)
			addLine("  provenance {", blockNone)
			for _, ln := range serializeObject(prov, 2) {
				addLine(ln, blockProvenance)
			}
			addLine("  }", blockNone)
		}
	}

	addLine("}", blockNone)
	return serializedRow{text: b.String(), blocks: blocks}
}

// serializeObject renders one object's fields as `  key: value` lines
// at the supplied indent depth. Nested objects open a brace block;
// arrays render inline as JSON so the line count stays bounded.
func serializeObject(obj map[string]any, depth int) []string {
	if obj == nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	indent := strings.Repeat("  ", depth)
	var out []string
	for _, k := range keys {
		v := obj[k]
		switch val := v.(type) {
		case map[string]any:
			out = append(out, fmt.Sprintf("%s%s {", indent, k))
			out = append(out, serializeObject(val, depth+1)...)
			out = append(out, indent+"}")
		case []any:
			out = append(out, fmt.Sprintf("%s%s: %s", indent, k, jsonScalar(val)))
		default:
			out = append(out, fmt.Sprintf("%s%s: %s", indent, k, formatScalar(val)))
		}
	}
	return out
}

// formatScalar renders a scalar JSON value in MemQL-friendly form:
// strings get quoted, booleans + numbers + null stay bare.
func formatScalar(v any) string {
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		return quoteString(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return jsonScalar(v)
	}
}

func jsonScalar(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// quoteString wraps a value in double quotes with JSON-style escapes.
// We use json.Marshal so embedded quotes, backslashes, and control
// characters round-trip cleanly into a single source line.
func quoteString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}

// rowToViewerLines is the full pipeline used by the concept detail
// pane: serialize the row, hand the text to Sense (when a client is
// available), and convert the resulting tokens into per-line
// HighlightSpans.
//
// `client` may be nil -- the viewer renders without highlighting in
// that case (no cluster connected, or Sense not yet wired). `tokens`
// may also be empty if Sense returned nothing; the viewer copes.
func rowToViewerLines(row map[string]any, tokens []sense.Token, theme ui.Theme) []ui.ViewerLine {
	s := serializeRow(row)
	if s.text == "" {
		return nil
	}
	rawLines := strings.Split(s.text, "\n")
	spansByLine := spansFromTokens(tokens, theme)
	out := make([]ui.ViewerLine, len(rawLines))
	for i, text := range rawLines {
		block := blockNone
		if i < len(s.blocks) {
			block = s.blocks[i]
		}
		out[i] = ui.ViewerLine{
			Text:  text,
			Block: block,
			Spans: spansByLine[i+1], // Sense uses 1-indexed lines
		}
	}
	return out
}

// spansFromTokens converts Sense tokens (1-indexed line/column) into
// per-line viewer HighlightSpans. Tokens that span multiple lines
// (unusual in MemQL) get split at line boundaries.
func spansFromTokens(tokens []sense.Token, theme ui.Theme) map[int][]ui.HighlightSpan {
	out := map[int][]ui.HighlightSpan{}
	for _, tok := range tokens {
		if tok.Range.Start.Line < 1 {
			continue
		}
		style := theme.TokenStyle(tok.Type)
		if tok.Range.Start.Line == tok.Range.End.Line {
			line := tok.Range.Start.Line
			out[line] = append(out[line], ui.HighlightSpan{
				Start: tok.Range.Start.Column - 1,
				End:   tok.Range.End.Column - 1,
				Style: style,
			})
			continue
		}
		// Multi-line token: clamp first line to end-of-line, drop
		// continuation lines (Sense rarely emits these for our
		// pseudo-MemQL output; ignoring them keeps the converter
		// simple).
		first := tok.Range.Start.Line
		out[first] = append(out[first], ui.HighlightSpan{
			Start: tok.Range.Start.Column - 1,
			End:   1 << 30, // "to end of line" -- styleRunes clamps
			Style: style,
		})
	}
	return out
}
