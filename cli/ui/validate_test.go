package ui

import "testing"

func TestValidateDomain(t *testing.T) {
	ok := []struct{ in, want string }{
		{"staging.copresent.ai", "staging.copresent.ai"},
		{"  Local.Znas.IO ", "local.znas.io"}, // trimmed + lower-cased
		{"a.b", "a.b"},
	}
	for _, tc := range ok {
		got, err := ValidateDomain(tc.in)
		if err != nil {
			t.Errorf("ValidateDomain(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateDomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",                  // required
		"localhost",         // not fully-qualified (no dot)
		"bad domain.com",    // space
		"under_score.com",   // underscore not allowed
		"-leading.dash.com", // leading dash on a label
	}
	for _, in := range bad {
		if _, err := ValidateDomain(in); err == nil {
			t.Errorf("ValidateDomain(%q) = nil error, want rejection", in)
		}
	}
}

func TestDomainToName(t *testing.T) {
	cases := map[string]string{
		"staging.copresent.ai": "staging-copresent-ai",
		"local.znas.io":        "local-znas-io",
		"A.B.C":                "a-b-c",
	}
	for in, want := range cases {
		if got := DomainToName(in); got != want {
			t.Errorf("DomainToName(%q) = %q, want %q", in, got, want)
		}
	}
	// Result must satisfy the cluster-name rule.
	if _, err := ValidateName(DomainToName("staging.copresent.ai")); err != nil {
		t.Errorf("DomainToName output failed ValidateName: %v", err)
	}
}
