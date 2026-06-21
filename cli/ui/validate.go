package ui

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Field-length ceilings. Kept in one place so form hints and
// validators stay in sync. Chosen to match real-world constraints:
//
//   - MaxNameLen 50 is generous for a user-chosen label; most
//     real cluster names end up under 20.
//   - MaxHostLen 253 is the RFC 1035 maximum for a fully-qualified
//     DNS name.
//   - MaxPortLen 5 covers the full uint16 port range ("65535").
//   - MaxURLLen 500 covers OIDC issuer URLs including path and
//     trailing slash; real-world issuers are much shorter.
//   - MaxClientIdLen 200 tolerates the longest OAuth client_id we've
//     seen in the wild.
const (
	MaxNameLen     = 50
	MaxHostLen     = 253
	MaxPortLen     = 5
	MaxURLLen      = 500
	MaxClientIdLen = 200
)

// dnsLabelRE matches a lower-case DNS-style label: starts and ends
// with alphanumeric, may contain dashes in between, no leading or
// trailing dashes. Used for cluster names -- they end up as concept
// ids in event topics, and that layer wants the stricter shape.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// hostRE accepts either a simple hostname (letters, digits, dots,
// dashes) or a plain-ish hostname:port combo. We keep it loose
// enough to allow "localhost", raw IPv4 ("127.0.0.1"), and FQDNs.
// IPv6 literals are NOT supported by this regex -- if we need them,
// callers should normalize to [::1]:port form and relax this.
var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]*[A-Za-z0-9])?$`)

// NormalizeName lowercases and trims the input, matching the
// server-side semantics (ids are case-insensitive in effect because
// we always lowercase before use).
func NormalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateName checks a cluster name against the shared rules:
// required, DNS-label style (lower-case alphanumeric + inner
// dashes), <= MaxNameLen. Returns the normalized value on success
// for callers that want to persist the clean form.
func ValidateName(s string) (string, error) {
	name := NormalizeName(s)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(name) > MaxNameLen {
		return "", fmt.Errorf("name is too long (max %d chars)", MaxNameLen)
	}
	if !dnsLabelRE.MatchString(name) {
		return "", fmt.Errorf("name must be alphanumeric; dashes allowed in the middle only")
	}
	return name, nil
}

// IsNameChar reports whether the given rune is allowed in the raw
// input stream for a name field. Used at keystroke time to silently
// drop invalid input instead of accepting it and erroring on save.
// Accepts both upper- and lower-case letters at input time (the
// validator lowercases later) so the user doesn't have to hunt for
// shift-lock.
func IsNameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-':
		return true
	default:
		return false
	}
}

// ValidateDomain checks a cluster domain (e.g. "staging.copresent.ai").
// Required, fully-qualified (must contain a dot), hostname-shaped, and
// within MaxHostLen. Returns the normalized (trimmed, lower-cased)
// domain. The Add/Edit form composes bff.<domain> / identity.<domain>
// from it, so the shape rules here mirror ValidateHost.
func ValidateDomain(s string) (string, error) {
	d := NormalizeName(s)
	if d == "" {
		return "", fmt.Errorf("domain is required")
	}
	if len(d) > MaxHostLen {
		return "", fmt.Errorf("domain is too long (max %d chars)", MaxHostLen)
	}
	if !strings.Contains(d, ".") {
		return "", fmt.Errorf("domain must be fully-qualified (e.g. staging.copresent.ai)")
	}
	if !hostRE.MatchString(d) {
		return "", fmt.Errorf("domain must be a hostname like staging.copresent.ai")
	}
	return d, nil
}

// DomainToName derives the clusters.yaml key from a domain: lower-case,
// dots become dashes, clipped to MaxNameLen, with surrounding dashes
// trimmed so the result satisfies the DNS-label name rule
// (dnsLabelRE). "staging.copresent.ai" -> "staging-copresent-ai".
func DomainToName(domain string) string {
	s := strings.ReplaceAll(NormalizeName(domain), ".", "-")
	if len(s) > MaxNameLen {
		s = s[:MaxNameLen]
	}
	return strings.Trim(s, "-")
}

// ValidateHost checks that the host looks like a reachable network
// address. Empty is NOT allowed -- a cluster without a host is a
// save-time error, which the form surfaces inline.
func ValidateHost(s string) error {
	host := strings.TrimSpace(s)
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if len(host) > MaxHostLen {
		return fmt.Errorf("host is too long (max %d chars)", MaxHostLen)
	}
	if !hostRE.MatchString(host) {
		return fmt.Errorf("host must be a hostname, IPv4, or 'localhost'")
	}
	return nil
}

// IsHostChar filters a single keystroke against the host charset.
func IsHostChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-' || r == '.':
		return true
	default:
		return false
	}
}

// ValidatePort checks that the port is a numeric string in the
// legal uint16 range [1, 65535]. Empty is ALLOWED by this
// validator -- callers decide whether the port is required for
// their particular form (cluster form treats it as required via
// the host+port join; the dispatcher still needs it).
func ValidatePort(s string) error {
	port := strings.TrimSpace(s)
	if port == "" {
		return nil
	}
	if len(port) > MaxPortLen {
		return fmt.Errorf("port must be 1..65535")
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port must be a number")
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port must be 1..65535")
	}
	return nil
}

// IsPortChar filters a single keystroke against the port charset
// (digits only).
func IsPortChar(r rune) bool {
	return r >= '0' && r <= '9'
}

// ValidateOptionalURL checks that, if present, the string parses as
// an absolute http(s) URL. Empty is allowed (fields like Issuer and
// Client ID stay optional in no-auth mode).
func ValidateOptionalURL(s string) error {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return nil
	}
	if len(raw) > MaxURLLen {
		return fmt.Errorf("URL is too long (max %d chars)", MaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("URL must start with http:// or https://")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must start with http:// or https://")
	}
	return nil
}

// ValidateOptionalClientId is a length + shape guard for OAuth
// client_id. Empty is allowed. Full Unicode is accepted (client_ids
// sometimes contain slashes, dots, tildes, even UUIDs). The only
// rule is "no whitespace, no control chars, reasonable length".
func ValidateOptionalClientId(s string) error {
	id := strings.TrimSpace(s)
	if id == "" {
		return nil
	}
	if len(id) > MaxClientIdLen {
		return fmt.Errorf("client ID is too long (max %d chars)", MaxClientIdLen)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("client ID contains control characters")
		}
	}
	return nil
}
