package apps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Detector reports the local app inventory. Safe for concurrent use; the
// worker calls Detect from the heartbeat loop and from Register.
//
// Every field is injectable so the whole thing is testable without a real
// claude or codex on PATH -- which matters, because CI has neither and a
// detector that could only be exercised by hand is a detector nobody
// exercises.
type Detector struct {
	// LookPath resolves a binary. Defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// RunVersion asks an app for its own version string. Defaults to a
	// bounded exec of the resolved binary.
	RunVersion func(ctx context.Context, bin string, args []string) (string, error)
	// Home is where the apps keep their own state. Defaults to $HOME.
	Home string
	// Now defaults to time.Now.
	Now func() time.Time
	// VersionTTL is how long a probed version is reused. Defaults to
	// DefaultVersionTTL.
	VersionTTL time.Duration

	mu    sync.Mutex
	cache map[string]versionEntry
}

// DefaultVersionTTL bounds how stale a cached version string may be.
//
// WHY CACHE AT ALL. Detect runs on EVERY heartbeat -- the engine applies
// an inventory change to its live registry immediately, so signing into
// Claude Code makes this machine selectable on the next beat rather than
// the next reconnect. At a 15s beat, an uncached probe would fork two
// processes a minute forever on somebody's laptop, to answer a question
// whose answer changes when they run an installer.
//
// So the SLOW half is cached and the FAST half never is: the version
// comes from a subprocess and is reused; presence and auth state are
// filesystem reads and are taken fresh every beat. That keeps the
// property the cadence exists for -- a sign-in shows up on the next beat
// -- without the process churn.
//
// The cache key includes the binary's size and mtime, so an in-place
// upgrade invalidates immediately rather than waiting out the TTL.
const DefaultVersionTTL = 5 * time.Minute

// versionProbeTimeout bounds one `--version` call. An app that hangs on
// its own version flag must not wedge the heartbeat loop behind it.
const versionProbeTimeout = 5 * time.Second

