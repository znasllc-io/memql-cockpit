# App Door Completion (cockpit half) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A session's LEVEL (the engine's word for how much intelligence a step needs) becomes each app's own model and effort on this machine, the model and effort the app REPORTED come back on the End, the machine owner can override the table in `policy.yaml`, and the app-session wire fields the pin already carries (memql#5096) are finally mapped.

**Architecture:** `internal/worker/harness` owns the level vocabulary (imported from the engine's `core/airoute`), the built-in per-app tables, the knob checks and each app's protocol for applying knobs and reporting what served. `internal/worker/tools` parses `apps.levels` and validates it against those checks. `internal/worker/appsession` merges the two, refuses a level before any side effect, passes the table to the harness and puts the report on `AppSessionEnd`. `memql worker apps` prints the effective table.

**Tech Stack:** Go 1.26 (single module), protobuf via the pinned `../memql` sibling, shell-script fakes for Claude Code and Codex.

**Record:** memql `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`, section 4, epic A (cockpit), decisions D8, D9, D10. Issues: #436 (epic), #437, #438, #439, #444 (found while picking up #439). Engine wire: memql#5393, PR memql#5424.

## Global Constraints

- One PR for the whole epic, branch `epic/app-door-completion`, never on main. This plan is deleted in the epic's merge.
- The pin moves in ONE commit with `go.mod`/`go.sum` (`.github/memql-pin` is the only pin; the `require` pseudo-version is not the pin).
- Levels are core/airoute's four: `fast`, `strong`, `reasoning`, `embeddings`, exactly as spelled. `embeddings` never runs through an app (D10). An unknown level refuses.
- Claude Code 2.1.270 (verified 2026-09-13): `--model <alias|name>`, `--effort` takes `low, medium, high, xhigh, max` and IGNORES any other word with a stderr warning; stream-json states the model (`system/init.model`, `result.modelUsage`) and NO effort anywhere.
- Codex 0.153.4 (verified 2026-09-13): the reasoning-effort key is `model_reasoning_effort` (the `codex` MCP tool's `config`; app-server `thread/start|resume` `config`); `model` is a typed param on both; the app states `model` + `reasoningEffort` on the `thread/start|resume` response and `model` + `reasoning_effort` on the mcp-server's `session_configured` event; `model/rerouted` announces a reroute.
- Report, never request: `AppSessionEnd.model/effort` carry what the APP stated; empty when it said nothing. Claude Code's effort is therefore always empty.
- `apps.levels`: absent block = built-in table; an entry replaces its level WHOLE; an entry the app would misread REFUSES its level (never falls back); unknown apps/levels are logged problems; the block REPLACES on SIGHUP.
- Pre-release, no shims: `reason` is never read as a prompt; the `memql.app_session.result` chunk is deleted, not kept beside `result_json`.
- Match the surrounding style: long WHY comments on every decision, sentences an operator can act on in every error.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/worker/harness/levels.go` (new) | Level vocabulary, `Knobs`, `Table`, `BuiltinLevels`, `ResolveLevel`, `CheckAppLevel`, `CheckKnobs`, `MergeLevels`, `DescribeKnobs`, `Spec.knobs` |
| `internal/worker/harness/levels_test.go` (new) | Per-app, per-level table tests; refusals; knob checks |
| `internal/worker/harness/harness.go` | `Spec.Level`, `Spec.Levels`; `TurnResult.Model`, `TurnResult.Effort` |
| `internal/worker/harness/claudeheadless.go` | knobs on argv; model from `modelUsage`; effort never |
| `internal/worker/harness/codexappserver.go` | knobs on `thread/start|resume`; report from the response and `model/rerouted` |
| `internal/worker/harness/codexmcp.go` | knobs on the `codex` tool; report from `session_configured` |
| `internal/worker/tools/policy.go` | `apps.levels` (`LevelKnobs`), `AppLevels`, `AppLevelProblems` |
| `internal/worker/appsession/session.go`, `chunks.go` | #5096 mapping; level resolution + refusal + transcript note; End model/effort |
| `internal/worker/appinventory.go`, `connect.go` | `Register.app_descriptors` |
| `internal/worker/apps_cmd.go` (new), `cli.go`, `fleet.go`, `pair.go` | `memql worker apps`; `Levels` wiring; level problems logged at load/SIGHUP |
| `internal/worker/modelcall/session.go` | `ModelCallStart.level` read into the log; effort never guessed |
| `.github/memql-pin`, `go.mod`, `go.sum` | the pin bump, one commit |
| `docs/local-apps.md`, `CLAUDE.md` | operator doc and repo rules |

---

### Task 1: The level vocabulary and the built-in tables (#437)

**Files:**
- Create: `internal/worker/harness/levels.go`
- Create: `internal/worker/harness/levels_test.go`
- Modify: `internal/worker/harness/harness.go` (Spec, TurnResult)

**Interfaces:**
- Produces: `harness.Knobs{Model, Effort string}`, `harness.Table map[string]Knobs`, `harness.AppLevels() []string`, `harness.BuiltinLevels(harnessWord string) Table`, `harness.ResolveLevel(level string, t Table) (Knobs, error)`, `harness.CheckAppLevel(level string) error`, `harness.CheckKnobs(harnessWord string, k Knobs) error`, `harness.MergeLevels(base, override Table) Table`, `harness.DescribeKnobs(harnessWord string, k Knobs) string`, `harness.ErrEmbeddingsLevel`, `Spec.Level string`, `Spec.Levels Table`, `(Spec).knobs(harnessWord string) (Knobs, error)`, `TurnResult.Model`, `TurnResult.Effort`.

- [ ] **Step 1: Write the failing tests** (`levels_test.go`)

```go
package harness

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinLevelsClaudeCode(t *testing.T) {
	want := Table{
		"fast":      {Model: "haiku"},
		"strong":    {Model: "sonnet", Effort: "high"},
		"reasoning": {Model: "opus", Effort: "xhigh"},
	}
	got := BuiltinLevels(HarnessClaudeHeadless)
	if len(got) != len(want) {
		t.Fatalf("claude table = %v, want %v", got, want)
	}
	for level, k := range want {
		if got[level] != k {
			t.Errorf("claude %s = %+v, want %+v", level, got[level], k)
		}
		if err := CheckKnobs(HarnessClaudeHeadless, got[level]); err != nil {
			t.Errorf("the built-in claude %s entry fails its own check: %v", level, err)
		}
	}
}

func TestBuiltinLevelsCodexSetsEffortNotModel(t *testing.T) {
	for _, word := range []string{HarnessCodexAppServer, HarnessCodexMCP} {
		got := BuiltinLevels(word)
		want := Table{"fast": {Effort: "low"}, "strong": {Effort: "medium"}, "reasoning": {Effort: "high"}}
		for level, k := range want {
			if got[level] != k {
				t.Errorf("%s %s = %+v, want %+v", word, level, got[level], k)
			}
		}
	}
}

func TestEveryAppLevelHasABuiltinEntryAndEmbeddingsHasNone(t *testing.T) {
	for _, word := range []string{HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP} {
		table := BuiltinLevels(word)
		for _, level := range AppLevels() {
			if _, ok := table[level]; !ok {
				t.Errorf("%s has no built-in entry for %s", word, level)
			}
		}
		if _, ok := table["embeddings"]; ok {
			t.Errorf("%s has an embeddings entry; no app serves one (D10)", word)
		}
	}
}

func TestResolveLevel(t *testing.T) {
	table := BuiltinLevels(HarnessClaudeHeadless)
	if k, err := ResolveLevel("", table); err != nil || k != (Knobs{}) {
		t.Errorf("no level = %+v, %v; want no knobs and no error", k, err)
	}
	if k, err := ResolveLevel("reasoning", table); err != nil || k.Model != "opus" {
		t.Errorf("reasoning = %+v, %v", k, err)
	}
	if _, err := ResolveLevel("embeddings", table); !errors.Is(err, ErrEmbeddingsLevel) {
		t.Errorf("embeddings err = %v, want ErrEmbeddingsLevel", err)
	}
	_, err := ResolveLevel("turbo", table)
	if err == nil || !strings.Contains(err.Error(), "fast, strong, reasoning, embeddings") {
		t.Errorf("unknown level err = %v, want it to name the four levels", err)
	}
	if _, err := ResolveLevel("Fast", table); err == nil {
		t.Error("a level is spelled exactly; Fast is not fast")
	}
	if _, err := ResolveLevel("fast", Table{}); err == nil {
		t.Error("a level the table has no entry for must refuse, not run at the app's defaults")
	}
}

func TestCheckKnobs(t *testing.T) {
	cases := []struct {
		word string
		k    Knobs
		ok   bool
	}{
		{HarnessClaudeHeadless, Knobs{Model: "sonnet[1m]", Effort: "max"}, true},
		{HarnessClaudeHeadless, Knobs{Effort: "extreme"}, false},
		{HarnessClaudeHeadless, Knobs{Model: "-p"}, false},
		{HarnessClaudeHeadless, Knobs{Model: "claude opus"}, false},
		{HarnessCodexAppServer, Knobs{Model: "gpt-5.5", Effort: "ultra"}, true},
		{HarnessCodexMCP, Knobs{Effort: "High!"}, false},
		{"telepathy", Knobs{Effort: "low"}, false},
	}
	for _, c := range cases {
		err := CheckKnobs(c.word, c.k)
		if (err == nil) != c.ok {
			t.Errorf("CheckKnobs(%s, %+v) = %v, want ok=%v", c.word, c.k, err, c.ok)
		}
	}
	err := CheckKnobs(HarnessClaudeHeadless, Knobs{Effort: "extreme"})
	if err == nil || !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
		t.Errorf("the refusal must name Claude Code's words: %v", err)
	}
}

func TestMergeLevelsReplacesWholeEntries(t *testing.T) {
	got := MergeLevels(BuiltinLevels(HarnessClaudeHeadless), Table{"strong": {Model: "opus"}})
	if got["strong"] != (Knobs{Model: "opus"}) {
		t.Errorf("strong = %+v; an owner's entry replaces the built-in one WHOLE, effort included", got["strong"])
	}
	if got["fast"] != (Knobs{Model: "haiku"}) {
		t.Errorf("fast = %+v; a level the owner did not list keeps the built-in entry", got["fast"])
	}
}

func TestDescribeKnobs(t *testing.T) {
	cases := map[string]string{
		DescribeKnobs(HarnessClaudeHeadless, Knobs{Model: "opus", Effort: "xhigh"}): "--model opus --effort xhigh",
		DescribeKnobs(HarnessCodexAppServer, Knobs{Effort: "low"}):                  "model_reasoning_effort=low, on the account's default model",
		DescribeKnobs(HarnessCodexMCP, Knobs{Model: "gpt-5.5", Effort: "high"}):     "model gpt-5.5, model_reasoning_effort=high",
		DescribeKnobs(HarnessClaudeHeadless, Knobs{}):                               "the app's own defaults",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("DescribeKnobs = %q, want %q", got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails:** `go test ./internal/worker/harness/ -run 'Level|Knobs' -count=1` → FAIL (undefined: BuiltinLevels ...).

- [ ] **Step 3: Implement `levels.go`** (full file; comments carry the WHY):

```go
package harness

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

var ErrEmbeddingsLevel = errors.New(`level "embeddings" never runs through an app: an embedding has to come from the embedder of the index it is written into, and neither Claude Code nor Codex exposes one`)

type Knobs struct {
	Model  string
	Effort string
}

type Table map[string]Knobs

func AppLevels() []string {
	var out []string
	for _, l := range airoute.Levels() {
		if l != airoute.LevelEmbeddings {
			out = append(out, string(l))
		}
	}
	return out
}

func BuiltinLevels(harnessWord string) Table {
	switch harnessWord {
	case HarnessClaudeHeadless:
		return Table{
			string(airoute.LevelFast):      {Model: "haiku"},
			string(airoute.LevelStrong):    {Model: "sonnet", Effort: "high"},
			string(airoute.LevelReasoning): {Model: "opus", Effort: "xhigh"},
		}
	case HarnessCodexAppServer, HarnessCodexMCP:
		return Table{
			string(airoute.LevelFast):      {Effort: "low"},
			string(airoute.LevelStrong):    {Effort: "medium"},
			string(airoute.LevelReasoning): {Effort: "high"},
		}
	}
	return nil
}

func CheckAppLevel(level string) error {
	parsed, err := airoute.ParseLevel(level)
	if err != nil {
		return err
	}
	if parsed == airoute.LevelEmbeddings {
		return ErrEmbeddingsLevel
	}
	return nil
}

func ResolveLevel(level string, table Table) (Knobs, error) {
	if level == "" {
		return Knobs{}, nil
	}
	if err := CheckAppLevel(level); err != nil {
		return Knobs{}, err
	}
	knobs, ok := table[level]
	if !ok {
		return Knobs{}, fmt.Errorf("level %q has no translation on this machine", level)
	}
	return knobs, nil
}

var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

var knobModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,199}$`)

var knobEffortWord = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func CheckKnobs(harnessWord string, k Knobs) error {
	if k.Model != "" && !knobModel.MatchString(k.Model) {
		return fmt.Errorf("model %q is not a model name: it must start with a letter or a digit and contain no spaces", k.Model)
	}
	if k.Effort == "" {
		return nil
	}
	switch harnessWord {
	case HarnessClaudeHeadless:
		if !slices.Contains(claudeEfforts, k.Effort) {
			return fmt.Errorf("effort %q is not one Claude Code takes (it takes %s, and ignores any other word rather than refusing it)",
				k.Effort, strings.Join(claudeEfforts, ", "))
		}
	case HarnessCodexAppServer, HarnessCodexMCP:
		if !knobEffortWord.MatchString(k.Effort) {
			return fmt.Errorf("effort %q is not an effort word (Codex takes the words its models advertise, such as low, medium, high or xhigh)", k.Effort)
		}
	default:
		return fmt.Errorf("this cockpit has no harness %q to check an effort against", harnessWord)
	}
	return nil
}

func MergeLevels(base, override Table) Table {
	out := make(Table, len(base)+len(override))
	for level, k := range base {
		out[level] = k
	}
	for level, k := range override {
		out[level] = k
	}
	return out
}

func DescribeKnobs(harnessWord string, k Knobs) string {
	if k.Model == "" && k.Effort == "" {
		return "the app's own defaults"
	}
	if harnessWord == HarnessClaudeHeadless {
		var parts []string
		if k.Model != "" {
			parts = append(parts, "--model "+k.Model)
		}
		if k.Effort != "" {
			parts = append(parts, "--effort "+k.Effort)
		}
		return strings.Join(parts, " ")
	}
	switch {
	case k.Model == "":
		return "model_reasoning_effort=" + k.Effort + ", on the account's default model"
	case k.Effort == "":
		return "model " + k.Model + ", at the model's default effort"
	}
	return "model " + k.Model + ", model_reasoning_effort=" + k.Effort
}

func (s Spec) knobs(harnessWord string) (Knobs, error) {
	table := s.Levels
	if table == nil {
		table = BuiltinLevels(harnessWord)
	}
	knobs, err := ResolveLevel(s.Level, table)
	if err != nil {
		return Knobs{}, err
	}
	if err := CheckKnobs(harnessWord, knobs); err != nil {
		return Knobs{}, err
	}
	return knobs, nil
}
```

And in `harness.go`, add to `Spec` (after `ResumeRef`): `Level string` and `Levels Table`; add to `TurnResult` (after `ExitCode`): `Model string` and `Effort string`, each with the report-not-request comment.

- [ ] **Step 4: Run:** `go test ./internal/worker/harness/ -count=1` → PASS.
- [ ] **Step 5: Commit:** `git add internal/worker/harness/levels.go internal/worker/harness/levels_test.go internal/worker/harness/harness.go && git commit -m "Issue #437: the level vocabulary, the built-in per-app tables and the knob checks"`

---

### Task 2: Claude Code: knobs on the argv, the model from `modelUsage`, never an effort (#437, #438)

**Files:** Modify `internal/worker/harness/claudeheadless.go`; Test `internal/worker/harness/claudeheadless_test.go`.

**Interfaces:** Consumes `Spec.knobs`, `Knobs`. Produces `claudeArgv(spec Spec, knobs Knobs, prompt, ref string) []string`; `TurnResult.Model` set from the result event.

- [ ] **Step 1: Failing tests.** Add the recorded 2.1.270 fixture and tests:

```go
// claudeLevelTurn is `claude -p --output-format stream-json --verbose
// --no-session-persistence --model haiku --effort low -- 'Reply with exactly
// the word: ok'`, run on 2026-09-13 against claude 2.1.270 and trimmed to the
// fields this client reads. modelUsage is keyed by the model that SPENT the
// tokens; nothing in the stream states an effort.
const claudeLevelTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-270","model":"claude-haiku-4-5-20251001","permissionMode":"default","claude_code_version":"2.1.270"}
{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]},"parent_tool_use_id":null,"session_id":"sess-270"}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"sess-270","total_cost_usd":0.0245737,"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":909,"outputTokens":284,"costUSD":0.0245737,"canonicalModel":"claude-haiku-4-5"}},"result":"ok","usage":{"input_tokens":10,"output_tokens":273}}`
```

