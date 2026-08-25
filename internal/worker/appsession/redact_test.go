package appsession

import (
	"strings"
	"testing"
)

func TestRedactor_ReplacesEverySecretItHolds(t *testing.T) {
	r := newRedactor(testBearer)
	const renewed = "eyJhbGciOiJSUzI1NiJ9.the-second-bearer.sig"
	r.add(renewed)

	in := "config says Authorization: Bearer " + testBearer + " and later " + renewed + " too"
	got := string(r.apply([]byte(in)))
	if strings.Contains(got, testBearer) || strings.Contains(got, renewed) {
		t.Fatalf("a credential survived redaction: %q", got)
	}
	// Renewal is why both are held: a chunk buffered before a renewal
	// can still carry the previous bearer.
	if strings.Count(got, string(redactedMarker)) != 2 {
		t.Errorf("want two visible redactions, got: %q", got)
	}
}

// TestRedactor_MarkerIsVisible. A reader should be able to tell that
// something was removed, rather than seeing a transcript that silently
// does not match what the app printed.
func TestRedactor_MarkerIsVisible(t *testing.T) {
	r := newRedactor(testBearer)
	got := string(r.apply([]byte(testBearer)))
	if got != string(redactedMarker) {
		t.Errorf("got %q, want the visible marker", got)
	}
}

// TestRedactor_IgnoresShortSecrets: a redactor that matched a
// two-character string would scribble over ordinary output.
func TestRedactor_IgnoresShortSecrets(t *testing.T) {
	r := newRedactor("ab")
	if got := string(r.apply([]byte("a table of abbreviations"))); got != "a table of abbreviations" {
		t.Errorf("a short secret was matched: %q", got)
	}
	if r.holdBack() != 0 {
		t.Errorf("holdBack = %d with no usable secret", r.holdBack())
	}
}

// TestRedactor_HoldBackCoversASplit is the reason streamReader retains a
// tail on an early flush: a credential split across two chunks would
// survive redaction in two halves, and the two halves are the credential.
func TestRedactor_HoldBackCoversASplit(t *testing.T) {
	r := newRedactor(testBearer)
	if got := r.holdBack(); got != len(testBearer)-1 {
		t.Errorf("holdBack = %d, want %d", got, len(testBearer)-1)
	}
}

func TestRedactor_NilIsSafe(t *testing.T) {
	var r *redactor
	if got := string(r.apply([]byte("anything"))); got != "anything" {
		t.Errorf("nil redactor altered data: %q", got)
	}
	if r.holdBack() != 0 {
		t.Error("nil redactor should hold nothing back")
	}
	r.add("something")
}
