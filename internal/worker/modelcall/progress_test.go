package modelcall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Exercise the runtime -> worker watchdog -> ModelCall wire hop. Reasoning
// and tool fragments can precede the first answer by more than the idle limit.
// They prove runtime progress but must never become user-visible answer text.
func TestRuntimeProgressPreventsFalseIdle(t *testing.T) {
	for _, tc := range []struct {
		name, frame  string
		openAI, idle bool
	}{
		{name: "ollama thinking", frame: `{"message":{"thinking":"private thought"}}`},
		{name: "compatible reasoning", openAI: true, frame: `{"choices":[{"delta":{"reasoning":"private thought"}}]}`},
		{name: "compatible reasoning_content", openAI: true, frame: `{"choices":[{"delta":{"reasoning_content":"private thought"}}]}`},
		{name: "compatible tool arguments", openAI: true, frame: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"write_file","arguments":" "}}]}}]}`},
		{name: "empty ollama frames remain idle", frame: `{"message":{"content":""}}`, idle: true},
		{name: "empty compatible frames remain idle", openAI: true, frame: `{"choices":[{"delta":{}}]}`, idle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				drain(r)
				flusher := w.(http.Flusher)
				for n := 0; n < 8; n++ {
					if tc.openAI {
						fmt.Fprintf(w, "data: %s\n\n", tc.frame)
					} else {
						fmt.Fprintln(w, tc.frame)
					}
					flusher.Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				if tc.openAI {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"document ready\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprintln(w, `{"message":{"content":"document ready"},"done":true,"done_reason":"stop"}`)
				}
			}))
			defer srv.Close()
			info := ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})
			if tc.openAI {
				info.Kind, info.Runtime = models.KindOpenAICompatible, models.KindOpenAICompatible
			}
			m := managerFor(inventoryWith(info))
			rec := newRecorder()
			st := start("progress", "m", KindChat)
			st.Limits.IdleTimeoutSeconds = 1
			m.Start(context.Background(), rec, st)
			end := rec.wait(t)
			if tc.idle {
				if end.GetFinishReason() != FinishTimeout || end.GetErrorCode() != CodeTimeout {
					t.Fatalf("empty runtime frames hid a stall: %+v", end)
				}
				return
			}
			if end.GetFinishReason() != FinishStop || end.GetError() != "" {
				t.Fatalf("active runtime was killed as idle: %+v", end)
			}
			if got := rec.content(); got != "document ready" {
				t.Fatalf("answer must exclude reasoning and partial tools: %q", got)
			}
		})
	}
}