Tests: `TestClaudeHeadlessEveryLevelReachesTheArgv` (table-driven over `BuiltinLevels(HarnessClaudeHeadless)`: `--model`/`--effort` values, `--effort` ABSENT for fast, both before `--`), `TestClaudeHeadlessNoLevelPassesNoKnobs`, `TestClaudeHeadlessOwnerTableWins`, `TestClaudeHeadlessRefusesAnUnknownLevelBeforeForking` (Start errors naming the four levels; the argv log was never written), `TestClaudeHeadlessRefusesEmbeddings`, `TestClaudeHeadlessRefusesAnEffortItWouldIgnore`, `TestClaudeHeadlessReportsTheModelFromModelUsage` (`Model == "claude-haiku-4-5-20251001"`, `Effort == ""` at level reasoning), `TestClaudeHeadlessPrefersTheSessionsOwnModel` (two modelUsage entries; init names the second), `TestClaudeHeadlessMostOutputWinsWithoutAnInitModel`, `TestClaudeHeadlessReportsNoModelWhenNothingSpent` (`claudeFailedResult`, `"modelUsage":{}`).

- [ ] **Step 2:** `go test ./internal/worker/harness/ -run ClaudeHeadless -count=1` → FAIL.
- [ ] **Step 3: Implement.** `claudeHeadless` gains `knobs Knobs`; `Start` resolves `spec.knobs(HarnessClaudeHeadless)` after the existing checks and refuses with `"harness: claude-headless: " + err`; `Turn` reads `knobs` with `spec` under the lock and calls `claudeArgv(spec, knobs, prompt, ref)`, which appends `--model` / `--effort` right after `--verbose`. `claudeEvent` gains `Subtype`, `Model`; `route` records `t.initModel` from `system/init`. `claudeResultEvent` gains `ModelUsage map[string]claudeModelUsage` (`OutputTokens int64 json:"outputTokens"`). `claudeTurn.servedModel()`:

