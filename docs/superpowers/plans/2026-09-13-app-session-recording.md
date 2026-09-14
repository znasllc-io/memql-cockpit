# App-Session Recording (cockpit half) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every tool call an app completes on this machine leaves the app session as ONE normalized `event` chunk in a single shape for Claude Code and both Codex harnesses, and every session's first event is an environment fingerprint.

**Architecture:** The harness package (`internal/worker/harness`) owns the wire shapes (`Action`, `Fingerprint`) and translates each app's own protocol into Actions: Claude Code's `tool_use`/`tool_result` pairs joined by id, the Codex app-server's `item/started`/`item/completed`, the Codex mcp-server's core begin/end events. A per-session `recording` numbers actions densely from 1 across turns and flushes calls that never finished as `incomplete`. `harness.Sink` gains `Record(Action)`; the app-session runner (`internal/worker/appsession`) implements it by reading the files an action names under a workspace-only content policy and sending the action uncapped. The session builds and sends the fingerprint (seq 0) before any kind starts.

**Tech Stack:** Go 1.26.1 (`go` directive; toolchain go1.26.6), standard library only (`encoding/json`, `crypto/sha256`, `path/filepath`, `os/exec`), the existing shell-script fake binaries in the harness and session tests.

**Spec:** the ENGINE repository's `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`, section 4 "Epic B -- Recording", the Cockpit bullet; decisions D2, D5, D12, D16, D17. Issues: epic memql-cockpit#440, tasks #441, #442, #443. Engine half: memql#5396 (not merged).

**Trees:** cockpit worktree `/home/znas/memql-projects/epic-app-session-recording` on branch `epic/app-session-recording` from `main` at `f0e4842`. Engine sibling `/home/znas/memql-projects/memql` (resolves `../memql` for the worktree). `.github/memql-pin` is `275623b3d90d57368d0efec62261705db400a33d` and does not move in this epic.

**Lifecycle of this file:** cockpit `CLAUDE.md` -- "the plans beside them are deleted by the PR that finishes them; the specs are the record". Task 7 deletes it in the same PR.

## Global Constraints

- One PR for all three issues (#441, #442, #443), branch `epic/app-session-recording`, merged to `main`.
- No proto change and no pin move: `AppSessionChunk.stream` and `seq` already exist. Do not touch `go.mod`, `go.sum` or `.github/memql-pin`.
- The event type words are `memql.app_session.action` and `memql.app_session.fingerprint`; both carry `"v": 1`.
- `Action.Seq` counts ACTIONS densely from 1 across every turn of a session; the fingerprint is seq 0. It is not the chunk seq.
- Every unknown stays unknown: `exitCode`, `isError`, `resultType`, `resultDigest`, `digest`, `bytes` are ABSENT when not reported, never zero.
- `Action.Tool` is the closed set `exec`, `fs_read`, `fs_write`, `fetch`, `mcp`, `agent`, `other`.
- Digests are `sha256:<lowercase hex>`.
- Contents are read only for successful calls, only inside the workspace (symlinks resolved), never the session scaffolding (`.memql-session/`, the written `.mcp.json`, `*.memql-session-backup`), only regular files checked on the open descriptor. Inline ceilings: 1 MiB per file, 4 MiB per event; digest ceiling 64 MiB (`maxPushFileBytes`).
- The recording is NOT subject to `limits.max_transcript_bytes`.
- A peer session (epic #436, branch `epic/app-door-completion`) edits `harness.Spec`, `TurnResult`, `claudeArgv`, `claudeResultEvent`, `ensureThread`, `controlPrompt`/`startResponseSchema`/`sendStructuredResult`, `connect.go`, `policy.go`, the pin. Do not edit those; keep edits in existing files to small hooks so either side rebases cheaply.
- Tests: `go test ./...` from the worktree root is the whole suite (single module). `gofmt -l .` prints nothing; `go vet ./...` is clean. Fuzz targets assert a PROPERTY and live in `fuzz_test.go` beside the code.
- Commit messages: `Issue #<N>: <description>` inside the epic (ccpm convention), each ending with:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01DFWSXFcdPudHxbXR68QX1K
  ```
- PR description ends with:
  ```
  🤖 Generated with [Claude Code](https://claude.com/claude-code)

  https://claude.ai/code/session_01DFWSXFcdPudHxbXR68QX1K
  ```
- No emojis in source, docs or test output.
- Fixtures are RECORDED, not invented: `claude` 2.1.270 and `codex-cli` 0.153.4 were run on 2026-09-13 in a scratch directory (a stdio MCP server named `echo` with one tool `echo`); the Codex item shapes are `codex app-server generate-ts` output from that binary; the mcp-server core events are `codex-rs/protocol/src/protocol.rs` at `rust-v0.153.4`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/worker/harness/record.go` (new) | Wire shapes `Action`, `Content`, `MCPTarget`, `Fingerprint` (+ parts), the closed vocabularies, `Digest`, `contentFor`, `FingerprintVariables` |
| `internal/worker/harness/recording.go` (new) | Per-session bookkeeping: `recording` (begin / complete / flush / stamp), `mergeCall` |
| `internal/worker/harness/result.go` (new) | Result reduction: `resultShape`, `inferTextType`, `jsonType`, `decodeValue`, `decodeTolerant`, `canonicalJSON`, `blocksValue`, `mcpResultValue`, `rawOrNull`, `isNull`, `argsOf` |
| `internal/worker/harness/claudeactions.go` (new) | Claude Code tool kinds, `claudeCall`, `claudeFinish`, `claudeExitCode`, `claudeMCPTarget` |
| `internal/worker/harness/codexactions.go` (new) | Codex app-server item mapping, mcp-server core-event mapping, `shellJoin`, `codexPath` |
| `internal/worker/harness/harness.go` | `Sink.Record`; `SinkFunc.Record` |
| `internal/worker/harness/claudeheadless.go` | Hooks: pair tool_use/tool_result in `routeBlocks`, init cwd, flush at turn end |
| `internal/worker/harness/codexappserver.go` | Hooks: `recordItem` on item started/completed, flush at turn end, `jsonrpcConn.record` |
| `internal/worker/harness/codexmcp.go` | Hooks: `recordEvent` on core tool events, flush at turn end |
| `internal/worker/appsession/record.go` (new) | `sessionSink`, `recordAction`, `contentPolicy`, `readForRecord`, ceilings |
| `internal/worker/appsession/openread_unix.go` / `openread_other.go` (new) | `openForRecord` (O_NONBLOCK / O_NOFOLLOW on unix) |
| `internal/worker/appsession/fingerprint.go` (new) | `sendFingerprint`, `fingerprint`, `listWorkspace`, `fingerprintVariables`, `inputDigests`, `toolchain` |
| `internal/worker/appsession/session.go` | `Options.ToolVersions`, `Manager.tools`, `session.policy` / `pulled`, fingerprint call, sink |
| `internal/worker/appsession/mcpconfig.go` | `mcpConfig.paths()` |
| `internal/worker/apps/detect.go` | `Detector.Version(ctx, id)` |
| `docs/local-apps.md`, `CLAUDE.md` | The recording, for operators and for the next session |

---

## Task 1: The recording's wire shape, bookkeeping and result reduction (foundation for #441 and #442)

**Files:**
- Create: `internal/worker/harness/record.go`, `internal/worker/harness/recording.go`, `internal/worker/harness/result.go`
- Modify: `internal/worker/harness/harness.go` (Sink / SinkFunc), `internal/worker/harness/claudeheadless_test.go` (the `recorder` test sink)
- Test: `internal/worker/harness/record_test.go`, `internal/worker/harness/result_test.go`

**Interfaces:**
- Produces: `harness.Action`, `harness.Content`, `harness.MCPTarget`, `harness.Fingerprint` (+ `FingerprintApp`, `Platform`, `ToolVersion`, `Variable`, `Input`); constants `ActionEventType`, `FingerprintEventType`, `RecordVersion`, `ActionExec|FSRead|FSWrite|Fetch|MCP|Agent|Other`, `ContentRead|Write|Delete`, `Omitted*`, `EncodingUTF8|Base64`, `DigestPrefix`; `func Digest([]byte) string`; `func FingerprintVariables(word string) []string`; unexported `contentFor(op, path, cwd string) []Content`, `newRecording() *recording` with `startTurn()`, `begin(Action)`, `complete(id string, finish func(*Action)) (Action, bool)`, `flush() []Action`; `mergeCall(begun, ended Action) Action`; `resultShape(any) (typ, digest string)`, `decodeValue(json.RawMessage) (any, bool)`, `decodeTolerant(json.RawMessage, any) bool`, `blocksValue(any) any`, `mcpResultValue(any) any`, `rawOrNull`, `isNull`, `argsOf(any) json.RawMessage`; `Sink.Record(Action)`.

- [ ] **Step 1: Write the failing tests** -- `internal/worker/harness/record_test.go`:

```go
package harness

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// record_test.go pins the recording's wire shape and its bookkeeping.
//
// THE NAMES ARE THE CONTRACT. The engine half (memql#5396) decodes these
// events by key, and it has not been written yet -- so this repository is
// DEFINING the keys, the way TestWireContract says it defines the model
// labels' `params` and `quant`. A rename here compiles everywhere and
// breaks a reader nobody can see from this side; the tests below make it
// a decision.

// jsonKeys marshals v and returns its top-level keys, sorted.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("%T did not marshal to an object: %v (%s)", v, err, body)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestActionWireContract(t *testing.T) {
	code, failed, size := 3, true, int64(5)
	data := "hello"
	full := Action{
		Type: ActionEventType, V: RecordVersion, Seq: 7, Turn: 2,
		ID: "toolu_1", ParentID: "toolu_0", Tool: ActionExec, AppTool: "Bash",
		Args: json.RawMessage(`{"command":"ls"}`), Cwd: "/w", Command: "ls",
		MCP: &MCPTarget{Server: "memql", Tool: "query"}, URL: "https://example.com", Query: "q",
		ExitCode: &code, IsError: &failed, ResultType: "string", ResultDigest: Digest([]byte("x")),
		Contents: []Content{{
			Op: ContentWrite, Path: "/w/a.txt", Digest: Digest([]byte("hello")), Bytes: &size,
			Encoding: EncodingUTF8, Data: &data, Omitted: OmittedOverBudget,
		}},
		Incomplete: true,
	}
	want := []string{
		"appTool", "args", "command", "contents", "cwd", "exitCode", "id", "incomplete",
		"isError", "mcp", "parentId", "query", "resultDigest", "resultType", "seq", "tool",
		"turn", "type", "url", "v",
	}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("Action keys = %v, want %v", got, want)
	}
	if got := jsonKeys(t, full.Contents[0]); !reflect.DeepEqual(got,
		[]string{"bytes", "data", "digest", "encoding", "omitted", "op", "path"}) {
		t.Errorf("Content keys = %v", got)
	}
	if got := jsonKeys(t, *full.MCP); !reflect.DeepEqual(got, []string{"server", "tool"}) {
		t.Errorf("MCPTarget keys = %v", got)
	}

	// ABSENT MEANS ABSENT. A call that reported nothing optional carries
	// none of the optional keys -- not an exitCode of 0, not an isError of
	// false, not an empty contents list.
	bare := Action{Type: ActionEventType, V: RecordVersion, Seq: 1, Turn: 1, ID: "c",
		Tool: ActionOther, AppTool: "Mystery", Args: json.RawMessage("null"), Cwd: "/w"}
	if got := jsonKeys(t, bare); !reflect.DeepEqual(got,
		[]string{"appTool", "args", "cwd", "id", "seq", "tool", "turn", "type", "v"}) {
		t.Errorf("a bare Action carries %v; every optional key must be absent", got)
	}
	if got := jsonKeys(t, Content{Op: ContentRead, Path: "/w/x"}); !reflect.DeepEqual(got, []string{"op", "path"}) {
		t.Errorf("a path-only Content carries %v", got)
	}

	// Zero is a fact when it is one: an empty file has zero bytes and
	// empty data, and both must travel.
	zero, empty := int64(0), ""
	body, _ := json.Marshal(Content{Op: ContentRead, Path: "/w/e", Bytes: &zero, Encoding: EncodingUTF8, Data: &empty})
	if string(body) != `{"op":"read","path":"/w/e","bytes":0,"encoding":"utf8","data":""}` {
		t.Errorf("an empty file marshals as %s", body)
	}
}

func TestFingerprintWireContract(t *testing.T) {
	entries, size := 4, int64(10)
	fp := Fingerprint{
		Type: FingerprintEventType, V: RecordVersion, Seq: 0, TakenAt: "2026-09-13T00:00:00Z",
		App:      FingerprintApp{ID: "claude-code", Version: "2.1.270 (Claude Code)", Harness: HarnessClaudeHeadless},
		Platform: Platform{OS: "linux", Arch: "amd64"},
		Tools:    []ToolVersion{{Name: "git", Version: "git version 2.43.0"}},
		Cwd:      "/w", CwdDigest: Digest(nil), CwdEntries: &entries, CwdTruncated: true,
		Variables: []Variable{{Name: "PATH", Set: true, Digest: Digest([]byte("/bin"))}},
		Inputs:    []Input{{Artifact: "art-1", Path: "/w/in.txt", Digest: Digest(nil), Bytes: &size, Omitted: OmittedTooLarge}},
	}
	want := []string{"app", "cwd", "cwdDigest", "cwdEntries", "cwdTruncated", "inputs",
		"platform", "seq", "takenAt", "tools", "type", "v", "variables"}
	if got := jsonKeys(t, fp); !reflect.DeepEqual(got, want) {
		t.Errorf("Fingerprint keys = %v, want %v", got, want)
	}
	if got := jsonKeys(t, fp.App); !reflect.DeepEqual(got, []string{"harness", "id", "version"}) {
		t.Errorf("FingerprintApp keys = %v", got)
	}
	if got := jsonKeys(t, fp.Inputs[0]); !reflect.DeepEqual(got,
		[]string{"artifact", "bytes", "digest", "omitted", "path"}) {
		t.Errorf("Input keys = %v", got)
	}
	// An unset variable says so; it does not vanish, because "unset" and
	// "never asked about" are different facts.
	if got := jsonKeys(t, Variable{Name: "TZ"}); !reflect.DeepEqual(got, []string{"name", "set"}) {
		t.Errorf("an unset Variable carries %v", got)
	}
	// seq 0 is the fingerprint's own number and must be ON the wire.
	body, _ := json.Marshal(fp)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	if decoded["seq"] != float64(0) {
		t.Errorf("seq = %v, want 0 present", decoded["seq"])
	}
}

func TestFingerprintVariablesAreNamedByTheHarness(t *testing.T) {
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	for _, word := range []string{HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP, ""} {
		names := FingerprintVariables(word)
		for _, common := range []string{"PATH", "SHELL", "LANG", "LC_ALL", "TZ"} {
			if !has(names, common) {
				t.Errorf("%q: %s is missing from %v", word, common, names)
			}
		}
		if has(names, "CODEX_HOME") {
			t.Errorf("%q names CODEX_HOME, which the cockpit makes fresh for every session", word)
		}
	}
	if !has(FingerprintVariables(HarnessClaudeHeadless), "CLAUDE_CONFIG_DIR") {
		t.Error("Claude Code's settings directory must be in its fingerprint")
	}
	// A copy every time: a caller that appends must not change the next
	// session's list.
	first := FingerprintVariables(HarnessCodexMCP)
	first[0] = "MUTATED"
	if FingerprintVariables(HarnessCodexMCP)[0] == "MUTATED" {
		t.Error("FingerprintVariables returned shared state")
	}
}

// TestSinkFuncCarriesAnActionAsAnEventChunk: a caller with no recording
// of its own to keep apart still receives every action, as the event
// chunk it would be on the wire.
func TestSinkFuncCarriesAnActionAsAnEventChunk(t *testing.T) {
	var streams, bodies []string
	sink := SinkFunc(func(stream string, data []byte) {
		streams = append(streams, stream)
		bodies = append(bodies, string(data))
	})
	sink.Record(Action{Type: ActionEventType, V: RecordVersion, Seq: 1, ID: "x", Tool: ActionOther,
		Args: json.RawMessage("null")})
	if len(streams) != 1 || streams[0] != StreamEvent {
		t.Fatalf("streams = %v, want one %q chunk", streams, StreamEvent)
	}
	var got Action
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil || got.Type != ActionEventType || got.ID != "x" {
		t.Errorf("chunk = %q (%v), want the action as JSON", bodies[0], err)
	}
	if bodies[0][len(bodies[0])-1] != '\n' {
		t.Errorf("chunk %q is not a line", bodies[0])
	}
	var none SinkFunc
	none.Record(Action{}) // must not panic
}

func TestContentForMakesRelativePathsAbsolute(t *testing.T) {
	if got := contentFor(ContentRead, "sub/../notes.txt", "/w"); len(got) != 1 || got[0].Path != "/w/notes.txt" {
		t.Errorf("contentFor = %+v, want /w/notes.txt", got)
	}
	if got := contentFor(ContentWrite, "/abs/x", "/w"); got[0].Path != "/abs/x" || got[0].Op != ContentWrite {
		t.Errorf("contentFor = %+v", got)
	}
	if got := contentFor(ContentRead, "  ", "/w"); got != nil {
		t.Errorf("an empty path named %+v", got)
	}
}

// --- the bookkeeping --------------------------------------------------

func TestRecordingNumbersDenselyAcrossTurnsInCompletionOrder(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{ID: "a", Tool: ActionExec})
	r.begin(Action{ID: "b", Tool: ActionFSRead})
	b, okB := r.complete("b", nil)
	a, okA := r.complete("a", nil)
	r.startTurn()
	r.begin(Action{ID: "c", Tool: ActionMCP})
	c, okC := r.complete("c", nil)
	if !okA || !okB || !okC {
		t.Fatal("a first completion was refused")
	}
	for i, got := range []Action{b, a, c} {
		if got.Seq != uint64(i+1) {
			t.Errorf("action %s has seq %d, want %d: seq counts completions, densely", got.ID, got.Seq, i+1)
		}
		if got.Type != ActionEventType || got.V != RecordVersion {
			t.Errorf("action %s was not stamped for the wire: %+v", got.ID, got)
		}
		if string(got.Args) != "null" {
			t.Errorf("action %s args = %s, want null when none were reported", got.ID, got.Args)
		}
	}
	if b.Turn != 1 || a.Turn != 1 || c.Turn != 2 {
		t.Errorf("turns = %d %d %d, want 1 1 2", b.Turn, a.Turn, c.Turn)
	}
}

func TestRecordingEmitsOneCallOnce(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{ID: "x", Tool: ActionExec})
	if _, ok := r.complete("x", nil); !ok {
		t.Fatal("first completion refused")
	}
	if _, ok := r.complete("x", nil); ok {
		t.Error("a repeated result recorded one call twice")
	}
	// A begin replayed after the call was emitted must not reopen it.
	r.begin(Action{ID: "x", Tool: ActionExec})
	if got := r.flush(); len(got) != 0 {
		t.Errorf("a replayed begin reopened an emitted call: %+v", got)
	}
}

func TestRecordingCompletionWithoutABeginStandsAlone(t *testing.T) {
	r := newRecording()
	r.startTurn()
	got, ok := r.complete("orphan", func(a *Action) {
		failed := false
		a.IsError = &failed
	})
	if !ok {
		t.Fatal("a result whose call was never seen was dropped; it is still a completed call")
	}
	if got.Tool != ActionOther || string(got.Args) != "null" || got.IsError == nil {
		t.Errorf("orphan = %+v, want an `other` call with no arguments and the reported verdict", got)
	}
}

func TestRecordingFlushRecordsWhatNeverFinished(t *testing.T) {
	r := newRecording()
	r.startTurn()
	code, failed := 0, false
	r.begin(Action{ID: "first", Tool: ActionFSWrite, Contents: []Content{{Op: ContentWrite, Path: "/w/a"}}})
	r.begin(Action{ID: "second", Tool: ActionExec, ExitCode: &code, IsError: &failed, ResultType: "string"})
	r.begin(Action{ID: "done", Tool: ActionExec})
	if _, ok := r.complete("done", nil); !ok {
		t.Fatal("complete refused")
	}
	open := r.flush()
	if len(open) != 2 || open[0].ID != "first" || open[1].ID != "second" {
		t.Fatalf("flush = %+v, want first then second, in the order they began", open)
	}
	for _, a := range open {
		if !a.Incomplete {
			t.Errorf("%s is not marked incomplete", a.ID)
		}
		if a.IsError != nil || a.ExitCode != nil || a.ResultType != "" || a.ResultDigest != "" {
			t.Errorf("%s kept a result nobody reported: %+v", a.ID, a)
		}
		if a.Contents != nil {
			t.Errorf("%s would have a file read for a call nobody saw finish", a.ID)
		}
	}
	if open[0].Seq != 2 || open[1].Seq != 3 {
		t.Errorf("seqs = %d, %d, want 2, 3 after the one completion", open[0].Seq, open[1].Seq)
	}
	if _, ok := r.complete("first", nil); ok {
		t.Error("a result arriving after the flush recorded the call a second time")
	}
	if again := r.flush(); len(again) != 0 {
		t.Errorf("a second flush found %+v", again)
	}
}

func TestRecordingIDlessCallsNeverPair(t *testing.T) {
	r := newRecording()
	r.startTurn()
	r.begin(Action{Tool: ActionExec})
	if got := r.flush(); len(got) != 0 {
		t.Errorf("a call with no id was held open: %+v", got)
	}
	if _, ok := r.complete("", nil); !ok {
		t.Error("an id-less completion was dropped")
	}
	if _, ok := r.complete("", nil); !ok {
		t.Error("a second id-less completion was dropped as a repeat; nothing says it is one")
	}
}

func TestRecordingIsANoOpWhenNil(t *testing.T) {
	var r *recording
	r.startTurn()
	r.begin(Action{ID: "x"})
	if _, ok := r.complete("x", nil); ok {
		t.Error("a nil recording emitted an action")
	}
	if got := r.flush(); got != nil {
		t.Errorf("a nil recording flushed %+v", got)
	}
}

func TestMergeCallKeepsWhatTheEndLeftOut(t *testing.T) {
	begun := Action{ID: "p", Tool: ActionFSWrite, AppTool: "patch_apply", Cwd: "/w",
		Args:     json.RawMessage(`{"changes":{"/w/a":{"type":"add","content":"x"}}}`),
		Contents: []Content{{Op: ContentWrite, Path: "/w/a"}}, Command: "c", Query: "q", URL: "u",
		MCP: &MCPTarget{Server: "s", Tool: "t"}, ParentID: "root"}
	ended := Action{ID: "p", Tool: ActionFSWrite, Args: json.RawMessage("null")}
	got := mergeCall(begun, ended)
	if string(got.Args) != string(begun.Args) || len(got.Contents) != 1 || got.Cwd != "/w" ||
		got.Command != "c" || got.Query != "q" || got.URL != "u" || got.MCP == nil ||
		got.ParentID != "root" || got.AppTool != "patch_apply" {
		t.Errorf("merge lost what only the begin said: %+v", got)
	}
	ended.Command = "ended"
	if mergeCall(begun, ended).Command != "ended" {
		t.Error("a field the end reported did not win")
	}
}
```

and `internal/worker/harness/result_test.go`:

```go
package harness

import (
	"encoding/json"
	"testing"
)

func TestResultShape(t *testing.T) {
	decoded := func(raw string) any {
		v, ok := decodeValue(json.RawMessage(raw))
		if !ok {
			t.Fatalf("fixture %q is not JSON", raw)
		}
		return v
	}
	cases := []struct {
		name       string
		value      any
		wantType   string
		wantDigest string
	}{
		{"no result at all", nil, "null", Digest([]byte("null"))},
		{"plain text", "notes.txt", "string", Digest([]byte("notes.txt"))},
		{"empty text", "", "string", Digest(nil)},
		// Text that is JSON is typed by what it parses as, and digested as
		// the exact text -- a command's output is the bytes it printed.
		{"text that is an object", `{"b": 1, "a": 2}`, "object", Digest([]byte(`{"b": 1, "a": 2}`))},
		{"text that is a number", "42", "number", Digest([]byte("42"))},
		{"text that is a boolean", "true", "boolean", Digest([]byte("true"))},
		{"text with JSON and more behind it", `{"a":1} trailing`, "string", Digest([]byte(`{"a":1} trailing`))},
		// A decoded value digests as canonical JSON: keys sorted, numbers
		// as written, nothing HTML-escaped.
		{"decoded object", decoded(`{"b":1.50,"a":[true,null],"h":"<&>"}`), "object",
			Digest([]byte(`{"a":[true,null],"b":1.50,"h":"<&>"}`))},
		{"decoded array", decoded(`[{"type":"tool_reference","tool_name":"x"}]`), "array",
			Digest([]byte(`[{"tool_name":"x","type":"tool_reference"}]`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, digest := resultShape(tc.value)
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}
			if digest != tc.wantDigest {
				t.Errorf("digest = %s, want %s", digest, tc.wantDigest)
			}
		})
	}
}

func TestDecodeValueTakesExactlyOneValue(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"a":1}`: true, ` [1,2] `: true, `"s"`: true, `null`: true,
		``: false, `   `: false, `1 2`: false, `{"a":`: false, `nope`: false,
	} {
		if _, got := decodeValue(json.RawMessage(raw)); got != ok {
			t.Errorf("decodeValue(%q) ok = %v, want %v", raw, got, ok)
		}
	}
	v, _ := decodeValue(json.RawMessage(`12345678901234567890`))
	if n, isNumber := v.(json.Number); !isNumber || n.String() != "12345678901234567890" {
		t.Errorf("a large number came back as %#v; numbers are kept as written", v)
	}
}

func TestDecodeTolerantKeepsTheFieldsThatFit(t *testing.T) {
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if !decodeTolerant(json.RawMessage(`{"id":"x","status":7}`), &got) {
		t.Fatal("a type change in one field refused the whole object")
	}
	if got.ID != "x" {
		t.Errorf("id = %q, want the field that did decode", got.ID)
	}
	if decodeTolerant(json.RawMessage(`{"id":`), &got) {
		t.Error("malformed JSON was accepted")
	}
}

func TestBlocksValue(t *testing.T) {
	one := []any{map[string]any{"type": "text", "text": "echo: ping"}}
	if got := blocksValue(one); got != "echo: ping" {
		t.Errorf("one text block = %#v, want its text", got)
	}
	two := []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}
	if got, ok := blocksValue(two).([]any); !ok || len(got) != 2 {
		t.Errorf("two text blocks = %#v, want the blocks: joining makes [a b] and [ab] one result", blocksValue(two))
	}
	image := []any{map[string]any{"type": "image", "data": "..."}}
	if _, ok := blocksValue(image).([]any); !ok {
		t.Error("a non-text block was reduced")
	}
	if got := blocksValue("already text"); got != "already text" {
		t.Errorf("text = %#v", got)
	}
}

