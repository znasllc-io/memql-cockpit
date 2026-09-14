package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// levels_policy_test.go pins apps.levels (memql-cockpit#438): the machine
// owner's override of the table a LEVEL is translated through.
//
// Its posture is the opposite of apps.allow beside it, and the tests below
// hold both halves of that: an absent block is NOT "nothing", it is the
// built-in table; and an entry the app would misread is not quietly dropped
// back to the built-in one, it REFUSES its level with a sentence naming the
// line to fix.

// THE BUILT-IN TABLE IS THE DEFAULT. apps.allow is default-deny because it
// decides WHETHER an app runs here; levels decide HOW it runs once it may,
// and the cockpit ships an answer for that. Every policy.yaml written before
// this key existed must keep working, at the built-in table.
func TestAppLevels_AbsentBlockMeansTheBuiltInTable(t *testing.T) {
	for name, p := range map[string]*tools.Policy{
		"the default policy":      tools.DefaultPolicy(),
		"a file with no levels":   policyFrom(t, "apps:\n  allow: [claude-code]\n"),
		"a file with no apps key": policyFrom(t, "shell:\n  allow: []\n"),
	} {
		t.Run(name, func(t *testing.T) {
			table, refused := p.AppLevels("claude-code")
			if len(table) != 0 || len(refused) != 0 {
				t.Errorf("table=%v refused=%v; want no entries, which the session reads as the built-in table",
					table, refused)
			}
			if problems := p.AppLevelProblems(); len(problems) != 0 {
				t.Errorf("problems = %v, want none", problems)
			}
		})
	}
}