```go
func (t *claudeTurn) servedModel() string {
	if t.result == nil || len(t.result.ModelUsage) == 0 {
		return ""
	}
	if _, ok := t.result.ModelUsage[t.initModel]; ok && t.initModel != "" {
		return t.initModel
	}
	best, most := "", int64(-1)
	for id, u := range t.result.ModelUsage {
		if u.OutputTokens > most || (u.OutputTokens == most && id < best) {
			best, most = id, u.OutputTokens
		}
	}
	return best
}
```

and `Turn` sets `res.Model = turn.servedModel()` beside `res.Usage` (Effort stays empty, with the comment saying why).
- [ ] **Step 4:** `go test ./internal/worker/harness/ -count=1` → PASS.
- [ ] **Step 5: Commit** `Issue #437: Claude Code runs a level as --model and --effort; Issue #438: the model comes back from modelUsage`.

---

### Task 3: Codex app-server: knobs on the thread, the report from the app (#437, #438)

**Files:** Modify `internal/worker/harness/codexappserver.go`; Test `internal/worker/harness/codexappserver_test.go`.

**Interfaces:** Produces `codexNotifyModelRerouted = "model/rerouted"`; `codexAppServer.served() (model, effort string)`; `codexTurnState.ranTheModel() bool`.

- [ ] **Step 1: Failing tests.** Split the fake so a test can choose the thread answer: `fakeCodexAppServerThread(t, threadResult, turnBody)` (the existing `fakeCodexAppServer` calls it with the current `"model":"gpt-5.1-codex"` answer). Tests: `TestCodexAppServerEveryLevelReachesThreadStart` (wire log: `thread/start` params carry `"config":{"model_reasoning_effort":"<e>"}` and NO `"model"` for the built-in table), `TestCodexAppServerOwnerModelReachesThreadStart`, `TestCodexAppServerLevelReachesThreadResume`, `TestCodexAppServerNoLevelSendsNoKnobs`, `TestCodexAppServerRefusesAnUnknownLevelBeforeLaunching` (no wire log at all), `TestCodexAppServerReportsWhatTheThreadStates` (thread answer `"model":"gpt-6-astra","reasoningEffort":"low"` → `Model`/`Effort`), `TestCodexAppServerRerouteReplacesTheModel` (turn body prints `{"jsonrpc":"2.0","method":"model/rerouted","params":{"threadId":"THREAD","turnId":"turn_%s","fromModel":"gpt-6-astra","toModel":"gpt-5.5","reason":"highRiskCyberActivity"}}`), `TestCodexAppServerFailedTurnReportsNoModel` (`codexTurnFails`), `TestCodexAppServerNullEffortIsEmpty`.
- [ ] **Step 2:** `go test ./internal/worker/harness/ -run CodexAppServer -count=1` → FAIL.
- [ ] **Step 3: Implement.** `Start` resolves `spec.knobs(HarnessCodexAppServer)` BEFORE `spec.Launch`. `ensureThread` calls `h.applyKnobs(params)` for both methods:

