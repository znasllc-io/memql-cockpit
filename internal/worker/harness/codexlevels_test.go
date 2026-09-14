package harness

import (
	"context"
	"os"
	"strings"
	"testing"
)

// codexlevels_test.go pins what a LEVEL does to a Codex session on both of
// its harnesses (memql-cockpit#437) and what comes back as the model and
// effort that served it (#438).
//
// THE SPELLINGS ARE VERIFIED, NOT ASSUMED. On 2026-09-13, against the
// installed codex-cli 0.153.4:
//
//   - `codex app-server generate-ts` gives ThreadStartParams and
//     ThreadResumeParams a typed `model` and an open `config` map, and
//     ThreadStartResponse / ThreadResumeResponse a `model` and a
//     `reasoningEffort` -- the app's statement of what the thread runs at.
//   - a live thread/start with `config:{"model_reasoning_effort":"low"}`
//     answered `"model":"gpt-6-astra","reasoningEffort":"low"`, and repeated
//     both on thread/started; nothing later in the turn restated either.
//   - the mcp-server's `codex` tool declares `model` and an open `config`
//     ("Individual config settings that will override what is in
//     CODEX_HOME/config.toml"); `codex-reply` declares only prompt and
//     threadId.
//   - a live `codex` call with `config:{"model_reasoning_effort":"low"}`
//     emitted `session_configured` with `"model":"gpt-6-astra"` and
//     `"reasoning_effort":"low"` -- the only place in that stream either
//     word appears.
//
// So model_reasoning_effort is the configuration key on both harnesses, and
// the report is the app's own statement on each.

// codexTurnRerouted is a successful turn during which the app moved to
// another model -- ModelReroutedNotification (0.153.4:
// `{threadId, turnId, fromModel, toModel, reason}`, reason
// highRiskCyberActivity). From that notification on, what serves is the
// model it moved to, whatever the thread was configured with.
const codexTurnRerouted = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"THREAD","turnId":"turn_%s","fromModel":"gpt-6-astra","toModel":"gpt-5.5","reason":"highRiskCyberActivity"}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_%s","completedAtMs":1500,"item":{"type":"agentMessage","id":"item_msg","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexTurnFailsAfterSpending is a turn that reported spend and then
// failed. The spend is the proof the model ran, so the thread's model is
// what served it even though the turn did not complete.
const codexTurnFailsAfterSpending = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"inProgress","error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}\n' "$id" "$turn"
printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"THREAD","turnId":"turn_%s","tokenUsage":{"total":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"last":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":50,"reasoningOutputTokens":0},"modelContextWindow":272000}}}\n' "$turn"
printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_%s","items":[],"itemsView":"full","status":"failed","error":{"message":"the stream was cut","codexErrorInfo":null,"additionalDetails":null,"misalignment":null},"startedAt":1,"completedAt":2,"durationMs":1000}}}\n' "$turn"
`

// codexStatedSettings is a thread answer that states a model AND an effort,
// the way 0.153.4 answered a thread/start configured at low.
const codexStatedSettings = `"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"reasoningEffort":"low"`

// --- the app-server -------------------------------------------------

func TestCodexAppServerEveryLevelReachesThreadStart(t *testing.T) {
	for level, want := range BuiltinLevels(HarnessCodexAppServer) {
		t.Run(level, func(t *testing.T) {
			bin, log := fakeCodexAppServer(t, codexTurnOK)
			spec := codexSpec(t, bin)
			spec.Level = level
			h := startCodexAppServer(t, spec)

			if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
				t.Fatalf("turn: %v", err)
			}
			starts := codexCalls(t, log, codexMethodThreadStart)
			if len(starts) != 1 {
				t.Fatalf("thread/start calls = %d, want 1", len(starts))
			}
			params := starts[0]
			if got := codexConfigEffort(params); got != want.Effort {
				t.Errorf("config.model_reasoning_effort = %q, want %q", got, want.Effort)
			}
			if _, ok := params["model"]; ok {
				t.Errorf("the built-in Codex table names no model, but thread/start carried one: %v", params)
			}
			// The level adds to the thread's settings; it must not displace
			// the two the harness already depends on.
			if params["approvalPolicy"] != "never" || params["cwd"] != spec.Workspace {
				t.Errorf("the level displaced approvalPolicy or cwd: %v", params)
			}
			// The effort rides the THREAD, where the app states it back,
			// never the turn -- see applyKnobs.
			for _, turn := range codexCalls(t, log, codexMethodTurnStart) {
				if _, ok := turn["effort"]; ok {
					t.Errorf("turn/start carried an effort: %v", turn)
				}
			}
		})
	}
}

func TestCodexAppServerOwnerModelReachesThreadStart(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.Level = "strong"
	spec.Levels = Table{"strong": {Model: "gpt-5.5", Effort: "xhigh"}}
	h := startCodexAppServer(t, spec)

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	params := codexCalls(t, log, codexMethodThreadStart)[0]
	if params["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want the owner's gpt-5.5", params["model"])
	}
	if got := codexConfigEffort(params); got != "xhigh" {
		t.Errorf("config.model_reasoning_effort = %q, want xhigh", got)
	}
}