// The documented shape, so an owner following docs/local-apps.md gets what
// they were told.
func TestAppLevels_LoadsTheDocumentedShape(t *testing.T) {
	p := policyFrom(t, `apps:
  allow:
    - claude-code
    - codex
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
	claude, refused := p.AppLevels("claude-code")
	if len(refused) != 0 {
		t.Fatalf("refused = %v, want none", refused)
	}
	if claude["reasoning"] != (harness.Knobs{Model: "fable", Effort: "max"}) {
		t.Errorf("claude-code reasoning = %+v", claude["reasoning"])
	}
	// An entry with neither knob is the owner choosing the app's own
	// defaults for that level. It stands, and it is not the built-in entry.
	if k, ok := claude["fast"]; !ok || k != (harness.Knobs{}) {
		t.Errorf("claude-code fast = %+v ok=%v, want an entry that stands with no knobs", k, ok)
	}
	if _, ok := claude["strong"]; ok {
		t.Error("a level the owner did not list must not appear: the built-in entry covers it")
	}
	codex, _ := p.AppLevels("codex")
	if codex["strong"] != (harness.Knobs{Model: "gpt-5.5", Effort: "xhigh"}) {
		t.Errorf("codex strong = %+v", codex["strong"])
	}
	if len(p.AppLevelProblems()) != 0 {
		t.Errorf("problems = %v, want none", p.AppLevelProblems())
	}
}

// The ids are the engine's, and apps.allow already reads them without
// regard to case or surrounding space; a level entry does the same, so the
// two keys an owner writes side by side agree about what they name.
func TestAppLevels_AppIDsReadLikeAppsAllow(t *testing.T) {
	p := policyFrom(t, "apps:\n  levels:\n    Claude-Code:\n      strong:\n        model: opus\n")
	table, _ := p.AppLevels("claude-code")
	if table["strong"].Model != "opus" {
		t.Errorf("table = %v, want the entry under Claude-Code to name claude-code", table)
	}
}

// AN ENTRY THE APP WOULD MISREAD REFUSES ITS LEVEL. Claude Code IGNORES an
// effort it does not know, so an owner's typo would otherwise run every
// strong session at a default nobody chose -- and falling back to the
// built-in entry instead would spend what the owner's entry was written to
// avoid spending.
func TestAppLevels_AnEntryTheAppWouldMisreadRefusesItsLevel(t *testing.T) {
	p := policyFrom(t, `apps:
  levels:
    claude-code:
      strong:
        effort: extreme
      fast:
        model: haiku
`)
	table, refused := p.AppLevels("claude-code")
	reason := refused["strong"]
	for _, want := range []string{
		"apps.levels.claude-code.strong",
		"low, medium, high, xhigh, max",
		"refuses strong sessions for claude-code",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal %q is missing %q", reason, want)
		}
	}
	if _, ok := table["strong"]; ok {
		t.Error("a refused entry must not also stand in the table")
	}
	if table["fast"].Model != "haiku" {
		t.Error("one bad entry must not take the app's other levels down with it")
	}
	if problems := p.AppLevelProblems(); len(problems) != 1 || problems[0] != reason {
		t.Errorf("problems = %v, want exactly the refusal, for the log and `memql worker apps`", problems)
	}
}

func TestAppLevels_AModelThatReadsAsAFlagRefusesItsLevel(t *testing.T) {
	p := policyFrom(t, "apps:\n  levels:\n    codex:\n      reasoning:\n        model: \"-c\"\n")
	_, refused := p.AppLevels("codex")
	if !strings.Contains(refused["reasoning"], "not a model name") {
		t.Errorf("refused = %v, want the reasoning level refused for its model", refused)
	}
}

// No session can ask for an app this cockpit does not drive, a word that is
// not a level, or the embeddings level through an app -- so those entries
// refuse nothing. They are PROBLEMS: logged when the file is read and shown
// by `memql worker apps`, because an entry that silently did nothing is
// indistinguishable from one that worked.
func TestAppLevels_UnknownAppsAndLevelsAreProblemsNotRefusals(t *testing.T) {
	p := policyFrom(t, `apps:
  levels:
    gemini-cli:
      fast: {model: gemini-3}
    codex:
      fastt: {effort: low}
      embeddings: {model: text-embed}
`)
	for _, app := range []string{"codex", "gemini-cli"} {
		if table, refused := p.AppLevels(app); len(table) != 0 || len(refused) != 0 {
			t.Errorf("%s: table=%v refused=%v, want nothing", app, table, refused)
		}
	}
	problems := p.AppLevelProblems()
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		`apps.levels.gemini-cli: this cockpit drives no app called "gemini-cli" (it drives claude-code, codex)`,
		`apps.levels.codex.fastt: unknown level "fastt"`,
		`apps.levels.codex.embeddings: level "embeddings" never runs through an app`,
		"so the entry is ignored",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems are missing %q:\n%s", want, joined)
		}
	}
	if len(problems) != 3 {
		t.Errorf("problems = %d, want one per bad entry: %v", len(problems), problems)
	}
}

// apps.levels REPLACES on reload, the way models.runtimes does: an entry is
// a record, and merging two generations of it would run one file's model at
// another file's effort. Removing the block returns the built-in table.
func TestAppLevels_ReplaceOnReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("apps:\n  levels:\n    claude-code:\n      strong: {model: opus, effort: max}\n")
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if table, _ := p.AppLevels("claude-code"); table["strong"].Model != "opus" {
		t.Fatalf("setup: want the entry loaded, got %v", table)
	}

	write("apps:\n  levels:\n    claude-code:\n      fast: {model: sonnet}\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	table, _ := p.AppLevels("claude-code")
	if _, ok := table["strong"]; ok {
		t.Error("a reload kept an entry the file no longer has")
	}
	if table["fast"].Model != "sonnet" {
		t.Errorf("fast = %+v, want the reloaded entry", table["fast"])
	}

	write("apps:\n  allow: [claude-code]\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if table, _ := p.AppLevels("claude-code"); len(table) != 0 {
		t.Errorf("after removing the block: %v, want the built-in table back", table)
	}
}

// The maps a caller gets are its own: a SIGHUP reload underneath a session
// that is still deciding must not change the table it is holding.
func TestAppLevels_ReturnsCopies(t *testing.T) {
	p := policyFrom(t, "apps:\n  levels:\n    claude-code:\n      strong: {model: opus}\n")
	table, _ := p.AppLevels("claude-code")
	table["strong"] = harness.Knobs{Model: "tampered"}
	again, _ := p.AppLevels("claude-code")
	if again["strong"].Model != "opus" {
		t.Error("editing the returned table edited the policy")
	}
}

func TestAppLevels_NilPolicy(t *testing.T) {
	var p *tools.Policy
	if table, refused := p.AppLevels("claude-code"); table != nil || refused != nil {
		t.Errorf("a nil policy has no entries: %v %v", table, refused)
	}
	if p.AppLevelProblems() != nil {
		t.Error("a nil policy has no problems")
	}
}