```go
func (h *codexAppServer) applyKnobs(params map[string]any) {
	if h.knobs.Model != "" {
		params["model"] = h.knobs.Model
	}
	if h.knobs.Effort != "" {
		params["config"] = map[string]any{"model_reasoning_effort": h.knobs.Effort}
	}
}
```

and decodes `Model string json:"model"` and `ReasoningEffort *string json:"reasoningEffort"` beside the thread id into `h.servedModel` / `h.servedEffort`. `handleNotification` gains a non-returning `case codexNotifyModelRerouted:` that replaces `h.servedModel` with `toModel`. `result(state)` sets `out.Model, out.Effort = h.served()` only when `state.ranTheModel()` (completed, or reported spend).
- [ ] **Step 4:** harness suite → PASS.
- [ ] **Step 5: Commit** `Issue #437: Codex app-server runs a level through model and model_reasoning_effort; Issue #438: the thread's stated model and effort come back`.

---

### Task 4: Codex MCP fallback: knobs on the `codex` tool, the report from `session_configured` (#437, #438)

**Files:** Modify `internal/worker/harness/codexmcp.go`; Test `internal/worker/harness/codexmcp_test.go`.

- [ ] **Step 1: Failing tests** with the recorded 0.153.4 event (trimmed):

```go
const codexMCPSessionConfigured = `printf '{"jsonrpc":"2.0","method":"codex/event","params":{"_meta":{"requestId":1,"threadId":"THREAD"},"msg":{"type":"session_configured","session_id":"THREAD","thread_id":"THREAD","model":"gpt-6-astra","model_provider_id":"openai","approval_policy":"never","reasoning_effort":"low"},"id":""}}\n'
`
```

