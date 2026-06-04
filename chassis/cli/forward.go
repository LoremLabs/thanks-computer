package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := client.New(target).RunCommand(ctx, append([]string{name}, rest...))
	if err != nil {
		if errors.Is(err, client.ErrCommandUnsupported) {
			return 0, false // server doesn't implement it → graceful fall-through
		}
		fmt.Fprintf(stderr, "txco: %v\n", err)
		return 1, true
	}

	if res.Stdout != "" {
		fmt.Fprint(stdout, res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(stderr, res.Stderr)
	}
	return res.Exit, true
}
