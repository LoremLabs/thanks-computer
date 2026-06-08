package cli

import (
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/clientcmd"
)

// runClientCmd runs an overlay-registered client command (see chassis/clientcmd).
// It strips the global connection flags (--profile/--addr/--tenant/...) out of
// args, builds the Env those flags imply, and hands the remaining args (the
// command's own flags + positionals) to the handler. This is how cloud-only
// verbs like `txco credits buy` reach the chassis without the cli package
// knowing anything about them.
func runClientCmd(h clientcmd.Handler, args []string, stdout, stderr io.Writer) int {
	globals, rest := splitForwardFlags(args)
	env := clientcmd.Env{
		Stdout: stdout,
		Stderr: stderr,
		TenantClient: func() (*client.Client, error) {
			addr := globals["addr"]
			if addr == "" {
				addr = globals["url"]
			}
			t := resolveTarget(globals["dir"], globals["target"], addr, globals["user"], globals["pass"], globals["profile"])
			if t.Addr == "" {
				return nil, fmt.Errorf("no chassis configured (pass --addr or run `txco login`)")
			}
			t.Tenant = resolveTenant(globals["tenant"], globals["profile"])
			return client.New(t), nil
		},
		OpenURL: openBrowser,
	}
	return h(env, rest)
}
