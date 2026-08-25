package apps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEnv builds a Detector wired to a temp home and a fixed PATH
// resolution, so none of this needs a real claude or codex.
type fakeEnv struct {
	home     string
	present  map[string]string // binary -> resolved path
	versions map[string]string // resolved path -> version output
	calls    atomic.Int64
	now      time.Time
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	return &fakeEnv{
		home:     t.TempDir(),
		present:  map[string]string{},
		versions: map[string]string{},
		now:      time.Unix(1_700_000_000, 0).UTC(),
	}
}

// install makes a binary resolvable and gives it a real file on disk, so
// the version cache's size+mtime stamp has something to read.
func (f *fakeEnv) install(t *testing.T, binary, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	f.present[binary] = path
	f.versions[path] = version
	return path
}

func (f *fakeEnv) writeJSON(t *testing.T, rel string, obj any) {
	t.Helper()
	path := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (f *fakeEnv) detector() *Detector {
	return &Detector{
		Home: f.home,
		Now:  func() time.Time { return f.now },
		LookPath: func(bin string) (string, error) {
			if p, ok := f.present[bin]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		RunVersion: func(_ context.Context, bin string, _ []string) (string, error) {
			f.calls.Add(1)
			if v, ok := f.versions[bin]; ok {
				return v, nil
			}
			return "", errors.New("no version")
		},
	}
}

func find(t *testing.T, got []Info, id string) Info {
	t.Helper()
	for _, a := range got {
		if a.Id == id {
			return a
		}
	}
	t.Fatalf("no entry for %q in %+v", id, got)
	return Info{}
}

// TestDetect_AbsentAppsAreOmitted: an app that is not on PATH produces no
// entry at all. There is nothing to report and nothing an operator can do
// about it, which is different from "installed but blocked".
func TestDetect_AbsentAppsAreOmitted(t *testing.T) {
	env := newFakeEnv(t)
	got := env.detector().Detect(context.Background(), []string{IDClaudeCode, IDCodex})
	if len(got) != 0 {
		t.Fatalf("nothing installed, got %+v", got)
	}
}

// TestDetect_PresentButNotAllowedIsReported is the distinction the issue
// turns on: an app that is present but missing from apps.allow reports
// allowed=false rather than being dropped. Dropping it renders identically
// to "not installed" in the portal, and sends an operator to look for the
// wrong problem.
func TestDetect_PresentButNotAllowedIsReported(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)")

	got := env.detector().Detect(context.Background(), nil)
	if len(got) != 1 {
		t.Fatalf("want the app reported, got %+v", got)
	}
	entry := find(t, got, IDClaudeCode)
	if entry.Allowed {
		t.Error("an app absent from apps.allow must report allowed=false")
	}
	if entry.Version != "2.1.4 (Claude Code)" {
		t.Errorf("version = %q, want the CLI's own string verbatim", entry.Version)
	}
}

// TestDetect_VersionIsVerbatim: the engine reduces the version to
// major.minor for the routing label, so pre-reducing it here would throw
// away the patch level the portal shows. Only trailing whitespace and
// anything past the first newline are dropped.
func TestDetect_VersionIsVerbatim(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)\nsome trailing banner\n")
	env.install(t, "codex", "  codex-cli 0.9.1  \n")

	got := env.detector().Detect(context.Background(), nil)
	if v := find(t, got, IDClaudeCode).Version; v != "2.1.4 (Claude Code)" {
		t.Errorf("claude version = %q", v)
	}
	if v := find(t, got, IDCodex).Version; v != "codex-cli 0.9.1" {
		t.Errorf("codex version = %q", v)
	}
}

// TestDetect_UnknownSignedInReportsFalse is the rule the whole epic rests
// on. A machine whose auth state cannot be read reports signed_in=false,
// so the engine derives no routing label and never selects it. A
// best-effort true would commit a plan to a machine that then refuses the
// run, and the failure would name the router rather than the auth state.
func TestDetect_UnknownSignedInReportsFalse(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.install(t, "codex", "0.9.1")

	got := env.detector().Detect(context.Background(), []string{IDClaudeCode, IDCodex})
	for _, a := range got {
		if a.SignedIn {
			t.Errorf("%s: no auth state on disk must read as signed_in=false", a.Id)
		}
		if a.Subscription != SubscriptionUnknown {
			t.Errorf("%s: subscription = %q, want %q", a.Id, a.Subscription, SubscriptionUnknown)
		}
	}
}

// TestDetect_SignedInFromCredentialsFile covers the Linux/no-keyring
// shape: ~/.claude/.credentials.json with a live access token.
func TestDetect_SignedInFromCredentialsFile(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.writeJSON(t, ".claude/.credentials.json", map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat-xxx"},
	})

	entry := find(t, env.detector().Detect(context.Background(), []string{IDClaudeCode}), IDClaudeCode)
	if !entry.SignedIn {
		t.Error("a live access token must read as signed in")
	}
	if !entry.Allowed {
		t.Error("apps.allow lists it, so allowed must be true")
	}
}

// TestDetect_SignedInFromAccountRecord covers macOS, where the token
// itself is in the Keychain that this must never read. The account record
// in ~/.claude.json is the only non-prompting signal there is.
func TestDetect_SignedInFromAccountRecord(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.writeJSON(t, ".claude.json", map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "someone@example.com"},
	})

	if !find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Error("an account record must read as signed in")
	}
}

