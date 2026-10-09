package modelcall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
				if tc.idle {
					// The idle clock begins after real runtime output.
					// Empty frames afterwards must not keep it alive.
					if tc.openAI {
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"started\"}}]}\n\n")
					} else {
						fmt.Fprintln(w, `{"message":{"thinking":"started"}}`)
					}
					flusher.Flush()
				}
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

// Large prompts can take longer to evaluate than the gap allowed between
// generated tokens. The absolute deadline still stops a runtime that never
// answers, while the existing stalled-after-output tests retain the idle guard.
func TestPromptEvaluationUsesCallDeadlineUntilFirstOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, `{"message":{"content":"Ready"},"done":true}`)
	}))
	defer srv.Close()
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	st := start("prefill", "m", KindChat)
	st.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 5, IdleTimeoutSeconds: 1, KeepaliveSeconds: 1}
	m.Start(context.Background(), rec, st)
	end := rec.wait(t)
	if end.GetFinishReason() != FinishStop {
		t.Fatalf("prompt evaluation was interrupted: %+v", end)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	keepalives := 0
	for _, delta := range rec.deltas {
		if delta.GetKeepalive() {
			keepalives++
		}
	}
	if keepalives == 0 {
		t.Fatal("no keepalive during prompt evaluation")
	}
}

func TestPromptThatNeverProducesOutputStillTimesOutAndReleasesSlot(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	defer srv.Close()
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})))
	rec := newRecorder()
	st := start("stuck-prefill", "m", KindChat)
	st.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 2, IdleTimeoutSeconds: 1, KeepaliveSeconds: 1}
	began := time.Now()
	m.Start(context.Background(), rec, st)
	end := rec.wait(t)
	if end.GetFinishReason() != FinishTimeout || time.Since(began) < 1800*time.Millisecond {
		t.Fatalf("startup did not use absolute deadline: %+v", end)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("runtime request was not cancelled")
	}
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		live := len(m.live)
		m.mu.Unlock()
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed-out call retained live state")
		}
		time.Sleep(time.Millisecond)
	}
	if !m.limiter.tryAcquire("m", 1, 1) {
		t.Fatal("timed-out prefill retained its model slot")
	}
	m.limiter.release("m")
}
