package authn

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A credential's scopes say which doors it opens. Each is a 3-segment
// capability, the same `domain:instance:action` shape the account plane's
// policy package matches:
//
//	imap:*:login              any mailbox this principal has, IMAP only
//	drive:dc_7Hq…:*           one drive collection, any verb
//	ipp:front-desk:print      print to one printer
//
// Scopes only ever NARROW: effective access is what the principal may do ∩
// what the credential's scopes cover. So a leaked printer password cannot
// read mail.
//
// The grammar is stricter than the account plane's on purpose:
//
//   - exactly three segments — no `admin:all`, bare `*`, or 2-segment alias;
//   - the DOMAIN is never `*`. A credential names the heads it opens, so a
//     head added later (IRC, MQTT) is closed to every existing password
//     until someone issues one for it.

// Scope is one parsed `domain:instance:action` capability.
type Scope struct {
	Domain, Instance, Action string
}

func (s Scope) String() string { return s.Domain + ":" + s.Instance + ":" + s.Action }

// MaxScopes bounds one credential's scope list.
const MaxScopes = 32

var (
	scopeWordRE     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	scopeInstanceRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]{0,127}$`)
)

// ParseScope validates one scope string.
func ParseScope(s string) (Scope, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return Scope{}, fmt.Errorf("scope %q: want domain:instance:action", s)
	}
	sc := Scope{Domain: parts[0], Instance: parts[1], Action: parts[2]}
	switch {
	case sc.Domain == "*":
		return Scope{}, fmt.Errorf("scope %q: the domain may not be *; name the head it opens (imap, drive, …)", s)
	case !scopeWordRE.MatchString(sc.Domain):
		return Scope{}, fmt.Errorf("scope %q: domain is lowercase letters, digits, _ and -", s)
	case sc.Instance != "*" && !scopeInstanceRE.MatchString(sc.Instance):
		return Scope{}, fmt.Errorf("scope %q: instance is * or letters, digits and . _ @ + - (1-128 chars)", s)
	case sc.Action != "*" && !scopeWordRE.MatchString(sc.Action):
		return Scope{}, fmt.Errorf("scope %q: action is * or lowercase letters, digits, _ and -", s)
	}
	return sc, nil
}

// Covers reports whether this granted scope covers want: segment by segment,
// a granted `*` matches anything, anything else must be equal. A `*` in want
// is matched only by a granted `*` — asking for "any" needs a grant of "any".
func (s Scope) Covers(want Scope) bool {
	return s.Domain == want.Domain &&
		(s.Instance == "*" || s.Instance == want.Instance) &&
		(s.Action == "*" || s.Action == want.Action)
}

// Scopes is a credential's scope list: validated, de-duplicated, sorted.
type Scopes []Scope

// ParseScopes validates a scope list. It must hold at least one scope (a
// credential that opens nothing is a mistake, not a policy) and at most
// MaxScopes.
func ParseScopes(in []string) (Scopes, error) {
	seen := make(map[Scope]bool, len(in))
	out := make(Scopes, 0, len(in))
	for _, raw := range in {
		sc, err := ParseScope(strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("scopes: at least one is required")
	}
	if len(out) > MaxScopes {
		return nil, fmt.Errorf("scopes: at most %d", MaxScopes)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// Cover reports whether any scope in the list covers want.
func (ss Scopes) Cover(want Scope) bool {
	for _, s := range ss {
		if s.Covers(want) {
			return true
		}
	}
	return false
}

// Allows is Cover for a caller holding the three segments.
func (ss Scopes) Allows(domain, instance, action string) bool {
	return ss.Cover(Scope{Domain: domain, Instance: instance, Action: action})
}

// Strings is the list in its stored / displayed form.
func (ss Scopes) Strings() []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.String()
	}
	return out
}

// encode is the column form: a JSON array of strings.
func (ss Scopes) encode() string {
	b, _ := json.Marshal(ss.Strings())
	return string(b)
}

// decodeScopes reads the column form back. A row this package wrote always
// parses; one that does not (hand-edited) yields an error rather than an
// empty list, so a corrupt row can never read as "no restrictions".
func decodeScopes(col string) (Scopes, error) {
	var raw []string
	if err := json.Unmarshal([]byte(col), &raw); err != nil {
		return nil, fmt.Errorf("stored scopes: %w", err)
	}
	return ParseScopes(raw)
}
