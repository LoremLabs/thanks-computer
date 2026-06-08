// Package clientcmd is the CLIENT-side analog of chassis/clicmd: a registry for
// overlay-supplied `txco` subcommands that run in the CLI process (signing a
// request to a tenant-scoped admin endpoint, opening a browser) rather than
// being forwarded to the server's /v1/cli. An overlay self-registers handlers
// from its init(); the product binary activates them with a blank import. Open
// core registers none, so on a self-hosted chassis these verbs are unknown and
// the CLI falls through to its normal unknown-subcommand handling.
//
// The seam is deliberately generic — no billing vocabulary. A handler receives
// an Env carrying everything it needs from the CLI (a signed tenant client, a
// browser opener, output writers) so it never reaches into the cli package's
// unexported signing internals.
package clientcmd

import (
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// Env is the CLI context handed to a client command. The cli package builds it
// from the global connection flags (--profile/--addr/--tenant/...) before
// invoking the handler.
type Env struct {
	Stdout, Stderr io.Writer
	// TenantClient returns a signed admin client scoped to the resolved tenant.
	// It errors when no chassis target is configured (not logged in / no --addr).
	TenantClient func() (*client.Client, error)
	// OpenURL opens a URL in the user's browser (falls back to printing).
	OpenURL func(url string) error
}

// Handler runs one client command. args is everything after the verb with the
// global connection flags already stripped (the command's own flags +
// positionals). The return value is the process exit code.
type Handler func(env Env, args []string) int

// Registration happens during module init() (single-threaded), so plain maps
// without a mutex match the clicmd / bgservice / serverext seams.
var (
	registry      = map[string]Handler{}
	adminRegistry = map[string]Handler{}
)

// Register adds a top-level verb handler (`txco <name> ...`). Called from a
// backend package's init().
func Register(name string, h Handler) { registry[name] = h }

// RegisterAdmin adds a handler under the `admin` group (`txco admin <sub> ...`),
// so an overlay can extend `admin` without open core knowing the subcommand.
func RegisterAdmin(sub string, h Handler) { adminRegistry[sub] = h }

// Lookup returns the top-level handler for name, if registered.
func Lookup(name string) (Handler, bool) { h, ok := registry[name]; return h, ok }

// LookupAdmin returns the `admin <sub>` handler for sub, if registered.
func LookupAdmin(sub string) (Handler, bool) { h, ok := adminRegistry[sub]; return h, ok }
