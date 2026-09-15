package modelcall

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// levels_test.go pins what a LEVEL on a model call does on this machine
// (memql#5393, memql-cockpit#438): nothing to the call, and nothing
// invented on the way back.
//
// The router has already chosen `model` from this machine's advertisement,
// so the level is a ledger and log fact here -- a level steers an app, which
// has knobs, while a runtime has only the model it was asked for. And the
// End's usage carries the model the runtime REPORTED and no effort, because
// neither Ollama nor an OpenAI-compatible response states one.
func TestModelCall_TheLevelSteersNothingAndNoEffortIsGuessed(t *testing.T) {
	srv, seen := ollamaChatStub(t, []string{"ok"}, true)
	m := managerFor(inventoryWith(ollamaModel(srv.URL, "llama3.1:8b",
		models.Attributes{ContextWindow: 8192, MaxConcurrent: 1})))
	rec := newRecorder()

	call := start("r-level", "llama3.1:8b", KindChat)
	call.Level = "reasoning"
	m.Start(context.Background(), rec, call)
	end := rec.wait(t)

	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(*seen, &sent); err != nil {
		t.Fatalf("the runtime saw no readable request: %v", err)
	}
	if sent.Model != "llama3.1:8b" {
		t.Errorf("the runtime was asked for %q; the level must not displace the model the router chose", sent.Model)
	}
	u := end.GetUsage()
	if u.GetModel() != "llama3.1:8b" {
		t.Errorf("usage.model = %q, want the runtime's own report", u.GetModel())
	}
	if u.GetEffort() != "" {
		t.Errorf("usage.effort = %q, want empty: no local runtime states an effort, and "+
			"the level is a request, not a report", u.GetEffort())
	}
}
