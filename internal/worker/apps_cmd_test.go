package worker

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// apps_cmd_test.go pins what `memql worker apps` tells an owner
// (memql-cockpit#438): for each app, whether the cluster can use it here and
// why not, how it is driven, and what every level becomes -- with each row
// saying whose entry it is, so an owner who edited apps.levels can see that
// the edit took.

func appsCmdPolicy(t *testing.T, body string) (*tools.Policy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p, path
}

func TestPrintAppInventory(t *testing.T) {
	policy, path := appsCmdPolicy(t, `apps:
  allow: [claude-code]
  levels:
    claude-code:
      reasoning:
        model: opus
        effort: max
      strong:
        effort: extreme
    gemini-cli:
      fast: {model: gemini-3}
`)
	inventory := []apps.Info{{
		Id: apps.IDClaudeCode, Version: "2.1.270 (Claude Code)", SignedIn: true, Allowed: true,
		Harness: apps.HarnessClaudeHeadless, StructuredResult: true, FollowUps: true,
	}}

	var out bytes.Buffer
	printAppInventory(&out, inventory, policy, path)
	got := out.String()

	for _, want := range []string{
		"claude-code  2.1.270 (Claude Code)",
		"allowed and signed in -- the cluster can send it sessions",
		"claude-headless: structured answers, follow-up turns",
		"fast", "--model haiku", "built-in",
		"reasoning", "--model opus --effort max", "policy.yaml",
		"strong", "REFUSED",
		"embeddings", "never through an app",
		"codex        not installed (no codex on PATH)",
		`apps.levels.claude-code.strong: effort "extreme"`,
		`this cockpit drives no app called "gemini-cli"`,
		path,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
	// The refused row must not ALSO show a knob: a row that says what a
	// level runs at and that it is refused is two claims, one of them false.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "REFUSED") && strings.Contains(line, "--model") {
			t.Errorf("a refused row still names knobs: %q", line)
		}
	}
}

// Each state an owner can fix names the fix.
func TestAppVerdict(t *testing.T) {
	cases := []struct {
		info apps.Info
		want string
	}{
		{apps.Info{Allowed: true, SignedIn: true}, "the cluster can send it sessions"},
		{apps.Info{Allowed: false, SignedIn: true}, "BLOCKED -- add it to apps.allow in /p/policy.yaml"},
		{apps.Info{Allowed: true, SignedIn: false}, "NOT signed in -- sign in to the app itself"},
		{apps.Info{Allowed: false, SignedIn: false}, "BLOCKED and NOT signed in"},
	}
	for _, c := range cases {
		if got := appVerdict(c.info, "/p/policy.yaml"); !strings.Contains(got, c.want) {
			t.Errorf("appVerdict(%+v) = %q, want it to contain %q", c.info, got, c.want)
		}
	}
}

func TestHarnessLine(t *testing.T) {
	got := harnessLine(apps.Info{Harness: apps.HarnessCodexMCP, StructuredResult: false, FollowUps: true})
	if !strings.Contains(got, "codex-mcp") || !strings.Contains(got, "no structured answers") {
		t.Errorf("harnessLine = %q, want the fallback's limit named", got)
	}
}

// An owner with no apps.levels block reads the built-in table and no
// problems section at all.
func TestPrintAppInventory_NoLevelsBlock(t *testing.T) {
	policy, path := appsCmdPolicy(t, "apps:\n  allow: [codex]\n")
	inventory := []apps.Info{{
		Id: apps.IDCodex, Version: "codex-cli 0.153.4", SignedIn: true, Allowed: true,
		Harness: apps.HarnessCodexAppServer, StructuredResult: true, FollowUps: true,
	}}
	var out bytes.Buffer
	printAppInventory(&out, inventory, policy, path)
	got := out.String()

	if !strings.Contains(got, "model_reasoning_effort=medium, on the account's default model") {
		t.Errorf("output is missing codex's built-in strong entry:\n%s", got)
	}
	if strings.Contains(got, "policy.yaml ") && strings.Contains(got, "problem") {
		t.Errorf("a file with no apps.levels has no problems to list:\n%s", got)
	}
	if !strings.Contains(got, "claude-code  not installed (no claude on PATH)") {
		t.Errorf("an app that is not installed must say so rather than vanish:\n%s", got)
	}
}
