package blob

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
)

// Permissions attach to NAMES, never to bytes: the same sha can sit behind
// guest/menu.pdf (requires blob:guest:read) and internal/menu.pdf (requires
// blob:internal:read), so declassification is always minting a new name.
// The vocabulary is the chassis' Shiro-style 3-segment capability string,
// matched by the same wildcard matcher the admin plane uses, under one
// domain: blob:<audience>:<action>. Wildcards compose on the GRANT side
// (blob:*:read, blob:*:*); a REQUIREMENT on a name is always concrete.
//
// v1 subject rule: the permission set an op holds is its DECLARED context —
// `WITH audience = "guest"` implies blob:guest:read, `WITH grants = [...]`
// lists explicit strings (the platform pipeline declares blob:cas:read for
// by-sha reads). Declared context is platform-authored code; the threat it
// closes is context mixing (a guest-lane op structurally unable to read
// internal names), not a malicious author. Actor-capability union is a
// later deepening that needs no schema change.

// CASRead is the privileged capability for addressing bytes by sha256
// directly — the infrastructure path (projection transforms re-reading
// artifacts). Without it the named, policy-enforced interface is the only
// door, so names are never a fence with an open gate.
const CASRead = "blob:cas:read"

// Domain is the capability domain every blob requirement lives in.
const Domain = "blob"

var (
	audienceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	permSegRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// ValidRequirement checks a permission string a NAME may require: exactly
// blob:<x>:<y> with concrete (non-wildcard, non-empty) segments.
func ValidRequirement(p string) error {
	parts := strings.Split(p, ":")
	if len(parts) != 3 {
		return fmt.Errorf("permission %q must have exactly three ':'-separated segments (blob:<audience>:<action>)", p)
	}
	if parts[0] != Domain {
		return fmt.Errorf("permission %q must be in the %q domain", p, Domain)
	}
	for _, seg := range parts[1:] {
		if seg == "*" {
			return fmt.Errorf("permission %q: a name's requirement cannot contain a wildcard (wildcards belong in grants)", p)
		}
		if !permSegRe.MatchString(seg) {
			return fmt.Errorf("permission %q: segment %q must match [A-Za-z0-9_.-]{1,64}", p, seg)
		}
	}
	return nil
}

// ValidGrant checks a string an op may DECLARE as held: three non-empty
// segments, wildcards allowed ("*" alone and "admin:all" are accepted by the
// matcher too, but an op declaring them is a smell — keep grants concrete
// or blob-scoped).
func ValidGrant(p string) error {
	parts := strings.Split(p, ":")
	if len(parts) != 3 {
		return fmt.Errorf("grant %q must have exactly three ':'-separated segments", p)
	}
	for _, seg := range parts {
		if seg != "*" && !permSegRe.MatchString(seg) {
			return fmt.Errorf("grant %q: segment %q must be '*' or match [A-Za-z0-9_.-]{1,64}", p, seg)
		}
	}
	return nil
}

// SubjectGrants builds the permission set an op holds from its declared
// context: an optional `audience` token (⇒ blob:<audience>:read) unioned
// with explicit `grants`. Either may be empty; the result is sorted and
// deduplicated so two ops declaring the same context compare equal.
func SubjectGrants(audience string, grants []string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	add := func(g string) {
		if _, dup := seen[g]; dup {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	if audience != "" {
		if !audienceRe.MatchString(audience) {
			return nil, fmt.Errorf("audience %q must match [A-Za-z0-9_-]{1,64}", audience)
		}
		add(Domain + ":" + audience + ":read")
	}
	for _, g := range grants {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if err := ValidGrant(g); err != nil {
			return nil, err
		}
		add(g)
	}
	sortStrings(out)
	return out, nil
}

// Allowed reports whether grants cover EVERY requirement (AND semantics: a
// name requiring two permissions needs both; authors express alternatives
// with wildcard grants). An empty requirement set is open within the tenant.
func Allowed(grants, required []string) bool {
	if len(required) == 0 {
		return true
	}
	return policy.CoversAll(grants, required) == ""
}

// CanReadByHash reports whether grants include the privileged by-sha path.
func CanReadByHash(grants []string) bool {
	return policy.Covers(grants, CASRead)
}
