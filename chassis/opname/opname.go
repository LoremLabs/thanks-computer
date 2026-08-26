// Package opname is the single source of truth for what a stack name and
// an operation (rule) name may contain. Names are persisted contract
// identifiers — they become DB keys, `stage = stack/scope` path
// components, trace directory names, and continuation `opDir` segments —
// so they are validated (rejected) at the write boundary rather than
// silently transformed. The filesystem sanitizers in the *store
// backends remain as belt-and-suspenders; this is the loud upstream gate.
//
// Pure: no FS/DB/deps, so both the admin API and the opstack loader can
// import it without a cycle. Tenant-reservation policy (e.g. boot/* is
// _sys-owned) is NOT here — that stays with the admin layer that knows
// the acting tenant.
package opname

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrName is the sentinel for any invalid name (wrapped with detail).
var ErrName = errors.New("invalid name")

// LikeEscapeChar is the ESCAPE character callers should declare when
// matching a name with SQL LIKE. It is not a legal name character, so it can
// never collide with real data.
const LikeEscapeChar = `\`

// EscapeLike escapes every SQL LIKE metacharacter in s so the result matches
// LITERALLY under `LIKE ? ESCAPE '\'`. Use it on any name (or user-supplied
// prefix) that is spliced into a LIKE pattern — see the '_' note on seg.
//
// Callers that deliberately want '%' to stay a wildcard (the ops resolver's
// `boot/%` ingress fallthrough is the only one) must escape '_' alone instead;
// see processor.escapeLikeLiterals.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, LikeEscapeChar, LikeEscapeChar+LikeEscapeChar)
	s = strings.ReplaceAll(s, "%", LikeEscapeChar+"%")
	return strings.ReplaceAll(s, "_", LikeEscapeChar+"_")
}

const (
	maxOpNameLen    = 64
	maxStackNameLen = 128
)

// seg is the per-segment charset shared by operation names and each
// "/"-separated stack-name segment. It deliberately excludes '.', '/',
// '%', whitespace and everything else, so '.'/'..' traversal segments and
// empty/whitespace names are impossible.
//
// '_' IS permitted, and is load-bearing: the channel convention (`_mail`,
// `_cron`, `_llm`, `_room`, `_inspect`, `_scheduled`) and the reserved
// `_sys/*` namespace are built on it. But '_' is ALSO a SQL LIKE
// single-character wildcard, so any query matching a stack name with LIKE
// must escape it — otherwise a lookup for `_mail` also returns `email`.
// The ops resolver does this via processor.escapeLikeLiterals; a new LIKE
// call site over these names must do the same. Do not "fix" this by
// banning '_' here: that would invalidate every channel stack in existence.
var seg = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Valid reports whether name is a usable operation/rule name.
func Valid(name string) error {
	if name == "" {
		return fmt.Errorf("%w: operation name is empty", ErrName)
	}
	if len(name) > maxOpNameLen {
		return fmt.Errorf("%w: operation name %q exceeds %d chars", ErrName, name, maxOpNameLen)
	}
	if !seg.MatchString(name) {
		return fmt.Errorf("%w: operation name %q must match [A-Za-z0-9_-]+", ErrName, name)
	}
	return nil
}

// ValidStack reports whether name is a usable stack name. A stack name is
// one or more "/"-joined segments (e.g. "hello-world", "_sys/boot");
// every segment obeys the same charset as an operation name, so there is
// no empty/'.'/'..'/'%'/leading-or-trailing-slash/double-slash form.
func ValidStack(name string) error {
	if name == "" {
		return fmt.Errorf("%w: stack name is empty", ErrName)
	}
	if len(name) > maxStackNameLen {
		return fmt.Errorf("%w: stack name %q exceeds %d chars", ErrName, name, maxStackNameLen)
	}
	for _, s := range strings.Split(name, "/") {
		if !seg.MatchString(s) {
			return fmt.Errorf("%w: stack name %q has an invalid segment %q (each segment must match [A-Za-z0-9_-]+)", ErrName, name, s)
		}
	}
	return nil
}