func TestMCPResultValuePrefersTheText(t *testing.T) {
	v, _ := decodeValue(json.RawMessage(
		`{"content":[{"type":"text","text":"{\"a\":1}"}],"structuredContent":{"a":1},"_meta":null}`))
	if got := mcpResultValue(v); got != `{"a":1}` {
		t.Errorf("= %#v, want the text block, which is what Claude Code shows the model", got)
	}
	v, _ = decodeValue(json.RawMessage(`{"content":[],"structuredContent":{"a":1}}`))
	if got, ok := mcpResultValue(v).(map[string]any); !ok || got["a"] == nil {
		t.Errorf("= %#v, want the structured content when there is no single text", mcpResultValue(v))
	}
	v, _ = decodeValue(json.RawMessage(`{"content":[{"type":"image"}],"structuredContent":null}`))
	if _, ok := mcpResultValue(v).([]any); !ok {
		t.Errorf("= %#v, want the blocks", mcpResultValue(v))
	}
}

func TestArgsOfAndNulls(t *testing.T) {
	if got := string(argsOf(map[string]any{"b": nil, "a": json.RawMessage(`{"x":1}`)})); got != `{"a":{"x":1},"b":null}` {
		t.Errorf("argsOf = %s", got)
	}
	if !isNull(nil) || !isNull(json.RawMessage(" null ")) || isNull(json.RawMessage("{}")) {
		t.Error("isNull is wrong")
	}
	if string(rawOrNull(nil)) != "null" || string(rawOrNull(json.RawMessage(`{}`))) != "{}" {
		t.Error("rawOrNull is wrong")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/worker/harness/ -run 'Wire|Fingerprint|Recording|MergeCall|ContentFor|SinkFunc|ResultShape|DecodeValue|DecodeTolerant|BlocksValue|MCPResult|ArgsOf' 2>&1 | head -20`
Expected: build failure -- `undefined: Action`, `undefined: newRecording`, `undefined: resultShape`.

- [ ] **Step 3: Implement** -- create `internal/worker/harness/record.go`:

```go
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// record.go is the RECORDING's wire shape: memql-cockpit#440, the cockpit
// half of epic B in the engine's
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md
// (decisions D2, D5, D12 and D16).
//
// ONE SHAPE, WHICHEVER APP. Every tool call an app completes leaves the
// session as one `event` chunk carrying an Action, and the session's
// first event is a Fingerprint. Both are normalised here, on the machine,
// out of each app's own protocol -- Claude Code's tool_use / tool_result
// pairs, the Codex app-server's items, the Codex mcp-server's begin / end
// events -- so the engine never parses a vendor format. A reader that has
// to know Claude Code calls it `Bash` and Codex calls it
// `commandExecution` has been handed a parser for two formats nobody
// promised to keep, which is the thing this package exists to delete.
//
// NO PROTO CHANGE. AppSessionChunk already carries `stream` and a
// monotonic `seq`, and an `event` chunk is DEFINED as a JSON body. The
// type words below are namespaced for the reason resultEventType's is:
// the same stream carries the apps' own events verbatim, and Claude
// Code's last line is literally {"type":"result"}.
//
// THE ENGINE PARSES NONE OF THIS YET. Its half (memql#5396) turns these
// events into work-spine rows; until it lands, an engine at the pin
// appends every chunk to the session's bounded transcript and maps every
// event chunk onto a live progress line, which is where these land too.
// So this file DEFINES the shape rather than transcribing one, the way the
// model labels' `params` and `quant` were defined here first, and
// TestActionWireContract pins the names so a rename is a decision.
//
// EVERY UNKNOWN STAYS UNKNOWN. A fact the app did not report is ABSENT,
// never zero: an exit code nobody reported is not 0, a call whose result
// never arrived did not succeed, and a file this machine would not read
// has no digest. The engine records an absence as an absence; a zero it
// would record as a fact.

// The two event types, and the version both shapes carry.
const (
	// ActionEventType names an event chunk carrying one Action.
	ActionEventType = "memql.app_session.action"
	// FingerprintEventType names the session's first event chunk.
	FingerprintEventType = "memql.app_session.fingerprint"
	// RecordVersion moves only for a change a reader cannot skip over. An
	// added field is not one: a reader ignores a key it does not know.
	RecordVersion = 1
)

// The action kinds, the closed vocabulary Action.Tool is drawn from.
//
// The first five are the engine's step types (its sixth, app_answer, is
// written from the End, not from here). The last two are this side's,
// and the difference between them is the difference between "known to
// touch nothing" and "not known at all":
//
//   - agent is the app's own bookkeeping: loading a deferred tool,
//     keeping its to-do list, handing a sub-task to a sub-agent whose own
//     calls are recorded separately. Nothing outside the app moved.
//   - other is a tool this package cannot classify, and its effects are
//     UNKNOWN. That is the fail-closed reading: a replay has to treat the
//     call as one it cannot reproduce, where guessing `agent` would let a
//     procedure skip a call that sent a notification.
const (
	ActionExec    = "exec"
	ActionFSRead  = "fs_read"
	ActionFSWrite = "fs_write"
	ActionFetch   = "fetch"
	ActionMCP     = "mcp"
	ActionAgent   = "agent"
	ActionOther   = "other"
)

// What an action did to the file a Content names.
const (
	ContentRead   = "read"
	ContentWrite  = "write"
	ContentDelete = "delete"
)

// Why a Content carries no bytes: a closed set, because the engine
// records it as the file's omission and a replay reads each one
// differently. The first two still carry the digest, so the file can be
// compared; the rest could not be read at all.
const (
	// OmittedOverCeiling: larger than one file may be inline.
	OmittedOverCeiling = "over_ceiling"
	// OmittedOverBudget: earlier files in the same action spent the
	// action's inline budget.
	OmittedOverBudget = "over_budget"
	// OmittedTooLarge: too large to hash; the size is still reported.
	OmittedTooLarge = "too_large"
	// OmittedOutsideWorkspace: the path leaves the session's workspace.
	OmittedOutsideWorkspace = "outside_workspace"
	// OmittedScaffolding: the session's own files -- the MCP
	// configuration with the per-run bearer in it, the transcript.
	OmittedScaffolding = "session_scaffolding"
	// OmittedNotFound: nothing is at the path.
	OmittedNotFound = "not_found"
	// OmittedNotRegular: a directory, a device, a pipe or a socket.
	OmittedNotRegular = "not_regular"
	// OmittedUnreadable: it could not be opened or read.
	OmittedUnreadable = "unreadable"
)

// How inline bytes are carried.
const (
	EncodingUTF8   = "utf8"
	EncodingBase64 = "base64"
)

// DigestPrefix names the hash every digest in the recording is.
const DigestPrefix = "sha256:"

// Digest is the recording's digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// Action is one tool call an app completed, normalised.
//
// Every pointer is a fact that can be ABSENT, and absent means the app
// did not say -- never zero.
type Action struct {
	// Type is ActionEventType and V is RecordVersion.
	Type string `json:"type"`
	V    int    `json:"v"`
	// Seq numbers the session's actions densely from 1, in the order they
	// completed, across every turn. It is NOT the chunk seq: it counts
	// actions only, so a reader holding 1, 2 and 4 knows exactly one call
	// is missing, where a gap in the chunk seq cannot say whether the lost
	// chunk was a call or a line of prose. The fingerprint is 0.
	Seq uint64 `json:"seq"`
	// Turn is the session's turn the call completed in, from 1. A
	// follow-up is a new turn, so this is what ties a call to the prompt
	// that asked for it.
	Turn int `json:"turn"`
	// ID is the app's OWN id for the call -- Claude Code's tool_use id,
	// Codex's item or call id -- and the only join back to the app's own
	// transcript.
	ID string `json:"id"`
	// ParentID is the call this one ran inside, when the app said so
	// (Claude Code's parent_tool_use_id, on a sub-agent's calls).
	ParentID string `json:"parentId,omitempty"`
	// Tool is the kind, from the closed set above; AppTool is the app's
	// own name for the tool, kept as provenance and never branched on.
	Tool    string `json:"tool"`
	AppTool string `json:"appTool"`
	// Args is the call's arguments WHOLE, as the app expressed them:
	// Claude Code's tool input, or the Codex item's own fields for a call
	// the app-server reports as an item rather than as arguments.
	Args json.RawMessage `json:"args"`
	// Cwd is the directory the call ran in, as far as the app said. Codex
	// names it on every command. Claude Code names it once, on init, and
	// its Bash tool keeps a `cd` across calls without reporting one -- so
	// a command that moved carries the move in its own text.
	Cwd string `json:"cwd"`
	// Command is the command line an exec ran, as one string.
	Command string `json:"command,omitempty"`
	// MCP is the server and tool an mcp call reached.
	MCP *MCPTarget `json:"mcp,omitempty"`
	// URL and Query are what a fetch reached for.
	URL   string `json:"url,omitempty"`
	Query string `json:"query,omitempty"`
	// ExitCode is the status the app REPORTED for an exec; absent when it
	// reported none.
	ExitCode *int `json:"exitCode,omitempty"`
	// IsError is the app's own verdict on the call. Absent only when
	// nobody knows: an Incomplete call, or an end event this build could
	// not read.
	IsError *bool `json:"isError,omitempty"`
	// ResultType is the inferred JSON type of the result -- object,
	// array, string, number, boolean or null. Text that parses as JSON is
	// typed by what it parses as, so a command that printed an object
	// returned an object; any other text is a string.
	ResultType string `json:"resultType,omitempty"`
	// ResultDigest is over the result: its exact text when it is text, its
	// canonical JSON otherwise. (ResultType, ResultDigest) together are
	// the result's identity.
	ResultDigest string `json:"resultDigest,omitempty"`
	// Contents are the files the call read or wrote, one entry each.
	Contents []Content `json:"contents,omitempty"`
	// Incomplete marks a call the app STARTED that the session never saw
	// finish -- the process died, the turn was cancelled, or its result
	// was too large to read. Recorded rather than dropped, so a gap reads
	// as a gap.
	Incomplete bool `json:"incomplete,omitempty"`
}

// MCPTarget is where an MCP call went.
type MCPTarget struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// Content is one file an action touched.
//
// A harness fills Op and Path and nothing else. The SESSION reads the
// file, because what this machine lets leave it is the machine's policy
// rather than a protocol's (appsession/record.go).
type Content struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	// Digest is over the file's bytes as they stood when the call
	// completed -- the whole file, not the window the app looked at.
	Digest string `json:"digest,omitempty"`
	// Bytes is the file's size.
	Bytes *int64 `json:"bytes,omitempty"`
	// Encoding and Data carry the bytes inline when they fit: the text
	// itself for utf8, base64 otherwise. A present Data may be empty -- an
	// empty file is still a file.
	Encoding string  `json:"encoding,omitempty"`
	Data     *string `json:"data,omitempty"`
	// Omitted says why Data is absent, from the closed set above.
	Omitted string `json:"omitted,omitempty"`
}

// contentFor names one file for the session to read, made absolute
// against cwd when the app named it relatively.
func contentFor(op, path, cwd string) []Content {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	return []Content{{Op: op, Path: filepath.Clean(path)}}
}

// Fingerprint is what the world looked like when the session started
// (D16): the session's FIRST event, seq 0.
//
// Every part of it is a fact a later replay compares before it acts,
// which is why anything that could identify the person is a digest
// rather than a value -- a variable's value can carry a home directory or
// a token, and "the same as last time" needs only the hash.
//
// The files the session READ are not here, because at the start it has
// read none: each read is on its own action, as a Content with op read,
// digested at the moment it happened -- the only moment that says what
// the app saw.
type Fingerprint struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	Seq     uint64 `json:"seq"`
	TakenAt string `json:"takenAt"`
	// App is what drove the session. Harness is empty for an `open`
	// session, which a person drives rather than a protocol.
	App      FingerprintApp `json:"app"`
	Platform Platform       `json:"platform"`
	// Tools are the developer tools on this machine's PATH that recorded
	// commands are most likely to run, each as it reported its own
	// version. A tool that is not installed is not in the list.
	Tools []ToolVersion `json:"tools"`
	// Cwd is the workspace. CwdDigest is over its LISTING -- names and
	// kinds, never contents, never the session's own scaffolding -- and
	// CwdEntries counts the entries the digest covers. Both are absent
	// when the workspace could not be listed.
	Cwd          string `json:"cwd"`
	CwdDigest    string `json:"cwdDigest,omitempty"`
	CwdEntries   *int   `json:"cwdEntries,omitempty"`
	CwdTruncated bool   `json:"cwdTruncated,omitempty"`
	// Variables are the environment variables the harness names as the
	// ones that change what a command does, each as set-or-not plus a
	// digest of its value.
	Variables []Variable `json:"variables"`
	// Inputs are the Library artifacts the session was handed, as they
	// landed in the workspace before the app started.
	Inputs []Input `json:"inputs"`
}

// FingerprintApp names the app, its version as it reports itself, and
// the harness that drove it.
type FingerprintApp struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Harness string `json:"harness,omitempty"`
}

// Platform is the operating system and architecture, as Go names them.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// ToolVersion is one tool's own report of its version.
type ToolVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Variable is one environment variable: set or not, and a digest of its
// value when set. Unset and set-to-empty are different facts, so Set is
// carried even when false.
type Variable struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Digest string `json:"digest,omitempty"`
}

// Input is one Library artifact as it landed in the workspace.
type Input struct {
	Artifact string `json:"artifact"`
	Path     string `json:"path"`
	Digest   string `json:"digest,omitempty"`
	Bytes    *int64 `json:"bytes,omitempty"`
	Omitted  string `json:"omitted,omitempty"`
}

// fingerprintCommonVariables change what ANY command does: which binary
// a name resolves to, which shell reads it, how its output is sorted and
// worded, and what time it thinks it is.
var fingerprintCommonVariables = []string{"PATH", "SHELL", "LANG", "LC_ALL", "TZ"}

// FingerprintVariables returns the environment variables the fingerprint
// records for a session driven through harness word.
//
// The harness names them because only the harness knows its app. Claude
// Code reads its settings -- permissions, allowed tools, its own MCP
// servers -- from CLAUDE_CONFIG_DIR, and ANTHROPIC_MODEL changes which
// model answers. CODEX_HOME is deliberately NOT named: the cockpit points
// it at a fresh directory for every session, so its digest would differ
// on every start by construction and match nothing.
func FingerprintVariables(word string) []string {
	out := append([]string(nil), fingerprintCommonVariables...)
	if word == HarnessClaudeHeadless {
		out = append(out, "CLAUDE_CONFIG_DIR", "ANTHROPIC_MODEL")
	}
	return out
}
```

`internal/worker/harness/recording.go`:

```go
package harness

import "sync"

// recording.go pairs each call with its result and numbers what comes
// out, for one session.
//
// PER SESSION, NOT PER TURN. The action seq runs across every turn, which
// is what lets a reader see a gap anywhere in a session; and an id seen
// in one turn is remembered into the next, so a result replayed on resume
// cannot record one call twice.

// recording is one session's bookkeeping. Every method is a no-op on a
// nil receiver, so a harness that was never started records nothing
// rather than panicking.
type recording struct {
	mu   sync.Mutex
	seq  uint64
	turn int
	// pending are calls begun and not yet finished, by the app's id, and
	// order is the order they began in -- the order a flush reports them.
	pending map[string]*Action
	order   []string
	// completed are ids already emitted, so a repeat is dropped.
	completed map[string]bool
}

func newRecording() *recording {
	return &recording{pending: map[string]*Action{}, completed: map[string]bool{}}
}

// startTurn opens the session's next turn.
func (r *recording) startTurn() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.turn++
	r.mu.Unlock()
}

// begin records a call the app has started.
//
// A call is not an action until it completes, so nothing is emitted here.
// A begin for an id the session already holds or already emitted is
// dropped: an app that replays a started item on reattach must not open a
// second record of one call. A call with no id has nothing to pair its
// result with; its completion stands alone.
func (r *recording) begin(a Action) {
	if r == nil || a.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completed[a.ID] {
		return
	}
	if _, held := r.pending[a.ID]; held {
		return
	}
	call := a
	r.pending[a.ID] = &call
	r.order = append(r.order, a.ID)
}

// complete finishes the call with id and returns it numbered, or false
// when that call was already emitted.
//
// finish writes what the completion reported onto the call as it began.
// When the begin never arrived -- the line carrying it was too large to
// read -- finish writes onto a bare call instead, because a result the
// session can see is still a completed call. The bare call is `other`
// with no arguments, which is exactly what is known about it.
func (r *recording) complete(id string, finish func(*Action)) (Action, bool) {
	if r == nil {
		return Action{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" && r.completed[id] {
		return Action{}, false
	}
	call, held := r.pending[id]
	if held {
		delete(r.pending, id)
		r.order = removeID(r.order, id)
	} else {
		call = &Action{ID: id, Tool: ActionOther}
	}
	if finish != nil {
		finish(call)
	}
	if id != "" {
		r.completed[id] = true
	}
	return r.stamp(*call), true
}

// flush closes every call still open as Incomplete, in the order they
// began.
//
// It runs at the end of every turn. A call begun in a turn that has ended
// will never finish -- Claude Code's next turn is a new process, and a
// Codex turn that ended took its in-flight items with it -- so holding it
// open would lose it silently, which is the one thing a recording must
// not do. What it would have reported is unknown, so nothing about a
// result survives into the record, and no file is read for it.
func (r *recording) flush() []Action {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Action, 0, len(r.order))
	for _, id := range r.order {
		call := r.pending[id]
		if call == nil {
			continue
		}
		call.Incomplete = true
		call.IsError, call.ExitCode = nil, nil
		call.ResultType, call.ResultDigest = "", ""
		call.Contents = nil
		r.completed[id] = true
		out = append(out, r.stamp(*call))
	}
	r.pending = map[string]*Action{}
	r.order = nil
	return out
}

// stamp numbers an action for the wire. Callers hold r.mu.
func (r *recording) stamp(a Action) Action {
	r.seq++
	a.Type = ActionEventType
	a.V = RecordVersion
	a.Seq = r.seq
	a.Turn = r.turn
	a.Args = rawOrNull(a.Args)
	return a
}

func removeID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

// mergeCall lays the ended report of a call over its begun one: every
// field the end carries wins, and a field it left empty keeps what the
// begin said. An older Codex's patch_apply_end carries no changes, and
// taking it alone would record a write to nowhere.
func mergeCall(begun, ended Action) Action {
	out := ended
	if out.ID == "" {
		out.ID = begun.ID
	}
	if out.ParentID == "" {
		out.ParentID = begun.ParentID
	}
	if out.AppTool == "" {
		out.AppTool = begun.AppTool
	}
	if isNull(out.Args) {
		out.Args = begun.Args
	}
	if out.Cwd == "" {
		out.Cwd = begun.Cwd
	}
	if out.Command == "" {
		out.Command = begun.Command
	}
	if out.MCP == nil {
		out.MCP = begun.MCP
	}
	if out.URL == "" {
		out.URL = begun.URL
	}
	if out.Query == "" {
		out.Query = begun.Query
	}
	if len(out.Contents) == 0 {
		out.Contents = begun.Contents
	}
	return out
}
```

`internal/worker/harness/result.go`:

```go
package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// result.go reduces a call's result to the two facts the recording keeps
// about it -- its inferred JSON type and a digest -- and holds the small
// JSON helpers the app translations share.

// resultShape types and digests a result value.
//
// value is the result as the app reported it, reduced to what it carries
// (blocksValue, mcpResultValue); nil means the app reported no result at
// all, which types as null. Text is digested as its exact bytes -- the
// same bytes a file holding that text digests to -- and anything else as
// its canonical JSON, so two results that differ only in key order or
// spacing are one result.
func resultShape(value any) (typ, digest string) {
	if s, ok := value.(string); ok {
		return inferTextType(s), Digest([]byte(s))
	}
	canon, err := canonicalJSON(value)
	if err != nil {
		return "", ""
	}
	return jsonType(value), Digest(canon)
}

// inferTextType types text by what it parses as, and as a string when it
// is not JSON at all.
func inferTextType(s string) string {
	v, ok := decodeValue(json.RawMessage(s))
	if !ok {
		return "string"
	}
	return jsonType(v)
}

// jsonType names a decoded value's JSON type.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

