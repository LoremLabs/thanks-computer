package server

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// txco://run/abort — end a run in flight on this chassis, from a rule.
//
// What lets a stack build its own stop, with its own bookkeeping first: a
// loop that must mark a run stopped before the run's dispatch is cut (so
// nothing reads it as abandoned and retries it) records that, then asks.
//
//	EXEC "txco://run/abort" WITH rid = ._run.dispatch_rid, reason = "stopped by the owner", into = "_abort"
//	EXEC "txco://run/abort" WITH stack = "worker", reason = "deploy"
//
//	_abort = {aborted: <n>, rid | stack}
//
// One of `rid` and `stack`; the run (or every live run that entered at, or
// is now in, the stack) must be the calling stack's tenant's — another
// tenant's run is not found. A hard stop, as `txco abort`: the run's ops in
// flight are cancelled, nothing later in it executes, its trace says
// `aborted` by "<tenant>/<stack>". A run may abort itself. On failure:
// `<into>.error.{code,message}` (txco_run_no_tenant, txco_run_invalid_arg,
// txco_run_not_live, txco_run_disabled).

type runAbortOut struct {
	Aborted int    `json:"aborted"`
	RID     string `json:"rid,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

func runAbort(ctx context.Context, live *processor.LiveRuns, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_abort")
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return identityErr(into, "run", "no_tenant", "no tenant in request scope"), nil
	}
	if live == nil {
		return identityErr(into, "run", "disabled", "no live-run registry on this node"), nil
	}
	rid := strings.TrimSpace(gjson.GetBytes(meta, "rid").String())
	stack := strings.TrimSpace(gjson.GetBytes(meta, "stack").String())
	switch {
	case rid == "" && stack == "":
		return identityErr(into, "run", "invalid_arg", "rid or stack is required"), nil
	case rid != "" && stack != "":
		return identityErr(into, "run", "invalid_arg", "rid or stack, not both"), nil
	}
	reason := strings.TrimSpace(gjson.GetBytes(meta, "reason").String())
	if rs := []rune(reason); len(rs) > 200 {
		reason = string(rs[:200])
	}
	by := tenant
	if s := processor.StackScope(ctx); s != "" {
		by += "/" + s
	}
	if stack != "" {
		n := live.AbortStack(tenant, stack, by, reason)
		return identityOK(into, runAbortOut{Aborted: n, Stack: stack}), nil
	}
	if !live.Abort(tenant, rid, by, reason) {
		return identityErr(into, "run", "not_live", "no run "+rid+" is in flight on this chassis"), nil
	}
	return identityOK(into, runAbortOut{Aborted: 1, RID: rid}), nil
}
