package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractTimeline(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		want     string
		wantErr  bool
		contains bool // when true, assert want is a substring of the result (raw-payload fallback)
	}{
		{
			name:    "timeline present",
			payload: `{"planId":"v1:planner:plan:abc","timeline":"step1 -> step2","complete":true,"stepCount":2}`,
			want:    "step1 -> step2",
		},
		{
			name:    "timeline with newlines preserved",
			payload: `{"timeline":"line1\nline2\nline3"}`,
			want:    "line1\nline2\nline3",
		},
		{
			name:     "empty timeline falls back to raw payload",
			payload:  `{"planId":"v1:planner:plan:abc","timeline":"","complete":false,"stepCount":0}`,
			want:     `"planId":"v1:planner:plan:abc"`,
			contains: true,
		},
		{
			name:     "missing timeline field falls back to raw payload",
			payload:  `{"planId":"v1:planner:plan:abc","stepCount":1}`,
			want:     `"stepCount":1`,
			contains: true,
		},
		{
			name:     "whitespace-only timeline falls back to raw payload",
			payload:  `{"timeline":"   ","stepCount":0}`,
			want:     `"timeline":"   "`,
			contains: true,
		},
		{
			name:    "empty payload is an error",
			payload: "",
			wantErr: true,
		},
		{
			name:    "invalid json is an error",
			payload: "{not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractTimeline([]byte(tt.payload))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("extractTimeline(%q) = %q, want error", tt.payload, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractTimeline(%q) unexpected error: %v", tt.payload, err)
			}
			if tt.contains {
				if !strings.Contains(got, tt.want) {
					t.Fatalf("extractTimeline(%q) = %q, want substring %q", tt.payload, got, tt.want)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("extractTimeline(%q) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

// TestExtractTimelineFromFlattenedRow exercises the exact shape the
// SDK Result.Single() produces for the synthetic v1:harness:trace row
// (bundle payload flattened onto the top level), confirming the
// json.Marshal(row) -> extractTimeline round-trip works.
func TestExtractTimelineFromFlattenedRow(t *testing.T) {
	row := map[string]any{
		"id":        "v1:harness:trace:abc",
		"concept":   "v1:harness:trace",
		"planId":    "v1:planner:plan:abc",
		"timeline":  "queued -> running -> succeeded",
		"complete":  true,
		"stepCount": float64(3), // JSON numbers decode to float64
	}
	payload, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	got, err := extractTimeline(payload)
	if err != nil {
		t.Fatalf("extractTimeline: %v", err)
	}
	if got != "queued -> running -> succeeded" {
		t.Fatalf("extractTimeline = %q, want the timeline string", got)
	}
}

func TestHandleCommandUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "no args prints usage and fails", args: nil, want: 1},
		{name: "help subcommand succeeds", args: []string{"help"}, want: 0},
		{name: "--help succeeds", args: []string{"--help"}, want: 0},
		{name: "unknown subcommand fails", args: []string{"bogus"}, want: 1},
		{name: "trace without planId fails", args: []string{"trace"}, want: 1},
		{name: "trace help succeeds", args: []string{"trace", "--help"}, want: 0},
		{name: "trace unknown flag fails", args: []string{"trace", "--bogus"}, want: 1},
		{name: "trace --cluster missing value fails", args: []string{"trace", "p1", "--cluster"}, want: 1},
		{name: "trace too many positionals fails", args: []string{"trace", "p1", "p2"}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HandleCommand(tt.args); got != tt.want {
				t.Fatalf("HandleCommand(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
