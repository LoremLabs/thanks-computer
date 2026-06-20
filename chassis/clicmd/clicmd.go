// Package clicmd is the server-side registry for CLI subcommands forwarded over
// the admin API — the "zero-install plugin" path. When `txco <name> ...` is not
// a built-in (and no local txco-<name> plugin handles it), the CLI POSTs the
// argv to /v1/cli and the server runs the registered handler for <name>,
// returning its rendered output + exit code.
//
// The seam is generic and billing-neutral: an overlay self-registers a command
// from its init() and the product binary activates it with a blank import — the
// same compile-time pattern as usage.Register / bgservice.Register. Open core
// registers no commands, so an unknown command 404s and the CLI gracefully falls
// back to its unknown-subcommand error.
package clicmd

import "context"

// Result is a forwarded command's rendered output plus a process-style exit
// code. The CLI prints Stdout/Stderr verbatim and exits with Exit.
type Result struct {
	Stdout string
	Stderr string
	Exit   int

	// Cursor + PollAfterMs make a forwarded command POLLABLE. When PollAfterMs
	// > 0, the forwarding CLI prints Stdout/Stderr, waits that many ms, then
	// re-invokes the SAME command passing Cursor back (read via Cursor(ctx)) —
	// looping until the user interrupts or a result with PollAfterMs == 0
	// arrives. This lets a command stream incrementally over repeated /v1/cli
	// requests with no streaming transport. Zero values keep the classic
	// single-shot behaviour, so existing commands are unaffected.
	Cursor      string
	PollAfterMs int
}

// Handler runs one forwarded command server-side. args is everything after the
// command name — for `txco credit grant add acme 5`, name is "credit" and args
// is ["grant","add","acme","5"]. The endpoint has already enforced super-admin.
// A returned error is an INTERNAL failure (rendered as HTTP 500); user-facing
// errors (bad args, not found) should be a Result with a non-zero Exit + Stderr,
// nil error — so they render like a normal CLI error, not a server fault.
type Handler func(ctx context.Context, args []string) (Result, error)

var registry = map[string]Handler{}

// Register adds a command handler. Called from a backend package's init();
// the product binary activates it with a blank import. Last registration wins.
func Register(name string, h Handler) {
	registry[name] = h
}

// Lookup returns the handler for name, if registered.
func Lookup(name string) (Handler, bool) {
	h, ok := registry[name]
	return h, ok
}

type cursorCtxKey struct{}

// WithCursor carries the poll cursor a forwarded command was invoked with into
// the handler's context (set by the /v1/cli endpoint). Empty on the first call.
func WithCursor(ctx context.Context, cursor string) context.Context {
	return context.WithValue(ctx, cursorCtxKey{}, cursor)
}

// Cursor returns the poll cursor for this invocation — empty on the first poll
// or for a non-pollable command. A handler echoes a new cursor via
// Result.Cursor to be passed back on the next poll.
func Cursor(ctx context.Context) string {
	c, _ := ctx.Value(cursorCtxKey{}).(string)
	return c
}