Tests: `TestCodexMCPEveryLevelReachesTheCodexTool` (tools/call `arguments.config.model_reasoning_effort`, no `model` for built-ins), `TestCodexMCPOwnerModelReachesTheCodexTool`, `TestCodexMCPReplyCarriesNoKnobs` (second turn: `codex-reply` arguments are exactly `threadId` + `prompt`), `TestCodexMCPReportsSessionConfigured`, `TestCodexMCPToolErrorReportsNoModel`, `TestCodexMCPRefusesAnUnknownLevelBeforeLaunching`.
- [ ] **Step 2:** FAIL. **Step 3: Implement:** `Start` resolves knobs before launching; the `codex` tool's args get `model` and `config.model_reasoning_effort`; `codex-reply` unchanged; `handleNotification`'s event struct gains `Model`, `ReasoningEffort *string` and a non-returning `session_configured` case storing them; the success path of `Turn` (after the `isError` check, before the schema check) sets `result.Model, result.Effort = h.served()`.
- [ ] **Step 4:** PASS. **Step 5: Commit** `Issue #437: the Codex MCP fallback runs a level through the codex tool's model and config; Issue #438: session_configured is the report`.

---

### Task 5: `policy.yaml apps.levels` (#438)

**Files:** Modify `internal/worker/tools/policy.go`; Test `internal/worker/tools/levels_policy_test.go` (new).