// An attach resumes a thread, and ThreadResumeParams takes the same
// overrides -- so a resumed session runs at its level too.
func TestCodexAppServerLevelReachesThreadResume(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.ResumeRef = codexFakeResumed
	spec.Level = "reasoning"
	h := startCodexAppServer(t, spec)

	if _, err := h.Turn(context.Background(), "carry on", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	resumes := codexCalls(t, log, codexMethodThreadResume)
	if len(resumes) != 1 {
		t.Fatalf("thread/resume calls = %d, want 1", len(resumes))
	}
	if got := codexConfigEffort(resumes[0]); got != "high" {
		t.Errorf("thread/resume config.model_reasoning_effort = %q, want high", got)
	}
	if resumes[0]["threadId"] != codexFakeResumed {
		t.Errorf("the level displaced the thread being resumed: %v", resumes[0])
	}
}

func TestCodexAppServerNoLevelSendsNoKnobs(t *testing.T) {
	bin, log := fakeCodexAppServer(t, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, bin))

	if _, err := h.Turn(context.Background(), "do the thing", &recorder{}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	params := codexCalls(t, log, codexMethodThreadStart)[0]
	for _, key := range []string{"model", "config"} {
		if _, ok := params[key]; ok {
			t.Errorf("a session with no level sent %q; the app's own settings must stand: %v", key, params)
		}
	}
}

func TestCodexAppServerRefusesAnUnknownLevelBeforeLaunching(t *testing.T) {
	for name, spec := range map[string]func(Spec) Spec{
		"an undefined word": func(s Spec) Spec { s.Level = "turbo"; return s },
		"embeddings":        func(s Spec) Spec { s.Level = "embeddings"; return s },
		"an effort that is not a word": func(s Spec) Spec {
			s.Level = "fast"
			s.Levels = Table{"fast": {Effort: "High!"}}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			bin, log := fakeCodexAppServer(t, codexTurnOK)
			h := &codexAppServer{}
			err := h.Start(context.Background(), spec(codexSpec(t, bin)))
			if err == nil {
				t.Fatal("Start accepted a level it cannot run at")
			}
			_ = h.Close()
			if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
				t.Errorf("an app-server was launched for a session that could never run (%v)", statErr)
			}
		})
	}
}

// THE REPORT IS THE APP'S STATEMENT. The fake answers thread/start with
// the settings the app says the thread runs at, and those -- not what this
// client sent -- are what a completed turn reports.
func TestCodexAppServerReportsWhatTheThreadStates(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnOK)
	spec := codexSpec(t, bin)
	spec.Level = "reasoning" // asks for high; the app states low
	h := startCodexAppServer(t, spec)

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Model/Effort = %q/%q, want the app's own statement gpt-6-astra/low, "+
			"never the request", res.Model, res.Effort)
	}
}

func TestCodexAppServerRerouteReplacesTheModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnRerouted)
	h := startCodexAppServer(t, codexSpec(t, bin))

	rec := &recorder{}
	res, err := h.Turn(context.Background(), "do the thing", rec)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want the model the app rerouted the turn to", res.Model)
	}
	if !strings.Contains(rec.joined(StreamEvent), `"model/rerouted"`) {
		t.Error("the reroute must still reach the transcript as an event")
	}
}

// A turn that failed before the model answered names no model: the thread's
// settings are what it was configured with, and a configured model that
// never ran -- an unknown name among them -- did not serve anything.
func TestCodexAppServerFailedTurnReportsNoModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnFails)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err == nil {
		t.Fatal("a failed turn reported success")
	}
	if res.Model != "" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want nothing for a turn that never showed the model ran",
			res.Model, res.Effort)
	}
}

func TestCodexAppServerFailedTurnThatSpentReportsTheModel(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t, codexStatedSettings, codexTurnFailsAfterSpending)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err == nil {
		t.Fatal("a failed turn reported success")
	}
	if res.Model != "gpt-6-astra" || res.Effort != "low" {
		t.Errorf("Model/Effort = %q/%q, want the thread's statement: the turn spent tokens, so the model ran",
			res.Model, res.Effort)
	}
}

func TestCodexAppServerNullEffortIsEmpty(t *testing.T) {
	bin, _ := fakeCodexAppServerSettings(t,
		`"model":"gpt-6-astra","modelProvider":"openai","serviceTier":null,"reasoningEffort":null`, codexTurnOK)
	h := startCodexAppServer(t, codexSpec(t, bin))

	res, err := h.Turn(context.Background(), "do the thing", &recorder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Model != "gpt-6-astra" || res.Effort != "" {
		t.Errorf("Model/Effort = %q/%q, want gpt-6-astra and no effort: null is the app saying nothing",
			res.Model, res.Effort)
	}
}

// codexConfigEffort pulls config.model_reasoning_effort out of decoded
// params, "" when either level is absent.
func codexConfigEffort(params map[string]any) string {
	cfg, _ := params["config"].(map[string]any)
	effort, _ := cfg["model_reasoning_effort"].(string)
	return effort
}
