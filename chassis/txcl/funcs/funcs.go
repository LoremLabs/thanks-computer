// Package funcs is the curated registry of inline txcl functions —
// the `&name(args...)` runtime calls that complement `txco://` ops.
// Functions are side-effect-free (no Unit access, no I/O, no bus
// dispatch, no suspend); they exist to give rule authors inline
// computation for things like base64 decoding, UUID generation, or
// path access without paying the bus-and-trace overhead of a full
// op invocation.
//
// New entries are added with discipline — see
// internal docs/todo-txcl-expressions.md §4 ("Discipline about what goes
// in"). PR 3 ships just two pilots: &uuid() and &now().
package funcs

import "fmt"

// Func is the signature every registered function shares. Args
// arrive already resolved (the runtime evaluator walked any nested
// FunctionCalls / PathRefs / Literals before calling here), so
// implementations only see concrete Go values.
//
// Implementations MUST be:
//   - synchronous and quick (no I/O, no Unit access);
//   - side-effect-free with respect to chassis state (the
//     nondeterminism in &uuid / &now is OK — they don't mutate
//     anything observable);
//   - safe for concurrent invocation (a single Func may be called
//     from many rules / goroutines at once).
//
// On error, return (nil, err). The runtime propagates the error up
// to the rule as a strict halt (see internal docs/todo-txcl-expressions.md
// §5). The `&try_*` safe variants (added in PR 4) swallow errors
// from the corresponding strict form and substitute nil instead.
type Func func(args []any) (any, error)

// registry holds the in-process function lookup table. Initialized
// in init() so adding a function is a single registry entry plus
// the implementation. Lookup is O(1).
var registry = map[string]Func{}

// register installs a function in the registry under `name`. Panics
// on duplicate name — registration is init-time and a collision is
// a programming bug, not a runtime concern.
func register(name string, fn Func) {
	if _, dup := registry[name]; dup {
		panic("funcs: duplicate registration for &" + name)
	}
	registry[name] = fn
}

// Call dispatches `name` through the registry with the already-
// resolved args. Returns (nil, error) on unknown name; otherwise
// the function's return value.
//
// Single entry point used by runtime.Resolve when it walks a
// FunctionCall node. No other caller in the codebase should
// invoke registered functions directly.
func Call(name string, args []any) (any, error) {
	fn, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown function &%s", name)
	}
	return fn(args)
}

// Has reports whether a function is registered. Useful for parser-
// time / validate-time lookups that want to flag unknown functions
// before the rule fires (deferred — PR 3 only does runtime
// dispatch; validate-time integration is a follow-up).
func Has(name string) bool {
	_, ok := registry[name]
	return ok
}