**Interfaces:** Produces `tools.LevelKnobs{Model, Effort string}`, `AppsPolicy.Levels map[string]map[string]LevelKnobs`, `(*Policy).AppLevels(appID string) (harness.Table, map[string]string)`, `(*Policy).AppLevelProblems() []string`.

- [ ] **Step 1: Failing tests** (package `tools_test`, reusing `policyFrom`):

```go
func TestAppLevels_AbsentBlockMeansTheBuiltInTable(t *testing.T) {
	p := policyFrom(t, "apps:\n  allow: [claude-code]\n")
	table, refused := p.AppLevels("claude-code")
	if len(table) != 0 || len(refused) != 0 || len(p.AppLevelProblems()) != 0 {
		t.Fatalf("absent block: table=%v refused=%v problems=%v; want nothing, which means the built-in table",
			table, refused, p.AppLevelProblems())
	}
}

func TestAppLevels_LoadsTheDocumentedShape(t *testing.T) {
	p := policyFrom(t, `apps:
  levels:
    claude-code:
      reasoning:
        model: fable
        effort: max
      fast: {}
    codex:
      strong:
        model: gpt-5.5
        effort: xhigh
`)
	claude, _ := p.AppLevels("claude-code")
	if claude["reasoning"] != (harness.Knobs{Model: "fable", Effort: "max"}) {
		t.Errorf("claude reasoning = %+v", claude["reasoning"])
	}
	if k, ok := claude["fast"]; !ok || k != (harness.Knobs{}) {
		t.Errorf("an empty entry is the app's own defaults, and it must stand: %+v ok=%v", k, ok)
	}
	codex, _ := p.AppLevels("codex")
	if codex["strong"] != (harness.Knobs{Model: "gpt-5.5", Effort: "xhigh"}) {
		t.Errorf("codex strong = %+v", codex["strong"])
	}
}

func TestAppLevels_AnEntryTheAppWouldMisreadRefusesItsLevel(t *testing.T) {
	p := policyFrom(t, "apps:\n  levels:\n    claude-code:\n      strong:\n        effort: extreme\n      fast:\n        model: haiku\n")
	table, refused := p.AppLevels("claude-code")
	reason := refused["strong"]
	for _, want := range []string{"apps.levels.claude-code.strong", "low, medium, high, xhigh, max", "refuses strong sessions"} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal %q is missing %q", reason, want)
		}
	}
	if _, ok := table["strong"]; ok {
		t.Error("a refused entry must not stand in the table")
	}
	if table["fast"].Model != "haiku" {
		t.Error("one bad entry must not take the app's other levels with it")
	}
}

func TestAppLevels_UnknownAppsAndLevelsAreProblemsNotRefusals(t *testing.T) {
	p := policyFrom(t, "apps:\n  levels:\n    gemini-cli:\n      fast: {model: x}\n    codex:\n      fastt: {effort: low}\n      embeddings: {model: y}\n")
	_, refused := p.AppLevels("codex")
	if len(refused) != 0 {
		t.Errorf("no session can ask for these, so nothing is refused: %v", refused)
	}
	joined := strings.Join(p.AppLevelProblems(), "\n")
	for _, want := range []string{`"gemini-cli"`, `"fastt"`, "embeddings"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q are missing %s", joined, want)
		}
	}
}

func TestAppLevels_ReplaceOnReload(t *testing.T) { /* write a block, LoadPolicy, rewrite without it, Reload: AppLevels("claude-code") is empty */ }

func TestAppLevels_NilPolicy(t *testing.T) {
	var p *tools.Policy
	if table, refused := p.AppLevels("claude-code"); table != nil || refused != nil {
		t.Error("a nil policy has no entries")
	}
}
```

