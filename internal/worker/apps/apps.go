// Package apps knows which local coding apps this machine has, whether
// they can actually be driven, and how to invoke them.
//
// It exists because the engine cannot discover any of this. The cockpit
// dials out from behind NAT; the engine reads what the cockpit reports on
// Register and every Heartbeat and derives `app:<id>` routing labels from
// it (memql#4359). Nothing here decides whether a run happens -- that is
// the engine's delegation policy plus the consent gates -- but everything
// here decides whether this machine is ELIGIBLE to be picked.
//
// Two rules run through the whole package:
//
//   - `signed_in=false` beats a guess. A label is derived only from an
//     entry that is BOTH allowed and signed in, precisely so the router
//     cannot select a machine that would then refuse the run. A
//     best-effort `true` produces a plan committed to a laptop that says
//     no, and the resulting failure names the router rather than the auth
//     state. So every probe here reports false when it cannot tell.
//
//   - `subscription` is REPORTED, never inferred. The closed set is
//     unknown / none / present, and "unknown" is the normal answer today.
//     The engine records unknown as billing "unknown", which is honest; a
//     value derived from "well, they have the binary" would be recorded
//     as measured.
//
// The engine's runnable set is closed (claude-code, codex). A cockpit may
// report more -- they are stored and never driven -- but there is no
// reason for this package to invent any.
package apps

import "strings"

// The app ids the engine can drive. Closed set, mirrored from
// component/worker/apps.go in the engine repo.
const (
	IDClaudeCode = "claude-code"
	IDCodex      = "codex"
)

// The closed set of subscription answers. Mirrored from the engine's
// NormalizeSubscription, which folds anything else -- empty included --
// to Unknown.
const (
	SubscriptionUnknown = "unknown"
	SubscriptionNone    = "none"
	SubscriptionPresent = "present"
)

// MaxFieldLen bounds each reported string. The engine truncates at 200
// on its side; matching it here means an operator reading the portal sees
// the same value the cockpit logged, rather than a longer one that got
// clipped in transit.
const MaxFieldLen = 200

// Info is one detected app, in the shape Register and Heartbeat carry.
//
// Allowed and SignedIn are separate on purpose and both must be true
// before the engine will route to this machine. Allowed is this machine's
// own policy.yaml verdict; SignedIn is the app's auth state as the
// cockpit can observe it without spending the user's tokens to ask.
type Info struct {
	Id           string
	Version      string
	SignedIn     bool
	Subscription string
	Allowed      bool
}

// Spec is everything the cockpit needs to detect and drive one app.
//
// It is data rather than an interface because the set is closed and small;
// a plugin seam here would be a seam for apps the engine cannot drive.
type Spec struct {
	// ID is the engine's app id.
	ID string
	// Binary is what the app calls itself on PATH.
	Binary string
	// VersionArgs asks the app for its own version. Whatever it prints
	// is reported verbatim -- the engine reduces it to major.minor for
	// the label, so pre-reducing it here would throw away the patch
	// level the portal shows.
	VersionArgs []string
	// StreamsJSON reports whether the headless run emits newline-
	// delimited JSON the engine can map to progress events. When false
	// the runner sends plain stdout as narration and never synthesises
	// event chunks out of it.
	StreamsJSON bool
}

// Specs returns the closed set, in stable id order.
func Specs() []Spec {
	return []Spec{
		{
			ID:     IDClaudeCode,
			Binary: "claude",
			// `claude --version` prints e.g. "2.1.4 (Claude Code)".
			VersionArgs: []string{"--version"},
			StreamsJSON: true,
		},
		{
			ID:     IDCodex,
			Binary: "codex",
			// `codex --version` prints e.g. "codex-cli 0.9.1".
			VersionArgs: []string{"--version"},
			StreamsJSON: false,
		},
	}
}

// SpecFor returns the spec for an app id.
func SpecFor(id string) (Spec, bool) {
	for _, s := range Specs() {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}

// IsKnownID reports whether id is in the engine's closed runnable set.
func IsKnownID(id string) bool {
	_, ok := SpecFor(strings.TrimSpace(id))
	return ok
}

// RunArgs builds the argv for a headless, autonomous run of prompt.
//
// This is the `run` kind: no human is attached, the engine reads the
// output, and the process must terminate on its own.
func (s Spec) RunArgs(prompt string) []string {
	switch s.ID {
	case IDClaudeCode:
		// -p is Claude Code's headless "print" mode. stream-json gives
		// the structured events the engine maps to progress; without
		// --verbose that format is refused by the CLI.
		return []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	case IDCodex:
		// `codex exec` is the non-interactive form.
		return []string{"exec", prompt}
	}
	return nil
}

// AttachArgs builds the argv that resumes the app's OWN session named by
// ref, streaming it the way a run streams.
//
// Returns nil when the app has no resume mechanism, which the runner
// turns into a named failure rather than a silent headless run.
func (s Spec) AttachArgs(ref string) []string {
	switch s.ID {
	case IDClaudeCode:
		return []string{"--resume", ref, "--output-format", "stream-json", "--verbose"}
	case IDCodex:
		return []string{"exec", "resume", ref}
	}
	return nil
}

// InteractiveArgs builds the argv that hands the app to a HUMAN with the
// prompt loaded -- the `open` kind. The workspace is the process's cwd, so
// it is not repeated in argv.
func (s Spec) InteractiveArgs(prompt string) []string {
	switch s.ID {
	case IDClaudeCode:
		if strings.TrimSpace(prompt) == "" {
			return nil
		}
		// A bare positional prompt starts an interactive session with
		// that first turn already sent.
		return []string{prompt}
	case IDCodex:
		if strings.TrimSpace(prompt) == "" {
			return nil
		}
		return []string{prompt}
	}
	return nil
}

// Truncate bounds a reported field to what the engine will store.
func Truncate(s string) string {
	if len(s) <= MaxFieldLen {
		return s
	}
	return s[:MaxFieldLen]
}

// NormalizeSubscription clamps a reported value to the closed set.
// Anything unrecognised -- empty included -- is "unknown", which is the
// honest answer rather than "none".
func NormalizeSubscription(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SubscriptionNone:
		return SubscriptionNone
	case SubscriptionPresent:
		return SubscriptionPresent
	}
	return SubscriptionUnknown
}
