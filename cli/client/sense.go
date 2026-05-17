package client

import (
	"context"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql-cockpit/cli/editor"
)

// SenseClient provides convenience methods for MemQL Sense operations over gRPC.
type SenseClient struct {
	dispatcher *Dispatcher
}

// NewSenseClient creates a client that calls Sense operations via the dispatcher.
func NewSenseClient(dispatcher *Dispatcher) *SenseClient {
	return &SenseClient{dispatcher: dispatcher}
}

// Tokenize sends source code to Sense and returns syntax tokens.
func (sc *SenseClient) Tokenize(ctx context.Context, source string) []editor.SenseToken {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseTokenize{
			SenseTokenize: &memqlv1.SenseTokenizeMsg{
				Source: source,
			},
		},
	}

	resp, err := sc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil
	}

	result := resp.GetSenseTokenizeResult()
	if result == nil {
		return nil
	}

	tokens := make([]editor.SenseToken, 0, len(result.GetTokens()))
	for _, t := range result.GetTokens() {
		tok := editor.SenseToken{
			Type:    t.GetType(),
			Literal: t.GetLiteral(),
		}
		if r := t.GetRange(); r != nil {
			if s := r.GetStart(); s != nil {
				tok.StartLine = int(s.GetLine())
				tok.StartCol = int(s.GetColumn())
			}
			if e := r.GetEnd(); e != nil {
				tok.EndLine = int(e.GetLine())
				tok.EndCol = int(e.GetColumn())
			}
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

// Diagnose sends source code to Sense and returns diagnostics.
func (sc *SenseClient) Diagnose(ctx context.Context, source string) []editor.SenseDiagnostic {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseDiagnose{
			SenseDiagnose: &memqlv1.SenseDiagnoseMsg{
				Source: source,
			},
		},
	}

	resp, err := sc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil
	}

	result := resp.GetSenseDiagnoseResult()
	if result == nil {
		return nil
	}

	diags := make([]editor.SenseDiagnostic, 0, len(result.GetDiagnostics()))
	for _, d := range result.GetDiagnostics() {
		diag := editor.SenseDiagnostic{
			Severity: int(d.GetSeverity()),
			Message:  d.GetMessage(),
			Code:     d.GetCode(),
		}
		if r := d.GetRange(); r != nil {
			if s := r.GetStart(); s != nil {
				diag.StartLine = int(s.GetLine())
				diag.StartCol = int(s.GetColumn())
			}
			if e := r.GetEnd(); e != nil {
				diag.EndLine = int(e.GetLine())
				diag.EndCol = int(e.GetColumn())
			}
		}
		diags = append(diags, diag)
	}
	return diags
}

// Complete sends source code and cursor position to Sense and returns completions.
func (sc *SenseClient) Complete(ctx context.Context, source string, line, col int) []editor.CompletionItem {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseComplete{
			SenseComplete: &memqlv1.SenseCompleteMsg{
				Source: source,
				Cursor: &memqlv1.SensePosition{
					Line:   int32(line),
					Column: int32(col),
				},
			},
		},
	}

	resp, err := sc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil
	}

	result := resp.GetSenseCompleteResult()
	if result == nil {
		return nil
	}

	items := make([]editor.CompletionItem, 0, len(result.GetItems()))
	for _, item := range result.GetItems() {
		items = append(items, editor.CompletionItem{
			Label:         item.GetLabel(),
			Kind:          item.GetKind(),
			Detail:        item.GetDetail(),
			Documentation: item.GetDocumentation(),
			InsertText:    item.GetInsertText(),
			SortPriority:  int(item.GetSortPriority()),
		})
	}
	return items
}

// Hover sends source code and cursor position to Sense and returns hover content.
func (sc *SenseClient) Hover(ctx context.Context, source string, line, col int) string {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseHover{
			SenseHover: &memqlv1.SenseHoverMsg{
				Source: source,
				Position: &memqlv1.SensePosition{
					Line:   int32(line),
					Column: int32(col),
				},
			},
		},
	}

	resp, err := sc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return ""
	}

	result := resp.GetSenseHoverResult()
	if result == nil {
		return ""
	}
	return result.GetContents()
}
