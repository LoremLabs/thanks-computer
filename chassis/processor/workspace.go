package processor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/usage"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// workspaceDefaultInto is where an exec's result lands when the rule sets
// no `WITH into`. `_`-prefixed so it never reaches a web body on its own.
const workspaceDefaultInto = "_workspace"

// ExecWorkspace runs a `workspace://<name>/<verb>` op against the configured
// workspace provider. The output is handed back to Exec's shared tail, so
// it flows the identical post-EXEC processing (EMIT overlay, per-scope
// merge, _txc.goto/halt) as every other transport.
//
// Error policy — exit codes are data. A command that exits 3 is a
// successful dispatch whose result says `exit: 3`. A transport or provider
// failure (timeout, capacity, the workspace would not wake) is ALSO
// returned as a merged payload — `<into>: {}` plus a top-level
// `workspace.error {code, message}` — so a rule can gate on it
// (`WHEN .workspace.error.code == "timeout"`). Only authoring errors
// return a Go error and drop the op: a malformed ref, a bad name, no
// provider configured, an untenanted request, or a malformed `secrets`
// block.
//
// Secrets: `WITH secrets.env.<NAME>.secret = "STORED"` (plus the optional
// `.format`) materializes a stored secret into the command's environment —
// the only place a workspace op accepts one. The processor splice has
// already put the cleartext in op.Secrets; here it is copied into
// ExecRequest.Env, and every materialized value is scrubbed from stdout,
// stderr and error text before anything is built into the payload (and so
// before the trace step sees it). See scrubSecrets for the caveat.
func (pu *Unit) ExecWorkspace(ctx context.Context, op operation.Operation) (event.Payload, error) {
	empty := event.Payload{Raw: `{}`, Type: event.JSON}

	name, verb, ok := workspace.ParseRef(op.Resonator.Exec)
	if !ok {
		return empty, fmt.Errorf("malformed workspace ref %q; want workspace://<name>/<verb> with verb in %v",
			op.Resonator.Exec, workspace.Verbs)
	}
	if pu.Workspaces == nil {
		return empty, errors.New("workspace:// op fired but no workspace provider configured (--workspace-provider)")
	}
	tenant := tenantScope(ctx)
	if tenant == "" {
		return empty, errors.New("workspace: no tenant in request scope")
	}
	// `WITH workspace = <expr>` replaces the ref's name entirely; the ref's
	// name is a static default. WITH values are resolved before dispatch,
	// so a path-valued expression arrives here as its value.
	if v := gjson.Get(op.Meta, "workspace"); v.Exists() {
		name = v.String()
	}
	if err := workspace.ValidateName(name); err != nil {
		return empty, err
	}
	// A malformed `secrets` block (a leaf with no `secret`, a bad format
	// template) is an authoring error like a bad name.
	refs, err := secrets.ParseRefs(op.Meta)
	if err != nil {
		return empty, err
	}

	into := normalizeEnvelopePath(gjson.Get(op.Meta, "into").String())
	if into == "" {
		into = workspaceDefaultInto
	}
	spec := workspace.Spec{Tenant: tenant, Stack: op.Stack, Name: name, Network: "public"}
	opID := fmt.Sprintf("%s/%d/%s", op.Stack, op.Scope, op.Name)
	rid, _ := ctx.Value(config.CtxKeyRid).(string)
	prov := pu.Workspaces.Provider().Name()
	scrub := func(s string) string { return string(scrubSecrets([]byte(s), op.Secrets)) }

	// Only the env channel exists for a workspace: a secret in a header or
	// body has no meaning here, and a silently ignored ref would be a
	// command running without the token it expected.
	for _, r := range refs {
		if !strings.HasPrefix(r.Path, "env.") || strings.TrimPrefix(r.Path, "env.") == "" {
			return workspaceFailure(into, prov, "", "bad_request",
				fmt.Sprintf("secrets.%s: workspace ops accept only secrets.env.<NAME>", r.Path), 0), nil
		}
	}

	switch verb {
	case "exec":
		req, err := workspaceRequest(op)
		if err != nil {
			return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
		}
		if err := applyWorkspaceSecrets(refs, op.Secrets, &req); err != nil {
			return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
		}
		if d, ok := ctx.Deadline(); ok {
			req.Timeout = time.Until(d)
		}
		start := time.Now()
		res, h, runID, err := pu.Workspaces.Exec(ctx, spec, req)
		wall := res.WallMS
		if wall == 0 {
			wall = time.Since(start).Milliseconds()
		}
		pu.workspaceAccount(ctx, rid, tenant, opID, wall, err, len(req.Stdin), len(res.Stdout)+len(res.Stderr))
		res.Stdout = scrubSecrets(res.Stdout, op.Secrets)
		res.Stderr = scrubSecrets(res.Stderr, op.Secrets)
		if err != nil {
			return workspaceFailure(into, prov, h.Ref, workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		payload := workspaceSuccess(into, prov, h.Ref, runID, res, wall)
		// `WITH checkpoint = true`: snapshot after a successful (exit 0)
		// exec — "after cloning", "after install" are the author's to
		// name. A failed checkpoint does not fail the exec: the result
		// stands and `<into>.checkpoint_error` says why.
		if gjson.Get(op.Meta, "checkpoint").Bool() && res.Exit == 0 {
			ref, _, cerr := pu.Workspaces.Checkpoint(ctx, spec, gjson.Get(op.Meta, "comment").String())
			if cerr != nil {
				payload.Raw, _ = sjson.Set(payload.Raw, into+".checkpoint_error.code", workspaceErrorCode(cerr))
				payload.Raw, _ = sjson.Set(payload.Raw, into+".checkpoint_error.message", scrub(cerr.Error()))
			} else {
				payload.Raw, _ = sjson.Set(payload.Raw, into+".checkpoint_ref", ref)
			}
		}
		return payload, nil

	case "create":
		start := time.Now()
		h, err := pu.Workspaces.Create(ctx, spec)
		wall := time.Since(start).Milliseconds()
		if err != nil {
			return workspaceFailure(into, prov, "", workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		p := workspaceVerbDone(into, prov, "created", wall)
		p.Raw, _ = sjson.Set(p.Raw, "_txc.workspace.computer", h.Ref)
		return p, nil

	case "wake":
		start := time.Now()
		h, runID, err := pu.Workspaces.Wake(ctx, spec)
		wall := time.Since(start).Milliseconds()
		if err != nil {
			return workspaceFailure(into, prov, h.Ref, workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		p := workspaceVerbDone(into, prov, "running", wall)
		p.Raw, _ = sjson.Set(p.Raw, "_txc.workspace.computer", h.Ref)
		p.Raw, _ = sjson.Set(p.Raw, "_txc.workspace.run", runID)
		return p, nil

	case "checkpoint":
		start := time.Now()
		ref, h, err := pu.Workspaces.Checkpoint(ctx, spec, gjson.Get(op.Meta, "comment").String())
		wall := time.Since(start).Milliseconds()
		pu.workspaceAccount(ctx, rid, tenant, opID, wall, err, 0, 0)
		if err != nil {
			return workspaceFailure(into, prov, h.Ref, workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		p := workspaceVerbDone(into, prov, "checkpointed", wall)
		p.Raw, _ = sjson.Set(p.Raw, into+".checkpoint_ref", ref)
		p.Raw, _ = sjson.Set(p.Raw, "_txc.workspace.computer", h.Ref)
		return p, nil

	case "destroy":
		start := time.Now()
		err := pu.Workspaces.Destroy(ctx, spec)
		wall := time.Since(start).Milliseconds()
		pu.workspaceAccount(ctx, rid, tenant, opID, wall, err, 0, 0)
		if err != nil {
			return workspaceFailure(into, prov, "", workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		return workspaceVerbDone(into, prov, "destroyed", wall), nil

	case "sleep":
		start := time.Now()
		err := pu.Workspaces.Sleep(ctx, spec)
		wall := time.Since(start).Milliseconds()
		if err != nil {
			return workspaceFailure(into, prov, "", workspaceErrorCode(err), scrub(err.Error()), wall), nil
		}
		return workspaceVerbDone(into, prov, "sleeping", wall), nil

	default:
		// Unreachable while ParseRef closes the verb vocabulary; kept so a
		// verb added to workspace.Verbs without a case here fails in-band
		// rather than silently.
		return workspaceFailure(into, prov, "", "unsupported_verb",
			fmt.Sprintf("verb %q is not supported in this version", verb), 0), nil
	}
}

// workspaceRequest reads the exec directives off the WITH clause:
// `command` (a shell line) or `args` (an argv array), `stdin`, `cwd`,
// `env` (an object of strings).
func workspaceRequest(op operation.Operation) (workspace.ExecRequest, error) {
	var req workspace.ExecRequest
	if v := gjson.Get(op.Meta, "command"); v.Exists() {
		if v.Type != gjson.String {
			return req, errors.New("WITH command must be a string")
		}
		req.Command = v.String()
	}
	if v := gjson.Get(op.Meta, "args"); v.Exists() {
		if !v.IsArray() {
			return req, errors.New("WITH args must be an array of strings")
		}
		for _, a := range v.Array() {
			req.Args = append(req.Args, a.String())
		}
	}
	if req.Command == "" && len(req.Args) == 0 {
		return req, errors.New("WITH command or args is required")
	}
	if req.Command != "" && len(req.Args) > 0 {
		return req, errors.New("WITH command and args are mutually exclusive")
	}
	if v := gjson.Get(op.Meta, "stdin"); v.Exists() {
		if v.Type == gjson.String {
			req.Stdin = []byte(v.String())
		} else {
			req.Stdin = []byte(v.Raw) // a JSON value is fed as its JSON text
		}
	}
	if v := gjson.Get(op.Meta, "cwd"); v.Exists() {
		req.Cwd = v.String()
	}
	if v := gjson.Get(op.Meta, "env"); v.Exists() {
		if !v.IsObject() {
			return req, errors.New("WITH env must be an object")
		}
		req.Env = map[string]string{}
		v.ForEach(func(k, val gjson.Result) bool {
			req.Env[k.String()] = val.String()
			return true
		})
	}
	return req, nil
}

// applyWorkspaceSecrets copies each `secrets.env.<NAME>` ref's materialized
// cleartext into req.Env[NAME], honoring `format`. A ref whose secret is
// absent from the bag is an optional secret the store could not supply
// (the splice already failed the op for a required one): it is skipped, so
// the command sees the variable unset and decides what absence means.
func applyWorkspaceSecrets(refs []secrets.Ref, bag secrets.SecretBag, req *workspace.ExecRequest) error {
	for _, r := range refs {
		name := strings.TrimPrefix(r.Path, "env.")
		cleartext, ok := bag.Get(r.Secret)
		if !ok {
			if r.Optional {
				continue
			}
			return fmt.Errorf("secret %q not materialized into op.Secrets (processor splice bug?)", r.Secret)
		}
		value := string(cleartext)
		if r.Format != "" {
			v, err := secrets.Substitute(r.Format, cleartext)
			if err != nil {
				return fmt.Errorf("secret %q format: %w", r.Secret, err)
			}
			value = v
		}
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		req.Env[name] = value
	}
	return nil
}

// workspaceErrorCode maps a transport/provider error to the in-band code.
func workspaceErrorCode(err error) string {
	var we *workspace.Error
	switch {
	case errors.As(err, &we):
		return we.Code
	case errors.Is(err, workspace.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, workspace.ErrNotAllowed):
		return "not_allowed"
	default:
		return "provider"
	}
}

// workspaceStamp writes the chassis-authored provenance under
// `_txc.workspace.*`. The sandbox never produces envelope JSON (its
// output is a string under `into`), so these fields are trusted by
// construction; the output sanitizer lets them through for transport
// "workspace" only (see sanitizeAuthorOutputFor).
func workspaceStamp(raw, provider, computer, run string, wallMS int64) string {
	raw, _ = sjson.Set(raw, "_txc.workspace.provider", provider)
	if computer != "" {
		raw, _ = sjson.Set(raw, "_txc.workspace.computer", computer)
	}
	if run != "" {
		raw, _ = sjson.Set(raw, "_txc.workspace.run", run)
	}
	raw, _ = sjson.Set(raw, "_txc.workspace.duration_ms", wallMS)
	return raw
}

func workspaceSuccess(into, provider, computer, run string, res workspace.ExecResult, wallMS int64) event.Payload {
	b := jsonx.NewObject()
	b.Set("exit", res.Exit)
	b.Set("stdout", string(res.Stdout))
	b.Set("stderr", string(res.Stderr))
	b.Set("stdout_truncated", res.StdoutTruncated)
	b.Set("stderr_truncated", res.StderrTruncated)
	raw, err := sjson.SetRaw("{}", into, b.String())
	if err != nil {
		raw = "{}"
	}
	raw = workspaceStamp(raw, provider, computer, run, wallMS)
	raw, _ = sjson.Set(raw, "_txc.workspace.exit", res.Exit)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func workspaceVerbDone(into, provider, state string, wallMS int64) event.Payload {
	raw, err := sjson.Set("{}", into+".state", state)
	if err != nil {
		raw = "{}"
	}
	return event.Payload{Raw: workspaceStamp(raw, provider, "", "", wallMS), Type: event.JSON}
}

// workspaceFailure is the in-band shape for a transport/provider failure:
// `<into>: {}` so downstream paths resolve, plus top-level
// `workspace.error {code, message}` (not under _txc, so a rule can gate on
// it the way it gates on chat.error).
func workspaceFailure(into, provider, computer, code, message string, wallMS int64) event.Payload {
	raw, err := sjson.SetRaw("{}", into, "{}")
	if err != nil {
		raw = "{}"
	}
	raw, _ = sjson.Set(raw, "workspace.error.code", code)
	raw, _ = sjson.Set(raw, "workspace.error.message", message)
	return event.Payload{Raw: workspaceStamp(raw, provider, computer, "", wallMS), Type: event.JSON}
}

// workspaceAccount emits the usage event (src="workspace") and charges
// wall-clock fuel, mirroring ExecCompute. Charged regardless of err so a
// slow failure still bills its time.
func (pu *Unit) workspaceAccount(ctx context.Context, rid, tenant, opID string, wallMS int64, err error, bytesIn, bytesOut int) {
	if pu.Usage != nil {
		status := "ok"
		if err != nil {
			status = "error"
		}
		pu.Usage.WriteEvent(usage.UsageEvent{
			RID:        rid,
			Tenant:     tenant,
			Src:        "workspace",
			Stack:      opID,
			DurationMS: wallMS,
			Status:     status,
			BytesIn:    bytesIn,
			BytesOut:   bytesOut,
		})
	}
	_ = addFuel(ctx, workspaceFuel(wallMS), opID)
}

// workspaceFuel is the wall-clock charge for one accounted workspace op:
// 1 fuel per started 30 s period (2 per minute), never less than 1 —
// 0 ms → 1, 30 000 ms → 1, 30 001 ms → 2, 5 minutes → 10.
func workspaceFuel(wallMS int64) int64 {
	if wallMS <= 0 {
		return fuelCostWorkspacePerPeriod
	}
	periods := (wallMS + workspaceFuelPeriodMS - 1) / workspaceFuelPeriodMS
	if periods < 1 {
		periods = 1
	}
	return periods * fuelCostWorkspacePerPeriod
}
