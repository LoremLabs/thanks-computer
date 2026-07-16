package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// forwardGlobalFlags are the connection/auth flags the forwarder consumes
// CLIENT-side (to resolve the target + signing profile) and does NOT forward to
// the server — they mirror the flags the built-in commands accept. Everything
// else (positionals and a command's own flags) is forwarded verbatim.
var forwardGlobalFlags = map[string]bool{
	"profile": true,
	"addr":    true,
	"url":     true,
	"target":  true,
	"user":    true,
	"pass":    true,
	"dir":     true,
	"tenant":  true,
}

// splitForwardFlags pulls the reserved global flags out of args — in both
// `--flag value` and `--flag=value` forms — and returns them plus the remaining
// args (positionals and any command-specific flags, preserved verbatim).
func splitForwardFlags(args []string) (globals map[string]string, rest []string) {
	globals = map[string]string{}
	for i := 0; i < len(args); i++ {
		name, isLong := strings.CutPrefix(args[i], "--")
		if !isLong {
			rest = append(rest, args[i])
			continue
		}
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			key, val := name[:eq], name[eq+1:]
			if forwardGlobalFlags[key] {
				globals[key] = val
			} else {
				rest = append(rest, args[i])
			}
			continue
		}
		if forwardGlobalFlags[name] {
			if i+1 < len(args) { // consume the value token
				globals[name] = args[i+1]
				i++
			}
			continue
		}
		rest = append(rest, args[i])
	}
	return globals, rest
}

// forwardToServer is the zero-install plugin path: an unknown subcommand (not a
// built-in, no local txco-<name> plugin) is silently forwarded to the connected
// chassis's /v1/cli as a signed request. The connection/auth flags (--profile,
// --addr, ...) are parsed out and used to resolve the target + signer; the rest
// of the argv is forwarded. We never probe or list — one best-effort request.
//
// ok=false means "fall through to the unknown-subcommand error":
//   - no chassis target is configured (nothing to ask), or
//   - the server returned 404 (it doesn't implement this command — the graceful
//     "not the right thing" path, e.g. open core).
//
// Any other outcome (the command ran, or the configured server returned an
// auth/server/network error) is surfaced with ok=true.
// forwardRequestTimeout bounds each /v1/cli request. Generous (vs the old 15s)
// so a command that collects/long-polls server-side — e.g. a pollable tail —
// isn't cut off mid-window; the shared http client's own 30s timeout is the
// real ceiling.
const forwardRequestTimeout = 35 * time.Second

func forwardToServer(name string, args []string, stdout, stderr io.Writer) (status int, ok bool) {
	globals, rest := splitForwardFlags(args)
	addr := globals["addr"]
	if addr == "" {
		addr = globals["url"]
	}
	target := resolveTarget(globals["dir"], globals["target"], addr, globals["user"], globals["pass"], globals["profile"])
	if target.Addr == "" {
		return 0, false // not logged in / no chassis configured
	}
	// Follow the same tenant the chassis URL + signature do, so a self-serve
	// verb runs under the caller's tenant (mirrors runClientCmd).
	target.Tenant = resolveTenant(globals["tenant"], effectiveProfile(globals["target"], globals["profile"]))
	c := client.New(target)
	argv := append([]string{name}, rest...)

	// Endpoint order: try the super-admin /v1/cli FIRST (the unchanged legacy
	// path — operator verbs like `credit grant` live there). If the server
	// doesn't implement the command there (404) AND a tenant is resolvable,
	// retry the SAME first call against the tenant-scoped /v1/tenants/{t}/cli,
	// where self-serve overlay verbs (`credits`/`billing`) live. 404 on the
	// applicable endpoint(s) → fall through to the unknown-subcommand error.
	// Admin-first keeps every existing forwarded command's behaviour identical;
	// tenant exec is purely an additive fallback.
	// A command may ask us to open a hosted page (Result.OpenURL) — best-effort,
	// interactive terminals only, and suppressible with TXCO_NO_BROWSER. The URL
	// is always printed too (in Stdout), so headless/piped callers still get it.
	noOpen := os.Getenv("TXCO_NO_BROWSER") != ""
	tenantMode := false
	cursor := ""
	first := true
	// Poll loop: a single request for the common case; a POLLABLE command
	// (Result.PollAfterMs > 0) makes us print, wait, and re-run with the
	// returned cursor, until it stops or the user interrupts (Ctrl-C kills the
	// process, the server sees the dropped connection and cleans up).
	for {
		ctx, cancel := context.WithTimeout(context.Background(), forwardRequestTimeout)
		var res *client.CommandResult
		var err error
		if tenantMode {
			res, err = c.RunTenantCommandCursor(ctx, target.Tenant, argv, cursor)
		} else {
			res, err = c.RunCommandCursor(ctx, argv, cursor)
		}
		cancel()
		if err != nil {
			if first && errors.Is(err, client.ErrCommandUnsupported) {
				if !tenantMode && target.Tenant != "" {
					tenantMode = true // admin exec 404'd → try the tenant endpoint
					continue
				}
				return 0, false // unsupported on the applicable endpoint(s) → graceful fall-through
			}
			fmt.Fprintf(stderr, "txco: %v\n", err)
			return 1, true
		}
		first = false
		if res.Stdout != "" {
			fmt.Fprint(stdout, res.Stdout)
		}
		if res.Stderr != "" {
			fmt.Fprint(stderr, res.Stderr)
		}
		if res.OpenURL != "" && !noOpen && banner.IsTTY(stdout) {
			_ = openBrowser(res.OpenURL) // best-effort; URL already printed as fallback
		}
		if res.PollAfterMs <= 0 {
			return res.Exit, true
		}
		cursor = res.Cursor
		time.Sleep(time.Duration(res.PollAfterMs) * time.Millisecond)
	}
}