// decodeValue decodes exactly one JSON value, numbers kept as written. It
// reports false for anything else, including a value with more text
// behind it -- "1 2" is not JSON, whatever a lenient reader makes of it.
func decodeValue(raw json.RawMessage) (any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// decodeTolerant decodes raw into v and keeps every field that decoded
// when one did not. A field whose TYPE changed in a later app release is
// then a fact not reported, rather than a whole call not recorded -- the
// tolerance every decoder in this package owes a binary its owner
// upgrades on their own schedule.
func decodeTolerant(raw json.RawMessage, v any) bool {
	err := json.Unmarshal(raw, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// canonicalJSON is the one encoding a value digests as: keys sorted (as
// encoding/json does for a map), no insignificant space, numbers as
// written, and no HTML escaping -- "<" is a byte the result had, not one
// to rewrite.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// blocksValue reduces MCP-style content blocks to what they carry: ONE
// text block is its text, and anything else stays the blocks.
//
// This is what makes one MCP call digest the same from both apps. Claude
// Code hands the model the server's blocks as the tool result, and the
// Codex app-server hands back the same blocks inside a result object --
// recorded from each on 2026-09-13, [{"type":"text","text":"echo: ping"}]
// both times. Several text blocks are NOT joined: ["a","b"] and ["ab"]
// are different results, and a join would make them one.
func blocksValue(v any) any {
	blocks, ok := v.([]any)
	if !ok || len(blocks) != 1 {
		return v
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || block["type"] != "text" {
		return v
	}
	if text, ok := block["text"].(string); ok {
		return text
	}
	return v
}

// mcpResultValue reduces an MCP CallToolResult to its value.
//
// The single text block wins over structuredContent. The MCP
// specification asks a server that returns structured content to return
// its serialised JSON as a text block too, and Claude Code shows the
// model only the blocks -- so preferring the text is what keeps a call
// through Codex digesting the same as the same call through Claude Code.
// The structured content stands in only when there is no single text.
func mcpResultValue(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return blocksValue(v)
	}
	content, hasContent := obj["content"]
	if hasContent {
		if text, ok := blocksValue(content).(string); ok {
			return text
		}
	}
	if structured, ok := obj["structuredContent"]; ok && structured != nil {
		return structured
	}
	if hasContent {
		return content
	}
	return v
}

// rawOrNull is raw, or JSON null when there is nothing in it: Args is
// always a JSON value on the wire, and null is how "no arguments were
// reported" is spelled.
func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// isNull reports whether raw is empty or JSON null.
func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// argsOf encodes arguments this package assembles itself, for a call an
// app reports as an item rather than as arguments.
func argsOf(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return body
}
```

and the Sink change:

In `internal/worker/harness/harness.go`, add `"encoding/json"` to the import block, then replace the `Sink` / `SinkFunc` block (from `// Sink receives a turn's output as it happens.` through the end of `func (f SinkFunc) Chunk`) with:

```go
// Sink receives a turn's output as it happens.
//
// Chunk is called from the harness's own goroutines and must not block
// for long; the session's chunk sender is the intended implementation and
// it is already sequenced. An error is not returned because there is
// nothing a harness could do with one -- the stream to the server dying
// is the session's problem, and it is watching for it.
//
// Record receives the RECORDING: one Action for every tool call the app
// completed (record.go). It is a method of its own rather than one more
// stream word because the two are bounded differently -- the session caps
// narration at limits.max_transcript_bytes and must never cap the
// recording -- and a stream word is one typo away from being the other.
type Sink interface {
	Chunk(stream string, data []byte)
	Record(a Action)
}

// SinkFunc adapts a function to Sink.
//
// It has no recording of its own to keep apart, so an Action reaches the
// function as the event chunk it would be on the wire. A caller that
// wants the recording sent past the transcript cap implements Sink
// itself, as the app-session runner does.
type SinkFunc func(stream string, data []byte)

// Chunk implements Sink.
func (f SinkFunc) Chunk(stream string, data []byte) {
	if f != nil {
		f(stream, data)
	}
}

// Record implements Sink.
func (f SinkFunc) Record(a Action) {
	if f == nil {
		return
	}
	body, err := json.Marshal(a)
	if err != nil {
		return
	}
	f(StreamEvent, append(body, '\n'))
}
```

In `internal/worker/harness/claudeheadless_test.go`, replace the `recorder` type and its `Chunk` method with:

```go
// recorder is a Sink that keeps every chunk in arrival order, and every
// Action the harness recorded.
type recorder struct {
	mu      sync.Mutex
	chunks  []recordedChunk
	actions []Action
}

func (r *recorder) Chunk(stream string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, recordedChunk{stream: stream, data: string(data)})
}

func (r *recorder) Record(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, a)
}

// recorded returns the actions, in the order the harness recorded them.
func (r *recorder) recorded() []Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Action(nil), r.actions...)
}
```

- [ ] **Step 4: Run the package's tests**

Run: `go test ./internal/worker/harness/ && go vet ./internal/worker/harness/`
Expected: `ok`. (Every existing harness test still passes: the vendor chunks are untouched, and the test `recorder` now satisfies the wider Sink.)

- [ ] **Step 5: Commit**

```bash
git add internal/worker/harness/record.go internal/worker/harness/recording.go internal/worker/harness/result.go \
  internal/worker/harness/harness.go internal/worker/harness/record_test.go internal/worker/harness/result_test.go \
  internal/worker/harness/claudeheadless_test.go
git commit -m "Issue #441: the recording's wire shape, per-session bookkeeping and result reduction"
```

---

## Task 2: Normalized action events from the Claude Code harness (#441)

**Files:**
- Create: `internal/worker/harness/claudeactions.go`
- Modify: `internal/worker/harness/claudeheadless.go` (hooks)
- Test: `internal/worker/harness/claudeactions_test.go`

**Interfaces:**
- Consumes: Task 1's `recording`, `contentFor`, `resultShape`, `blocksValue`, `decodeValue`, `decodeTolerant`, `rawOrNull`, `Sink.Record`; the test helpers `fakeClaude`, `prints`, `printsPerTurn`, `claudeSpec`, `startClaude`, `recorder` in `claudeheadless_test.go`.
- Produces: `claudeKind(name) string`, `claudeMCPTarget(name) *MCPTarget`, `claudeCall(id, name, input, parent, cwd) Action`, `claudeFinish(a *Action, content json.RawMessage, isError bool, record json.RawMessage)`, `claudeExitCode(args, result, isError, record) *int`; test helpers `runClaudeFixture`, `exitOf`, `errOf`, fixtures `claudeRecordedTurn`, `claudeRefusalsTurn` (Task 3's cross-app test reuses `runClaudeFixture` and `claudeRecordedTurn`).

- [ ] **Step 1: Write the failing test** -- `internal/worker/harness/claudeactions_test.go`:

```go
package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// claudeactions_test.go drives the Claude Code client against RECORDED
// tool traffic and asserts the recording it produces.
//
// Every fixture was captured from claude 2.1.270 on 2026-09-13 by running
// the real binary in a scratch directory holding notes.txt, with one stdio
// MCP server named `echo` whose one tool echoes its text back. The lines
// are trimmed to the fields a reader here could care about: the workspace
// path is shortened to /w, and the thinking, hook, rate-limit and
// thinking_tokens lines are dropped. Everything else -- including the
// ToolSearch call Claude Code makes to load the deferred MCP tool before
// it can call it -- is as printed.

// claudeRecordedTurn is `claude -p --output-format stream-json --verbose
// --model haiku --mcp-config <echo> --allowedTools "Read Bash Write Edit
// mcp__echo__echo" -- "<read notes.txt; run ls -1; run a command that
// exits 3; write out.txt; edit it; call the echo tool with ping>"`.
const claudeRecordedTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-rec","claude_code_version":"2.1.270","model":"claude-haiku-4-5-20251001","permissionMode":"default"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"I'll execute these steps in order. Starting with step 1:"}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01UJqot242Qo8TW9Y6KgRamm","name":"Read","input":{"file_path":"/w/notes.txt"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01UJqot242Qo8TW9Y6KgRamm","type":"tool_result","content":"1\talpha\n2\tbeta\n3\t"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"type":"text","file":{"filePath":"/w/notes.txt","content":"alpha\nbeta\n","numLines":3,"startLine":1,"totalLines":3}}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01T2zDSWmuhhNFhEmJgWnV8J","name":"Bash","input":{"command":"ls -1","description":"List files in current directory"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01T2zDSWmuhhNFhEmJgWnV8J","type":"tool_result","content":"notes.txt","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"stdout":"notes.txt","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_017mvXJGvxMPCGy7EwdyX68g","name":"Bash","input":{"command":"sh -c 'echo oops >&2; exit 3'","description":"Run shell command with error output and exit code 3"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Exit code 3\noops","is_error":true,"tool_use_id":"toolu_017mvXJGvxMPCGy7EwdyX68g"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":"Error: Exit code 3\noops"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01ULvDD2AmfTHJ8V4GN3ESkH","name":"Write","input":{"file_path":"/w/out.txt","content":"hello"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01ULvDD2AmfTHJ8V4GN3ESkH","type":"tool_result","content":"File created successfully at: /w/out.txt (file state is current in your context \u2014 no need to Read it back)"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"type":"create","filePath":"/w/out.txt","content":"hello","structuredPatch":[],"originalFile":null,"userModified":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_018Ru4q14KCxHfuz6PU77qPD","name":"Edit","input":{"replace_all":false,"file_path":"/w/out.txt","old_string":"hello","new_string":"goodbye"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_018Ru4q14KCxHfuz6PU77qPD","type":"tool_result","content":"The file /w/out.txt has been updated successfully. (file state is current in your context \u2014 no need to Read it back)"}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"filePath":"/w/out.txt","oldString":"hello","newString":"goodbye","originalFile":"hello","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-hello","\\ No newline at end of file","+goodbye","\\ No newline at end of file"]}],"userModified":false,"replaceAll":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01GbW6bALaYNmPfoiRmcHaNL","name":"ToolSearch","input":{"query":"select:mcp__echo__echo","max_results":1}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01GbW6bALaYNmPfoiRmcHaNL","content":[{"type":"tool_reference","tool_name":"mcp__echo__echo"}]}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":{"matches":["mcp__echo__echo"],"query":"select:mcp__echo__echo","total_deferred_tools":117}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01U1dXc3Da8DmxEaxmw4wQyf","name":"mcp__echo__echo","input":{"text":"ping"}}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01U1dXc3Da8DmxEaxmw4wQyf","type":"tool_result","content":[{"type":"text","text":"echo: ping"}]}]},"parent_tool_use_id":null,"session_id":"sess-rec","tool_use_result":[{"type":"text","text":"echo: ping"}]}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}]},"parent_tool_use_id":null,"session_id":"sess-rec"}
{"type":"result","subtype":"success","is_error":false,"num_turns":8,"session_id":"sess-rec","total_cost_usd":0.0542359,"usage":{"input_tokens":68,"output_tokens":1657},"result":"done"}`

// claudeRefusalsTurn is the same binary and day with --allowedTools
// "Bash" only, asked to run `grep zzz notes.txt`, to run `sleep 3` in the
// background, to WebFetch a page and to Write a file. The grep found
// nothing and exited 1, which Claude Code reports as a SUCCESS; the sleep
// has not exited at all; the fetch and the write were refused for want of
// a permission this run did not grant.
const claudeRefusalsTurn = `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-refusals"}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_013VJPmcoJCjDKYZxPCcD58Y","name":"Bash","input":{"command":"grep zzz notes.txt","description":"Search for zzz in notes.txt"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_013VJPmcoJCjDKYZxPCcD58Y","type":"tool_result","content":"(Bash completed with no output)","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"returnCodeInterpretation":"No matches found","noOutputExpected":false}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01PD6yUd3gwmAfzvjj1bUmYV","name":"Bash","input":{"command":"sleep 3","description":"Sleep for 3 seconds","run_in_background":true}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01PD6yUd3gwmAfzvjj1bUmYV","type":"tool_result","content":"Command running in background with ID: brccvjji8. Output is being written to: /tmp/tasks/brccvjji8.output. You will be notified when it completes. To check interim output, use Read on that file path.","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,"backgroundTaskId":"brccvjji8"}}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01SJBiTETMsStTt1hepwqiCH","name":"WebFetch","input":{"url":"https://example.com","prompt":"Fetch this page"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Claude requested permissions to use WebFetch, but you haven't granted it yet.","is_error":true,"tool_use_id":"toolu_01SJBiTETMsStTt1hepwqiCH"}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":"Error: Claude requested permissions to use WebFetch, but you haven't granted it yet."}
{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01KggUaE4U4D7rLtoagMrraN","name":"Write","input":{"file_path":"/w/denied.txt","content":"x"}}]},"parent_tool_use_id":null,"session_id":"sess-refusals"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Claude requested permissions to write to /w/denied.txt, but you haven't granted it yet.","is_error":true,"tool_use_id":"toolu_01KggUaE4U4D7rLtoagMrraN"}]},"parent_tool_use_id":null,"session_id":"sess-refusals","tool_use_result":"Error: Claude requested permissions to write to /w/denied.txt, but you haven't granted it yet."}
{"type":"result","subtype":"success","is_error":false,"num_turns":9,"session_id":"sess-refusals","total_cost_usd":0.02,"usage":{"input_tokens":40,"output_tokens":900},"result":"done"}`

// runClaudeFixture runs one turn of events through a fresh client and
// returns what the sink recorded.
func runClaudeFixture(t *testing.T, events string) (*recorder, TurnResult, error) {
	t.Helper()
	bin, _ := fakeClaude(t, prints(events))
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	res, err := h.Turn(context.Background(), "do the steps", rec)
	return rec, res, err
}

// exitOf and errOf spell a pointer fact so an absent one reads as
// "absent" in a failure message rather than as a zero.
func exitOf(a Action) string {
	if a.ExitCode == nil {
		return "absent"
	}
	return strconv.Itoa(*a.ExitCode)
}

func errOf(a Action) string {
	if a.IsError == nil {
		return "absent"
	}
	if *a.IsError {
		return "true"
	}
	return "false"
}

// TestClaudeRecordsOneActionPerCompletedCall is #441's first acceptance
// criterion: the recorded transcript yields exactly one event per
// completed tool call, numbered densely in the order the calls finished.
func TestClaudeRecordsOneActionPerCompletedCall(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	want := []struct {
		id, tool, appTool, exit, isError, resultType string
	}{
		{"toolu_01UJqot242Qo8TW9Y6KgRamm", ActionFSRead, "Read", "absent", "false", "string"},
		{"toolu_01T2zDSWmuhhNFhEmJgWnV8J", ActionExec, "Bash", "0", "false", "string"},
		{"toolu_017mvXJGvxMPCGy7EwdyX68g", ActionExec, "Bash", "3", "true", "string"},
		{"toolu_01ULvDD2AmfTHJ8V4GN3ESkH", ActionFSWrite, "Write", "absent", "false", "string"},
		{"toolu_018Ru4q14KCxHfuz6PU77qPD", ActionFSWrite, "Edit", "absent", "false", "string"},
		{"toolu_01GbW6bALaYNmPfoiRmcHaNL", ActionAgent, "ToolSearch", "absent", "false", "array"},
		{"toolu_01U1dXc3Da8DmxEaxmw4wQyf", ActionMCP, "mcp__echo__echo", "absent", "false", "string"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d actions, want one per completed call (%d): %+v", len(got), len(want), got)
	}
	for i, w := range want {
		a := got[i]
		if a.Seq != uint64(i+1) || a.Turn != 1 {
			t.Errorf("action %d: seq %d turn %d, want seq %d turn 1", i, a.Seq, a.Turn, i+1)
		}
		if a.Type != ActionEventType || a.V != RecordVersion {
			t.Errorf("action %d is not stamped for the wire: %s v%d", i, a.Type, a.V)
		}
		if a.ID != w.id || a.Tool != w.tool || a.AppTool != w.appTool {
			t.Errorf("action %d = %s %s %s, want %s %s %s", i, a.ID, a.Tool, a.AppTool, w.id, w.tool, w.appTool)
		}
		if exitOf(a) != w.exit || errOf(a) != w.isError || a.ResultType != w.resultType {
			t.Errorf("action %d (%s): exit %s isError %s type %q, want %s %s %q",
				i, a.AppTool, exitOf(a), errOf(a), a.ResultType, w.exit, w.isError, w.resultType)
		}
		if a.Cwd != "/w" {
			t.Errorf("action %d cwd = %q, want the init event's /w", i, a.Cwd)
		}
		if !strings.HasPrefix(a.ResultDigest, DigestPrefix) {
			t.Errorf("action %d digest = %q", i, a.ResultDigest)
		}
	}

	read, ls, write, edit, mcp := got[0], got[1], got[3], got[4], got[6]
	if !reflect.DeepEqual(read.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt"}}) {
		t.Errorf("Read contents = %+v", read.Contents)
	}
	if read.ResultDigest != Digest([]byte("1\talpha\n2\tbeta\n3\t")) {
		t.Errorf("Read digests %s, want the result the app saw", read.ResultDigest)
	}
	if ls.Command != "ls -1" || ls.ResultDigest != Digest([]byte("notes.txt")) {
		t.Errorf("ls = %q %s", ls.Command, ls.ResultDigest)
	}
	for _, w := range []Action{write, edit} {
		if !reflect.DeepEqual(w.Contents, []Content{{Op: ContentWrite, Path: "/w/out.txt"}}) {
			t.Errorf("%s contents = %+v", w.AppTool, w.Contents)
		}
	}
	if mcp.MCP == nil || *mcp.MCP != (MCPTarget{Server: "echo", Tool: "echo"}) {
		t.Errorf("mcp target = %+v", mcp.MCP)
	}
	if mcp.ResultDigest != Digest([]byte("echo: ping")) {
		t.Errorf("mcp digest %s, want the single text block's text", mcp.ResultDigest)
	}
	var args map[string]any
	if err := json.Unmarshal(mcp.Args, &args); err != nil || args["text"] != "ping" {
		t.Errorf("mcp args = %s, want the input whole", mcp.Args)
	}
}

// TestClaudeActionFollowsTheAppsOwnReport: the vendor chunk still goes out
// exactly as before, and the recording rides beside it -- it replaces
// nothing a reader of the transcript already had.
func TestClaudeActionFollowsTheAppsOwnReport(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if tool := rec.on(StreamTool); len(tool) != 14 {
		t.Errorf("tool chunks = %d, want the 7 calls and 7 results forwarded verbatim", len(tool))
	}
	if got := rec.joined(StreamText); !strings.Contains(got, "done") {
		t.Errorf("the prose stayed where it was? StreamText = %q", got)
	}
	for _, c := range rec.on(StreamEvent) {
		if strings.Contains(c, ActionEventType) {
			t.Errorf("an action went out as a plain chunk rather than through Record: %q", c)
		}
	}
}

// TestClaudeToolResultWithIsErrorSetsIsError is #441's second acceptance
// criterion, on the refusals Claude Code actually printed -- and the
// exit-status rule on the successes it reported that did NOT exit 0.
func TestClaudeToolResultWithIsErrorSetsIsError(t *testing.T) {
	rec, _, err := runClaudeFixture(t, claudeRefusalsTurn)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 4 {
		t.Fatalf("recorded %d actions, want 4: %+v", len(got), got)
	}
	grep, bg, fetch, write := got[0], got[1], got[2], got[3]
	if errOf(grep) != "false" || exitOf(grep) != "absent" {
		t.Errorf("grep that found nothing: isError %s exit %s, want false and ABSENT -- it exited 1",
			errOf(grep), exitOf(grep))
	}
	if errOf(bg) != "false" || exitOf(bg) != "absent" {
		t.Errorf("background sleep: isError %s exit %s, want false and ABSENT -- it has not exited",
			errOf(bg), exitOf(bg))
	}
	if fetch.Tool != ActionFetch || fetch.URL != "https://example.com" || errOf(fetch) != "true" {
		t.Errorf("refused fetch = %+v", fetch)
	}
	if write.Tool != ActionFSWrite || errOf(write) != "true" || write.Contents != nil {
		t.Errorf("refused write = %+v, want isError and no file read: nothing was written", write)
	}
}

func TestClaudeExitCodeIsReadOnlyWhereItWasReported(t *testing.T) {
	fg := json.RawMessage(`{"command":"ls"}`)
	cases := []struct {
		name    string
		args    json.RawMessage
		result  any
		isError bool
		record  string
		want    string
	}{
		{"failure opening Exit code", fg, "Exit code 3\noops", true, `"Error: Exit code 3\noops"`, "3"},
		{"negative status", fg, "Exit code -1", true, ``, "-1"},
		{"refusal", fg, "Claude requested permissions to use Bash, but you haven't granted it yet.", true, ``, "absent"},
		{"timeout", fg, "Command timed out after 2m 0.0s", true, ``, "absent"},
		{"clean success", fg, "notes.txt", false, `{"stdout":"notes.txt","stderr":"","interrupted":false}`, "0"},
		{"reinterpreted status", fg, "(Bash completed with no output)", false,
			`{"interrupted":false,"returnCodeInterpretation":"No matches found"}`, "absent"},
		{"backgrounded by the record", fg, "Command running in background", false,
			`{"interrupted":false,"backgroundTaskId":"b1"}`, "absent"},
		{"backgrounded by the input", json.RawMessage(`{"command":"sleep 3","run_in_background":true}`),
			"Command running in background", false, `{"interrupted":false}`, "absent"},
		{"interrupted", fg, "", false, `{"interrupted":true}`, "absent"},
		{"no record", fg, "notes.txt", false, ``, "absent"},
		{"record without the flag", fg, "notes.txt", false, `{"stdout":"notes.txt"}`, "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Action{ExitCode: claudeExitCode(tc.args, tc.result, tc.isError, json.RawMessage(tc.record))}
			if got := exitOf(a); got != tc.want {
				t.Errorf("exit = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestClaudeCallThatNeverAnsweredIsRecordedIncomplete: the process died
// with a call in flight. The call is recorded -- a gap that reads as a
// gap -- and nothing about its result is claimed.
func TestClaudeCallThatNeverAnsweredIsRecordedIncomplete(t *testing.T) {
	events := `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-cut"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_cut","name":"Write","input":{"file_path":"/w/half.txt","content":"x"}}]},"parent_tool_use_id":null,"session_id":"sess-cut"}`
	bin, _ := fakeClaude(t, prints(events)+"exit 9\n")
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "write", rec); err == nil {
		t.Fatal("a turn whose process exited 9 reported success")
	}
	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded %d actions, want the one call that never finished", len(got))
	}
	a := got[0]
	if !a.Incomplete || a.ID != "toolu_cut" || a.Tool != ActionFSWrite || a.Seq != 1 {
		t.Errorf("= %+v, want toolu_cut, incomplete, seq 1", a)
	}
	if a.IsError != nil || a.ResultType != "" || a.Contents != nil {
		t.Errorf("an unfinished call claimed a result: %+v", a)
	}
	if !strings.Contains(string(a.Args), "half.txt") {
		t.Errorf("args = %s, want what the app asked for", a.Args)
	}
}

// TestClaudeOversizeResultLeavesItsCallIncomplete: a result line past the
// parse bound is streamed out, never parsed -- so its call can only be
// recorded as unfinished, never as a success nobody saw.
func TestClaudeOversizeResultLeavesItsCallIncomplete(t *testing.T) {
	script := "printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_big\",\"name\":\"Read\",\"input\":{\"file_path\":\"/w/big.log\"}}]},\"session_id\":\"sess-big\"}'\n" +
		"s=xxxxxxxxxxxxxxxx\n" +
		"while [ ${#s} -lt 1400000 ]; do s=$s$s; done\n" +
		"printf '{\"type\":\"user\",\"message\":{\"content\":[{\"tool_use_id\":\"toolu_big\",\"type\":\"tool_result\",\"content\":\"%s\"}]},\"session_id\":\"sess-big\"}\\n' \"$s\"\n" +
		prints(`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-big","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`)
	bin, _ := fakeClaude(t, script)
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "read a big file", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 1 || !got[0].Incomplete || got[0].ID != "toolu_big" {
		t.Fatalf("recorded %+v, want toolu_big incomplete", got)
	}
}

// TestClaudeSubAgentCallsCarryTheirParent: a sub-agent's calls are the
// sub-agent's, and the call that spawned it is the only thing that says so.
func TestClaudeSubAgentCallsCarryTheirParent(t *testing.T) {
	events := `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-sub"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_task","name":"Task","input":{"description":"look","prompt":"list the files"}}]},"parent_tool_use_id":null,"session_id":"sess-sub"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_inner","name":"Bash","input":{"command":"ls"}}]},"parent_tool_use_id":"toolu_task","session_id":"sess-sub"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_inner","type":"tool_result","content":"a.txt","is_error":false}]},"parent_tool_use_id":"toolu_task","session_id":"sess-sub","tool_use_result":{"stdout":"a.txt","stderr":"","interrupted":false}}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_task","type":"tool_result","content":[{"type":"text","text":"one file: a.txt"}]}]},"parent_tool_use_id":null,"session_id":"sess-sub"}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-sub","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"done"}`
	rec, _, err := runClaudeFixture(t, events)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("recorded %d, want 2", len(got))
	}
	inner, task := got[0], got[1]
	if inner.ID != "toolu_inner" || inner.ParentID != "toolu_task" || inner.Tool != ActionExec || exitOf(inner) != "0" {
		t.Errorf("inner = %+v", inner)
	}
	if task.ID != "toolu_task" || task.ParentID != "" || task.Tool != ActionAgent {
		t.Errorf("task = %+v", task)
	}
}

// TestClaudeRecordingRunsAcrossTurns: the second turn is a new process,
// and its calls continue the session's numbering rather than restarting it.
func TestClaudeRecordingRunsAcrossTurns(t *testing.T) {
	turn := func(id string) string {
		return `{"type":"system","subtype":"init","cwd":"/w","session_id":"sess-two"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":"true"}}]},"parent_tool_use_id":null,"session_id":"sess-two"}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":"","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-two","tool_use_result":{"stdout":"","stderr":"","interrupted":false}}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-two","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`
	}
	bin, _ := fakeClaude(t, printsPerTurn(turn("toolu_first"), turn("toolu_second")))
	h := startClaude(t, claudeSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "one", rec); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := h.Turn(context.Background(), "two", rec); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 2 || got[0].Seq != 1 || got[0].Turn != 1 || got[1].Seq != 2 || got[1].Turn != 2 {
		t.Fatalf("recorded %+v, want seq 1 in turn 1 then seq 2 in turn 2", got)
	}
}

func TestClaudeRepeatedResultIsRecordedOnce(t *testing.T) {
	result := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_r","type":"tool_result","content":"ok","is_error":false}]},"parent_tool_use_id":null,"session_id":"sess-r"}`
	events := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_r","name":"Bash","input":{"command":"true"}}]},"parent_tool_use_id":null,"session_id":"sess-r"}
` + result + "\n" + result + `
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-r","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}`
	rec, _, err := runClaudeFixture(t, events)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := rec.recorded(); len(got) != 1 {
		t.Errorf("recorded %d actions for one call", len(got))
	}
}

func TestClaudeKindsAndMCPTargets(t *testing.T) {
	for name, want := range map[string]string{
		"Bash": ActionExec, "Read": ActionFSRead, "Grep": ActionFSRead, "Write": ActionFSWrite,
		"NotebookEdit": ActionFSWrite, "WebFetch": ActionFetch, "ToolSearch": ActionAgent,
		"StructuredOutput": ActionAgent, "Task": ActionAgent, "mcp__memql__query": ActionMCP,
		"PushNotification": ActionOther, "CronCreate": ActionOther, "SomethingNew": ActionOther,
	} {
		if got := claudeKind(name); got != want {
			t.Errorf("claudeKind(%q) = %q, want %q", name, got, want)
		}
	}
	for name, want := range map[string]*MCPTarget{
		"mcp__echo__echo":                 {Server: "echo", Tool: "echo"},
		"mcp__claude_ai_Gmail__get_draft": {Server: "claude_ai_Gmail", Tool: "get_draft"},
		"mcp__memql__query":               {Server: "memql", Tool: "query"},
		"mcp__noseparator":                nil,
		"mcp____tool":                     nil,
		"Bash":                            nil,
	} {
		got := claudeMCPTarget(name)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("claudeMCPTarget(%q) = %+v, want %+v", name, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/worker/harness/ -run 'TestClaude' 2>&1 | head -20`
Expected: build failure -- `undefined: claudeExitCode`, `undefined: claudeKind`, `undefined: claudeMCPTarget`.

- [ ] **Step 3: Implement** -- create `internal/worker/harness/claudeactions.go`:

```go
package harness

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// claudeactions.go turns Claude Code's tool traffic into Actions.
//
// Claude Code reports a call as a `tool_use` block on an assistant
// message -- id, name, input -- and its result as a `tool_result` block on
// a later user message -- tool_use_id, content, is_error -- with the
// tool's own structured record beside it on the envelope, as
// `tool_use_result`. All three were RECORDED from claude 2.1.270 on
// 2026-09-13 (claudeactions_test.go carries the lines). The pair is
// joined by id, and a call becomes an Action only when its result arrives.

// claudeToolKinds classifies Claude Code's own tool names.
//
// A name missing here is `other`, the fail-closed reading: 2.1.270
// already ships tools that schedule cron jobs, send push notifications
// and trigger remote agents, and a later release will ship more. MCP
// tools are told apart by their prefix instead (claudeKind).
var claudeToolKinds = map[string]string{
	"Bash": ActionExec,

	"Read": ActionFSRead,
	// Glob, Grep and LS read the filesystem but return listings and
	// matches, never one file's content, so they are reads with no
	// Contents.
	"Glob": ActionFSRead,
	"Grep": ActionFSRead,
	"LS":   ActionFSRead,

	"Write":        ActionFSWrite,
	"Edit":         ActionFSWrite,
	"MultiEdit":    ActionFSWrite,
	"NotebookEdit": ActionFSWrite,

	"WebFetch":  ActionFetch,
	"WebSearch": ActionFetch,

	// The app's own bookkeeping. Task and Agent hand work to a sub-agent,
	// whose own calls are recorded separately with this one as their
	// parentId; the rest load deferred tools, keep the to-do list, or are
	// the --json-schema answer itself (StructuredOutput).
	"Task":             ActionAgent,
	"Agent":            ActionAgent,
	"ToolSearch":       ActionAgent,
	"TodoWrite":        ActionAgent,
	"TaskCreate":       ActionAgent,
	"TaskGet":          ActionAgent,
	"TaskList":         ActionAgent,
	"TaskUpdate":       ActionAgent,
	"TaskOutput":       ActionAgent,
	"Skill":            ActionAgent,
	"EnterPlanMode":    ActionAgent,
	"ExitPlanMode":     ActionAgent,
	"AskUserQuestion":  ActionAgent,
	"StructuredOutput": ActionAgent,
}

// claudeMCPPrefix starts the name of every MCP tool Claude Code exposes:
// mcp__<server>__<tool>.
const claudeMCPPrefix = "mcp__"

// claudeKind classifies one tool name.
func claudeKind(name string) string {
	if strings.HasPrefix(name, claudeMCPPrefix) {
		return ActionMCP
	}
	if kind, ok := claudeToolKinds[name]; ok {
		return kind
	}
	return ActionOther
}

// claudeMCPTarget splits mcp__<server>__<tool>.
//
// Claude Code joins the two with a double underscore after rewriting the
// server's name into the characters it allows, so a server whose own name
// holds one cannot be split back unambiguously. The first "__" after the
// prefix ends the server, which is right for every name without one; a
// name that does not split at all names no target rather than a wrong one.
func claudeMCPTarget(name string) *MCPTarget {
	rest, ok := strings.CutPrefix(name, claudeMCPPrefix)
	if !ok {
		return nil
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return nil
	}
	return &MCPTarget{Server: server, Tool: tool}
}

// claudeToolInput is the part of a tool's input the recording reads.
type claudeToolInput struct {
	Command         string `json:"command"`
	FilePath        string `json:"file_path"`
	NotebookPath    string `json:"notebook_path"`
	URL             string `json:"url"`
	Query           string `json:"query"`
	RunInBackground bool   `json:"run_in_background"`
}

// claudeCall builds the call half of an Action from a tool_use block.
func claudeCall(id, name string, input json.RawMessage, parent, cwd string) Action {
	a := Action{
		ID:       id,
		ParentID: parent,
		Tool:     claudeKind(name),
		AppTool:  name,
		Args:     rawOrNull(input),
		Cwd:      cwd,
	}
	var in claudeToolInput
	_ = decodeTolerant(input, &in)
	switch a.Tool {
	case ActionExec:
		a.Command = in.Command
	case ActionFetch:
		a.URL, a.Query = in.URL, in.Query
	case ActionMCP:
		a.MCP = claudeMCPTarget(name)
	}
	// Only the whole-file tools name a file whose content is the point of
	// the call.
	switch name {
	case "Read":
		a.Contents = contentFor(ContentRead, in.FilePath, cwd)
	case "Write", "Edit", "MultiEdit":
		a.Contents = contentFor(ContentWrite, in.FilePath, cwd)
	case "NotebookEdit":
		a.Contents = contentFor(ContentWrite, in.NotebookPath, cwd)
	}
	return a
}

// claudeFinish writes what a tool_result reported onto its call.
//
// A FAILED call reads no file: a Write that was refused wrote nothing, and
// what sits at its path is somebody else's content.
func claudeFinish(a *Action, content json.RawMessage, isError bool, record json.RawMessage) {
	failed := isError
	a.IsError = &failed
	value, _ := decodeValue(content)
	result := blocksValue(value)
	a.ResultType, a.ResultDigest = resultShape(result)
	if a.Tool == ActionExec {
		a.ExitCode = claudeExitCode(a.Args, result, isError, record)
	}
	if isError {
		a.Contents = nil
	}
}

// claudeExitLine is how a failed Bash call reports its status: the
// result's text opens "Exit code <n>" (recorded: "Exit code 3\noops").
var claudeExitLine = regexp.MustCompile(`^Exit code (-?\d+)`)

// claudeExitCode is the exit status Claude Code REPORTED for a Bash call,
// or nil when it reported none.
//
// Claude Code puts no exit status on the wire as a field, so both answers
// below are read out of what it does say, and each is claimed only where
// it is certain -- every case recorded from 2.1.270:
//
//   - a failed call's text opens "Exit code <n>", and n is the status. A
//     failure that says anything else -- a permission refusal, a timeout
//     -- reported no status, and none is claimed.
//   - a call that succeeded exited 0 ONLY when its structured record says
//     it ran to the end in the foreground and Claude Code did not
//     reinterpret the status. `grep` finding nothing exits 1 and comes
//     back as a success carrying returnCodeInterpretation "No matches
//     found"; a call sent to the background has not exited at all.
//     Reading 0 into either would record a fact that is false.
func claudeExitCode(args json.RawMessage, result any, isError bool, record json.RawMessage) *int {
	if isError {
		text, _ := result.(string)
		m := claudeExitLine.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		return &n
	}
	var in claudeToolInput
	if decodeTolerant(args, &in) && in.RunInBackground {
		return nil
	}
	var rec struct {
		Interrupted              *bool  `json:"interrupted"`
		BackgroundTaskID         string `json:"backgroundTaskId"`
		ReturnCodeInterpretation string `json:"returnCodeInterpretation"`
	}
	if json.Unmarshal(record, &rec) != nil || rec.Interrupted == nil || *rec.Interrupted {
		return nil
	}
	if rec.BackgroundTaskID != "" || strings.TrimSpace(rec.ReturnCodeInterpretation) != "" {
		return nil
	}
	zero := 0
	return &zero
}
```