type versionEntry struct {
	version string
	stamp   string
	at      time.Time
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Detector) home() string {
	if d.Home != "" {
		return d.Home
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func (d *Detector) lookPath(bin string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(bin)
	}
	return exec.LookPath(bin)
}

func (d *Detector) runVersion(ctx context.Context, bin string, args []string) (string, error) {
	if d.RunVersion != nil {
		return d.RunVersion(ctx, bin, args)
	}
	ctx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Detect returns the inventory to report, sorted by id.
//
// allow is policy.yaml's apps.allow. An app that is present but not
// allowed is reported with allowed=false rather than omitted: the portal
// then shows it as present-and-blocked, which is the state an operator
// can act on. Omitting it would render identically to "not installed",
// and send them looking for the wrong problem.
//
// An app that is not on PATH is omitted entirely -- there is nothing to
// report and nothing to act on.
func (d *Detector) Detect(ctx context.Context, allow []string) []Info {
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[strings.ToLower(strings.TrimSpace(a))] = true
	}

	home := d.home()
	var out []Info
	for _, spec := range Specs() {
		path, err := d.lookPath(spec.Binary)
		if err != nil || strings.TrimSpace(path) == "" {
			continue
		}
		state := probeAuth(spec.ID, home)
		out = append(out, Info{
			Id:           spec.ID,
			Version:      Truncate(d.version(ctx, spec, path)),
			SignedIn:     state.signedIn,
			Subscription: NormalizeSubscription(state.subscription),
			Allowed:      allowed[spec.ID],
		})
	}
	// Stable order. The engine sorts by id anyway, but an unstable one
	// here would rewrite the registration row on every beat for no
	// actual change.
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// version returns the app's own version string, cached against the
// binary's identity.
func (d *Detector) version(ctx context.Context, spec Spec, path string) string {
	stamp := binaryStamp(path)
	ttl := d.VersionTTL
	if ttl <= 0 {
		ttl = DefaultVersionTTL
	}

	d.mu.Lock()
	if entry, ok := d.cache[spec.ID]; ok && entry.stamp == stamp && d.now().Sub(entry.at) < ttl {
		d.mu.Unlock()
		return entry.version
	}
	d.mu.Unlock()

	raw, err := d.runVersion(ctx, path, spec.VersionArgs)
	if err != nil {
		// A binary that will not report its version is still present,
		// and still worth reporting: the engine reads an unparseable
		// version as "any version" rather than as absent. Do not cache
		// the failure -- a half-finished install should resolve itself
		// on the next beat, not in five minutes.
		return ""
	}
	version := strings.TrimSpace(raw)
	if i := strings.IndexAny(version, "\r\n"); i >= 0 {
		version = strings.TrimSpace(version[:i])
	}

	d.mu.Lock()
	if d.cache == nil {
		d.cache = make(map[string]versionEntry, len(Specs()))
	}
	d.cache[spec.ID] = versionEntry{version: version, stamp: stamp, at: d.now()}
	d.mu.Unlock()
	return version
}

// binaryStamp identifies a binary by size and mtime, so replacing it in
// place invalidates the cached version without waiting out the TTL.
func binaryStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return path
	}
	return path + "|" + fi.ModTime().UTC().Format(time.RFC3339Nano) + "|" + itoa(fi.Size())
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// authState is what an app's own files say about itself.
type authState struct {
	signedIn     bool
	subscription string
}

// probeAuth reads the app's OWN state files.
//
// It never shells out to a real prompt to find out -- that spends the
// user's tokens to answer a status question, on a 15-second cadence -- and
// it never touches the OS keyring. The worker runs as a LaunchAgent on
// macOS and a keyring read there raises a Keychain prompt on a machine
// whose owner is not necessarily at it; the same reason the worker's run
// path already keeps the lazy file default for its own credentials.
//
// The consequence is stated plainly rather than papered over: on a macOS
// box where Claude Code put its token in the Keychain and wrote no
// account record, this reports signedIn=false. That is the designed
// direction of the error. The machine is simply not selected, and the
// portal says "not signed in" -- rather than showing a green row for a
// machine that will refuse the run after a plan has committed to it.
func probeAuth(id, home string) authState {
	if strings.TrimSpace(home) == "" {
		return authState{}
	}
	switch id {
	case IDClaudeCode:
		return probeClaudeCode(home)
	case IDCodex:
		return probeCodex(home)
	}
	return authState{}
}

func probeClaudeCode(home string) authState {
	state := authState{}

	// ~/.claude/.credentials.json is the token file Claude Code writes
	// where no OS keyring is available. Its presence with a non-empty
	// access token is a positive signal on its own.
	if obj, ok := readJSONObject(filepath.Join(home, ".claude", ".credentials.json")); ok {
		if oauth, ok := obj["claudeAiOauth"].(map[string]any); ok {
			if s, _ := oauth["accessToken"].(string); strings.TrimSpace(s) != "" {
				state.signedIn = true
			}
			state.subscription = reportedSubscription(oauth)
		}
	}

	// ~/.claude.json carries the account record. On macOS the token
	// itself lives in the Keychain, which this must not read, so the
	// account record is the only non-prompting signal available.
	if obj, ok := readJSONObject(filepath.Join(home, ".claude.json")); ok {
		if account, ok := obj["oauthAccount"].(map[string]any); ok {
			uuid, _ := account["accountUuid"].(string)
			email, _ := account["emailAddress"].(string)
			if strings.TrimSpace(uuid) != "" || strings.TrimSpace(email) != "" {
				state.signedIn = true
			}
			if sub := reportedSubscription(account); sub != "" {
				state.subscription = sub
			}
		}
		if sub := reportedSubscription(obj); sub != "" {
			state.subscription = sub
		}
	}
	return state
}

func probeCodex(home string) authState {
	state := authState{}
	obj, ok := readJSONObject(filepath.Join(home, ".codex", "auth.json"))
	if !ok {
		return state
	}
	if key, _ := obj["OPENAI_API_KEY"].(string); strings.TrimSpace(key) != "" {
		state.signedIn = true
	}
	if tokens, ok := obj["tokens"].(map[string]any); ok {
		access, _ := tokens["access_token"].(string)
		idTok, _ := tokens["id_token"].(string)
		if strings.TrimSpace(access) != "" || strings.TrimSpace(idTok) != "" {
			state.signedIn = true
		}
		if sub := reportedSubscription(tokens); sub != "" {
			state.subscription = sub
		}
	}
	if sub := reportedSubscription(obj); sub != "" {
		state.subscription = sub
	}
	return state
}

// subscriptionKeys are the keys an app may write its own plan into. This
// is a READ of what the app said, not a derivation: a key that is absent
// leaves the answer at "unknown", and no other field is consulted to fill
// it in. "unknown" is the expected value on most machines today, and the
// engine records it as billing "unknown" rather than as free.
var subscriptionKeys = []string{"subscriptionType", "subscription", "chatgpt_plan_type", "planType"}

// reportedSubscription maps a plan value the app wrote to the closed set.
// Returns "" when the app said nothing, so a caller can distinguish that
// from a deliberate "unknown".
func reportedSubscription(obj map[string]any) string {
	for _, key := range subscriptionKeys {
		raw, present := obj[key]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "":
			continue
		case "none", "free":
			return SubscriptionNone
		case "unknown":
			return SubscriptionUnknown
		default:
			// A named plan -- "pro", "max", "team", "plus", "enterprise"
			// -- is the app stating it has one. The specific tier is not
			// modelled: the engine's closed set is three values, and it
			// bills subscription runs the same way whichever tier it is.
			return SubscriptionPresent
		}
	}
	return ""
}

// readJSONObject reads a JSON object, returning ok=false for anything
// missing, unreadable or not an object. A malformed state file must read
// as "cannot tell", never as an error that stops the heartbeat.
func readJSONObject(path string) (map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false
	}
	return obj, true
}
