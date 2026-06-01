package tenants

import (
	"net"
	"regexp"
	"strings"
)

// CanonicalizeHost parses a Host-header-or-similar string into the
// canonical form used as the lookup key in `tenant_hostnames`.
// Returns (canonical, ok). On any ambiguity (too many colons, empty
// result, leading/trailing punctuation that can't be cleaned up) it
// returns ok=false — the caller chooses whether that's a hard error
// (admin write) or a silent miss (resolver read).
//
// Both the admin write path and the resolver read path call this
// function so a request with Host: "EXAMPLE.com:8080" matches a stored
// row keyed on "example.com". Centralised here because the cases are
// fiddly (`[::1]:8080`, trailing dot, mixed case, `host:bad:port`) and
// duplicating the logic across read/write is exactly the kind of
// place lookups silently diverge.
//
// v1 does NOT IDNA/punycode. Non-ASCII input is rejected at the admin
// write boundary by `IsValidHostname`, and on the resolver path simply
// misses (falling through to boot/%/0).
func CanonicalizeHost(input string) (string, bool) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", false
	}
	// Try host:port form first. SplitHostPort returns just the host on
	// success and correctly handles [::1]:8080 (returns "::1" without
	// the brackets). Three outcomes:
	//
	//   - nil err   → use the parsed host.
	//   - "missing port" → no port present; keep the input. If it was
	//     a bracketed IPv6 with no port like "[::1]", strip the
	//     surrounding brackets here.
	//   - other err → malformed (e.g. "too many colons" for bare IPv6
	//     "::1" or junk like "host:bad:port"). Reject.
	//
	// Rejecting bare IPv6 is intentional — the admin should bracket
	// it, matching the URL convention.
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	} else if strings.Contains(err.Error(), "missing port") {
		if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
			s = s[1 : len(s)-1]
		}
	} else {
		return "", false
	}
	s = strings.ToLower(s)
	// One trailing dot is allowed in DNS (the absolute form); strip
	// it so foo.local and foo.local. compare equal.
	if strings.HasSuffix(s, ".") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return "", false
	}
	return s, true
}

// hostnameRE is the strict admin-side validator. RFC 952 / 1123-ish:
// each label is 1–63 chars, alphanumeric with optional internal
// hyphens, labels joined by dots. ASCII-only by design (no IDNA in
// v1). Rejects IP literals (`1.2.3.4`, `::1`) — those go through
// `--ingress-config` if you really need them.
var hostnameRE = regexp.MustCompile(
	`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// IsValidHostname is the strict admin-write predicate. It's called
// AFTER CanonicalizeHost, so input is already lowercased / port-
// stripped / trailing-dot-stripped. Rejects IP literals and other
// shapes that pass canonicalisation but shouldn't be stored.
func IsValidHostname(canonical string) bool {
	if canonical == "" || len(canonical) > 253 {
		return false
	}
	// Reject anything that parses as an IP literal — IPs flow through
	// the YAML resolver path, not the DB.
	if ip := net.ParseIP(canonical); ip != nil {
		return false
	}
	return hostnameRE.MatchString(canonical)
}

// devLocalSuffixes are hostname suffixes whose ownership the operator
// of a local development chassis self-evidently has (the hostnames all
// resolve to 127.0.0.1, either by RFC convention or by a chassis-side
// wildcard DNS record). Auto-verifying these on claim removes the
// DNS-TXT round-trip from the every-new-feature smoke path.
var devLocalSuffixes = []string{
	".localhost",
	".local",
	".local.thanks.computer",
}

// IsDevLocalHostname reports whether canonical is a hostname that a
// dev-mode chassis can auto-verify on claim, skipping the DNS-TXT
// proof-of-ownership round-trip. The matcher is conservative:
//
//   - `localhost` — RFC 6761, must resolve to loopback
//   - `*.localhost` — RFC 6761 §6.3
//   - `*.local` — RFC 6762 mDNS (Bonjour); a developer-machine convention
//   - `*.local.thanks.computer` — wildcard A record we publish to 127.0.0.1
//     so any developer can use `<feature>.local.thanks.computer` without
//     touching /etc/hosts
//
// IP literals (127.0.0.1, ::1) are rejected upstream by IsValidHostname,
// so they never reach this predicate. Public TLDs (.com, .org, ...) and
// any operator-owned domain are never matched.
//
// Caller is responsible for passing an already-canonicalized host (call
// CanonicalizeHost first). The match is case-sensitive on the
// canonical form.
func IsDevLocalHostname(canonical string) bool {
	if canonical == "" {
		return false
	}
	if canonical == "localhost" {
		return true
	}
	for _, suf := range devLocalSuffixes {
		if strings.HasSuffix(canonical, suf) {
			return true
		}
	}
	return false
}
