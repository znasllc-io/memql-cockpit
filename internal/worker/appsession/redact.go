package appsession

import (
	"bytes"
	"sync"
)

// redact.go keeps the session bearer out of the transcript.
//
// The transcript is persisted on the engine side and rendered in the
// portal, so a chunk carrying the credential publishes it to everywhere
// that record reaches. The app can echo its own configuration for
// entirely reasonable reasons -- a debug flag, an error message quoting
// the file it failed to parse, `claude mcp list` -- and none of those are
// misbehaviour worth blocking. Redacting on the way out is what makes
// them harmless.
//
// Renewal means there is more than one secret to hide: a chunk buffered
// before a renewal can still carry the previous bearer, so every
// credential the session has ever held stays in the set.

// redactedMarker replaces a credential in outgoing bytes. It is visible
// on purpose: a reader should be able to tell that something was removed,
// rather than seeing a transcript that silently does not match what the
// app printed.
var redactedMarker = []byte("[redacted: memql session credential]")

type redactor struct {
	mu      sync.RWMutex
	secrets [][]byte
	longest int
}

func newRedactor(secrets ...string) *redactor {
	r := &redactor{}
	for _, s := range secrets {
		r.add(s)
	}
	return r
}

// add registers another secret. Short values are ignored: a redactor that
// matched a two-character string would scribble over ordinary output, and
// a bearer is never short.
func (r *redactor) add(secret string) {
	if r == nil || len(secret) < 8 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.secrets {
		if string(existing) == secret {
			return
		}
	}
	r.secrets = append(r.secrets, []byte(secret))
	if len(secret) > r.longest {
		r.longest = len(secret)
	}
}

// holdBack is how many trailing bytes a partial flush must retain so a
// secret cannot be split across two chunks and survive in halves.
func (r *redactor) holdBack() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.longest == 0 {
		return 0
	}
	return r.longest - 1
}

// apply returns data with every registered secret replaced.
func (r *redactor) apply(data []byte) []byte {
	if r == nil || len(data) == 0 {
		return data
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := data
	for _, secret := range r.secrets {
		if bytes.Contains(out, secret) {
			out = bytes.ReplaceAll(out, secret, redactedMarker)
		}
	}
	return out
}

// apply2 is apply for a string -- the End's error text, which can quote a
// config file or a request the app made.
func (r *redactor) apply2(s string) string {
	if r == nil || s == "" {
		return s
	}
	return string(r.apply([]byte(s)))
}