then hook it into the client:

Edits to `internal/worker/harness/claudeheadless.go` (hooks only; the translation is `claudeactions.go`):

1. Replace the event-type and block-type constant blocks:

```go
const (
	claudeTypeSystem    = "system"
	claudeTypeAssistant = "assistant"
	claudeTypeUser      = "user"
	claudeTypeResult    = "result"
)

// claudeSubtypeInit is the system event that opens every turn; it names
// the working directory the app runs in.
const claudeSubtypeInit = "init"

// Content block types inside an assistant or user message. The last two
// are the recording's (claudeactions.go): a call, and its result.
const (
	claudeBlockText       = "text"
	claudeBlockThinking   = "thinking"
	claudeBlockToolUse    = "tool_use"
	claudeBlockToolResult = "tool_result"
)
```

2. `claudeHeadless` gains, after `running bool`:

```go
	// rec is the session's recording. It outlives every turn's process,
	// because the action seq runs across the whole session.
	rec *recording
```

3. In `Start`, before `h.started = true`: `h.rec = newRecording()`.

4. In `Turn`: read `rec` with the rest under the lock
   (`spec, ref, started, closed, running, rec := h.spec, h.ref, h.started, h.closed, h.running, h.rec`),
   call `rec.startTurn()` on the line after the `defer func() { ... h.running = false ... }()` block,
   build the turn as `turn := &claudeTurn{sink: sink, rec: rec, cwd: spec.Workspace}`, and after
   `pumps.Wait()` add:

```go

	// A call begun in this turn and never answered never will be: the next
	// turn is a new process with no memory of this one's ids. It is
	// recorded as incomplete rather than dropped (recording.flush).
	for _, a := range rec.flush() {
		turn.record(a)
	}
```

5. `claudeTurn` gains `rec *recording` (after `sinkMu`, commented "the session's recording, shared by every turn") and, under "Written by the stdout pump only.", `cwd string` (commented "the working directory every call this turn makes is recorded in: the workspace, until the app's own init event names it"). After `emit`, add:

```go
// record hands one finished call to the sink, under the lock every chunk
// takes, so an action never lands between two halves of something else.
func (t *claudeTurn) record(a Action) {
	if t.sink == nil {
		return
	}
	t.sinkMu.Lock()
	defer t.sinkMu.Unlock()
	t.sink.Record(a)
}
```

6. In `route`, after the session-id capture, add the init cwd, and pass the envelope to `routeBlocks`:

```go
	// The app names its working directory once, on init, and every call
	// it makes is recorded there (see Action.Cwd for what that does and
	// does not promise).
	if ev.Type == claudeTypeSystem {
		var env claudeRecordEnvelope
		if decodeTolerant(trimmed, &env) && env.Subtype == claudeSubtypeInit && strings.TrimSpace(env.Cwd) != "" {
			t.cwd = env.Cwd
		}
	}
```

   and the assistant/user arm becomes:

```go
	case claudeTypeAssistant, claudeTypeUser:
		var env claudeRecordEnvelope
		_ = decodeTolerant(trimmed, &env)
		if t.routeBlocks(ev.Message, env, line) {
			return
		}
		t.emit(StreamEvent, line)
```

7. Replace `routeBlocks` with:

```go
// routeBlocks splits a message's content across the streams and reports
// whether it understood the shape.
//
// A false return means the caller forwards the whole envelope instead:
// `content` is a plain string in some message shapes, and guessing a
// stream for a shape this client does not recognise is how a chunk ends
// up on the wrong side of the text / structure split.
//
// It is also where the RECORDING is fed: a tool_use block opens a call, a
// tool_result block closes it, and a call that closed goes to the sink as
// an Action right after the envelope that reported it.
func (t *claudeTurn) routeBlocks(msg json.RawMessage, env claudeRecordEnvelope, line []byte) bool {
	if len(msg) == 0 {
		return false
	}
	var message claudeMessage
	if err := json.Unmarshal(msg, &message); err != nil || len(message.Content) == 0 {
		return false
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(message.Content, &raws); err != nil || len(raws) == 0 {
		return false
	}
	blocks := make([]*claudeContentBlock, len(raws))
	results := 0
	for i, raw := range raws {
		var b claudeContentBlock
		if !decodeTolerant(raw, &b) {
			continue
		}
		blocks[i] = &b
		if b.Type == claudeBlockToolResult {
			results++
		}
	}
	parent := ""
	if env.ParentToolUseID != nil {
		parent = strings.TrimSpace(*env.ParentToolUseID)
	}
	// The envelope's structured record belongs to ONE result. With several
	// on one message nothing says which it describes, so none of them
	// reads it -- which costs a Bash call its exit status of 0 and never
	// invents one.
	var record json.RawMessage
	if results == 1 {
		record = env.ToolUseResult
	}

	structural := false
	var finished []Action
	for _, b := range blocks {
		if b == nil {
			structural = true
			continue
		}
		switch b.Type {
		case claudeBlockText:
			t.emit(StreamText, []byte(b.Text))
		case claudeBlockThinking:
			// The block's `signature` is an opaque blob with no reader
			// on this side and a length that would dominate a
			// transcript, so only the prose leaves.
			t.emit(StreamText, []byte(b.Thinking))
		case claudeBlockToolUse:
			structural = true
			t.rec.begin(claudeCall(b.ID, b.Name, b.Input, parent, t.cwd))
		case claudeBlockToolResult:
			structural = true
			if a, ok := t.rec.complete(b.ToolUseID, func(a *Action) {
				if a.ParentID == "" {
					a.ParentID = parent
				}
				if a.Cwd == "" {
					a.Cwd = t.cwd
				}
				claudeFinish(a, b.Content, b.IsError, record)
			}); ok {
				finished = append(finished, a)
			}
		default:
			structural = true
		}
	}
	if structural {
		t.emit(StreamTool, line)
	}
	// After the app's own report of the call, never before: a reader of
	// the transcript sees the call, then what the recording made of it.
	for _, a := range finished {
		t.record(a)
	}
	return true
}
```

8. `claudeContentBlock` gains, after `Thinking`:

```go
	// A call (tool_use) and its result (tool_result), for the recording.
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
```

   and after `claudeContentBlock` add:

```go
// claudeRecordEnvelope is what the recording reads off an envelope. It is
// decoded APART from claudeEvent, so that a field of an unexpected type
// here can cost the recording a fact but can never cost a line its route.
type claudeRecordEnvelope struct {
	Subtype         string          `json:"subtype"`
	Cwd             string          `json:"cwd"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	ToolUseResult   json.RawMessage `json:"tool_use_result"`
}
```

- [ ] **Step 4: Run the package's tests**

Run: `go test ./internal/worker/harness/ -count=1 && go vet ./internal/worker/harness/`
Expected: `ok` -- the new tests and every existing claude-headless test.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/harness/claudeactions.go internal/worker/harness/claudeactions_test.go internal/worker/harness/claudeheadless.go
git commit -m "Issue #441: one normalized action per completed Claude Code tool call"
```

---

## Task 3: The same events from both Codex harnesses (#442)

**Files:**
- Create: `internal/worker/harness/codexactions.go`, `internal/worker/harness/fuzz_test.go`
- Modify: `internal/worker/harness/codexappserver.go`, `internal/worker/harness/codexmcp.go` (hooks)
- Test: `internal/worker/harness/codexactions_test.go`

**Interfaces:**
- Consumes: Task 1's bookkeeping and helpers; Task 2's `runClaudeFixture`, `claudeRecordedTurn`, `exitOf`, `errOf`; the existing fakes `fakeCodexAppServer`, `codexSpec`, `startCodexAppServer`, `fakeCodexMCP`, `startCodexMCP`, `codexMCPToolOK`.
- Produces: `codexItemCall(raw, workspace) (Action, codexItem, bool)`, `codexItemFinish(*Action, codexItem)`, `codexCoreCall(codexCoreEvent, workspace) (Action, int, bool)`, `codexCoreFinish(*Action, codexCoreEvent)`, `shellJoin([]string) string`, `codexPath(string) string`; `jsonrpcConn.record(Action)`.

- [ ] **Step 1: Write the failing tests** -- `internal/worker/harness/codexactions_test.go`:

```go
package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// codexactions_test.go drives both Codex clients against recorded tool
// traffic and asserts the recording, then holds the two apps to one shape.
//
// codexTurnRecorded was CAPTURED from codex-cli 0.153.4 on 2026-09-13:
// `codex app-server -c 'mcp_servers.echo...'` driven through initialize,
// thread/start {cwd, approvalPolicy: never} and one turn/start asking for
// the same six steps the Claude Code fixture took. Codex read notes.txt
// with `cat`, wrote out.txt with `printf` and edited it with `sed -i` --
// shell commands, all of them, which is exactly why its reads are found
// through its own command parse. Item lines are as printed with the
// workspace path shortened to /w; the account, rate-limit, MCP-startup and
// token-usage notifications are dropped. The fake answers with the lines
// verbatim through a quoted heredoc, so no shell ever rewrites them.
const codexTurnRecorded = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_rec","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365994259,"item":{"type":"commandExecution","id":"exec-b77282f9-db64-4899-96a4-18794035bba6","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'cat notes.txt'","cwd":"/w","processId":"97104","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"read","command":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365994259,"item":{"type":"commandExecution","id":"exec-b77282f9-db64-4899-96a4-18794035bba6","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'cat notes.txt'","cwd":"/w","processId":"97104","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"read","command":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"aggregatedOutput":"alpha\nbeta\n","exitCode":0,"durationMs":0}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365996388,"item":{"type":"commandExecution","id":"exec-02337035-bc20-401a-a4e4-630a60d68442","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'ls -1'","cwd":"/w","processId":"97074","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365996388,"item":{"type":"commandExecution","id":"exec-02337035-bc20-401a-a4e4-630a60d68442","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc 'ls -1'","cwd":"/w","processId":"97074","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"listFiles","command":"ls -1","path":null}],"aggregatedOutput":"notes.txt\n","exitCode":0,"durationMs":0}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789365998809,"item":{"type":"commandExecution","id":"exec-d8855f30-ddea-4313-a829-8f981ca16799","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sh -c 'echo oops >&2; exit 3'\"","cwd":"/w","processId":"39845","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"sh -c 'echo oops >&2; exit 3'"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789365998809,"item":{"type":"commandExecution","id":"exec-d8855f30-ddea-4313-a829-8f981ca16799","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sh -c 'echo oops >&2; exit 3'\"","cwd":"/w","processId":"39845","source":"unifiedExecStartup","status":"failed","commandActions":[{"type":"unknown","command":"sh -c 'echo oops >&2; exit 3'"}],"aggregatedOutput":"oops\n","exitCode":3,"durationMs":0}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366001388,"item":{"type":"commandExecution","id":"exec-a9bda5da-698f-4906-ae4c-a5eba582397d","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"printf 'hello\\\\n' > out.txt\"","cwd":"/w","processId":"68520","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"printf 'hello\\n' > out.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366001388,"item":{"type":"commandExecution","id":"exec-a9bda5da-698f-4906-ae4c-a5eba582397d","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"printf 'hello\\\\n' > out.txt\"","cwd":"/w","processId":"68520","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"unknown","command":"printf 'hello\\n' > out.txt"}],"aggregatedOutput":null,"exitCode":0,"durationMs":0}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366004490,"item":{"type":"commandExecution","id":"exec-c4568bae-9956-48d6-9e08-8445424f7bfd","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sed -i 's/hello/goodbye/' out.txt\"","cwd":"/w","processId":"67768","source":"unifiedExecStartup","status":"inProgress","commandActions":[{"type":"unknown","command":"sed -i 's/hello/goodbye/' out.txt"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366004491,"item":{"type":"commandExecution","id":"exec-c4568bae-9956-48d6-9e08-8445424f7bfd","pluginId":null,"scriptPath":null,"command":"/bin/bash -lc \"sed -i 's/hello/goodbye/' out.txt\"","cwd":"/w","processId":"67768","source":"unifiedExecStartup","status":"completed","commandActions":[{"type":"unknown","command":"sed -i 's/hello/goodbye/' out.txt"}],"aggregatedOutput":null,"exitCode":0,"durationMs":0}}}
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_rec","startedAtMs":1789366009184,"item":{"type":"mcpToolCall","id":"exec-6df80e5a-51b0-4d75-aa52-79e22177519a","server":"echo","tool":"echo","status":"inProgress","arguments":{"text":"ping"},"appContext":null,"pluginId":null,"readOnlyHint":null,"result":null,"error":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366009187,"item":{"type":"mcpToolCall","id":"exec-6df80e5a-51b0-4d75-aa52-79e22177519a","server":"echo","tool":"echo","status":"completed","arguments":{"text":"ping"},"appContext":null,"pluginId":null,"readOnlyHint":null,"result":{"content":[{"type":"text","text":"echo: ping"}],"structuredContent":null,"_meta":null},"error":null,"durationMs":2}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_rec","completedAtMs":1789366011232,"item":{"type":"agentMessage","id":"msg_0bf177e203eb66a2016aa78efb1d3087d0a97f0552752a34a9","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_rec","items":[],"itemsView":"notLoaded","status":"completed","error":null,"startedAt":1789365989,"completedAt":1789366011,"durationMs":21513}}}
CODEX_JSON
`

// codexTurnFileChange is a fileChange item in the v2 ThreadItem shape
// (codex-cli 0.153.4 generate-ts: {type, id, changes: [{path, kind, diff}],
// status}). The recorded turn has none -- that Codex wrote with the shell
// -- so this one is BUILT from the type rather than captured, and says so.
const codexTurnFileChange = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_fc","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_fc","startedAtMs":1,"item":{"type":"fileChange","id":"call_patch","changes":[{"path":"/w/out.txt","kind":{"type":"add"},"diff":"hello\n"}],"status":"inProgress"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":2,"item":{"type":"fileChange","id":"call_patch","changes":[{"path":"/w/out.txt","kind":{"type":"add"},"diff":"hello\n"},{"path":"/w/old.txt","kind":{"type":"delete"},"diff":""},{"path":"/w/a.txt","kind":{"type":"update","move_path":"/w/b.txt"},"diff":"@@"},{"path":"rel.txt","kind":{"type":"update","move_path":null},"diff":"@@"}],"status":"completed"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":3,"item":{"type":"fileChange","id":"call_refused","changes":[{"path":"/w/nope.txt","kind":{"type":"add"},"diff":"x"}],"status":"declined"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"THREAD","turnId":"turn_fc","completedAtMs":4,"item":{"type":"agentMessage","id":"msg_fc","text":"done","phase":"final_answer","memoryCitation":null,"delivery":null,"questions":null}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_fc","items":[],"itemsView":"notLoaded","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}
CODEX_JSON
`

// codexTurnLeavesACallOpen starts a command and ends the turn interrupted
// before the command completes.
const codexTurnLeavesACallOpen = `
printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn_open","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":null,"completedAt":null,"durationMs":null}}}\n' "$id"
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"THREAD","turnId":"turn_open","startedAtMs":1,"item":{"type":"commandExecution","id":"exec-open","command":"/bin/bash -lc 'sleep 60'","cwd":"/w","processId":null,"source":"agent","status":"inProgress","commandActions":[{"type":"unknown","command":"sleep 60"}],"aggregatedOutput":null,"exitCode":null,"durationMs":null}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"THREAD","turn":{"id":"turn_open","items":[],"itemsView":"notLoaded","status":"interrupted","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}
CODEX_JSON
`

// codexMCPToolRecorded is the fallback's core events for the same calls,
// in the field spellings of codex-rs/protocol/src/protocol.rs at
// rust-v0.153.4 (ExecCommandBeginEvent / EndEvent, PatchApplyBeginEvent /
// EndEvent, McpToolCallBeginEvent / EndEvent, ViewImageToolCallEvent),
// with the cwd and image path as the file:// URLs PathUri serialises to.
// The patch_apply_end carries no `changes` -- the field is
// `#[serde(default)]`, and an older Codex does not send it -- and the
// last command never ends.
const codexMCPToolRecorded = `
cat <<'CODEX_JSON'
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_exec","turn_id":"turn_1","command":["bash","-lc","cat notes.txt"],"cwd":"file:///w","parsed_cmd":[{"type":"read","cmd":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"call_exec","turn_id":"turn_1","command":["bash","-lc","cat notes.txt"],"cwd":"file:///w","parsed_cmd":[{"type":"read","cmd":"cat notes.txt","name":"notes.txt","path":"/w/notes.txt"}],"source":"agent","stdout":"alpha\nbeta\n","stderr":"","aggregated_output":"alpha\nbeta\n","exit_code":0,"duration":{"secs":0,"nanos":1000000},"formatted_output":"alpha\nbeta\n","status":"completed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_fail","turn_id":"turn_1","command":["bash","-lc","sh -c 'echo oops >&2; exit 3'"],"cwd":"file:///w","parsed_cmd":[{"type":"unknown","cmd":"sh -c 'echo oops >&2; exit 3'"}],"source":"agent"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_end","call_id":"call_fail","turn_id":"turn_1","command":["bash","-lc","sh -c 'echo oops >&2; exit 3'"],"cwd":"file:///w","parsed_cmd":[{"type":"unknown","cmd":"sh -c 'echo oops >&2; exit 3'"}],"source":"agent","stdout":"","stderr":"oops\n","aggregated_output":"oops\n","exit_code":3,"duration":{"secs":0,"nanos":1000000},"formatted_output":"oops\n","status":"failed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"patch_apply_begin","call_id":"call_patch","turn_id":"turn_1","auto_approved":true,"changes":{"/w/out.txt":{"type":"add","content":"hello\n"}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"patch_apply_end","call_id":"call_patch","turn_id":"turn_1","stdout":"Success. Updated the following files:\nA /w/out.txt\n","stderr":"","success":true,"status":"completed"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"mcp_tool_call_begin","call_id":"call_mcp","invocation":{"server":"echo","tool":"echo","arguments":{"text":"ping"}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"mcp_tool_call_end","call_id":"call_mcp","invocation":{"server":"echo","tool":"echo","arguments":{"text":"ping"}},"duration":{"secs":0,"nanos":2000000},"result":{"Ok":{"content":[{"type":"text","text":"echo: ping"}],"isError":false}}},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"view_image_tool_call","call_id":"call_img","path":"file:///w/shot.png"},"_meta":{"requestId":1}}}
{"jsonrpc":"2.0","method":"codex/event","params":{"id":"sub_1","msg":{"type":"exec_command_begin","call_id":"call_never","turn_id":"turn_1","command":["sleep","60"],"cwd":"file:///w","parsed_cmd":[],"source":"agent"},"_meta":{"requestId":1}}}
CODEX_JSON
printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"done"}],"structuredContent":{"threadId":"THREAD","content":"done"}}}\n' "$id"
`

func runCodexAppServer(t *testing.T, body string) ([]Action, Spec, error) {
	t.Helper()
	bin, _ := fakeCodexAppServer(t, body)
	spec := codexSpec(t, bin)
	h := startCodexAppServer(t, spec)
	rec := &recorder{}
	_, err := h.Turn(context.Background(), "do the steps", rec)
	return rec.recorded(), spec, err
}

// TestCodexAppServerRecordsOneActionPerCompletedItem: one action per
// completed tool item, in order, with Codex's own exit codes.
func TestCodexAppServerRecordsOneActionPerCompletedItem(t *testing.T) {
	got, spec, err := runCodexAppServer(t, codexTurnRecorded)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	want := []struct {
		tool, command, exit, isError string
	}{
		{ActionExec, "/bin/bash -lc 'cat notes.txt'", "0", "false"},
		{ActionExec, "/bin/bash -lc 'ls -1'", "0", "false"},
		{ActionExec, `/bin/bash -lc "sh -c 'echo oops >&2; exit 3'"`, "3", "true"},
		{ActionExec, `/bin/bash -lc "printf 'hello\\n' > out.txt"`, "0", "false"},
		{ActionExec, `/bin/bash -lc "sed -i 's/hello/goodbye/' out.txt"`, "0", "false"},
		{ActionMCP, "", "absent", "false"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d actions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		a := got[i]
		if a.Seq != uint64(i+1) || a.Turn != 1 || a.Tool != w.tool || a.Command != w.command {
			t.Errorf("action %d = seq %d turn %d %s %q, want seq %d turn 1 %s %q",
				i, a.Seq, a.Turn, a.Tool, a.Command, i+1, w.tool, w.command)
		}
		if exitOf(a) != w.exit || errOf(a) != w.isError {
			t.Errorf("action %d: exit %s isError %s, want %s %s", i, exitOf(a), errOf(a), w.exit, w.isError)
		}
	}
	cat, printf, mcp := got[0], got[3], got[5]
	if !reflect.DeepEqual(cat.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt"}}) {
		t.Errorf("cat contents = %+v, want Codex's own parse of the read", cat.Contents)
	}
	if cat.ResultDigest != Digest([]byte("alpha\nbeta\n")) || cat.Cwd != "/w" || cat.AppTool != "commandExecution" {
		t.Errorf("cat = %+v", cat)
	}
	if printf.ResultType != "string" || printf.ResultDigest != Digest(nil) || printf.Contents != nil {
		t.Errorf("printf = %+v, want the empty text it printed and no read", printf)
	}
	if mcp.MCP == nil || *mcp.MCP != (MCPTarget{Server: "echo", Tool: "echo"}) || mcp.Cwd != spec.Workspace {
		t.Errorf("mcp = %+v", mcp)
	}
	if mcp.ResultDigest != Digest([]byte("echo: ping")) || mcp.ResultType != "string" {
		t.Errorf("mcp result = %s %s", mcp.ResultType, mcp.ResultDigest)
	}
}

func TestCodexAppServerFileChangeIsAWrite(t *testing.T) {
	got, spec, err := runCodexAppServer(t, codexTurnFileChange)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recorded %d, want the change and the refusal: %+v", len(got), got)
	}
	change, refused := got[0], got[1]
	if change.Tool != ActionFSWrite || change.AppTool != "fileChange" || errOf(change) != "false" || change.ResultType != "null" {
		t.Errorf("change = %+v", change)
	}
	want := []Content{
		{Op: ContentWrite, Path: "/w/out.txt"},
		{Op: ContentDelete, Path: "/w/old.txt"},
		{Op: ContentDelete, Path: "/w/a.txt"},
		{Op: ContentWrite, Path: "/w/b.txt"},
		{Op: ContentWrite, Path: filepath.Join(spec.Workspace, "rel.txt")},
	}
	if !reflect.DeepEqual(change.Contents, want) {
		t.Errorf("contents = %+v, want %+v: the COMPLETED item's changes, a move as a delete and a write", change.Contents, want)
	}
	if !strings.Contains(string(change.Args), `"changes"`) {
		t.Errorf("args = %s", change.Args)
	}
	if refused.ID != "call_refused" || errOf(refused) != "true" || refused.Contents != nil {
		t.Errorf("a declined change = %+v, want isError and no file read", refused)
	}
}

func TestCodexAppServerTurnThatEndsWithOpenItemsRecordsThemIncomplete(t *testing.T) {
	got, _, err := runCodexAppServer(t, codexTurnLeavesACallOpen)
	if err == nil {
		t.Fatal("an interrupted turn reported success")
	}
	if len(got) != 1 || !got[0].Incomplete || got[0].ID != "exec-open" || got[0].IsError != nil {
		t.Fatalf("recorded %+v, want exec-open incomplete with no verdict", got)
	}
	if got[0].Command != "/bin/bash -lc 'sleep 60'" {
		t.Errorf("command = %q, want what the started item said", got[0].Command)
	}
}

func TestCodexMCPFallbackRecordsCoreEvents(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPToolRecorded)
	spec := codexSpec(t, bin)
	h := startCodexMCP(t, spec)
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "do the steps", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 6 {
		t.Fatalf("recorded %d actions, want 6: %+v", len(got), got)
	}
	exec, fail, patch, mcp, img, never := got[0], got[1], got[2], got[3], got[4], got[5]
	if exec.Command != "bash -lc 'cat notes.txt'" || exec.Cwd != "/w" || exitOf(exec) != "0" || errOf(exec) != "false" {
		t.Errorf("exec = %+v", exec)
	}
	if !reflect.DeepEqual(exec.Contents, []Content{{Op: ContentRead, Path: "/w/notes.txt"}}) ||
		exec.ResultDigest != Digest([]byte("alpha\nbeta\n")) {
		t.Errorf("exec read = %+v %s", exec.Contents, exec.ResultDigest)
	}
	if exitOf(fail) != "3" || errOf(fail) != "true" || fail.Command != `bash -lc 'sh -c '\''echo oops >&2; exit 3'\'''` {
		t.Errorf("failed exec = %+v", fail)
	}
	if patch.Tool != ActionFSWrite || errOf(patch) != "false" ||
		!reflect.DeepEqual(patch.Contents, []Content{{Op: ContentWrite, Path: "/w/out.txt"}}) {
		t.Errorf("patch = %+v, want the begin's changes kept when the end carries none", patch)
	}
	if mcp.MCP == nil || mcp.MCP.Server != "echo" || mcp.ResultDigest != Digest([]byte("echo: ping")) || errOf(mcp) != "false" {
		t.Errorf("mcp = %+v", mcp)
	}
	if img.Tool != ActionFSRead || !reflect.DeepEqual(img.Contents, []Content{{Op: ContentRead, Path: "/w/shot.png"}}) {
		t.Errorf("image view = %+v", img)
	}
	if never.ID != "call_never" || !never.Incomplete || never.Command != "sleep 60" {
		t.Errorf("never-ended exec = %+v", never)
	}
}

// TestCodexMCPEndThatCarriesNoOutputRecordsNoResult: the older end event
// in codexMCPToolOK carries an exit code and nothing it printed. The
// result is unknown, and unknown is absent -- not the digest of nothing.
func TestCodexMCPEndThatCarriesNoOutputRecordsNoResult(t *testing.T) {
	bin, _ := fakeCodexMCP(t, codexMCPToolOK)
	h := startCodexMCP(t, codexSpec(t, bin))
	rec := &recorder{}
	if _, err := h.Turn(context.Background(), "list", rec); err != nil {
		t.Fatalf("turn: %v", err)
	}
	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded %+v", got)
	}
	a := got[0]
	if a.Command != "ls -a" || exitOf(a) != "0" || errOf(a) != "false" || a.Cwd != "/w" {
		t.Errorf("= %+v, want the begin's command and cwd with the end's status", a)
	}
	if a.ResultType != "" || a.ResultDigest != "" {
		t.Errorf("result = %q %q, want absent", a.ResultType, a.ResultDigest)
	}
}

