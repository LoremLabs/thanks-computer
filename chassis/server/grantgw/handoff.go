package grantgw

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/rungrant"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// The variables a command is handed with its run grant (chassis/grantwire
// owns the names: the launcher reads them too).
const (
	EnvRun    = grantwire.EnvRun
	EnvToken  = grantwire.EnvToken
	EnvSocket = grantwire.EnvSocket
	EnvBin    = grantwire.EnvBin
)

// ErrHandoff wraps every reason a run grant cannot be handed to a command;
// the message says which.
var ErrHandoff = errors.New("grant")

func handoffErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrHandoff, fmt.Sprintf(format, a...))
}

// ForExec hands run grant grantID to one command. It opens each named
// sandbox — every secret decided, traced and charged as a request of the
// grant, `via: exec` — and returns the variables the sandboxes set, with
// the values that must never appear in the command's output. When this
// node has the grant socket, the command also gets what it needs to open
// sandboxes itself at run time (`txco sandbox`): the run's name, a token
// signed here, the socket, the binary.
//
// tenant and stack are the dispatching rule's own, and workspaceID is the
// workspace the command is about to run in. The grant must be live, minted
// by that stack, and minted for that workspace — so a rule cannot hand a
// command another stack's grant, or carry a grant to a machine it was not
// meant for.
//
// A sandbox that is refused is an error naming the sandbox, never the
// reason: the trace has that. Nothing is handed over then, and the command
// must not start.
//
// This is the only place a token is made, and it goes into the command's
// environment and nowhere else: no op returns one, so none reaches an
// envelope or a trace.
func (g *Gateway) ForExec(ctx context.Context, tenant, stack, workspaceID, grantID string, sandboxes []string) (map[string]string, [][]byte, error) {
	g.mu.RLock()
	socket, bin := g.socket, g.bin
	g.mu.RUnlock()
	if len(sandboxes) == 0 && socket == "" {
		return nil, nil, handoffErr("no sandbox was named and this node has no grant socket for the command to open one itself " +
			"(add `grant` to --personalities): nothing would be handed over")
	}
	if g.signer == nil {
		return nil, nil, handoffErr("this node has no master key to sign a run grant with (--secret-master-key)")
	}
	tenantID, err := g.tenantID(ctx, tenant)
	if err != nil {
		return nil, nil, handoffErr("tenant %q not found", tenant)
	}
	grant, err := g.ids.GetRunGrant(ctx, tenantID, stack, grantID)
	switch {
	case errors.Is(err, authn.ErrNotFound):
		return nil, nil, handoffErr("no run grant %q", grantID)
	case errors.Is(err, authn.ErrNotOwner):
		return nil, nil, handoffErr("run grant %s was minted by another stack", grantID)
	case err != nil:
		return nil, nil, handoffErr("reading run grant %s: %v", grantID, err)
	case !grant.Live(g.now()):
		return nil, nil, handoffErr("run grant %s has expired, ended or been revoked", grantID)
	case grant.WorkspaceID == "":
		return nil, nil, handoffErr("run grant %s was minted for no workspace: mint it WITH workspace", grantID)
	case grant.WorkspaceID != workspaceID:
		return nil, nil, handoffErr("run grant %s was minted for workspace %q, not this one", grantID, grant.Workspace)
	}
	// Every sandbox must be one the grant names, and no two may set one
	// variable — checked before anything is asked for, so a refusal here
	// charges nothing.
	setBy := map[string]string{}
	seen := map[string]bool{}
	for _, name := range sandboxes {
		if seen[name] {
			continue
		}
		seen[name] = true
		env, ok := grant.Sandbox(name)
		if !ok {
			return nil, nil, handoffErr("run grant %s does not name sandbox %q (it names: %s)", grantID, name, names(grant.SandboxNames()))
		}
		for v := range env {
			if other, dup := setBy[v]; dup {
				return nil, nil, handoffErr("sandboxes %s and %s both set %s", other, name, v)
			}
			setBy[v] = name
		}
	}

	env := map[string]string{}
	var scrub [][]byte
	for _, name := range sandboxes {
		if !seen[name] {
			continue // opened already
		}
		seen[name] = false
		ans := g.OpenSandbox(ctx, Open{TenantID: tenantID, GrantID: grant.ID, Sandbox: name, Via: ViaExec})
		if !ans.Allowed {
			for _, v := range scrub {
				secrets.Zero(v)
			}
			if ans.Unavailable {
				return nil, nil, handoffErr("sandbox %q could not be opened: the chassis could not decide", name)
			}
			return nil, nil, handoffErr("sandbox %q was refused", name)
		}
		for v, value := range ans.Env {
			env[v] = string(value)
			scrub = append(scrub, value)
		}
	}

	if socket == "" {
		return env, scrub, nil
	}
	token, err := g.signer.Sign(rungrant.Claims{
		Grant: grant.ID, Tenant: grant.TenantID, Principal: grant.Principal.ID,
		Run: grant.Run, Generation: grant.Generation, Depth: grant.Depth,
		Trace: grant.TraceID, Expires: grant.ExpiresAt.Unix(),
	})
	if err != nil {
		for _, v := range scrub {
			secrets.Zero(v)
		}
		return nil, nil, handoffErr("signing run grant %s: %v", grantID, err)
	}
	env[EnvRun], env[EnvToken], env[EnvSocket] = grant.Run, token, socket
	scrub = append(scrub, []byte(token))
	if bin != "" {
		env[EnvBin] = bin
	}
	return env, scrub, nil
}

func names(in []string) string {
	if len(in) == 0 {
		return "none"
	}
	return strings.Join(in, ", ")
}
