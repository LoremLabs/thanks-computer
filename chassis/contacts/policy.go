package contacts

import (
	"encoding/json"
	"fmt"
)

// Policy vocabulary: per address book, five verbs, each one of four modes.
// The chassis has no opinion about what an address book holds; it has one
// about which client mutations a stack hears of, and when. Same vocabulary
// as the calendar personality, with `mkaddressbook` for `mkcalendar`.
//
//	deny     refused at the protocol layer (403), no round trip
//	local    protocol state only, no event
//	observe  commit, then tell the `_contacts` stack (fire-and-forget)
//	stack    ask the `_contacts` stack first; absent/false @contacts.res.ok ⇒ 403
//
// Resolution: the address book's own policy, then the account's default
// policy, then the chassis default — `put`/`delete` observe, `proppatch`
// local (display name / description changes), `mkaddressbook`/`remove` deny
// (address books are provisioned by ops unless a product opens that).
const (
	VerbPut           = "put"
	VerbDelete        = "delete"
	VerbMkaddressbook = "mkaddressbook"
	VerbRemove        = "remove"
	VerbProppatch     = "proppatch"

	ModeDeny    = "deny"
	ModeLocal   = "local"
	ModeObserve = "observe"
	ModeStack   = "stack"
)

// PolicyVerbs lists the verbs a policy object may name.
var PolicyVerbs = []string{VerbPut, VerbDelete, VerbMkaddressbook, VerbRemove, VerbProppatch}

// ValidMode reports whether s is a policy mode.
func ValidMode(s string) bool {
	switch s {
	case ModeDeny, ModeLocal, ModeObserve, ModeStack:
		return true
	}
	return false
}

// ValidatePolicy checks a policy object: every key a known verb, every
// value a mode.
func ValidatePolicy(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("policy must be an object of verb → deny|local|observe|stack")
	}
	known := map[string]bool{}
	for _, v := range PolicyVerbs {
		known[v] = true
	}
	for k, v := range m {
		if !known[k] {
			return fmt.Errorf("policy verb %q is not one of %v", k, PolicyVerbs)
		}
		if !ValidMode(v) {
			return fmt.Errorf("policy %s=%q is not deny|local|observe|stack", k, v)
		}
	}
	return nil
}

// PolicyMode resolves one verb for an address book (nil for a top-level
// MKCOL, which resolves from the account alone).
func PolicyMode(ab *Addressbook, acct *Account, verb string) string {
	if ab != nil {
		if m, ok := lookupMode(ab.Policy, verb); ok {
			return m
		}
	}
	if acct != nil {
		if m, ok := lookupMode(acct.Policy, verb); ok {
			return m
		}
	}
	switch verb {
	case VerbMkaddressbook, VerbRemove:
		return ModeDeny
	case VerbProppatch:
		return ModeLocal
	}
	return ModeObserve
}

func lookupMode(raw json.RawMessage, verb string) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	if s, ok := m[verb]; ok && ValidMode(s) {
		return s, true
	}
	return "", false
}