- [ ] **Step 2:** `go test ./internal/worker/tools/ -run AppLevels -count=1` → FAIL.
- [ ] **Step 3: Implement** `LevelKnobs`, the `Levels` field with the posture doc, the REPLACE in `reload`, and `readAppLevels(raw) appLevelsRead{table, refused, problems}` (sorted keys; app ids lower-cased; `harness.CheckAppLevel` for the level word; `harness.CheckKnobs(spec.Harness, knobs)` with `spec := apps.SpecFor(appID)`), with `AppLevels` / `AppLevelProblems` reading it under `RLock` and returning fresh maps.
- [ ] **Step 4:** PASS. **Step 5: Commit** `Issue #438: policy.yaml apps.levels overrides the built-in table, and an entry the app would misread refuses its level`.

---

### Task 6: The memql#5096 fields the pin already carries (#444)

**Files:** Modify `internal/worker/appsession/session.go`, `internal/worker/appsession/chunks.go`, `internal/worker/appinventory.go`, `internal/worker/connect.go`; Tests `internal/worker/appsession/session_test.go`, `internal/worker/appinventory_test.go`.

- [ ] **Step 1: Failing tests.** In `session_test.go`: change the three follow-up tests to send `Prompt:` instead of `Reason:`; add `TestSession_AReasonIsNeverAPrompt` (a `message` control carrying only `Reason` runs no second turn); add `TestSession_ResponseSchemaReachesTheHarness` (start with `ResponseSchemaJson` → the fake claude's argv has `--json-schema` with that schema; the fake prints a result with `structured_output`; `End.ResultJson` is that object); replace `TestSession_StructuredResultLeavesAsTheFinalEventChunk` with `TestSession_StructuredResultRidesTheEnd` (result on `ResultJson`, NO chunk contains `memql.app_session.result`; no result → `ResultJson == ""`). In `appinventory_test.go`: `TestAppDescriptorsToProto` (one per Info with a harness, flags copied, id truncated, none for an empty harness) and `TestBuildRegister_CarriesAppDescriptors`.
- [ ] **Step 2:** FAIL.
- [ ] **Step 3: Implement.** `controlPrompt` → `strings.TrimSpace(c.GetPrompt())`; `startResponseSchema` → `strings.TrimSpace(s.GetResponseSchemaJson())`; `sendEnd` sets `ResultJson: string(result)`; delete `resultEventType`, `structuredResultChunk`, `sendStructuredResult`, `emitUncapped` and the `capped` parameter; rewrite the three seam comments to say what the fields ARE now. Add:

```go
func appDescriptorsToProto(inventory []apps.Info) []*memqlv1.AppDescriptor {
	var out []*memqlv1.AppDescriptor
	for _, a := range inventory {
		if strings.TrimSpace(a.Harness) == "" {
			continue
		}
		out = append(out, &memqlv1.AppDescriptor{
			Id:               apps.Truncate(a.Id),
			Harness:          a.Harness,
			StructuredResult: a.StructuredResult,
			FollowUps:        a.FollowUps,
		})
	}
	return out
}
```

and `AppDescriptors: appDescriptorsToProto(inventory)` in `buildRegister`; update the `apps.Info` "NOT ON THE WIRE YET" comment.
- [ ] **Step 4:** `go test ./internal/worker/... -count=1` → PASS.
- [ ] **Step 5: Commit** `Issue #444: map the follow-up prompt, the response schema, the result and the app descriptors the pin already carries`.

---

### Task 7: The session runs at its level and reports what served (#437, #438, #439)

**Files:** Modify `internal/worker/appsession/session.go`, `chunks.go`; Test `internal/worker/appsession/levels_test.go` (new).

**Interfaces:** Produces `appsession.Options.Levels func(appID string) (harness.Table, map[string]string)`; `levelPlan{level string; table harness.Table; knobs harness.Knobs; owner bool}`; `(*session).resolveLevel(apps.Spec) (levelPlan, error)`; `levelNote(apps.Spec, levelPlan) string`.

- [ ] **Step 1: Failing tests:** `TestSession_LevelBecomesTheAppsKnobs` (reasoning → argv `--model opus --effort xhigh`; a stderr chunk `[memql] level reasoning runs claude-code with --model opus --effort xhigh (the cockpit's built-in table)`), `TestSession_NoLevelPassesNoKnobsAndNoNote`, `TestSession_PolicyLevelsChangeTheArgv` (a real `tools.Policy` from YAML, `Options.Levels: policy.AppLevels`; the owner's entry reaches the argv and the note says `policy.yaml`; the same session with no block uses the built-in table), `TestSession_RefusesEmbeddingsBeforeTouchingTheMachine` (End error carries the D10 sentence; the app never ran; no Library pull happened; the MCP ledger is empty), `TestSession_RefusesAnUnknownLevel`, `TestSession_AnEntryTheAppWouldMisreadRefusesOnlyItsLevel`, `TestSession_EndCarriesTheModelTheAppReported` (init + modelUsage → `End.Model`, `End.Effort == ""`), `TestSession_EndCarriesNoModelWhenTheAppSaidNothing`, `TestSession_TheLastTurnThatReportedAModelWins` (unit test on `recordTurn`).
- [ ] **Step 2:** FAIL. **Step 3: Implement** `resolveLevel` in `execute` right after `resolveApp` for the run/attach kinds (open reads no level), `hspec.Level/Levels = plan.level/plan.table`, the note after a successful `h.Start`, `recordTurn` keeping the (model, effort) PAIR from the last turn that reported a model, `sendEnd` setting `Model`/`Effort` and logging them.
- [ ] **Step 4:** PASS. **Step 5: Commit** `Issue #437: a session runs at its level's knobs and refuses one it cannot translate; Issue #438: the End carries what the app said it served`.

---

### Task 8: Wiring, the problems log, and `memql worker apps` (#438)

**Files:** Create `internal/worker/apps_cmd.go`, `internal/worker/apps_cmd_test.go`; Modify `internal/worker/cli.go`, `internal/worker/fleet.go`, `internal/worker/pair.go`.

- [ ] **Step 1: Failing test** `TestPrintAppInventory` (policy from YAML with one good override and one refused entry and one unknown app; inventory: claude-code allowed + signed in, codex absent): output contains `claude-code`, `allowed and signed in`, `--model opus --effort xhigh`, `built-in`, `policy.yaml`, `REFUSED`, `codex        not installed`, `never through an app`, `Problems in`.
- [ ] **Step 2:** FAIL. **Step 3: Implement** `handleApps` / `printAppInventory` / `appVerdict` / `harnessLine`; `case "apps": handleApps(args[1:])` and a usage line; `Levels: policy.AppLevels` in `cli.go` and `Levels: f.policy.AppLevels` in `fleet.go` (nil-safe); `logLevelProblems(logger, policy)` after every `LoadPolicy` and successful `Reload` in `cli.go` and `pair.go`.
- [ ] **Step 4:** `go test ./internal/worker/ -count=1` → PASS; `go run ./cmd/memql worker apps` prints the real machine's table.
- [ ] **Step 5: Commit** `Issue #438: memql worker apps prints each app's level table and why; the worker logs every apps.levels problem`.

---

### Task 9: Model calls: the level is read, the effort is never guessed (#438)

**Files:** Modify `internal/worker/modelcall/session.go`; Test `internal/worker/modelcall/session_test.go`.

- [ ] **Step 1: Failing test** `TestModelCall_TheLevelSteersNothingAndNoEffortIsGuessed`: a chat call with `Level: "reasoning"` against the package's fake runtime ends with `Usage.Model` = the runtime's report and `Usage.Effort == ""`, and the request the runtime saw names the same model the start did.
- [ ] **Step 2/3:** add the debug log line carrying `level` in `run`, and the `usageProto` comment that no runtime states an effort.
- [ ] **Step 4/5:** PASS; commit `Issue #438: a model call carries its level into the log and never guesses an effort`.

---

### Task 10: The pin bump, `go.mod` in the same commit (#439)

Blocked on memql PR #5424 merging (and, per the peer session on epic #425, on descending from memql#5423's merge if that lands first).

- [ ] **Step 1:** `git -C ../memql fetch origin && git -C ../memql checkout --detach <merge-sha>` (the worktree sibling), where `<merge-sha>` is the memql `main` commit that contains #5424 (and #5423 if merged).
- [ ] **Step 2:** Replace the sha in `.github/memql-pin` and add a dated paragraph naming #5424 / memql#5393 and what the bump carries.
- [ ] **Step 3:** `go mod tidy` (expect `google.golang.org/grpc` and `github.com/znasllc-io/memql/core` to move to the direct block and stale `go.sum` lines to go).
- [ ] **Step 4:** `go build ./... && go vet ./... && go test ./... -count=1` → PASS.
- [ ] **Step 5: Commit** `.github/memql-pin go.mod go.sum` together: `Issue #439: pin to the memql merge carrying the level and the served model and effort`.

---

### Task 11: Docs

- [ ] `docs/local-apps.md`: a "Levels" section (what a level is, the built-in table, the `apps.levels` override with an example, refusals, what comes back, `memql worker apps` output), and a note that follow-ups, structured answers and descriptors now travel.
- [ ] `CLAUDE.md` "Local apps" section: the level rules that fail silently (translation on the machine, report not request, whole-entry replace, refused never defaulted, embeddings never).
- [ ] Commit `docs: levels, the report and memql worker apps`.

---

### Task 12: Verify, review, ship

- [ ] `make lint && make test && bash scripts/install/lib_test.sh`; `go test -race ./internal/worker/...`.
- [ ] Code review (superpowers:requesting-code-review) and fix what it finds.
- [ ] Delete this plan in the final commit; open ONE PR closing #436 #437 #438 #439 #444; merge when green; close the issues with completion comments; remove the worktrees and branches.