// TestRecordingIsTheSameShapeFromBothApps is #442's acceptance criterion:
// the same actions, from Claude Code's fixture and from the Codex
// app-server's, record in the same shape -- the same keys, the same kind,
// the same verdict, and where the apps report the same bytes, the same
// digest. What legitimately differs is named: the app's own id and tool
// word, the arguments as each app expressed them, and a result each app
// words its own way (Claude Code's Bash drops the trailing newline Codex
// keeps; its Write answers in prose where a Codex file change answers
// nothing).
func TestRecordingIsTheSameShapeFromBothApps(t *testing.T) {
	cRec, _, err := runClaudeFixture(t, claudeRecordedTurn)
	if err != nil {
		t.Fatalf("claude turn: %v", err)
	}
	claude := cRec.recorded()
	codex, _, err := runCodexAppServer(t, codexTurnRecorded)
	if err != nil {
		t.Fatalf("codex turn: %v", err)
	}
	change, _, err := runCodexAppServer(t, codexTurnFileChange)
	if err != nil {
		t.Fatalf("codex file-change turn: %v", err)
	}
	pairs := []struct {
		name          string
		claude, codex Action
		sameResult    bool
	}{
		{"a command that succeeded", claude[1], codex[1], false},
		{"a command that exited 3", claude[2], codex[2], false},
		{"a file written", claude[3], change[0], false},
		{"an MCP call", claude[6], codex[5], true},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if a, b := jsonKeys(t, p.claude), jsonKeys(t, p.codex); !reflect.DeepEqual(a, b) {
				t.Errorf("keys differ:\n claude %v\n codex  %v", a, b)
			}
			if p.claude.Tool != p.codex.Tool || exitOf(p.claude) != exitOf(p.codex) || errOf(p.claude) != errOf(p.codex) {
				t.Errorf("claude %s exit %s isError %s, codex %s exit %s isError %s",
					p.claude.Tool, exitOf(p.claude), errOf(p.claude), p.codex.Tool, exitOf(p.codex), errOf(p.codex))
			}
			if p.claude.Tool == ActionExec && p.claude.ResultType != p.codex.ResultType {
				t.Errorf("result types %q and %q", p.claude.ResultType, p.codex.ResultType)
			}
			if p.sameResult && (p.claude.ResultDigest != p.codex.ResultDigest || p.claude.ResultType != p.codex.ResultType) {
				t.Errorf("one server answer digests two ways: %s %s vs %s %s",
					p.claude.ResultType, p.claude.ResultDigest, p.codex.ResultType, p.codex.ResultDigest)
			}
			if p.claude.Tool == ActionFSWrite &&
				(len(p.codex.Contents) == 0 || !reflect.DeepEqual(p.claude.Contents[0], p.codex.Contents[0])) {
				t.Errorf("the one file both wrote: claude %+v, codex %+v", p.claude.Contents, p.codex.Contents)
			}
		})
	}
	// The MCP call's arguments are the one argument set both apps pass
	// through untouched, so they must be the same value.
	var a, b any
	_ = json.Unmarshal(claude[6].Args, &a)
	_ = json.Unmarshal(codex[5].Args, &b)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(claude[6].MCP, codex[5].MCP) {
		t.Errorf("mcp args %s vs %s, target %+v vs %+v", claude[6].Args, codex[5].Args, claude[6].MCP, codex[5].MCP)
	}
	// And the same file read -- Claude Code's Read tool, Codex's `cat` --
	// names the same file for the session to digest, though the calls are
	// different kinds.
	if !reflect.DeepEqual(claude[0].Contents, codex[0].Contents) {
		t.Errorf("the one read of notes.txt: claude %+v, codex %+v", claude[0].Contents, codex[0].Contents)
	}
}

func TestCodexItemsThatAreNotCallsAreNotRecorded(t *testing.T) {
	for _, raw := range []string{
		`{"type":"agentMessage","id":"m","text":"hi","phase":"final_answer"}`,
		`{"type":"reasoning","id":"r","summary":[],"content":[]}`,
		`{"type":"userMessage","id":"u","content":[]}`,
		`not json`,
	} {
		if _, _, ok := codexItemCall(json.RawMessage(raw), "/w"); ok {
			t.Errorf("%s was recorded as a call", raw)
		}
	}
	a, _, ok := codexItemCall(json.RawMessage(`{"type":"webSearch","id":"w","query":"go","action":{"type":"openPage","url":"https://go.dev"}}`), "/w")
	if !ok || a.Tool != ActionFetch || a.Query != "go" || a.URL != "https://go.dev" {
		t.Errorf("web search = %+v", a)
	}
}

