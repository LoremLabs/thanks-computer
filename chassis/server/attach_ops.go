package server

// txco://attach/{invite,status,revoke,stop} — a workspace held by a machine
// that dialed in with `txco attach` (chassis/attached).
//
//	invite  WITH workspace, label?, ttl?
//	        → {ok:true, code, expires_at}. A one-use code for the named
//	          workspace of THIS stack. The code is answered once; only its
//	          hash is kept. A name the provider already holds answers
//	          error.code = "name_in_use": attaching never destroys anything.
//	status  WITH workspace
//	        → {enrolled, attached, since?, machine?, root?, os?, version?}.
//	          `enrolled` is the DB's word that a machine holds the name;
//	          `attached`, that one is connected right now.
//	revoke  WITH workspace
//	        → {ok:true}. The credential is revoked; no request reaches the
//	          machine from then on. The name stays attached and reads `away`
//	          until `workspace://<name>/destroy` gives it back to the provider.
//	stop    WITH workspace, job
//	        → {ok:true}. Asks the machine to end a detached job.
//
// The workspace is the caller's own: tenant from processor.TenantScope and
// stack from the pinned route, exactly as workspace:// resolves a name, so a
// code made here is for the same workspace an exec from this stack reaches.
// There is no way to name another stack's or another tenant's.
//
// Output lands under `into` (default `_attach`); failures as
// `<into>.error.{code,message}` with a nil Go error.

import (
	"context"
	"errors"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

type attachDeps struct {
	svc *attached.Service // nil ⇒ every op answers txco_attach_disabled
}

func attachErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func attachErrFrom(into string, err error) event.Payload {
	var we *workspace.Error
	switch {
	case errors.Is(err, attached.ErrNameInUse):
		return attachErr(into, "name_in_use", "a workspace of that name already exists; destroy it first, or attach under another name")
	case errors.As(err, &we):
		return attachErr(into, we.Code, we.Message)
	default:
		return attachErr(into, "txco_attach_store", err.Error())
	}
}

// attachPrelude is the common head of every handler: the op's meta, its
// `into`, and the caller's own workspace named by WITH workspace.
func attachPrelude(ctx context.Context, d attachDeps) (spec workspace.Spec, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = intoPath(meta, "_attach")
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return spec, nil, into, attachErr(into, "txco_attach_no_tenant", "no tenant in request scope"), false
	}
	if d.svc == nil {
		return spec, nil, into, attachErr(into, "txco_attach_disabled", "this chassis does not serve attached workspaces (no workspace provider, or no runtime store)"), false
	}
	stack := workspace.AppStack(processor.StackScope(ctx))
	if stack == "" {
		return spec, nil, into, attachErr(into, "txco_attach_no_stack", "no stack in request scope"), false
	}
	name := gjson.GetBytes(meta, "workspace").String()
	if err := workspace.ValidateName(name); err != nil {
		return spec, nil, into, attachErr(into, "bad_request", "WITH workspace: "+err.Error()), false
	}
	return workspace.Spec{Tenant: tenant, Stack: stack, Name: name}, meta, into, event.Payload{}, true
}

func attachInvite(ctx context.Context, d attachDeps, _ []byte) (event.Payload, error) {
	spec, meta, into, ep, ok := attachPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	var ttl time.Duration
	if v := gjson.GetBytes(meta, "ttl"); v.Exists() {
		parsed, err := time.ParseDuration(v.String())
		if err != nil || parsed <= 0 {
			return attachErr(into, "bad_request", `WITH ttl must be a duration such as "1h"`), nil
		}
		ttl = parsed
	}
	code, expires, err := d.svc.Invite(ctx, spec, gjson.GetBytes(meta, "label").String(), ttl)
	if err != nil {
		return attachErrFrom(into, err), nil
	}
	b := jsonx.NewObject()
	b.Set("ok", true)
	b.Set("code", code)
	b.Set("expires_at", expires.UTC().Format(time.RFC3339))
	raw, _ := sjson.SetRaw(`{}`, into, b.String())
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func attachStatus(ctx context.Context, d attachDeps, _ []byte) (event.Payload, error) {
	spec, _, into, ep, ok := attachPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	st, err := d.svc.Status(ctx, spec)
	if err != nil {
		return attachErrFrom(into, err), nil
	}
	b := jsonx.NewObject()
	b.Set("enrolled", st.Enrolled)
	b.Set("attached", st.Attached)
	if st.Enrolled {
		b.Set("machine", st.Machine)
		b.Set("root", st.Root)
		b.Set("os", st.OS)
	}
	if st.Attached {
		b.Set("version", st.Version)
		b.Set("since", st.Since.UTC().Format(time.RFC3339))
	}
	raw, _ := sjson.SetRaw(`{}`, into, b.String())
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func attachRevoke(ctx context.Context, d attachDeps, _ []byte) (event.Payload, error) {
	spec, _, into, ep, ok := attachPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	if err := d.svc.Revoke(ctx, spec); err != nil {
		return attachErrFrom(into, err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".ok", true)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func attachStop(ctx context.Context, d attachDeps, _ []byte) (event.Payload, error) {
	spec, meta, into, ep, ok := attachPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	job := gjson.GetBytes(meta, "job").String()
	if job == "" {
		return attachErr(into, "bad_request", "WITH job is required"), nil
	}
	if err := d.svc.Stop(ctx, spec, job); err != nil {
		return attachErrFrom(into, err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".ok", true)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// registerAttachOps installs the four handlers. A nil service leaves them
// installed and answering txco_attach_disabled, so a stack can branch.
func registerAttachOps(pu *processor.Unit, d attachDeps) {
	for name, fn := range map[string]func(context.Context, attachDeps, []byte) (event.Payload, error){
		"txco://attach/invite": attachInvite,
		"txco://attach/status": attachStatus,
		"txco://attach/revoke": attachRevoke,
		"txco://attach/stop":   attachStop,
	} {
		fn := fn
		pu.Handle([]byte(name), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return fn(ctx, d, in)
			}))
	}
}