// TestDetect_CodexAuthShapes: either an API key or an OAuth token pair
// counts as signed in.
func TestDetect_CodexAuthShapes(t *testing.T) {
	t.Run("api key", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{"OPENAI_API_KEY": "sk-xxx"})
		if !find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an API key must read as signed in")
		}
	})
	t.Run("oauth tokens", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{
			"tokens": map[string]any{"access_token": "at-xxx"},
		})
		if !find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an oauth token must read as signed in")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{})
		if find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an auth file with no credential must read as NOT signed in")
		}
	})
}

// TestDetect_SubscriptionIsReportedNeverInferred: the value comes from a
// key the app itself wrote, and stays "unknown" when the app said
// nothing. The engine records unknown as billing "unknown", which is the
// honest answer; a value derived from "they have the binary" would be
// recorded as measured.
func TestDetect_SubscriptionIsReportedNeverInferred(t *testing.T) {
	cases := []struct {
		name  string
		claim map[string]any
		want  string
	}{
		{"silent", map[string]any{"accountUuid": "u-1"}, SubscriptionUnknown},
		{"named plan", map[string]any{"accountUuid": "u-1", "subscriptionType": "max"}, SubscriptionPresent},
		{"explicit none", map[string]any{"accountUuid": "u-1", "subscriptionType": "none"}, SubscriptionNone},
		{"explicit free", map[string]any{"accountUuid": "u-1", "subscriptionType": "free"}, SubscriptionNone},
		{"explicit unknown", map[string]any{"accountUuid": "u-1", "subscriptionType": "unknown"}, SubscriptionUnknown},
		{"empty string", map[string]any{"accountUuid": "u-1", "subscriptionType": ""}, SubscriptionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnv(t)
			env.install(t, "claude", "2.1.4")
			env.writeJSON(t, ".claude.json", map[string]any{"oauthAccount": tc.claim})
			got := find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).Subscription
			if got != tc.want {
				t.Errorf("subscription = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetect_MalformedStateFileReadsAsCannotTell: a half-written state
// file must not panic, error, or be read optimistically. It reads as
// "cannot tell", which is signed_in=false.
func TestDetect_MalformedStateFileReadsAsCannotTell(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	path := filepath.Join(env.home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Error("a malformed state file must not read as signed in")
	}
}

// TestDetect_OrderIsStable: the engine sorts by id anyway, but an
// unstable order here would rewrite the registration row on every beat
// for no actual change.
func TestDetect_OrderIsStable(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "codex", "0.9.1")
	env.install(t, "claude", "2.1.4")

	var first []string
	for i := range 25 {
		var ids []string
		for _, a := range env.detector().Detect(context.Background(), nil) {
			ids = append(ids, a.Id)
		}
		if i == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("order drifted on run %d: %v vs %v", i, ids, first)
		}
	}
	if strings.Join(first, ",") != IDClaudeCode+","+IDCodex {
		t.Errorf("order = %v, want sorted by id", first)
	}
}

// TestDetect_VersionIsCachedButAuthIsNot pins the cadence bargain. Detect
// runs on every heartbeat; the version comes from a subprocess and is
// reused, while auth state is a file read and is taken fresh -- so
// signing into an app shows up on the NEXT BEAT without forking two
// processes a minute on somebody's laptop forever.
func TestDetect_VersionIsCachedButAuthIsNot(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	d := env.detector()

	for range 5 {
		d.Detect(context.Background(), nil)
	}
	if got := env.calls.Load(); got != 1 {
		t.Errorf("version probed %d times across 5 beats, want 1", got)
	}

	// Sign in between beats. The very next beat must see it.
	env.writeJSON(t, ".claude/.credentials.json", map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat-xxx"},
	})
	if !find(t, d.Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Fatal("signing in must be visible on the next beat, not the next reconnect")
	}
	if got := env.calls.Load(); got != 1 {
		t.Errorf("the auth re-read cost a version probe (%d calls)", got)
	}
}

// TestDetect_UpgradeInPlaceInvalidatesTheCache: replacing the binary must
// not leave a stale version on the wire until the TTL expires. The cache
// key carries the binary's size and mtime for exactly this.
func TestDetect_UpgradeInPlaceInvalidatesTheCache(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "claude", "2.1.4")
	d := env.detector()

	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.1.4" {
		t.Fatalf("version = %q", v)
	}

	// Upgrade in place, well inside the TTL.
	env.versions[path] = "2.2.0"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n# upgraded\n"), 0o755); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, env.now.Add(time.Hour), env.now.Add(time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.2.0" {
		t.Errorf("version = %q after an in-place upgrade, want 2.2.0", v)
	}
}

// TestDetect_VersionProbeFailureStillReportsTheApp: a binary that will
// not answer --version is still installed. The engine reads an
// unparseable version as "any version" rather than as absent, and the
// failure is not cached -- a half-finished install should resolve itself
// on the next beat, not in five minutes.
func TestDetect_VersionProbeFailureStillReportsTheApp(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "claude", "2.1.4")
	delete(env.versions, path)
	d := env.detector()

	entry := find(t, d.Detect(context.Background(), nil), IDClaudeCode)
	if entry.Version != "" {
		t.Errorf("version = %q, want empty", entry.Version)
	}
	env.versions[path] = "2.1.4"
	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.1.4" {
		t.Errorf("a failed probe was cached: version = %q on the next beat", v)
	}
}

// TestTruncate_MatchesTheEngineBound keeps the portal showing the same
// string the cockpit logged, rather than a longer one clipped in transit.
func TestTruncate_MatchesTheEngineBound(t *testing.T) {
	long := strings.Repeat("v", MaxFieldLen+50)
	if got := Truncate(long); len(got) != MaxFieldLen {
		t.Errorf("len = %d, want %d", len(got), MaxFieldLen)
	}
	if got := Truncate("2.1.4"); got != "2.1.4" {
		t.Errorf("a short value was altered: %q", got)
	}
}