func TestShellJoin(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"ls", "-a"}, "ls -a"},
		{[]string{"bash", "-lc", "ls -1"}, "bash -lc 'ls -1'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"printf", ""}, "printf ''"},
		{[]string{"=cmd", "a=b"}, "'=cmd' a=b"},
		{[]string{"go", "test", "./..."}, "go test ./..."},
	} {
		if got := shellJoin(tc.argv); got != tc.want {
			t.Errorf("shellJoin(%q) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}

func TestCodexPath(t *testing.T) {
	for in, want := range map[string]string{
		"file:///w/notes.txt":           "/w/notes.txt",
		"file:///w/with%20space.txt":    "/w/with space.txt",
		"file://localhost/w/a":          "/w/a",
		"file://elsewhere/share/a":      "",
		"/plain/path":                   "/plain/path",
		"":                              "",
		"  file:///w/trimmed.txt  ":     "/w/trimmed.txt",
	} {
		if got := codexPath(in); got != want {
			t.Errorf("codexPath(%q) = %q, want %q", in, got, want)
		}
	}
}
```

and the fuzz targets, `internal/worker/harness/fuzz_test.go`:

```go
package harness

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Fuzz targets for the recording's two translations that take input from
// an app. Each asserts the PROPERTY that would actually go wrong, never
// merely "does not panic": nothing here panics, and the failure worth
// finding is a plausible answer that is wrong.

// splitShellWords reads a command line the way a POSIX shell reads the
// only two forms shellJoin writes: bare words, and single-quoted runs
// joined by the '\'' escape.
func splitShellWords(line string) ([]string, bool) {
	var words []string
	var word strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
				continue
			}
			word.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\':
			if i+1 >= len(line) {
				return nil, false
			}
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		return nil, false
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, true
}

// FuzzShellJoinRoundTrips. The fallback's exec events carry an argv, and
// the recording carries it as one command line. The property: a shell
// reading that line back gets EXACTLY the argv the app ran -- the same
// words, the same boundaries, nothing expanded. A rendering that did not
// round-trip would record a command the app never ran, and a replay would
// run it.
func FuzzShellJoinRoundTrips(f *testing.F) {
	for _, seed := range [][2]string{
		{"bash", "-lc"}, {"sh -c 'echo oops >&2; exit 3'", ""}, {"it's", "a\"b"},
		{"=cmd", "$HOME"}, {"a b", "`id`"}, {"*.go", "~"}, {"\n", "\t"}, {"", "'"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		argv := []string{"cmd", a, b}
		line := shellJoin(argv)
		got, ok := splitShellWords(line)
		if !ok || len(got) != len(argv) {
			t.Fatalf("shellJoin(%q) = %s, which reads back as %q", argv, line, got)
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Fatalf("shellJoin(%q) = %s: word %d reads back as %q", argv, line, i, got[i])
			}
		}
		// A word left bare is only ever made of characters no shell reads
		// specially -- checked against a list of its own, not against the
		// set shellWord trusts, so a character added there by mistake is
		// caught here.
		for _, w := range argv {
			if shellWord(w) != w {
				continue
			}
			if w == "" || w[0] == '=' || strings.ContainsAny(w, " \t\n'\"\\$`*?[]{}~!;&|<>()#^") {
				t.Fatalf("shellWord left %q bare", w)
			}
		}
	})
}

var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// FuzzResultShapeIsClosed. Whatever an app returns, the recording types
// it from the six JSON types the engine reads and digests it in the one
// shape it compares -- and a type other than "string" is only ever given
// to text that really is that JSON. A result typed "object" that does not
// parse would make a replay compare the wrong way.
func FuzzResultShapeIsClosed(f *testing.F) {
	for _, seed := range []string{
		"", "notes.txt", `{"a":1}`, `[1,2]`, "42", "true", "null", `"quoted"`,
		`{"a":1} trailing`, "\xff\xfe", " 7 ", "1e400", `{"a":`,
	} {
		f.Add(seed)
	}
	closed := map[string]bool{"object": true, "array": true, "string": true, "number": true, "boolean": true, "null": true}
	f.Fuzz(func(t *testing.T, text string) {
		typ, digest := resultShape(text)
		if !closed[typ] || !digestShape.MatchString(digest) {
			t.Fatalf("resultShape(%q) = %q %q", text, typ, digest)
		}
		if digest != Digest([]byte(text)) {
			t.Fatalf("text digests as something other than its own bytes")
		}
		if typ != "string" && !json.Valid([]byte(text)) {
			t.Fatalf("resultShape(%q) typed text that is not JSON as %q", text, typ)
		}
		if v, ok := decodeValue(json.RawMessage(text)); ok {
			if again, _ := resultShape(v); !closed[again] {
				t.Fatalf("decoded %q typed as %q", text, again)
			}
		}
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/worker/harness/ -run 'Codex|SameShape|ShellJoin' 2>&1 | head -20`
Expected: build failure -- `undefined: codexItemCall`, `undefined: shellJoin`, `undefined: codexPath`.

- [ ] **Step 3: Implement** -- create `internal/worker/harness/codexactions.go`:

```go
package harness

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// codexactions.go turns both Codex protocols' tool activity into Actions.
//
// The app-server reports a call as a ThreadItem, twice: on item/started
// with status inProgress, and on item/completed with its outcome. Every
// field read below is in the ts-rs types `codex app-server generate-ts`
// printed from codex-cli 0.153.4 on 2026-09-13 (v2/ThreadItem.ts and its
// neighbours), and the command, MCP and failed-exit shapes were recorded
// from that binary driving a real turn the same day (codexactions_test.go
// carries the lines).
//
// The mcp-server fallback reports the same calls as begin / end pairs of
// core events keyed by call_id, with the fields of
// codex-rs/protocol/src/protocol.rs at rust-v0.153.4 -- the last tag that
// ships the crate.
//
// A CODEX READ IS A COMMAND. Codex has no read tool; it runs `cat` or
// `sed -n` through its shell, and says so in the command's own parse
// (`commandActions`, or `parsed_cmd` on the fallback). That parse is the
// only way a Codex read can carry the digest of what it saw, so an exec
// whose parse names a read carries a Content for it -- the call is still
// the exec it was.

// codexItem is the part of an app-server ThreadItem the recording reads.
// A field a variant does not have decodes as its zero value, which every
// reader below takes as "not reported".
type codexItem struct {
	Type              string               `json:"type"`
	ID                string               `json:"id"`
	Status            string               `json:"status"`
	Command           string               `json:"command"`
	Cwd               string               `json:"cwd"`
	CommandActions    []codexCommandAction `json:"commandActions"`
	AggregatedOutput  *string              `json:"aggregatedOutput"`
	ExitCode          *int                 `json:"exitCode"`
	Changes           json.RawMessage      `json:"changes"`
	Server            string               `json:"server"`
	Tool              string               `json:"tool"`
	Arguments         json.RawMessage      `json:"arguments"`
	Result            json.RawMessage      `json:"result"`
	Error             *codexItemError      `json:"error"`
	Query             string               `json:"query"`
	Action            json.RawMessage      `json:"action"`
	Results           json.RawMessage      `json:"results"`
	ContentItems      json.RawMessage      `json:"contentItems"`
	Success           *bool                `json:"success"`
	Path              string               `json:"path"`
	Prompt            *string              `json:"prompt"`
	ReceiverThreadIDs []string             `json:"receiverThreadIds"`
	DurationMs        *int64               `json:"durationMs"`
	Name              string               `json:"name"`
	Namespace         *string              `json:"namespace"`
	Output            json.RawMessage      `json:"output"`
	RevisedPrompt     *string              `json:"revisedPrompt"`
	Failure           json.RawMessage      `json:"failure"`
}

// codexCommandAction is one entry of Codex's own parse of a command:
// v2 CommandAction on the app-server, ParsedCommand on the fallback. Both
// are tagged by `type`, and both call a read "read" and carry its path.
type codexCommandAction struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type codexItemError struct {
	Message string `json:"message"`
}

// codexItemCall builds the call half of an Action from a ThreadItem, and
// reports false for an item that is not a tool call at all -- the
// assistant's prose, its reasoning, its plan.
func codexItemCall(raw json.RawMessage, workspace string) (Action, codexItem, bool) {
	var it codexItem
	if !decodeTolerant(raw, &it) {
		return Action{}, it, false
	}
	a := Action{ID: it.ID, AppTool: it.Type, Cwd: workspace}
	switch it.Type {
	case "commandExecution":
		a.Tool = ActionExec
		a.Command = it.Command
		a.Args = argsOf(map[string]any{"command": it.Command})
		if strings.TrimSpace(it.Cwd) != "" {
			a.Cwd = it.Cwd
		}
		a.Contents = codexReadContents(it.CommandActions, a.Cwd)
	case "fileChange":
		a.Tool = ActionFSWrite
		a.Args = argsOf(map[string]any{"changes": rawOrNull(it.Changes)})
		a.Contents = codexChangeContents(it.Changes, a.Cwd)
	case "mcpToolCall":
		a.Tool = ActionMCP
		a.Args = rawOrNull(it.Arguments)
		a.MCP = &MCPTarget{Server: it.Server, Tool: it.Tool}
	case "dynamicToolCall":
		a.Tool = ActionOther
		a.Args = rawOrNull(it.Arguments)
	case "webSearch":
		a.Tool = ActionFetch
		a.Args = argsOf(map[string]any{"query": it.Query, "action": rawOrNull(it.Action)})
		a.Query = it.Query
		a.URL = codexActionURL(it.Action)
	case "imageView":
		a.Tool = ActionFSRead
		a.Args = argsOf(map[string]any{"path": it.Path})
		a.Contents = contentFor(ContentRead, it.Path, a.Cwd)
	case "imageGeneration":
		a.Tool = ActionOther
		a.Args = argsOf(map[string]any{"revisedPrompt": it.RevisedPrompt})
	case "collabAgentToolCall":
		a.Tool = ActionAgent
		a.Args = argsOf(map[string]any{"tool": it.Tool, "prompt": it.Prompt, "receiverThreadIds": it.ReceiverThreadIDs})
	case "sleep":
		a.Tool = ActionAgent
		a.Args = argsOf(map[string]any{"durationMs": it.DurationMs})
	case "functionCallOutput":
		a.Tool = ActionOther
		a.Args = argsOf(map[string]any{"name": it.Name, "namespace": it.Namespace})
	default:
		return Action{}, it, false
	}
	return a, it, true
}

// codexItemFinish writes what a completed item reported onto its call.
//
// `completed` is the only status that means the call did what it was
// asked; failed and declined are the two that do not, and anything newer
// is read the same way, because a status this build has not heard of is
// not evidence of success.
func codexItemFinish(a *Action, it codexItem) {
	statusFailed := it.Status != "completed"
	failed := false
	var result any
	switch it.Type {
	case "commandExecution":
		failed = statusFailed || (it.ExitCode != nil && *it.ExitCode != 0)
		a.ExitCode = it.ExitCode
		// A command that printed nothing reports null, and its result is
		// the empty text it produced -- recorded 2026-09-13 from a
		// `printf ... > out.txt` that completed with exitCode 0 and
		// aggregatedOutput null.
		out := ""
		if it.AggregatedOutput != nil {
			out = *it.AggregatedOutput
		}
		result = out
	case "fileChange", "collabAgentToolCall":
		failed = statusFailed
	case "mcpToolCall":
		failed = statusFailed || it.Error != nil
		if it.Error != nil {
			result = it.Error.Message
		} else if v, ok := decodeValue(it.Result); ok {
			result = mcpResultValue(v)
		}
	case "dynamicToolCall":
		failed = statusFailed || (it.Success != nil && !*it.Success)
		result, _ = decodeValue(it.ContentItems)
	case "webSearch":
		result, _ = decodeValue(it.Results)
	case "imageGeneration":
		failed = !isNull(it.Failure)
		// The image as the app returned it: digested, never carried.
		result, _ = decodeValue(it.Result)
	case "functionCallOutput":
		result, _ = decodeValue(it.Output)
	}
	a.IsError = &failed
	a.ResultType, a.ResultDigest = resultShape(result)
	if failed {
		a.Contents = nil
	}
}

// codexReadContents names the files Codex's own parse of a command says
// it reads. The parse is Codex's, "best-effort" in its own words, and a
// command it could not parse reads nothing here rather than something
// guessed.
func codexReadContents(actions []codexCommandAction, cwd string) []Content {
	var out []Content
	seen := map[string]bool{}
	for _, act := range actions {
		if act.Type != "read" {
			continue
		}
		for _, c := range contentFor(ContentRead, act.Path, cwd) {
			if !seen[c.Path] {
				seen[c.Path] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// codexChangeContents names the files an app-server fileChange touched:
// v2 FileUpdateChange, {path, kind: {type, move_path}, diff}.
func codexChangeContents(raw json.RawMessage, cwd string) []Content {
	var changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		} `json:"kind"`
	}
	if !decodeTolerant(raw, &changes) {
		return nil
	}
	var out []Content
	for _, ch := range changes {
		out = append(out, changeContents(ch.Path, ch.Kind.Type, ch.Kind.MovePath, cwd)...)
	}
	return out
}

// changeContents is one file change as Contents: an add or an update
// writes the file, a delete removes it, and a move is both -- the old path
// is gone and the new one written.
func changeContents(path, kind, movePath, cwd string) []Content {
	switch kind {
	case "delete":
		return contentFor(ContentDelete, path, cwd)
	case "update":
		if strings.TrimSpace(movePath) != "" {
			return append(contentFor(ContentDelete, path, cwd), contentFor(ContentWrite, movePath, cwd)...)
		}
	}
	return contentFor(ContentWrite, path, cwd)
}

// codexActionURL is the page a web-search action opened, when it opened
// one: WebSearchAction's openPage and findInPage carry a url.
func codexActionURL(raw json.RawMessage) string {
	var act struct {
		URL *string `json:"url"`
	}
	if !decodeTolerant(raw, &act) || act.URL == nil {
		return ""
	}
	return *act.URL
}

// --- the mcp-server fallback ----------------------------------------

// codexCoreEvent is the part of a core EventMsg the recording reads.
// Fields are snake_case here, unlike the app-server's.
type codexCoreEvent struct {
	Type             string                     `json:"type"`
	CallID           string                     `json:"call_id"`
	Command          []string                   `json:"command"`
	Cwd              string                     `json:"cwd"`
	ParsedCmd        []codexCommandAction       `json:"parsed_cmd"`
	AggregatedOutput *string                    `json:"aggregated_output"`
	Stdout           *string                    `json:"stdout"`
	Stderr           *string                    `json:"stderr"`
	ExitCode         *int                       `json:"exit_code"`
	Status           string                     `json:"status"`
	Changes          map[string]json.RawMessage `json:"changes"`
	Success          *bool                      `json:"success"`
	Invocation       *codexInvocation           `json:"invocation"`
	Result           json.RawMessage            `json:"result"`
	Query            string                     `json:"query"`
	Action           json.RawMessage            `json:"action"`
	Results          json.RawMessage            `json:"results"`
	Path             string                     `json:"path"`
}

// codexInvocation is McpInvocation: which server, which tool, what
// arguments.
type codexInvocation struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// What a core event is to the recording.
const (
	corePhaseBegin = iota + 1
	corePhaseEnd
	// corePhaseBoth is an event that is a whole call on its own:
	// view_image_tool_call has no begin and no end.
	corePhaseBoth
)

// codexCoreCall builds the call half of an Action from a core event, and
// reports which phase of the call the event is. ok is false for an event
// that is not a call.
//
// An exec's cwd is left EMPTY when the event names none: an older end
// event carries only the call id and the status, and a cwd defaulted here
// would overwrite the one its begin reported (mergeCall keeps the end's
// non-empty fields). The caller fills the workspace in last.
func codexCoreCall(ev codexCoreEvent, workspace string) (Action, int, bool) {
	cwd := codexPath(ev.Cwd)
	base := cwd
	if base == "" {
		base = workspace
	}
	phase := corePhaseEnd
	if strings.HasSuffix(ev.Type, "_begin") {
		phase = corePhaseBegin
	}
	var a Action
	switch ev.Type {
	case "exec_command_begin", "exec_command_end":
		a = Action{ID: ev.CallID, Tool: ActionExec, AppTool: "exec_command", Cwd: cwd}
		if len(ev.Command) > 0 {
			a.Command = shellJoin(ev.Command)
			a.Args = argsOf(map[string]any{"command": ev.Command})
		}
		a.Contents = codexReadContents(ev.ParsedCmd, base)
	case "patch_apply_begin", "patch_apply_end":
		a = Action{ID: ev.CallID, Tool: ActionFSWrite, AppTool: "patch_apply", Cwd: workspace}
		// An older end event carries no changes; leaving these empty is
		// what lets the begin's stand (mergeCall).
		if len(ev.Changes) > 0 {
			a.Args = argsOf(map[string]any{"changes": ev.Changes})
			a.Contents = codexCoreChangeContents(ev.Changes, workspace)
		}
	case "mcp_tool_call_begin", "mcp_tool_call_end":
		a = Action{ID: ev.CallID, Tool: ActionMCP, AppTool: "mcp_tool_call", Cwd: workspace}
		if ev.Invocation != nil {
			a.Args = rawOrNull(ev.Invocation.Arguments)
			a.MCP = &MCPTarget{Server: ev.Invocation.Server, Tool: ev.Invocation.Tool}
		}
	case "web_search_begin", "web_search_end":
		a = Action{ID: ev.CallID, Tool: ActionFetch, AppTool: "web_search", Cwd: workspace,
			Query: ev.Query, URL: codexActionURL(ev.Action)}
		if ev.Type == "web_search_end" {
			a.Args = argsOf(map[string]any{"query": ev.Query, "action": rawOrNull(ev.Action)})
		}
	case "view_image_tool_call":
		path := codexPath(ev.Path)
		a = Action{ID: ev.CallID, Tool: ActionFSRead, AppTool: "view_image_tool_call", Cwd: workspace,
			Args: argsOf(map[string]any{"path": path}), Contents: contentFor(ContentRead, path, workspace)}
		phase = corePhaseBoth
	default:
		return Action{}, 0, false
	}
	return a, phase, true
}

// codexCoreFinish writes what an end event reported onto its call.
//
// An end event this build cannot read the outcome of reports nothing --
// isError stays absent rather than guessing either way.
func codexCoreFinish(a *Action, ev codexCoreEvent) {
	statusFailed := ev.Status != "" && ev.Status != "completed"
	var failed *bool
	var result any
	known := true
	switch ev.Type {
	case "exec_command_end":
		f := statusFailed || (ev.ExitCode != nil && *ev.ExitCode != 0)
		failed = &f
		a.ExitCode = ev.ExitCode
		switch {
		case ev.AggregatedOutput != nil:
			result = *ev.AggregatedOutput
		case ev.Stdout != nil || ev.Stderr != nil:
			result = deref(ev.Stdout) + deref(ev.Stderr)
		default:
			// An end that carries no output at all reported no result.
			known = false
		}
	case "patch_apply_end":
		f := statusFailed || (ev.Success != nil && !*ev.Success)
		failed = &f
	case "mcp_tool_call_end":
		result, failed = codexCoreMCPResult(ev.Result)
		known = failed != nil
	case "web_search_end":
		f := false
		failed = &f
		result, _ = decodeValue(ev.Results)
	case "view_image_tool_call":
		f := false
		failed = &f
	}
	a.IsError = failed
	if known {
		a.ResultType, a.ResultDigest = resultShape(result)
	}
	if failed == nil || *failed {
		a.Contents = nil
	}
}

// codexCoreMCPResult reads mcp_tool_call_end's result, which the core
// protocol serialises as a Rust Result: {"Ok": CallToolResult} or
// {"Err": "reason"}. A shape it cannot read reports nothing -- not a
// success and not a failure.
func codexCoreMCPResult(raw json.RawMessage) (any, *bool) {
	var r struct {
		Ok  json.RawMessage `json:"Ok"`
		Err *string         `json:"Err"`
	}
	if !decodeTolerant(raw, &r) {
		return nil, nil
	}
	if r.Err != nil {
		failed := true
		return *r.Err, &failed
	}
	v, ok := decodeValue(r.Ok)
	if !ok {
		return nil, nil
	}
	failed := false
	if obj, ok := v.(map[string]any); ok {
		if flag, ok := obj["isError"].(bool); ok {
			failed = flag
		}
	}
	return mcpResultValue(v), &failed
}

// codexCoreChangeContents names the files a core patch touched, in path
// order: the changes arrive as a map, and map order is not an order.
func codexCoreChangeContents(changes map[string]json.RawMessage, cwd string) []Content {
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []Content
	for _, p := range paths {
		var kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		}
		_ = decodeTolerant(changes[p], &kind)
		out = append(out, changeContents(p, kind.Type, kind.MovePath, cwd)...)
	}
	return out
}

// codexPath reads a path the core protocol sent. rust-v0.153.4 serialises
// PathUri as a file:// URL (codex-rs/utils/path-uri, `impl Serialize for
// PathUri`), and the older releases this fallback exists for send a plain
// path; both are read, and a URL naming another host is no local path.
func codexPath(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "file://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Host != "" && u.Host != "localhost") {
		return ""
	}
	return u.Path
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// shellJoin renders an argv as one command line a POSIX shell reads back
// as the same argv.
//
// Only the fallback needs it: its exec events carry the argv as a list,
// where the app-server carries one string and Claude Code's Bash tool is
// handed one. A word made only of characters no shell treats specially
// stays bare, so `bash -lc 'ls -1'` reads the way a person would type it;
// anything else is single-quoted, the one quoting that means the same
// thing in every POSIX shell. FuzzShellJoinRoundTrips holds it to that.
func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellWord(a)
	}
	return strings.Join(words, " ")
}

func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	// A leading `=` is an expansion in zsh, the default shell on a Mac.
	bare := s[0] != '='
	for _, r := range s {
		if !shellSafe(r) {
			bare = false
			break
		}
	}
	if bare {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-_./=:,+@%", r)
}
```

then hook it into both clients:

Edits to `internal/worker/harness/codexappserver.go` (hooks only; the translation is `codexactions.go`):

1. After `jsonrpcConn.emit`, add:

```go
// record hands one finished call to the turn's sink. Like a chunk, an
// action that finishes with no turn in flight has nowhere to go -- and
// its seq is already spent, so the gap it leaves is visible downstream.
func (c *jsonrpcConn) record(a Action) {
	c.mu.Lock()
	s := c.sink
	c.mu.Unlock()
	if s != nil {
		s.Record(a)
	}
}
```

2. `codexAppServer` gains, after `turn *codexTurnState`:

```go
	// rec is the session's recording. It is set before the process starts
	// because the reader goroutine may deliver an item the moment it does.
	rec *recording
```

3. In `Start`, right after `h.spec = spec`: `h.rec = newRecording()`.

4. In `Turn`, right after the `defer func() { h.mu.Lock(); h.turn = nil; h.mu.Unlock() }()` block:

```go
	h.rec.startTurn()
	// The calls this turn started and never finished are recorded when it
	// ends, whichever way it ends -- and before the sink is detached, which
	// is the deferred call registered first and so run last.
	defer h.flushRecording()
```

5. In `handleNotification`, the `codexNotifyItemStarted, codexNotifyItemCompleted` arm becomes:

```go
	case codexNotifyItemStarted, codexNotifyItemCompleted:
		var n struct {
			TurnID string          `json:"turnId"`
			Item   json.RawMessage `json:"item"`
		}
		if json.Unmarshal(params, &n) != nil || len(n.Item) == 0 {
			break
		}
		kind, id, text, phase := codexReadItem(n.Item)
		if codexToolItems[kind] {
			h.conn.emit(StreamTool, n.Item)
			h.recordItem(method, n.Item)
			return
		}
		// A call this build routes as progress rather than as tool
		// activity -- an image view, a sub-agent, a sleep -- is still a
		// call, and still recorded; anything else is ignored there.
		h.recordItem(method, n.Item)
		if kind == codexItemAgentMessage && method == codexNotifyItemCompleted {
```
   (the rest of the arm is unchanged).

6. After `handleNotification`, add:

```go
// recordItem feeds one item to the session's recording: a started item
// opens a call, a completed one closes it.
func (h *codexAppServer) recordItem(method string, raw json.RawMessage) {
	call, item, ok := codexItemCall(raw, h.spec.Workspace)
	if !ok {
		return
	}
	switch method {
	case codexNotifyItemStarted:
		h.rec.begin(call)
	case codexNotifyItemCompleted:
		if a, ok := h.rec.complete(call.ID, func(a *Action) {
			*a = mergeCall(*a, call)
			codexItemFinish(a, item)
		}); ok {
			h.conn.record(a)
		}
	}
}

// flushRecording records the calls the turn started and never finished.
func (h *codexAppServer) flushRecording() {
	for _, a := range h.rec.flush() {
		h.conn.record(a)
	}
}
```

Edits to `internal/worker/harness/codexmcp.go`:

1. `codexMCP` gains, after `turn *codexMCPTurn`:

```go
	// rec is the session's recording, set before the server starts.
	rec *recording
```

2. In `Start`, right after `h.spec = spec`: `h.rec = newRecording()`.

3. In `Turn`, right after the `defer func() { h.mu.Lock(); h.turn = nil; h.mu.Unlock() }()` block:

```go
	h.rec.startTurn()
	defer h.flushRecording()
```

4. In `handleNotification`, the tool-event case becomes:

```go
	case codexMCPToolEvents[event.Msg.Type]:
		h.conn.emit(StreamTool, raw)
		h.recordEvent(params)
		return
```

5. After `handleNotification`, add:

```go
// recordEvent feeds one core event to the session's recording: a begin
// opens a call, an end closes it, and an event that is a whole call on
// its own does both.
func (h *codexMCP) recordEvent(params json.RawMessage) {
	var envelope struct {
		Msg codexCoreEvent `json:"msg"`
	}
	if !decodeTolerant(params, &envelope) {
		return
	}
	call, phase, ok := codexCoreCall(envelope.Msg, h.spec.Workspace)
	if !ok {
		return
	}
	if phase == corePhaseBegin {
		if call.Cwd == "" {
			call.Cwd = h.spec.Workspace
		}
		h.rec.begin(call)
		return
	}
	if a, ok := h.rec.complete(call.ID, func(a *Action) {
		*a = mergeCall(*a, call)
		if a.Cwd == "" {
			a.Cwd = h.spec.Workspace
		}
		codexCoreFinish(a, envelope.Msg)
	}); ok {
		h.conn.record(a)
	}
}

// flushRecording records the calls the turn started and never finished.
func (h *codexMCP) flushRecording() {
	for _, a := range h.rec.flush() {
		h.conn.record(a)
	}
}
```

- [ ] **Step 4: Run the package's tests, and the fuzz targets briefly**

Run: `go test ./internal/worker/harness/ -count=1 && go test ./internal/worker/harness/ -run '^$' -fuzz FuzzShellJoinRoundTrips -fuzztime 10s && go test ./internal/worker/harness/ -run '^$' -fuzz FuzzResultShapeIsClosed -fuzztime 10s`
Expected: `ok` three times.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/harness/codexactions.go internal/worker/harness/codexactions_test.go internal/worker/harness/fuzz_test.go \
  internal/worker/harness/codexappserver.go internal/worker/harness/codexmcp.go
git commit -m "Issue #442: Codex app-server and mcp-server calls recorded in the same shape"
```

---

## Task 4: The session sends the recording, reading files under a workspace-only policy (#441, chunks.go side)

**Files:**
- Create: `internal/worker/appsession/record.go`, `internal/worker/appsession/openread_unix.go`, `internal/worker/appsession/openread_other.go`
- Modify: `internal/worker/appsession/session.go` (policy field, sink), `internal/worker/appsession/mcpconfig.go` (`paths`), `internal/worker/appsession/fuzz_test.go`
- Test: `internal/worker/appsession/record_test.go`

**Interfaces:**
- Consumes: `harness.Action`, `harness.Content`, `harness.Omitted*`, `harness.Encoding*`, `harness.Digest`, `harness.DigestPrefix`; `session.emitChunk`, `session.emitUncapped`, `backupSuffix`, `maxPushFileBytes`, `testBearer`.
- Produces: `sessionSink{s *session}` (implements `harness.Sink`), `session.recordAction`, `newContentPolicy(workspace string, excluded ...string) *contentPolicy`, `(*contentPolicy).fill([]harness.Content)`, `readForRecord(path string, keepMax int64) fileRead`, `resolvedPath`, `within`, `sessionScaffoldDir`, `mcpConfig.paths() (config, backup string)`; test helpers `writeFile`, `policyFor`.

- [ ] **Step 1: Write the failing tests** -- `internal/worker/appsession/record_test.go`:

```go
//go:build linux || darwin

package appsession

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// record_test.go holds the content policy to what it promises: a file the
// app names is read only when it is inside the workspace, outside the
// session's scaffolding, and a regular file -- and the bytes that travel
// are the file's own, bounded.

func writeFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func policyFor(ws string) *contentPolicy {
	return newContentPolicy(ws,
		filepath.Join(ws, sessionScaffoldDir),
		filepath.Join(ws, ".mcp.json"),
		filepath.Join(ws, ".mcp.json"+backupSuffix))
}

func readOne(p *contentPolicy, op, path string) harness.Content {
	c := []harness.Content{{Op: op, Path: path}}
	p.fill(c)
	return c[0]
}

func TestContentPolicyReadsOnlyInsideTheWorkspace(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	notes := writeFile(t, ws, "notes.txt", "alpha\nbeta\n")
	secret := writeFile(t, outside, "secret.txt", "not the session's")
	writeFile(t, ws, ".mcp.json", `{"mcpServers":{"memql":{"headers":{"Authorization":"Bearer `+testBearer+`"}}}}`)
	writeFile(t, ws, sessionScaffoldDir+"/codex/config.toml", "bearer = \""+testBearer+"\"\n")
	writeFile(t, ws, ".mcp.json"+backupSuffix, "the person's own config")
	if err := os.Symlink(secret, filepath.Join(ws, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(notes, filepath.Join(ws, "link-in")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := policyFor(ws)

	cases := []struct {
		name, path, omitted, data string
	}{
		{"a file in the workspace", notes, "", "alpha\nbeta\n"},
		{"a relative path, under the workspace", "notes.txt", "", "alpha\nbeta\n"},
		{"a link that stays inside", filepath.Join(ws, "link-in"), "", "alpha\nbeta\n"},
		{"a file elsewhere", secret, harness.OmittedOutsideWorkspace, ""},
		{"a link out of the workspace", filepath.Join(ws, "link-out"), harness.OmittedOutsideWorkspace, ""},
		{"a climb out with ..", filepath.Join(ws, "..", filepath.Base(outside), "secret.txt"), harness.OmittedOutsideWorkspace, ""},
		{"the MCP configuration with the bearer", filepath.Join(ws, ".mcp.json"), harness.OmittedScaffolding, ""},
		{"the session directory", filepath.Join(ws, sessionScaffoldDir, "codex", "config.toml"), harness.OmittedScaffolding, ""},
		{"a configuration moved aside", filepath.Join(ws, ".mcp.json"+backupSuffix), harness.OmittedScaffolding, ""},
		{"nothing there", filepath.Join(ws, "missing.txt"), harness.OmittedNotFound, ""},
		{"a directory", filepath.Join(ws, "dir"), harness.OmittedNotRegular, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readOne(p, harness.ContentRead, tc.path)
			if got.Omitted != tc.omitted {
				t.Fatalf("omitted = %q, want %q (%+v)", got.Omitted, tc.omitted, got)
			}
			if tc.omitted != "" {
				if got.Data != nil || got.Encoding != "" {
					t.Errorf("an omitted file still carried bytes: %+v", got)
				}
				if tc.omitted != harness.OmittedNotRegular && got.Digest != "" {
					t.Errorf("a file that was not read has a digest: %+v", got)
				}
				return
			}
			if got.Data == nil || *got.Data != tc.data || got.Encoding != harness.EncodingUTF8 {
				t.Errorf("data = %+v, want %q", got, tc.data)
			}
			if got.Digest != harness.Digest([]byte(tc.data)) || got.Bytes == nil || *got.Bytes != int64(len(tc.data)) {
				t.Errorf("digest/bytes = %s %v", got.Digest, got.Bytes)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want the path the app named", got.Path)
			}
		})
	}
}

// TestContentPolicyNeverWaitsOnAPipe: a named pipe opened for reading
// blocks until a writer appears, which here would be never. The open is
// non-blocking and the pipe is refused on its descriptor.
func TestContentPolicyNeverWaitsOnAPipe(t *testing.T) {
	ws := t.TempDir()
	fifo := filepath.Join(ws, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan harness.Content, 1)
	go func() { done <- readOne(policyFor(ws), harness.ContentRead, fifo) }()
	select {
	case got := <-done:
		if got.Omitted != harness.OmittedNotRegular {
			t.Errorf("a pipe = %+v, want not_regular", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a named pipe hung the recording")
	}
}

func TestContentPolicyBoundsWhatTravels(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)

	big := writeFile(t, ws, "big.bin", strings.Repeat("x", int(maxInlineFileBytes)+1))
	got := readOne(p, harness.ContentWrite, big)
	if got.Omitted != harness.OmittedOverCeiling || got.Data != nil {
		t.Errorf("a file over the inline ceiling = %+v", got)
	}
	if got.Digest != harness.Digest([]byte(strings.Repeat("x", int(maxInlineFileBytes)+1))) {
		t.Error("a file over the inline ceiling lost its digest; it can still be compared")
	}

	// Five files of the per-file ceiling in one action: four fit the event
	// budget, and the fifth is digested but not carried.
	var batch []harness.Content
	for i := 0; i < 5; i++ {
		path := writeFile(t, ws, "part"+string(rune('a'+i)), strings.Repeat("y", int(maxInlineFileBytes)))
		batch = append(batch, harness.Content{Op: harness.ContentWrite, Path: path})
	}
	p.fill(batch)
	for i, c := range batch[:4] {
		if c.Data == nil || c.Omitted != "" {
			t.Errorf("file %d = %+v, want it inline", i, c)
		}
	}
	if batch[4].Omitted != harness.OmittedOverBudget || batch[4].Data != nil || batch[4].Digest == "" {
		t.Errorf("the fifth file = %+v, want over_budget with its digest", batch[4])
	}

	// Past the digest ceiling nothing is read at all; the size still says
	// how big it was. A sparse file makes this cheap to build.
	huge := filepath.Join(ws, "huge.bin")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxDigestFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got = readOne(p, harness.ContentRead, huge)
	if got.Omitted != harness.OmittedTooLarge || got.Digest != "" || got.Bytes == nil || *got.Bytes != maxDigestFileBytes+1 {
		t.Errorf("a file past the digest ceiling = %+v", got)
	}
}

func TestContentPolicyCarriesBinaryAsBase64AndEmptyAsEmpty(t *testing.T) {
	ws := t.TempDir()
	p := policyFor(ws)
	raw := []byte{0xff, 0xfe, 0x00, 0x41}
	path := filepath.Join(ws, "blob.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := readOne(p, harness.ContentRead, path)
	if got.Encoding != harness.EncodingBase64 || got.Data == nil || *got.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("binary = %+v", got)
	}
	if got.Digest != harness.Digest(raw) {
		t.Error("the digest is over the bytes, not over their encoding")
	}

	empty := writeFile(t, ws, "empty.txt", "")
	got = readOne(p, harness.ContentWrite, empty)
	if got.Data == nil || *got.Data != "" || got.Bytes == nil || *got.Bytes != 0 || got.Omitted != "" {
		t.Errorf("an empty file = %+v, want present, empty and inline", got)
	}
}

func TestContentPolicyLeavesADeleteAlone(t *testing.T) {
	ws := t.TempDir()
	path := writeFile(t, ws, "still-here.txt", "x")
	got := readOne(policyFor(ws), harness.ContentDelete, path)
	if got.Digest != "" || got.Data != nil || got.Omitted != "" || got.Bytes != nil {
		t.Errorf("a delete read the file it deleted: %+v", got)
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/w", "/w", true},
		{"/w", "/w/a/b", true},
		{"/w", "/w/..foo", true},
		{"/w", "/wx", false},
		{"/w", "/", false},
		{"/w", "/other/w", false},
	} {
		if got := within(tc.root, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}
```

and the fuzz target:

Append to `internal/worker/appsession/fuzz_test.go` (add `"os"` to its imports):

```go
// FuzzContentPolicyStaysInTheWorkspace. The path comes from the APP, and
// a prompt from somewhere else drives the app; the policy decides whether
// this machine reads the file back into the recording. The property is
// the one that would actually go wrong: whatever the path -- climbs,
// links, the scaffolding by another spelling -- the policy either refuses
// it or names a file inside the workspace, outside the scaffolding, with
// no link left in the path to follow.
func FuzzContentPolicyStaysInTheWorkspace(f *testing.F) {
	for _, seed := range []string{
		"notes.txt", "/etc/passwd", "../../etc/passwd", "..", ".", "", "/",
		".mcp.json", ".memql-session/codex/auth.json", "./.memql-session/../.mcp.json",
		"sub/../../x", "a/./b", "link-out", "link-out/deeper", "link-in/../notes.txt",
		".mcp.json.memql-session-backup", "\x00", strings.Repeat("../", 40) + "etc",
	} {
		f.Add(seed)
	}
	ws, outside := f.TempDir(), f.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("x"), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link-out")); err != nil {
		f.Fatal(err)
	}
	if err := os.Symlink(ws, filepath.Join(ws, "link-in")); err != nil {
		f.Fatal(err)
	}
	p := newContentPolicy(ws, filepath.Join(ws, sessionScaffoldDir), filepath.Join(ws, ".mcp.json"))
	root := resolvedPath(ws)

	f.Fuzz(func(t *testing.T, path string) {
		real, why := p.resolve(path)
		if why != "" {
			if real != "" {
				t.Fatalf("resolve(%q) refused (%s) and still named %q", path, why, real)
			}
			return
		}
		sep := string(filepath.Separator)
		if real != root && !strings.HasPrefix(real, root+sep) {
			t.Fatalf("resolve(%q) = %q, outside the workspace %q", path, real, root)
		}
		for _, name := range []string{sessionScaffoldDir, ".mcp.json"} {
			scaffold := filepath.Join(root, name)
			if real == scaffold || strings.HasPrefix(real, scaffold+sep) {
				t.Fatalf("resolve(%q) = %q, the session's scaffolding", path, real)
			}
		}
		if again, err := filepath.EvalSymlinks(real); err == nil && again != real {
			t.Fatalf("resolve(%q) = %q, which still has a link in it (-> %q)", path, real, again)
		}
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/worker/appsession/ -run 'ContentPolicy|Within' 2>&1 | head -20`
Expected: build failure -- `undefined: newContentPolicy`, `undefined: sessionScaffoldDir`.

- [ ] **Step 3: Implement** -- create `internal/worker/appsession/record.go`:

```go
package appsession

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// record.go is the session's half of the recording (memql-cockpit#440).
//
// The harness decides WHICH files a call touched, out of the app's own
// protocol; this file decides whether their bytes leave this machine, and
// then sends the call on as an `event` chunk the transcript cap never
// swallows. The split is the point: what a protocol says and what this
// machine allows are different questions, and only the second is the
// machine owner's.

// sessionScaffoldDir is the session's own directory inside the workspace:
// the transcript, and for Codex the per-session CODEX_HOME with the bearer
// in its config.toml and the user's auth.json linked into it.
const sessionScaffoldDir = ".memql-session"

// The recording's ceilings.
const (
	// maxInlineFileBytes is the largest file whose bytes travel inside its
	// action -- the line length this package's process reader and the
	// harness already stop at.
	maxInlineFileBytes int64 = 1 << 20
	// maxInlineEventBytes bounds what one action's files carry inline
	// together. The stream is shared with every other message this machine
	// sends, and a file change touching forty files must not become one
	// forty-megabyte message holding up a heartbeat.
	maxInlineEventBytes int64 = 4 << 20
	// maxDigestFileBytes is the largest file the recording hashes; past it
	// the size is reported and nothing is read. The same ceiling the
	// produced-file push draws.
	maxDigestFileBytes = maxPushFileBytes
)

// sessionSink is the harness.Sink every turn of a session writes to.
type sessionSink struct{ s *session }

// Chunk sends narration the way it always went: redacted, numbered, and
// under the transcript cap.
func (k sessionSink) Chunk(stream string, data []byte) {
	_ = k.s.emitChunk(stream, data)
}

// Record sends one action.
//
// UNCAPPED, and that is why the recording has a method of its own.
// limits.max_transcript_bytes bounds the NARRATION the engine keeps on the
// session row; an action is not narration, it is the record of what the
// app did, and dropping the fortieth call because the app was chatty
// about the first thirty-nine would record a session that stopped doing
// things halfway through. The structured answer is exempt for the same
// reason (emitUncapped).
func (k sessionSink) Record(a harness.Action) {
	k.s.recordAction(a)
}

// recordAction reads the files the action names, under this machine's
// policy, and sends it.
func (s *session) recordAction(a harness.Action) {
	s.contentPolicy().fill(a.Contents)
	body, err := json.Marshal(a)
	if err != nil {
		s.logger.Warn("an app action could not be encoded for the recording",
			"id", a.ID, "seq", a.Seq, "error", err)
		return
	}
	// A send that fails for good leaves a hole in the action seq, and the
	// seq is dense, so that hole is how the engine learns a call is missing.
	if err := s.emitUncapped(StreamEvent, append(body, '\n')); err != nil {
		s.logger.Warn("an app action could not be sent", "id", a.ID, "seq", a.Seq, "error", err)
	}
}

func (s *session) contentPolicy() *contentPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// contentPolicy decides which of the files an app names this machine
// reads back into the recording.
//
// THE APP NAMES THE PATH, AND THE APP IS DRIVEN BY A PROMPT FROM SOMEWHERE
// ELSE. So the path is untrusted input, the stance CheckWorkspace takes
// toward the workspace itself: a file is read only if it resolves --
// symlinks followed -- to somewhere inside this session's workspace, is
// not the session's own scaffolding (the MCP configuration carrying the
// per-run bearer, the transcript, Codex's per-session home with the
// user's auth.json linked into it), and is a regular file, checked on the
// OPEN descriptor so a path swapped for a pipe cannot hang the session.
// Anything else is recorded by its path and the reason, and nothing is
// read.
//
// What this does NOT claim: an app that wants a file off this machine can
// print it into its own tool results, which travel verbatim as they
// always have. The policy's job is narrower and still worth doing -- the
// recording itself never widens what leaves, by reading a file the app
// named but did not reach, or one a symlink led out of the workspace.
type contentPolicy struct {
	root     string
	excluded []string
}

// newContentPolicy builds the policy for a workspace, never reading the
// paths in excluded or anything beneath them. Every path is compared
// resolved, so the answer is the same on macOS, where /tmp is itself a
// link.
func newContentPolicy(workspace string, excluded ...string) *contentPolicy {
	p := &contentPolicy{root: resolvedPath(workspace)}
	for _, e := range excluded {
		if strings.TrimSpace(e) != "" {
			p.excluded = append(p.excluded, resolvedPath(e))
		}
	}
	return p
}

// resolve returns the file to open for path, or the reason not to.
func (p *contentPolicy) resolve(path string) (string, string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.root, path)
	}
	real := resolvedPath(path)
	if !within(p.root, real) {
		return "", harness.OmittedOutsideWorkspace
	}
	for _, e := range p.excluded {
		if within(e, real) {
			return "", harness.OmittedScaffolding
		}
	}
	if strings.HasSuffix(real, backupSuffix) {
		// A configuration the session moved aside is the person's own,
		// held out of the way until the session restores it -- not part of
		// the run.
		return "", harness.OmittedScaffolding
	}
	return real, ""
}

// fill reads the files an action names, in place, under one inline
// budget for the whole action.
func (p *contentPolicy) fill(contents []harness.Content) {
	if p == nil {
		return
	}
	budget := maxInlineEventBytes
	for i := range contents {
		p.read(&contents[i], &budget)
	}
}

func (p *contentPolicy) read(c *harness.Content, budget *int64) {
	if c.Op == harness.ContentDelete {
		// Nothing is left at the path to read, by definition.
		return
	}
	real, why := p.resolve(c.Path)
	if why != "" {
		c.Omitted = why
		return
	}
	got := readForRecord(real, maxInlineFileBytes)
	c.Bytes, c.Digest, c.Omitted = got.size, got.digest, got.omitted
	switch {
	case got.omitted != "":
	case got.data == nil:
		c.Omitted = harness.OmittedOverCeiling
	case int64(len(got.data)) > *budget:
		c.Omitted = harness.OmittedOverBudget
	default:
		*budget -= int64(len(got.data))
		c.Encoding, c.Data = encodeInline(got.data)
	}
}

// fileRead is what readForRecord learned about one file.
type fileRead struct {
	size   *int64
	digest string
	// data is the file's bytes when there were at most keepMax of them.
	data    []byte
	omitted string
}

// readForRecord hashes one file and keeps its bytes when they fit.
func readForRecord(path string, keepMax int64) fileRead {
	f, err := openForRecord(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileRead{omitted: harness.OmittedNotFound}
		}
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	if !info.Mode().IsRegular() {
		return fileRead{omitted: harness.OmittedNotRegular}
	}
	size := info.Size()
	if size > maxDigestFileBytes {
		return fileRead{size: &size, omitted: harness.OmittedTooLarge}
	}
	keep := &capBuffer{max: keepMax}
	if size > keepMax {
		keep.max = 0
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(hash, keep), io.LimitReader(f, maxDigestFileBytes+1))
	if err != nil {
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	if n > maxDigestFileBytes {
		// It grew while it was being read.
		return fileRead{size: &n, omitted: harness.OmittedTooLarge}
	}
	out := fileRead{size: &n, digest: harness.DigestPrefix + hex.EncodeToString(hash.Sum(nil))}
	if n <= keepMax && int64(len(keep.buf)) == n {
		out.data = keep.buf
		if out.data == nil {
			out.data = []byte{}
		}
	}
	return out
}

// encodeInline carries bytes as the text they are, when they are text,
// and as base64 otherwise.
func encodeInline(data []byte) (string, *string) {
	if utf8.Valid(data) {
		s := string(data)
		return harness.EncodingUTF8, &s
	}
	s := base64.StdEncoding.EncodeToString(data)
	return harness.EncodingBase64, &s
}

// capBuffer keeps at most max bytes and reports every write whole, so the
// hash beside it in a MultiWriter still sees the entire file.
type capBuffer struct {
	buf []byte
	max int64
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - int64(len(b.buf)); room > 0 {
		if int64(len(p)) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

// resolvedPath resolves symlinks as far as the path exists and keeps the
// rest as written, so a file that does not exist yet still compares
// against the directory it would be in.
func resolvedPath(path string) string {
	clean := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean
	}
	return filepath.Join(resolvedPath(parent), filepath.Base(clean))
}

// within reports whether path is root or lies beneath it. Both must be
// resolved: the test is lexical, and only sound on paths with no links
// left in them.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
```

`internal/worker/appsession/openread_unix.go`:

```go
//go:build linux || darwin

package appsession

import (
	"os"
	"syscall"
)

// openForRecord opens a file for the recording to read.
//
// O_NONBLOCK so that a path that became a named pipe after the policy
// looked at it opens at once instead of waiting for a writer that may
// never come -- it is then refused as not regular, on the descriptor.
// O_NOFOLLOW so that a final component that became a link in the same
// window is refused rather than followed out of the workspace.
func openForRecord(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
```

`internal/worker/appsession/openread_other.go`:

```go
//go:build !linux && !darwin

package appsession

import "os"

// openForRecord opens a file for the recording to read. The pipe and the
// link race openread_unix.go closes are unix hazards.
func openForRecord(path string) (*os.File, error) {
	return os.Open(path)
}
```

Then, from the session edits below, apply ONLY items 3 (the `policy` field; `pulled` too, it is used in Task 5), 4's first block (the policy built beside `s.mcp`), 6 (the sink) and the `mcpconfig.go` `paths` method. The fingerprint call, the Options/Manager changes and `pullInputs` are Task 5's.

Edits to `internal/worker/appsession/session.go`:

1. `Options` gains, after `Detector *apps.Detector`:

```go
	// ToolVersions reports the developer tools the session fingerprint
	// lists. Nil probes this machine (fingerprint.go); tests set it, so a
	// session test does not fork every compiler on the machine running it.
	ToolVersions func(ctx context.Context) []harness.ToolVersion
```

2. `Manager` gains `tools *toolchain` (after `logger`), `NewManager` builds it
   (`m := &Manager{opts: opts, logger: logger, sessions: map[string]*session{}, tools: newToolchain()}`),
   and after `Live` add:

```go
// toolVersions is the fingerprint's toolchain: Options.ToolVersions when
// a caller supplied one, this machine's probe otherwise.
func (m *Manager) toolVersions(ctx context.Context) []harness.ToolVersion {
	if m.opts.ToolVersions != nil {
		return m.opts.ToolVersions(ctx)
	}
	return m.tools.versions(ctx)
}
```

3. `session` gains, after `library *Library` in the `mu` group:

```go
	// policy decides which files the recording reads back (record.go);
	// pulled are the Library inputs as they landed, for the fingerprint.
	policy *contentPolicy
	pulled []pulledInput
```

4. In `execute`, the MCP configuration publication becomes:

```go
	config, backup := mcp.paths()
	s.mu.Lock()
	s.mcp = mcp
	// What the recording may read back from this workspace: never the
	// session's own scaffolding, which from here on holds the bearer.
	s.policy = newContentPolicy(workspace, filepath.Join(workspace, sessionScaffoldDir), config, backup)
	s.mu.Unlock()
```

   and after `s.before = before`:

```go

	// THE FINGERPRINT IS THE SESSION'S FIRST EVENT (fingerprint.go): the
	// world as the app is about to find it -- inputs landed, scaffolding
	// written and left out -- sent before any kind starts anything.
	s.sendFingerprint(ctx, spec, workspace)
```

5. In `pullInputs`, the loop keeps where each input landed:

```go
	for _, id := range inputs {
		path, err := library.Pull(ctx, id, workspace)
		if err != nil {
			// Name the id that failed. "an input could not be fetched"
			// sends whoever reads this to check all of them.
			return err
		}
		s.mu.Lock()
		s.pulled = append(s.pulled, pulledInput{artifact: id, path: path})
		s.mu.Unlock()
	}
```

6. In `runTurns`, the sink becomes the session's own:

```go
	// Every chunk the app produces arrives here already classified by the
	// harness, which reads the app's own protocol, and every call the app
	// completes arrives as an Action for the recording (record.go).
	sink := sessionSink{s: s}
```

Edit to `internal/worker/appsession/mcpconfig.go`, after `Env`:

```go
// paths is where this configuration's bearer lives and, when a person's
// own configuration was moved aside for it, where theirs went.
func (m *mcpConfig) paths() (config, backup string) {
	if m == nil {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configPath, m.backupPath
}
```

Edit to `internal/worker/apps/detect.go`, after `ResolveSpec`:

```go
// Version returns the app's own version string, as Detect reports it and
// from the same cache, or "" when the app is not on PATH or would not say.
//
// The session fingerprint asks this, so the version it records and the
// version the registration advertised are one probe's answer rather than
// two that could disagree.
func (d *Detector) Version(ctx context.Context, id string) string {
	spec, ok := SpecFor(strings.TrimSpace(id))
	if !ok {
		return ""
	}
	path, err := d.lookPath(spec.Binary)
	if err != nil || strings.TrimSpace(path) == "" {
		return ""
	}
	return Truncate(d.version(ctx, spec, path))
}
```

and in `internal/worker/apps/detect_test.go`:

```go
// TestVersion_IsTheInventorysAnswer: the fingerprint's app version comes
// from the same probe and the same cache the inventory reports from.
func TestVersion_IsTheInventorysAnswer(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)")
	d := env.detector()
	if got := d.Version(context.Background(), IDClaudeCode); got != "2.1.4 (Claude Code)" {
		t.Errorf("Version = %q", got)
	}
	d.Detect(context.Background(), nil)
	if got := env.calls.Load(); got != 1 {
		t.Errorf("the version was probed %d times for one binary, want once", got)
	}
	if got := d.Version(context.Background(), IDCodex); got != "" {
		t.Errorf("an app not on PATH has version %q", got)
	}
	if got := d.Version(context.Background(), "not-an-app"); got != "" {
		t.Errorf("an id outside the closed set has version %q", got)
	}
}
```

Edits to `internal/worker/appsession/session_test.go`:

1. `newRig` passes a Detector whose version probe is stubbed and a fixed toolchain, so no session test forks the fake app with `--version` (several fakes fork a grandchild on EVERY invocation) or the machine's compilers:

```go
	h.manager = NewManager(Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:    h.state,
		LibraryBase: lib.server.URL,
		HTTPClient:  lib.server.Client(),
		Allowed:     func(id string) bool { return allowed[id] },
		Detector: &apps.Detector{RunVersion: func(context.Context, string, []string) (string, error) {
			return rigAppVersion, nil
		}},
		ToolVersions: func(context.Context) []harness.ToolVersion { return rigTools },
	})
```

   with, beside `newRig`:

```go
// The rig's fixed answers for the fingerprint's app version and tools.
const rigAppVersion = "9.9.9 (rig)"

var rigTools = []harness.ToolVersion{{Name: "git", Version: "git version 2.43.0"}}
```

2. Add the recording's end-to-end tests (see Task 6).

- [ ] **Step 4: Run the package's tests and the fuzz target briefly**

Run: `go test ./internal/worker/appsession/ -count=1 && go test ./internal/worker/appsession/ -run '^$' -fuzz FuzzContentPolicyStaysInTheWorkspace -fuzztime 10s`
Expected: `ok` twice.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/appsession/record.go internal/worker/appsession/record_test.go \
  internal/worker/appsession/openread_unix.go internal/worker/appsession/openread_other.go \
  internal/worker/appsession/session.go internal/worker/appsession/mcpconfig.go internal/worker/appsession/fuzz_test.go
git commit -m "Issue #441: the session sends each action uncapped, with file contents read under a workspace-only policy"
```

---

## Task 5: The environment fingerprint as the first event (#443)

**Files:**
- Create: `internal/worker/appsession/fingerprint.go`
- Modify: `internal/worker/appsession/session.go` (Options.ToolVersions, Manager.tools, fingerprint call, pullInputs), `internal/worker/apps/detect.go` (`Version`), `internal/worker/appsession/session_test.go` (`newRig`)
- Test: `internal/worker/appsession/fingerprint_test.go`, `internal/worker/appsession/recording_test.go`, `internal/worker/apps/detect_test.go`

**Interfaces:**
- Consumes: Task 4's `readForRecord`, `sessionScaffoldDir`, `writeFile`, `mcpConfig.paths`; `harness.Fingerprint` and parts; `harness.FingerprintVariables`; `apps.Truncate`; `pushExcludedDirs`.
- Produces: `session.sendFingerprint(ctx, spec, workspace)`, `listWorkspace(root, configPath, backupPath string, limit int) (workspaceListing, error)`, `fingerprintVariables(names, env []string) []harness.Variable`, `toolchain`, `newToolchain()`, `Options.ToolVersions`, `Manager.toolVersions(ctx)`, `(*apps.Detector).Version(ctx, id) string`; test fixtures `rigAppVersion`, `rigTools`, helper `recordedEvents`.

- [ ] **Step 1: Write the failing tests** -- `internal/worker/appsession/fingerprint_test.go`:

```go
//go:build linux || darwin

package appsession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// fingerprint_test.go holds each part of the fingerprint to what it
// claims: a listing of names and kinds, variables as digests, tools as
// they report themselves and never a shim that opens a window.

func listing(t *testing.T, root, config, backup string) workspaceListing {
	t.Helper()
	l, err := listWorkspace(root, config, backup, maxListingEntries)
	if err != nil {
		t.Fatalf("listWorkspace: %v", err)
	}
	return l
}

func TestListWorkspaceIsNamesAndKindsNotContents(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	// The same tree, built in a different order.
	writeFile(t, a, "src/main.go", "package main")
	writeFile(t, a, "README.md", "hi")
	writeFile(t, b, "README.md", "hi")
	writeFile(t, b, "src/main.go", "package main")
	if listing(t, a, "", "").digest != listing(t, b, "", "").digest {
		t.Fatal("the same tree built in another order listed differently")
	}
	// Contents are not the listing's business: the action that read a file
	// digests what it saw.
	writeFile(t, b, "README.md", "a different body entirely")
	if listing(t, a, "", "").digest != listing(t, b, "", "").digest {
		t.Error("editing a file changed the listing digest")
	}
	writeFile(t, b, "NEW.md", "")
	if listing(t, a, "", "").digest == listing(t, b, "", "").digest {
		t.Error("adding a file did not change the listing digest")
	}
	if l := listing(t, a, "", ""); l.entries != 3 || l.truncated || !strings.HasPrefix(l.digest, harness.DigestPrefix) {
		t.Errorf("listing = %+v, want 3 entries (src/, src/main.go, README.md)", l)
	}
}

func TestListWorkspaceLeavesTheSessionOut(t *testing.T) {
	pristine, session := t.TempDir(), t.TempDir()
	writeFile(t, pristine, ".mcp.json", "the person's own")
	writeFile(t, pristine, "app.go", "package app")

	// The same workspace as a Claude Code session sees it: the person's
	// .mcp.json moved aside, the cockpit's written in its place, and the
	// session directory beside them.
	writeFile(t, session, ".mcp.json"+backupSuffix, "the person's own")
	writeFile(t, session, ".mcp.json", "the cockpit's, with the bearer")
	writeFile(t, session, "app.go", "package app")
	writeFile(t, session, sessionScaffoldDir+"/transcript.log", "[text] hi")
	writeFile(t, session, sessionScaffoldDir+"/codex/config.toml", "bearer")

	got := listing(t, session, filepath.Join(session, ".mcp.json"), filepath.Join(session, ".mcp.json"+backupSuffix))
	if want := listing(t, pristine, "", ""); got.digest != want.digest || got.entries != want.entries {
		t.Errorf("the session's workspace lists as %+v, want the workspace as the person left it: %+v", got, want)
	}
}

func TestListWorkspaceListsDependencyDirectoriesWithoutEnteringThem(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "node_modules/left-pad/index.js", "module.exports")
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, root, "index.js", "require('left-pad')")
	if l := listing(t, root, "", ""); l.entries != 3 {
		t.Errorf("entries = %d, want node_modules/, .git/ and index.js -- the directories named, never entered", l.entries)
	}
	bare := t.TempDir()
	writeFile(t, bare, "index.js", "require('left-pad')")
	if listing(t, root, "", "").digest == listing(t, bare, "", "").digest {
		t.Error("whether node_modules is there is a fact a command depends on, and the listing lost it")
	}
}

func TestListWorkspaceSaysWhenItStopped(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, name, "")
	}
	l, err := listWorkspace(root, "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !l.truncated || l.entries != 3 {
		t.Errorf("listing = %+v, want 3 entries and truncated", l)
	}
	if _, err := listWorkspace(filepath.Join(root, "missing"), "", "", 3); err == nil {
		t.Error("a workspace that is not there listed without an error")
	}
}

