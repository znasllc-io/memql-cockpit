package concepts

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// ---------------------------------------------------------------------------
// rowCardFields -- the (primary, subtitle) projection used by the
// row list when a concept carries @displayCard hints.
// ---------------------------------------------------------------------------

func TestRowCardFields_NoCard_FallsBackToLegacyLabel(t *testing.T) {
	row := map[string]any{
		"id":        "v1:agents:agent:abc",
		"createdAt": "2026-05-20T10:00:00Z",
		"payload": map[string]any{
			"name": "Sofia",
		},
	}
	primary, sub := rowCardFields(row, nil)
	if primary != "Sofia" {
		t.Errorf("primary = %q, want Sofia", primary)
	}
	if !strings.Contains(sub, "v1:agents:agent:abc") {
		t.Errorf("subtitle should contain the row id when no card; got %q", sub)
	}
}

func TestRowCardFields_WithCard_UsesPrimarySlot(t *testing.T) {
	row := map[string]any{
		"id":        "v1:agents:agent:abc",
		"createdAt": time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
		"payload": map[string]any{
			"name":        "Sofia",
			"role":        "specialist",
			"ownerUserId": "v1:identity:user:znas",
			"active":      true,
		},
	}
	card := &client.DisplayCard{
		Primary:   "name",
		Secondary: "role",
		Tertiary:  "ownerUserId",
		Status:    "active",
	}
	primary, sub := rowCardFields(row, card)
	if primary != "Sofia" {
		t.Errorf("primary = %q, want Sofia", primary)
	}
	if !strings.Contains(sub, "specialist") {
		t.Errorf("subtitle should include secondary slot value; got %q", sub)
	}
	if !strings.Contains(sub, "v1:identity:user:znas") {
		t.Errorf("subtitle should include tertiary slot value; got %q", sub)
	}
	if !strings.Contains(sub, "true") {
		t.Errorf("subtitle should include status slot value; got %q", sub)
	}
	if !strings.Contains(sub, "ago") {
		t.Errorf("subtitle should include relative time; got %q", sub)
	}
}

func TestRowCardFields_EmptyPrimaryFallsBackToRowId(t *testing.T) {
	row := map[string]any{
		"id":      "v1:agents:agent:abc",
		"payload": map[string]any{"role": "specialist"},
	}
	card := &client.DisplayCard{Primary: "name", Secondary: "role"}
	primary, _ := rowCardFields(row, card)
	if primary != "v1:agents:agent:abc" {
		t.Errorf("primary should fall back to row id when payload[primary] empty; got %q", primary)
	}
}

func TestRowCardFields_OmitsEmptySlots(t *testing.T) {
	// Card asks for secondary + status; row payload only has primary.
	// The subtitle should NOT pad with separators for missing slots.
	row := map[string]any{
		"id":      "v1:planner:plan:zzz",
		"payload": map[string]any{"goal": "Compute Q3 numbers"},
	}
	card := &client.DisplayCard{
		Primary:   "goal",
		Secondary: "kind",
		Status:    "status",
	}
	primary, sub := rowCardFields(row, card)
	if primary != "Compute Q3 numbers" {
		t.Errorf("primary = %q, want goal value", primary)
	}
	// No leading separator. The subtitle is empty (no slots filled,
	// no createdAt either).
	if sub != "" {
		t.Errorf("subtitle = %q, want empty when no slots resolve", sub)
	}
}

// ---------------------------------------------------------------------------
// payloadFieldString -- value coercion for the slot projections.
// ---------------------------------------------------------------------------

func TestPayloadFieldString_HandlesEveryDisplayableType(t *testing.T) {
	payload := map[string]any{
		"text":      "hello",
		"isActive":  true,
		"isOff":     false,
		"intish":    float64(42),
		"floatlike": 3.14,
		"realInt":   int(7),
		"int64ish":  int64(99),
		"empty":     nil,
	}
	cases := []struct {
		field, want string
	}{
		{"text", "hello"},
		{"isActive", "true"},
		{"isOff", "false"},
		{"intish", "42"},
		{"floatlike", "3.14"},
		{"realInt", "7"},
		{"int64ish", "99"},
		{"empty", ""},
		{"missing", ""},
	}
	for _, c := range cases {
		got := payloadFieldString(payload, c.field)
		if got != c.want {
			t.Errorf("payloadFieldString(%q) = %q, want %q", c.field, got, c.want)
		}
	}
}

func TestPayloadFieldString_NilPayloadOrEmptyField(t *testing.T) {
	if got := payloadFieldString(nil, "name"); got != "" {
		t.Errorf("nil payload should return empty; got %q", got)
	}
	if got := payloadFieldString(map[string]any{"name": "Sofia"}, ""); got != "" {
		t.Errorf("empty field should return empty; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// relativeTime -- the row-list subtitle's date format.
// ---------------------------------------------------------------------------

func TestRelativeTime_RecentBuckets(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		delta time.Duration
		want  string
	}{
		{30 * time.Second, "just now"},
		{3 * time.Minute, "3m ago"},
		{45 * time.Minute, "45m ago"},
		{2 * time.Hour, "2h ago"},
		{36 * time.Hour, "yesterday"},
		{72 * time.Hour, "3d ago"},
	}
	for _, c := range cases {
		ts := now.Add(-c.delta).Format(time.RFC3339)
		got := relativeTime(ts)
		if got != c.want {
			t.Errorf("relativeTime(now-%s) = %q, want %q", c.delta, got, c.want)
		}
	}
}

func TestRelativeTime_OlderRendersAsCalendarDate(t *testing.T) {
	now := time.Now().UTC()
	// Two months ago -- well past the "Nd ago" window.
	old := now.AddDate(0, -2, 0).Format(time.RFC3339)
	got := relativeTime(old)
	if got == "" {
		t.Fatal("expected non-empty for an old date")
	}
	// Sanity: it should look like "Jan 2" or "Jan 2, 2006" style.
	if !strings.Contains(got, " ") {
		t.Errorf("expected calendar-date format (e.g. 'Mar 14'); got %q", got)
	}
}

func TestRelativeTime_RejectsMalformedInput(t *testing.T) {
	if got := relativeTime(""); got != "" {
		t.Errorf("empty input should return empty; got %q", got)
	}
	if got := relativeTime("not-a-date"); got != "" {
		t.Errorf("malformed input should return empty; got %q", got)
	}
}

func TestRelativeTime_FutureClampToCalendar(t *testing.T) {
	// A future date shouldn't yield negative "ago" weirdness; we
	// fall through to the calendar format.
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	got := relativeTime(future)
	if strings.Contains(got, "ago") || got == "" {
		t.Errorf("future date should render as calendar format; got %q", got)
	}
}
