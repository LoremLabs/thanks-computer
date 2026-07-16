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

	// OpenURL, when set, asks the forwarding CLI to open this URL in the user's
	// browser (best-effort, only on an interactive terminal) after printing
	// Stdout — for commands whose result is a hosted page (a Stripe Checkout /
	// billing-portal URL, an OAuth page, …). ALWAYS also include the URL in
	// Stdout so headless/piped callers still get it. Empty = no-op.
	OpenURL string

	// AwaitCallback tells the forwarding CLI to keep running after OpenURL and
	// BLOCK on its local loopback server (started when it passed a callback via
	// Callback headers — see clicmd.Callback) until the hosted page redirects
	// back, or a timeout. Set this only when the command wired that callback URL
	// into the hosted flow's return target. No callback provided ⇒ leave false.
	AwaitCallback bool
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

// --- tenant-scoped registry ------------------------------------------------
//
// Tenant commands run under a RESOLVED tenant (POST /v1/tenants/{tenant}/cli)
// instead of the super-admin /v1/cli. That endpoint enforces MEMBERSHIP (not
// super-admin) via resolveTenantMiddleware and stamps the tenant onto the
// handler's context, so a handler reads its tenant with
// auth.FromContext(ctx).TenantSlug. Same Handler/Result shape; a SEPARATE
// registry keeps tenant verbs (e.g. self-serve `credits buy`) off the
// super-admin endpoint and admin verbs off the tenant endpoint. The CLI's
// forwarder tries /v1/cli first, then falls back to the tenant endpoint.
var tenantRegistry = map[string]Handler{}

// RegisterTenant adds a tenant-scoped command handler, activated by a blank
// import like Register. Last registration wins. Open core registers none.
func RegisterTenant(name string, h Handler) { tenantRegistry[name] = h }

// LookupTenant returns the tenant-scoped handler for name, if registered.
func LookupTenant(name string) (Handler, bool) {
	h, ok := tenantRegistry[name]
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

// CallbackInfo is the forwarding CLI's local loopback callback: the URL its
// 127.0.0.1 server listens on, plus a random state nonce the CLI verifies on the
// redirect back. A command that returns a hosted page (Stripe Checkout, OAuth)
// can wire these into the page's return target so the result comes back to the
// CLI; it then sets Result.AwaitCallback so the CLI blocks for it.
type CallbackInfo struct {
	URL   string
	State string
}

type callbackCtxKey struct{}

// WithCallback carries the forwarding CLI's loopback callback into the handler's
// context (set by the exec endpoint from request headers). Absent ⇒ the caller
// isn't waiting for a callback (headless / old client), so fall back to a static
// return.
func WithCallback(ctx context.Context, cb CallbackInfo) context.Context {
	return context.WithValue(ctx, callbackCtxKey{}, cb)
}

// Callback returns the forwarding CLI's loopback callback, if any. url == ""
// means none was provided.
func Callback(ctx context.Context) (url, state string) {
	if cb, ok := ctx.Value(callbackCtxKey{}).(CallbackInfo); ok {
		return cb.URL, cb.State
	}
	return "", ""
}