func TestFingerprintVariablesAreDigestsNotValues(t *testing.T) {
	env := []string{"PATH=/home/someone/bin:/usr/bin", "LANG=", "TZ=UTC", "TZ=Europe/Madrid"}
	got := fingerprintVariables([]string{"PATH", "LANG", "TZ", "SHELL"}, env)
	want := []harness.Variable{
		{Name: "PATH", Set: true, Digest: harness.Digest([]byte("/home/someone/bin:/usr/bin"))},
		{Name: "LANG", Set: true, Digest: harness.Digest(nil)},
		{Name: "TZ", Set: true, Digest: harness.Digest([]byte("Europe/Madrid"))},
		{Name: "SHELL", Set: false},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("variable %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "someone") || strings.Contains(string(body), "Madrid") {
		t.Errorf("a variable's value reached the fingerprint: %s", body)
	}
}

// fakeTools is a toolchain whose binaries are files in a temp directory
// and whose probes are counted.
type fakeTools struct {
	dir      string
	mu       sync.Mutex
	runs     map[string]int
	versions map[string]string
}

func newFakeTools(t *testing.T, versions map[string]string) (*fakeTools, *toolchain) {
	t.Helper()
	f := &fakeTools{dir: t.TempDir(), runs: map[string]int{}, versions: versions}
	for name := range versions {
		writeFile(t, f.dir, name, "#!/bin/sh\n")
	}
	tc := &toolchain{
		lookPath: func(name string) (string, error) {
			path := filepath.Join(f.dir, name)
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			return path, nil
		},
		run: func(_ context.Context, bin string, _ []string) (string, error) {
			name := filepath.Base(bin)
			f.mu.Lock()
			f.runs[name]++
			f.mu.Unlock()
			v, ok := f.versions[name]
			if !ok || v == "" {
				return "", errors.New("no version")
			}
			return "\n" + v + "\nsecond line\n", nil
		},
		goos:     "linux",
		devTools: func() bool { return false },
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
	return f, tc
}

func (f *fakeTools) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[name]
}

func TestToolchainReportsEachToolAsItReportsItself(t *testing.T) {
	_, tc := newFakeTools(t, map[string]string{
		"go": "go version go1.26.6 linux/amd64", "git": "git version 2.43.0", "npm": "",
	})
	got := tc.versions(context.Background())
	want := []harness.ToolVersion{
		{Name: "git", Version: "git version 2.43.0"},
		{Name: "go", Version: "go version go1.26.6 linux/amd64"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("versions = %+v, want %+v: the first line, in name order, and a tool that did not answer left out", got, want)
	}
}

func TestToolchainAsksEachBinaryOnce(t *testing.T) {
	f, tc := newFakeTools(t, map[string]string{"git": "git version 2.43.0", "npm": ""})
	tc.versions(context.Background())
	tc.versions(context.Background())
	if n := f.count("git"); n != 1 {
		t.Errorf("git was asked %d times across two sessions, want once", n)
	}
	if n := f.count("npm"); n != 1 {
		t.Errorf("npm, which does not answer, was asked %d times; its silence is an answer until it changes", n)
	}
	// Replacing the binary is an upgrade, and the next session asks again.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.dir, "git"), later, later); err != nil {
		t.Fatal(err)
	}
	tc.versions(context.Background())
	if n := f.count("git"); n != 2 {
		t.Errorf("git was asked %d times after it changed, want 2", n)
	}
}

func TestToolchainNeverRunsAMacShimWithoutTheDeveloperTools(t *testing.T) {
	var runs atomic.Int32
	tc := &toolchain{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(context.Context, string, []string) (string, error) {
			runs.Add(1)
			return "git version 2.39.5 (Apple Git-154)", nil
		},
		goos:     "darwin",
		devTools: func() bool { return false },
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
	if got := tc.versions(context.Background()); len(got) != 0 || runs.Load() != 0 {
		t.Fatalf("with no developer tools, %d stubs ran and %+v was reported; each would have raised an install dialog", runs.Load(), got)
	}
	tc.devTools = func() bool { return true }
	if got := tc.versions(context.Background()); len(got) == 0 || runs.Load() == 0 {
		t.Error("with the developer tools installed, /usr/bin/git is git and must be asked")
	}
}
```

`internal/worker/appsession/recording_test.go` (the end-to-end tests, including #443's acceptance criterion and #441's cap and contents):

```go
//go:build linux || darwin

package appsession

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// recording_test.go drives whole sessions and reads the recording off the
// wire the way the engine will: event chunks, decoded by their type word.

// recordedEvents splits the event chunks a session sent into its
// fingerprints and its actions, in the order they were sent.
func recordedEvents(t *testing.T, chunks []recordedChunk) ([]harness.Fingerprint, []harness.Action) {
	t.Helper()
	var fps []harness.Fingerprint
	var actions []harness.Action
	for _, c := range chunks {
		if c.stream != StreamEvent {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(c.data), &head) != nil {
			continue
		}
		switch head.Type {
		case harness.FingerprintEventType:
			var fp harness.Fingerprint
			if err := json.Unmarshal([]byte(c.data), &fp); err != nil {
				t.Fatalf("a fingerprint chunk does not decode: %v (%s)", err, c.data)
			}
			fps = append(fps, fp)
		case harness.ActionEventType:
			var a harness.Action
			if err := json.Unmarshal([]byte(c.data), &a); err != nil {
				t.Fatalf("an action chunk does not decode: %v (%s)", err, c.data)
			}
			actions = append(actions, a)
		}
	}
	return fps, actions
}

const quietClaude = `
echo '{"type":"system","subtype":"init","session_id":"app-quiet"}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"app-quiet","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"ok"}'
`

// TestSession_FingerprintIsTheFirstEvent is #443's acceptance criterion:
// the first event of every session is the fingerprint, with the tool
// versions and the workspace digest -- for a run, for an attach, and for a
// session whose harness never gets as far as a turn.
func TestSession_FingerprintIsTheFirstEvent(t *testing.T) {
	cases := []struct {
		name    string
		app     string
		mutate  func(*memqlv1.AppSessionStart)
		harness string
	}{
		{"run", apps.IDClaudeCode, nil, harness.HarnessClaudeHeadless},
		{"attach", apps.IDClaudeCode, func(s *memqlv1.AppSessionStart) {
			s.Kind = KindAttach
			s.AppSessionRef = "app-quiet"
		}, harness.HarnessClaudeHeadless},
		{"a codex that dies at its handshake", apps.IDCodex, func(s *memqlv1.AppSessionStart) {
			s.App = apps.IDCodex
		}, harness.HarnessCodexMCP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.app == apps.IDCodex {
				fakeApp(t, "codex", "exit 9\n")
			} else {
				fakeApp(t, "claude", quietClaude)
			}
			h := newRig(t, tc.app)
			writeFile(t, h.workspace, "README.md", "hi")
			h.start(t, tc.mutate)

			chunks := h.sender.recorded()
			if len(chunks) == 0 {
				t.Fatal("the session sent nothing")
			}
			if chunks[0].stream != StreamEvent || chunks[0].seq != 1 {
				t.Fatalf("the first chunk is %q seq %d, want the fingerprint as event seq 1", chunks[0].stream, chunks[0].seq)
			}
			fps, _ := recordedEvents(t, chunks[:1])
			if len(fps) != 1 {
				t.Fatalf("the first chunk is not the fingerprint: %s", chunks[0].data)
			}
			fp := fps[0]
			if fp.V != harness.RecordVersion || fp.Seq != 0 || fp.TakenAt == "" {
				t.Errorf("fingerprint = %+v", fp)
			}
			if fp.App != (harness.FingerprintApp{ID: tc.app, Version: rigAppVersion, Harness: tc.harness}) {
				t.Errorf("app = %+v, want %s %s %s", fp.App, tc.app, rigAppVersion, tc.harness)
			}
			if !reflect.DeepEqual(fp.Tools, rigTools) {
				t.Errorf("tools = %+v, want the tool versions", fp.Tools)
			}
			if fp.Cwd != h.workspace || !strings.HasPrefix(fp.CwdDigest, harness.DigestPrefix) {
				t.Errorf("cwd = %q digest %q", fp.Cwd, fp.CwdDigest)
			}
			// README.md alone: the cockpit's .mcp.json and .memql-session
			// are the session's, not the workspace's.
			if fp.CwdEntries == nil || *fp.CwdEntries != 1 {
				t.Errorf("cwdEntries = %v, want 1", fp.CwdEntries)
			}
			if fp.Platform.OS != runtime.GOOS || fp.Platform.Arch != runtime.GOARCH {
				t.Errorf("platform = %+v", fp.Platform)
			}
			var path *harness.Variable
			for i := range fp.Variables {
				if fp.Variables[i].Name == "PATH" {
					path = &fp.Variables[i]
				}
			}
			if path == nil || !path.Set || path.Digest != harness.Digest([]byte(os.Getenv("PATH"))) {
				t.Errorf("PATH = %+v, want the digest of the PATH the app runs with", path)
			}
			if strings.Contains(chunks[0].data, os.Getenv("PATH")) {
				t.Error("the fingerprint carries PATH's value; it carries a digest")
			}
		})
	}
}

func TestSession_FingerprintDigestsTheInputs(t *testing.T) {
	fakeApp(t, "claude", quietClaude)
	h := newRig(t)
	h.library.mu.Lock()
	h.library.inputs["spec-1"] = []byte("the specification body")
	h.library.mu.Unlock()
	h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Inputs = []string{"spec-1"}
	})
	fps, _ := recordedEvents(t, h.sender.recorded())
	if len(fps) != 1 || len(fps[0].Inputs) != 1 {
		t.Fatalf("fingerprints = %+v", fps)
	}
	in := fps[0].Inputs[0]
	if in.Artifact != "spec-1" || in.Path != filepath.Join(h.workspace, "spec-1.txt") ||
		in.Digest != harness.Digest([]byte("the specification body")) || in.Bytes == nil || *in.Bytes != 22 {
		t.Errorf("input = %+v", in)
	}
}

// TestSession_ActionsCarryTheirFilesPastTheCap: the recording goes out
// whole after the transcript cap has bitten, a file the app wrote travels
// with its bytes, and a file the app named inside the scaffolding or
// outside the workspace travels as a path and a reason -- never a byte of
// it, and never the bearer.
func TestSession_ActionsCarryTheirFilesPastTheCap(t *testing.T) {
	outside := writeFile(t, t.TempDir(), "elsewhere.txt", "not the session's")
	fakeApp(t, "claude", fmt.Sprintf(`
printf 'goodbye' > out.txt
printf '%%s\n' '{"type":"system","subtype":"init","cwd":"'"$PWD"'","session_id":"app-rec"}'
i=0
while [ $i -lt 40 ]; do
  echo "narration line $i xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  i=$((i+1))
done
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_w","name":"Write","input":{"file_path":"'"$PWD"'/out.txt","content":"goodbye"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_w","type":"tool_result","content":"File created successfully"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_cfg","name":"Read","input":{"file_path":"'"$PWD"'/.mcp.json"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_cfg","type":"tool_result","content":"(the configuration)"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_out","name":"Read","input":{"file_path":"%s"}}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_out","type":"tool_result","content":"(elsewhere)"}]},"parent_tool_use_id":null,"session_id":"app-rec"}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"app-rec","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2},"result":"done"}'
`, outside))
	h := newRig(t)
	end := h.start(t, func(s *memqlv1.AppSessionStart) {
		s.Limits = &memqlv1.AppSessionLimits{MaxTranscriptBytes: 300}
	})
	if end.GetExitCode() != 0 || end.GetError() != "" {
		t.Fatalf("end = %d %q", end.GetExitCode(), end.GetError())
	}
	if !strings.Contains(h.sender.transcript(), "live transcript truncated") {
		t.Fatal("the cap never bit, so this test proves nothing about the recording outliving it")
	}
	_, actions := recordedEvents(t, h.sender.recorded())
	if len(actions) != 3 {
		t.Fatalf("actions = %d past the cap, want all 3: %+v", len(actions), actions)
	}
	for i, a := range actions {
		if a.Seq != uint64(i+1) || len(a.Contents) != 1 {
			t.Fatalf("action %d = %+v", i, a)
		}
	}
	wrote := actions[0].Contents[0]
	if wrote.Data == nil || *wrote.Data != "goodbye" || wrote.Digest != harness.Digest([]byte("goodbye")) ||
		wrote.Bytes == nil || *wrote.Bytes != 7 || wrote.Encoding != harness.EncodingUTF8 {
		t.Errorf("the written file = %+v, want its bytes as they stood when the call completed", wrote)
	}
	if got := actions[1].Contents[0]; got.Omitted != harness.OmittedScaffolding || got.Data != nil || got.Digest != "" {
		t.Errorf("the MCP configuration = %+v, want session_scaffolding and nothing read", got)
	}
	if got := actions[2].Contents[0]; got.Omitted != harness.OmittedOutsideWorkspace || got.Data != nil {
		t.Errorf("a file outside the workspace = %+v, want outside_workspace and nothing read", got)
	}
	for _, c := range h.sender.recorded() {
		if strings.Contains(c.data, testBearer) {
			t.Fatalf("the bearer reached a chunk: %s", c.data)
		}
		if strings.Contains(c.data, "not the session's") {
			t.Fatalf("a file outside the workspace reached a chunk: %s", c.data)
		}
	}
}
```

and `TestVersion_IsTheInventorysAnswer` plus the `newRig` change from the session edits (Task 4's listing).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/worker/appsession/ ./internal/worker/apps/ 2>&1 | head -20`
Expected: build failure -- `undefined: listWorkspace`, `unknown field ToolVersions`, `d.Version undefined`.

- [ ] **Step 3: Implement** -- create `internal/worker/appsession/fingerprint.go`:

