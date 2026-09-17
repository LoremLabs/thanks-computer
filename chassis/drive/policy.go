package drive

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A collection's CLIENT policy: which verbs a WebDAV client may use on
// which subtree. It exists because a drive can have two writers with
// different standing — the person at the mount, and the stack that curates
// what the person dropped — and a folder the stack owns must not be
// rewritten by a client that thinks it knows better.
//
// THE POLICY BINDS THE HEAD, NEVER THE STORE. `txco://drive/*` talks to the
// store directly and is never checked: a stack always has full access to
// its own collection, which is what lets it keep filing and moving inside a
// subtree its clients may only read. A policy is therefore not a security
// boundary between principals (a drive account is bound to ONE collection,
// so there is only ever one client). It is a division of labour.
//
// Shape: path prefix → verb → mode. The longest matching prefix decides;
// an absent prefix, verb or mode allows. "" is the whole collection.
//
//	{"Knowledge": {"write": "deny"}}
//
// reads as: a client may list, read, rename, delete and make folders under
// Knowledge/, but may not write a file's CONTENT there — the stack puts the
// bytes in that tree, the person works in the drop zone. The vocabulary
// deliberately mirrors an IMAP mailbox's per-verb policy (chassis/imap),
// so one idea covers both heads.
//
// If a collection ever grows a second account, this gains a subject
// dimension and today's rows become "applies to everyone".
type Policy map[string]map[string]string

// The verbs a policy can name. They are the client's INTENT, not the HTTP
// method: one method can mean two things (a MOVE is a move_out of its
// source and a move_in of its destination), and one intent can arrive by
// two methods (PUT and COPY both put content somewhere).
const (
	VerbWrite   = "write"    // PUT a file's content (create or overwrite)
	VerbCreate  = "create"   // MKCOL a directory
	VerbDelete  = "delete"   // DELETE a resource
	VerbMoveIn  = "move_in"  // MOVE/COPY whose DESTINATION is in the subtree
	VerbMoveOut = "move_out" // MOVE whose SOURCE is in the subtree
)

// The modes a verb can carry. Absent means allow, so a policy only ever
// has to name what it refuses.
const (
	ModeAllow = "allow"
	ModeDeny  = "deny"
)

var policyVerbs = map[string]bool{
	VerbWrite: true, VerbCreate: true, VerbDelete: true, VerbMoveIn: true, VerbMoveOut: true,
}

var policyModes = map[string]bool{ModeAllow: true, ModeDeny: true}

// PolicyVerbs lists the known verbs, sorted — for error messages and docs.
func PolicyVerbs() []string {
	out := make([]string, 0, len(policyVerbs))
	for v := range policyVerbs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ParsePolicy reads a stored policy. An empty string is no policy at all
// (every verb allowed), which is what every collection has until a stack
// sets one.
func ParsePolicy(raw string) (Policy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return nil, nil
	}
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("drive: policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate refuses a policy the head could not honour: an unknown verb or
// mode is a typo that would otherwise fail OPEN, silently allowing what the
// author meant to refuse.
func (p Policy) Validate() error {
	for prefix, verbs := range p {
		if _, err := NormalizePath(prefix); err != nil {
			return fmt.Errorf("drive: policy path %q: %w", prefix, err)
		}
		for verb, mode := range verbs {
			if !policyVerbs[verb] {
				return fmt.Errorf("drive: policy %q: unknown verb %q (want one of %s)", prefix, verb, strings.Join(PolicyVerbs(), ", "))
			}
			if !policyModes[strings.ToLower(mode)] {
				return fmt.Errorf("drive: policy %q %s: mode must be %s or %s", prefix, verb, ModeAllow, ModeDeny)
			}
		}
	}
	return nil
}

// String serializes a policy for storage; the zero policy is "".
func (p Policy) String() string {
	if len(p) == 0 {
		return ""
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// Allows reports whether a client may apply verb at path. The LONGEST
// matching prefix decides, so a policy can refuse a tree and then readmit
// one folder inside it. Anything a policy does not name is allowed: the
// default is an ordinary drive.
func (p Policy) Allows(path, verb string) bool {
	if len(p) == 0 {
		return true
	}
	norm, err := NormalizePath(path)
	if err != nil {
		return true // the write itself will refuse a bad path, with a better error
	}
	best, found := "", false
	for prefix := range p {
		pn, perr := NormalizePath(prefix)
		if perr != nil {
			continue
		}
		if pn != norm && !IsInside(norm, pn) {
			continue
		}
		if !found || len(pn) > len(best) {
			best, found = pn, true
		}
	}
	if !found {
		return true
	}
	// The map is keyed by the prefix as written, so find it again.
	for prefix, verbs := range p {
		if pn, perr := NormalizePath(prefix); perr != nil || pn != best {
			continue
		}
		return !strings.EqualFold(verbs[verb], ModeDeny)
	}
	return true
}
