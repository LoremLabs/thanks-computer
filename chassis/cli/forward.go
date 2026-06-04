package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// forwardToServer is the zero-install plugin path: an unknown subcommand (not a
// built-in, no local txco-<name> plugin) is silently forwarded to the connected
// chassis's /v1/cli as a signed request. If that server implements the command,
// its output + exit code are rendered here. We never probe or list — this is a
// single best-effort request.
//
// ok=false means "fall through to the unknown-subcommand error":
//   - no chassis target is configured (nothing to ask), or
//   - the server returned 404 (it doesn't implement this command — the common
//     case against open core, and the graceful "not the right thing" path).
//
// Any other outcome (the command ran, or the configured server returned an
// auth/server/network error) is surfaced with ok=true, because the user clearly
// intended to reach a server.
func forwardToServer(name string, args []string, stdout, stderr io.Writer) (status int, ok bool) {
	target := resolveTarget("", "", "", "", "", "")
	if target.Addr == "" {
		return 0, false // not logged in / no chassis configured
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := client.New(target).RunCommand(ctx, append([]string{name}, args...))
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