```go
package appsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// fingerprint.go builds the session's FIRST event (memql-cockpit#443,
// decision D16 of the engine's recording record): what the world looked
// like when the session started, so a later replay can tell whether it is
// standing in the same one before it acts.

const (
	// maxListingEntries bounds the workspace listing. A workspace is a
	// project, not a home directory, but nothing forces that, and the
	// fingerprint must not become a walk of somebody's whole disk; a
	// listing cut short says so (cwdTruncated).
	maxListingEntries = 100_000

	// toolProbeTimeout bounds one tool's version probe, and toolProbeBudget
	// all of them together: they run concurrently, in front of the app's
	// start, so the pathological machine costs the budget once.
	toolProbeTimeout = 3 * time.Second
	toolProbeBudget  = 4 * time.Second

	// toolVersionTTL is how long a probed version is reused. The binary's
	// size and mtime are in the cache key too, so an upgrade shows at once.
	toolVersionTTL = 5 * time.Minute
)

// sendFingerprint sends the fingerprint as the session's first chunk.
//
// It never fails the session. A part that cannot be established is absent
// from the fingerprint, and a fingerprint that cannot be sent leaves a
// recording that starts at its first action -- worse than a whole one,
// and far better than refusing to run.
func (s *session) sendFingerprint(ctx context.Context, spec apps.Spec, workspace string) {
	body, err := json.Marshal(s.fingerprint(ctx, spec, workspace))
	if err != nil {
		s.logger.Warn("the session fingerprint could not be encoded", "error", err)
		return
	}
	if err := s.emitUncapped(StreamEvent, append(body, '\n')); err != nil {
		s.logger.Warn("the session fingerprint could not be sent", "error", err)
	}
}

// fingerprint describes the world the session starts in.
func (s *session) fingerprint(ctx context.Context, spec apps.Spec, workspace string) harness.Fingerprint {
	harnessWord := spec.Harness
	if s.start.GetKind() == KindOpen {
		// A person drives an open session; no protocol does.
		harnessWord = ""
	}
	fp := harness.Fingerprint{
		Type:    harness.FingerprintEventType,
		V:       harness.RecordVersion,
		Seq:     0,
		TakenAt: time.Now().UTC().Format(time.RFC3339Nano),
		App: harness.FingerprintApp{
			ID:      spec.ID,
			Version: s.manager.opts.Detector.Version(ctx, spec.ID),
			Harness: harnessWord,
		},
		Platform: harness.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Tools:    s.manager.toolVersions(ctx),
		Cwd:      workspace,
		// The environment the APP gets: the worker's, with the session's
		// own additions last, the way startChildStdin builds it.
		Variables: fingerprintVariables(harness.FingerprintVariables(spec.Harness),
			append(os.Environ(), s.mcpEnv()...)),
		Inputs: s.inputDigests(),
	}
	config, backup := s.mcpPaths()
	if l, err := listWorkspace(workspace, config, backup, maxListingEntries); err == nil {
		entries := l.entries
		fp.CwdDigest, fp.CwdEntries, fp.CwdTruncated = l.digest, &entries, l.truncated
	} else {
		s.logger.Warn("the workspace could not be listed for the session fingerprint", "error", err)
	}
	return fp
}

// mcpPaths is where the session's MCP configuration sits and, when a
// person's own was moved aside for it, where theirs went.
func (s *session) mcpPaths() (config, backup string) {
	s.mu.Lock()
	mcp := s.mcp
	s.mu.Unlock()
	return mcp.paths()
}

// fingerprintVariables records each named variable as set-or-not plus a
// digest of its value, as env would give it to a process: the LAST entry
// for a name wins, which is what os/exec does with a duplicate.
func fingerprintVariables(names, env []string) []harness.Variable {
	values := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			values[k] = v
		}
	}
	out := make([]harness.Variable, 0, len(names))
	for _, name := range names {
		v, set := values[name]
		entry := harness.Variable{Name: name, Set: set}
		if set {
			entry.Digest = harness.Digest([]byte(v))
		}
		out = append(out, entry)
	}
	return out
}

// pulledInput is one Library input and where it landed.
type pulledInput struct {
	artifact string
	path     string
}

// inputDigests describes the Library inputs as they landed, before the
// app could touch them.
func (s *session) inputDigests() []harness.Input {
	s.mu.Lock()
	pulled := append([]pulledInput(nil), s.pulled...)
	s.mu.Unlock()
	out := make([]harness.Input, 0, len(pulled))
	for _, p := range pulled {
		got := readForRecord(p.path, 0)
		out = append(out, harness.Input{
			Artifact: p.artifact, Path: p.path,
			Digest: got.digest, Bytes: got.size, Omitted: got.omitted,
		})
	}
	return out
}

// workspaceListing is the digest of a workspace's listing.
type workspaceListing struct {
	digest    string
	entries   int
	truncated bool
}

// listWorkspace digests the workspace's LISTING: the name and kind of
// everything in it, never a byte of any file.
//
// Names and kinds, because the listing answers "is this the same
// project" and contents answer "did this file change" -- which the action
// that READ a file already answers, precisely, for the files that
// mattered. Folding sizes or times in here would make every edit to any
// file, and every fresh checkout, a different world.
//
// Dependency and cache directories are LISTED BUT NOT ENTERED: whether
// node_modules or .git is there is a fact a command depends on, and what
// is inside them changes on every install and every commit. The session's
// own scaffolding is not listed at all -- it is this cockpit's, not the
// project's -- and a configuration the session moved aside is listed under
// the name it had, so the listing describes the workspace as the person
// left it.
func listWorkspace(root, configPath, backupPath string, limit int) (workspaceListing, error) {
	configRel := relUnder(root, configPath)
	backupRel := relUnder(root, backupPath)
	var lines []string
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// A corner that cannot be read is left out, not fatal: this is
			// a fingerprint, not an inventory.
			return nil
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && d.Name() == sessionScaffoldDir {
			return filepath.SkipDir
		}
		if len(lines) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			lines = append(lines, "d "+rel+"/")
			if pushExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case configRel != "" && rel == configRel:
			// The session's own configuration, holding its bearer.
			return nil
		case backupRel != "" && rel == backupRel:
			rel = configRel
		}
		kind := "o"
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			kind = "l"
		case d.Type().IsRegular():
			kind = "f"
		}
		lines = append(lines, kind+" "+rel)
		return nil
	})
	if err != nil {
		return workspaceListing{}, err
	}
	sort.Strings(lines)
	hash := sha256.New()
	for _, line := range lines {
		_, _ = io.WriteString(hash, line+"\n")
	}
	return workspaceListing{
		digest:    harness.DigestPrefix + hex.EncodeToString(hash.Sum(nil)),
		entries:   len(lines),
		truncated: truncated,
	}, nil
}

// relUnder is path relative to root, in slash form, or "" when path is
// empty or not beneath root.
func relUnder(root, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// --- the tools -------------------------------------------------------

// toolchain reports the versions of the developer tools a recorded
// command most likely ran through.
//
// A FIXED LIST, ASKED CHEAPLY. Which tools a session will use is not
// known when it starts, so the list is the toolchains coding agents reach
// for, each asked for its version the way it prints it. The answers are
// cached against each binary's size and mtime, the way the app detector
// caches the apps', so sessions back to back fork these once, not once a
// session. The probes run in the root directory, so what they report is
// the machine's default for each tool: a version manager or a project's
// own `toolchain` line can pick another for the command itself, and that
// command's own output then says so.
//
// NEVER A SHIM THAT OPENS A WINDOW. On a Mac without the command-line
// developer tools, /usr/bin/git, /usr/bin/python3 and /usr/bin/make are
// stubs that answer `--version` by raising an install dialog -- from a
// LaunchAgent, on a machine whose owner may not be at it, which is the
// hazard the worker already avoids for the Keychain. So on darwin a tool
// that resolves into /usr/bin is asked only when those tools are
// installed, and is otherwise absent from the list: the safe direction.
type toolchain struct {
	lookPath func(string) (string, error)
	run      func(ctx context.Context, bin string, args []string) (string, error)
	goos     string
	devTools func() bool
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]toolEntry
}

type toolEntry struct {
	stamp   string
	version string
	ok      bool
	at      time.Time
}

// toolProbe is one tool and how it asks for its own version -- `go
// version` is a subcommand, not a flag.
type toolProbe struct {
	name string
	args []string
}

// toolProbes is the list, in name order, which is the order reported.
var toolProbes = []toolProbe{
	{"cargo", []string{"--version"}},
	{"docker", []string{"--version"}},
	{"git", []string{"--version"}},
	{"go", []string{"version"}},
	{"make", []string{"--version"}},
	{"node", []string{"--version"}},
	{"npm", []string{"--version"}},
	{"python3", []string{"--version"}},
	{"rustc", []string{"--version"}},
}

func newToolchain() *toolchain {
	return &toolchain{
		lookPath: exec.LookPath,
		run:      runToolVersion,
		goos:     runtime.GOOS,
		devTools: macDeveloperToolsInstalled,
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
}

// versions asks every tool concurrently, under one budget, and returns
// the ones that answered, in name order.
func (t *toolchain) versions(ctx context.Context) []harness.ToolVersion {
	ctx, cancel := context.WithTimeout(ctx, toolProbeBudget)
	defer cancel()
	found := make([]*harness.ToolVersion, len(toolProbes))
	var wg sync.WaitGroup
	for i, p := range toolProbes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, ok := t.version(ctx, p); ok {
				found[i] = &harness.ToolVersion{Name: p.name, Version: v}
			}
		}()
	}
	wg.Wait()
	out := []harness.ToolVersion{}
	for _, v := range found {
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// version is one tool's own report of its version, cached against its
// binary.
func (t *toolchain) version(ctx context.Context, p toolProbe) (string, bool) {
	path, err := t.lookPath(p.name)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", false
	}
	if t.goos == "darwin" && strings.HasPrefix(path, "/usr/bin/") && !t.devTools() {
		return "", false
	}
	stamp := toolStamp(path)
	t.mu.Lock()
	if e, ok := t.cache[p.name]; ok && e.stamp == stamp && t.now().Sub(e.at) < toolVersionTTL {
		t.mu.Unlock()
		return e.version, e.ok
	}
	t.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	raw, err := t.run(probeCtx, path, p.args)
	version := apps.Truncate(firstNonEmptyLine(raw))
	ok := err == nil && version != ""
	if ctx.Err() != nil {
		// The budget ran out, which says nothing about this tool; asking
		// again next session is the honest answer.
		return version, ok
	}
	t.mu.Lock()
	t.cache[p.name] = toolEntry{stamp: stamp, version: version, ok: ok, at: t.now()}
	t.mu.Unlock()
	return version, ok
}

// runToolVersion runs one version probe in the root directory, with
// nothing on stdin and stderr thrown away.
func runToolVersion(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = string(filepath.Separator)
	out, err := cmd.Output()
	return string(out), err
}

// macDeveloperToolsInstalled reports whether the command-line developer
// tools or Xcode are installed, which is what turns /usr/bin/git and its
// neighbours from install-dialog stubs into the tools themselves.
func macDeveloperToolsInstalled() bool {
	for _, dir := range []string{
		"/Library/Developer/CommandLineTools/usr/bin",
		"/Applications/Xcode.app/Contents/Developer/usr/bin",
	} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// toolStamp identifies a binary by path, size and mtime, so replacing it
// in place invalidates its cached version at once.
func toolStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path
	}
	return path + "|" + info.ModTime().UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(info.Size(), 10)
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
```

then apply the rest of the session edits (items 1, 2, 4's fingerprint call, 5), `Detector.Version`, and `newRig`.

- [ ] **Step 4: Run the whole suite**

Run: `go test ./... -count=1 && go vet ./... && gofmt -l .`
Expected: every package `ok`; `go vet` and `gofmt -l` print nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/appsession/fingerprint.go internal/worker/appsession/fingerprint_test.go \
  internal/worker/appsession/recording_test.go internal/worker/appsession/session.go internal/worker/appsession/session_test.go \
  internal/worker/apps/detect.go internal/worker/apps/detect_test.go
git commit -m "Issue #443: the environment fingerprint is every session's first event"
```

---

## Task 6: Documentation, full verification, and the plan's deletion

**Files:**
- Modify: `docs/local-apps.md` (a "What a session records" section after "Chunk streams"; the `event` row of the chunk-streams table; two rows in "When it does not work"), `CLAUDE.md` (a section after "Watched-folder backup"; the `internal/worker/` line of the structure map)
- Delete: `docs/superpowers/plans/2026-09-13-app-session-recording.md`

- [ ] **Step 1: Write the operator doc section** -- insert before `## The three kinds` (after the chunk-streams table and its paragraph):

---

## What a session records

Every tool call an app completes on this machine leaves the session as **one
`event` chunk, in one shape**, whichever app made it and whichever protocol
drove it — so the cluster never has to learn that Claude Code calls it `Bash`
and Codex calls it `commandExecution`. The session's **first** event describes
the machine as the run found it. Together they are the recording the engine
turns into work-spine rows ([memql#5396](https://github.com/znasllc-io/memql/issues/5396));
an engine that predates that shows each one as a progress line and keeps it in
the transcript, so nothing is lost in the meantime. Nothing on the wire changed
to carry them: an `event` chunk has always been a JSON body.

The model's **prose is not recorded here**. It stays on `text` and in the
transcript artifact; the recording is about what the app *did*.

### One action per completed call

```json
{
  "type": "memql.app_session.action", "v": 1,
  "seq": 3, "turn": 1,
  "id": "toolu_01ULvDD2AmfTHJ8V4GN3ESkH",
  "tool": "fs_write", "appTool": "Write",
  "args": {"file_path": "/work/out.txt", "content": "hello"},
  "cwd": "/work",
  "isError": false,
  "resultType": "string",
  "resultDigest": "sha256:8f6c…",
  "contents": [{"op": "write", "path": "/work/out.txt",
                "digest": "sha256:2cf2…", "bytes": 5,
                "encoding": "utf8", "data": "hello"}]
}
```

| Field | Meaning |
|---|---|
| `seq` | Counts **actions**, densely, from 1, across every turn of the session. A gap means an action is missing, exactly. The fingerprint is 0. |
| `turn` | The turn the call finished in. A follow-up is a new turn. |
| `id` | The app's own id for the call — the only join back to the app's transcript. |
| `parentId` | The call this one ran inside, for a Claude Code sub-agent's calls. |
| `tool` | `exec`, `fs_read`, `fs_write`, `fetch`, `mcp`, `agent` (the app's own bookkeeping — nothing outside it moved) or `other` (not classified; effects **unknown**). |
| `appTool` | The app's own name for the tool. Provenance only. |
| `args` | The call's arguments, **whole**, as the app expressed them. |
| `command` / `mcp` / `url` / `query` | The one thing a reader needs without parsing `args`: the command line an `exec` ran, the `{server, tool}` an `mcp` call reached, what a `fetch` asked for. |
| `exitCode` | Present **only when the app reported one**. |
| `isError` | The app's own verdict. Absent only on an `incomplete` call. |
| `resultType` / `resultDigest` | The result's inferred JSON type and `sha256`. Text that is JSON is typed by what it parses as. |
| `contents` | The files the call read or wrote — see below. |
| `incomplete` | The app started this call and the session never saw it finish: the process died, the turn was cancelled, or the result was too large to read. Recorded, never dropped, never assumed. |

**What is recorded as unknown stays unknown.** Claude Code puts no exit status
on the wire: a failed Bash call says `Exit code 3` in its text, and that `3` is
recorded; a successful one is recorded as `0` **only** when its own record
shows it ran to the end in the foreground. `grep` finding nothing exits 1 and
Claude Code reports it as a success — that call's `exitCode` is absent, not 0.
The same for a command sent to the background, and for a refusal.

### How each app's tools are classified

| `tool` | Claude Code | Codex (`codex-app-server`) | Codex (`codex-mcp`) |
|---|---|---|---|
| `exec` | `Bash` | `commandExecution` | `exec_command_*` |
| `fs_read` | `Read`, `Glob`, `Grep`, `LS` | `imageView` | `view_image_tool_call` |
| `fs_write` | `Write`, `Edit`, `MultiEdit`, `NotebookEdit` | `fileChange` | `patch_apply_*` |
| `fetch` | `WebFetch`, `WebSearch` | `webSearch` | `web_search_*` |
| `mcp` | `mcp__<server>__<tool>` | `mcpToolCall` | `mcp_tool_call_*` |
| `agent` | `Task`, `ToolSearch`, `TodoWrite`, `Skill`, `StructuredOutput`, … | `collabAgentToolCall`, `sleep` | — |
| `other` | anything else | `dynamicToolCall`, `imageGeneration` | — |

**Codex reads with its shell.** It has no read tool; it runs `cat` or `sed -n`.
Its own parse of the command names the file it read, and that is how a Codex
`exec` carries a `contents` entry with `op: "read"` — the same file, digested
the same way, as Claude Code's `Read` of it.

### File contents

A call that read or wrote a file names it, and the **session** decides whether
its bytes leave this machine — the harness never reads a file itself. A file is
read, at the moment the call completes, only when:

- the call **succeeded** — a refused write wrote nothing;
- the path resolves, **symlinks followed**, to somewhere **inside the workspace**;
- it is not the session's own scaffolding: `.mcp.json` with the per-run bearer,
  `.memql-session/` (the transcript, Codex's per-session home and the `auth.json`
  linked into it), or a configuration moved aside;
- it is a **regular file**, checked on the opened descriptor, so a path swapped
  for a named pipe cannot hang the session.

| Size | What travels |
|---|---|
| up to 1 MiB | the bytes inline (`utf8`, or `base64` when they are not text), with the digest |
| more than 4 MiB already inline in the same action | the digest, `omitted: over_budget` |
| over 1 MiB | the digest, `omitted: over_ceiling` |
| over 64 MiB | the size, `omitted: too_large` |

Anything that was not read carries its path and a reason: `outside_workspace`,
`session_scaffolding`, `not_found`, `not_regular`, `unreadable`. The digest is
over the **whole file** as it stood — not over the lines the app happened to
look at — because it is what a later replay compares before it trusts the file
is the same.

The recording is **not** subject to `limits.max_transcript_bytes`. That limit
bounds the narration the engine keeps on the session row; dropping the fortieth
call because the app was chatty about the first thirty-nine would record a
session that stopped halfway. Like every chunk, the recording passes the
session's redactor on the way out.

### The fingerprint

```json
{
  "type": "memql.app_session.fingerprint", "v": 1, "seq": 0,
  "takenAt": "2026-09-13T23:00:41.113Z",
  "app": {"id": "claude-code", "version": "2.1.270 (Claude Code)", "harness": "claude-headless"},
  "platform": {"os": "linux", "arch": "amd64"},
  "tools": [{"name": "git", "version": "git version 2.43.0"},
            {"name": "go", "version": "go version go1.26.6 linux/amd64"}],
  "cwd": "/work", "cwdDigest": "sha256:…", "cwdEntries": 42,
  "variables": [{"name": "PATH", "set": true, "digest": "sha256:…"},
                {"name": "TZ", "set": false}],
  "inputs": [{"artifact": "art_123", "path": "/work/spec.md", "digest": "sha256:…", "bytes": 2048}]
}
```

- **`tools`** are `cargo`, `docker`, `git`, `go`, `make`, `node`, `npm`,
  `python3` and `rustc`, where installed, each as it reports its own version.
  They are asked once and cached against each binary, so back-to-back sessions
  cost nothing. On a Mac without the command-line developer tools,
  `/usr/bin/git` and its neighbours are stubs that answer by opening an install
  dialog, and they are never asked.
- **`cwdDigest`** is over the workspace's **listing** — names and kinds, never
  contents. `node_modules/`, `.git/` and the other dependency directories are
  listed but not entered; the session's scaffolding is left out, so the listing
  describes the workspace as you left it.
- **`variables`** are `PATH`, `SHELL`, `LANG`, `LC_ALL` and `TZ` (plus
  `CLAUDE_CONFIG_DIR` and `ANTHROPIC_MODEL` for Claude Code), as set-or-not and
  a **digest** of the value — never the value, which can carry a home directory
  or a token.
- **`inputs`** are the Library artifacts handed to the session, as they landed.
  The files the session *read* are on its actions, digested when they were read.

Change the chunk-streams table's `event` row to: ``| `event` | structured progress, as the app reported it -- and the recording, below |``. Add to "When it does not work":

```markdown
| an action's file carries `omitted: outside_workspace` or `session_scaffolding` | the app named a file the recording will not read: outside the session's workspace, or the session's own files. Correct refusal; the path is still recorded |
| an action carries `incomplete: true` | the app started the call and the turn ended before it finished -- a cancel, a crash, or a result line over 1 MiB the harness could not parse |
```

- [ ] **Step 2: Write the CLAUDE.md section** -- insert before `## The role is a slug with a rank`:

## App-session recording (memql-cockpit#440)

Every tool call an app completes leaves the session as ONE normalized
`event` chunk, and the session's first event is the environment
fingerprint. Engine half: epic memql#5396, **not merged**; the record is
the ENGINE repository's
`docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md`
(epic B; D2, D5, D12, D16), and this repository has no separate record.
`internal/worker/harness/record.go` is the wire shape;
`claudeactions.go` / `codexactions.go` translate each app;
`internal/worker/appsession/record.go` and `fingerprint.go` are the
machine's side. Operator doc: [docs/local-apps.md](docs/local-apps.md).

**One shape, normalized HERE, so the engine never parses a vendor
format.** `memql.app_session.action` carries `seq, turn, id, parentId?,
tool, appTool, args, cwd, command?, mcp?, url?, query?, exitCode?,
isError?, resultType?, resultDigest?, contents?, incomplete?`; `tool` is
the closed set exec / fs_read / fs_write / fetch / mcp / agent / other.
`agent` is the app's own bookkeeping (known to touch nothing); `other` is
unclassified and its effects are UNKNOWN -- guessing `agent` for a tool
that sent a notification would let a replay skip it. No proto change:
the type words are namespaced because the same stream carries the apps'
own events verbatim. `TestActionWireContract` pins the names; the engine
has not written its decoder, so this repository DEFINES them.

**`seq` counts ACTIONS, densely, from 1, across every turn; the
fingerprint is 0.** It is not the chunk seq: a gap in it says exactly one
call is missing, which a chunk gap cannot.

**Every unknown stays unknown.** `exitCode` is present only when the app
reported one. Claude Code has no exit-code field: a failed Bash result's
text opens "Exit code N", and a succeeded one exited 0 ONLY when
`tool_use_result` says it ran to the end in the foreground with no
`returnCodeInterpretation` -- `grep` finding nothing exits 1 and comes
back as a success. A call the app started and never finished is flushed
at the end of its turn with `incomplete: true` and no result: never
dropped, never assumed. A result line over the 1 MiB parse bound is one
way that happens.

**The harness names files; the SESSION decides what leaves the machine.**
Contents are read when the call completes, only if it succeeded, only if
the path resolves (symlinks followed) inside the workspace and outside
the scaffolding (`.mcp.json` with the per-run bearer, `.memql-session/`,
a moved-aside config), and only a regular file checked on the OPEN
descriptor (`O_NONBLOCK|O_NOFOLLOW`, so a pipe cannot hang the session).
1 MiB per file and 4 MiB per action travel inline; up to 64 MiB is
digested; the rest carries a closed `omitted` reason. The digest is over
the whole file, not the window the app read. Codex reads with its shell,
so its reads come from the app-server's own command parse
(`commandActions`) and nothing else.

**The recording is uncapped.** `limits.max_transcript_bytes` bounds the
narration the engine keeps on the row; an action is not narration, so
`Sink.Record` is a method of its own (never a stream word a typo could
turn into narration) and the session sends it past the cap, like the
structured answer.

**The fingerprint is facts to compare, so values are digests.** App id,
version (from the Detector's cache) and harness; platform; a fixed
toolchain asked `--version` in `/`, cached by binary stamp -- on darwin a
`/usr/bin` shim is never run without the developer tools, because it
answers by opening an install dialog from a LaunchAgent; the workspace
LISTING digest (names and kinds, dependency directories listed but not
entered, scaffolding left out, a moved-aside config listed under its own
name); the harness-named variables as set-or-not plus digest; the Library
inputs. `CODEX_HOME` is deliberately not a named variable: it is fresh
for every session and would match nothing.

and extend the structure map's `internal/worker/` entry with `harness/ records every completed tool call as one normalized action (record.go) -- see App-session recording`.

- [ ] **Step 3: Verify everything, against the local engine AND the pinned one**

Run:
```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test -count=1 -timeout=300s ./...
bash scripts/install/lib_test.sh
for t in FuzzShellJoinRoundTrips FuzzResultShapeIsClosed; do go test ./internal/worker/harness/ -run '^$' -fuzz "$t" -fuzztime 20s; done
go test ./internal/worker/appsession/ -run '^$' -fuzz FuzzContentPolicyStaysInTheWorkspace -fuzztime 20s
```
Expected: all `ok`, nothing printed by `gofmt -l`. Then build and test once more with `../memql` checked out at `.github/memql-pin` (a detached memql worktree beside a detached cockpit worktree of this branch), because CI resolves against the pin and the local sibling may have moved past it.

- [ ] **Step 4: Delete this plan and commit the docs**

```bash
git rm docs/superpowers/plans/2026-09-13-app-session-recording.md
git add docs/local-apps.md CLAUDE.md
git commit -m "Issue #443: document the recording; delete the epic's plan"
```


---

## Self-review

**Spec coverage** (the record's Cockpit bullet for epic B, and each issue's acceptance):

| Requirement | Where |
|---|---|
| One normalized action event per completed tool call, both apps | Tasks 1-3 (`Action`; Claude pairs; app-server items; mcp-server core events) |
| `{id, seq, tool, args, cwd, exitCode, isError, resultDigest, resultType, contentInline?}` | `Action` carries every one. `contentInline?` is `contents`: a LIST because one Codex file change touches several files, and each entry carries the digest even when its bytes do not fit, which is what the engine's `contentOmitted` needs |
| Emitted as an `event` chunk | Task 4 (`sessionSink.Record` -> `emitUncapped(StreamEvent)`) |
| The fingerprint, once, as the first event | Task 5 (`sendFingerprint` before the kind switch) |
| Tool versions, cwd listing digest, variables named by the harness, files-read digests (D16) | Task 5 (`tools`, `cwdDigest`, `variables` via `harness.FingerprintVariables`); files READ are per action (`contents` op `read`), which is the only moment they can be digested -- stated in `Fingerprint`'s doc, the operator doc and CLAUDE.md |
| Transcript prose stays on stdout/text | Unchanged; `TestClaudeActionFollowsTheAppsOwnReport` asserts the vendor chunks and prose are untouched |
| No proto change | Nothing in `go.mod`, the pin or the generated code moves |
| #441: a fixture stream-json transcript yields exactly one event per completed tool call with monotonic seq | `TestClaudeRecordsOneActionPerCompletedCall` (recorded fixture, 7 calls, seq 1..7) |
| #441: a tool_result with is_error sets isError | `TestClaudeToolResultWithIsErrorSetsIsError` (recorded refusals), `TestClaudeRecordsOneActionPerCompletedCall` (the exit-3 call) |
| #442: an app-server fixture yields the same event shape as the Claude Code fixture for the same actions | `TestRecordingIsTheSameShapeFromBothApps` |
| #443: the first event of every session is the fingerprint with tool versions and the cwd digest | `TestSession_FingerprintIsTheFirstEvent` (run, attach, and a harness that never starts) |
| D5: arguments whole; content-addressed file contents | `Args` is never truncated; `contents` carries the sha256 of the whole file |
| D12: the app's id, sequence, cwd, exit code, error flag | `ID`, `Seq`, `Cwd`, `ExitCode`, `IsError` |
| Out-of-order events recorded as a gap | The engine's; the dense action seq is what lets it see one |

**Placeholder scan:** no TBD / TODO / "similar to"; every code step carries its code.

**Type consistency:** `recording.complete(id string, finish func(*Action)) (Action, bool)` is used that way in `claudeheadless.go`, `codexappserver.go` and `codexmcp.go`; `codexItemCall` returns `(Action, codexItem, bool)` at both call sites; `codexCoreCall` returns `(Action, int, bool)` and its phase is compared to `corePhaseBegin`; `listWorkspace(root, configPath, backupPath string, limit int)` is called with `maxListingEntries` in `fingerprint.go` and small limits in tests; `readForRecord(path, keepMax)` returns `fileRead{size *int64, digest, data []byte, omitted}` and both callers read those fields; `rigAppVersion` / `rigTools` are declared beside `newRig` and used in `recording_test.go`.
