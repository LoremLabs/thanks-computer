package calendar

import (
	"encoding/json"
	"fmt"
)

// Policy vocabulary: per calendar, five verbs, each one of four modes. The
// chassis has no opinion about what a calendar holds; it has one about
// which client mutations a stack hears of, and when.
//
//	deny     refused at the protocol layer (403), no round trip
//	local    protocol state only, no event
//	observe  commit, then tell the `_calendar` stack (fire-and-forget)
//	stack    ask the `_calendar` stack first; absent/false @calendar.res.ok ⇒ 403
//
// Resolution: the calendar's own policy, then the account's default policy,
// then the chassis default — `put`/`delete` observe, `proppatch` local
// (display name / colour changes), `mkcalendar`/`remove` deny (calendars
// are provisioned by ops unless a product opens that).
const (
	VerbPut        = "put"
	VerbDelete     = "delete"
	VerbMkcalendar = "mkcalendar"
	VerbRemove     = "remove"
	VerbProppatch  = "proppatch"

	ModeDeny    = "deny"
	ModeLocal   = "local"
	ModeObserve = "observe"
	ModeStack   = "stack"
)

// PolicyVerbs lists the verbs a policy object may name.
var PolicyVerbs = []string{VerbPut, VerbDelete, VerbMkcalendar, VerbRemove, VerbProppatch}

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

// PolicyMode resolves one verb for a calendar (nil for a top-level
// MKCALENDAR, which resolves from the account alone).
func PolicyMode(cal *Calendar, acct *Account, verb string) string {
	if cal != nil {
		if m, ok := lookupMode(cal.Policy, verb); ok {
			return m
		}
	}
	if acct != nil {
		if m, ok := lookupMode(acct.Policy, verb); ok {
			return m
		}
	}
	switch verb {
	case VerbMkcalendar, VerbRemove:
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
