package server

// txco://state/{create,get,transition} — durable state records with an
// atomic compare-and-swap transition and a durable event per transition
// (chassis/state). A record is (machine, id) inside the tenant: a state,
// a version and opaque JSON data. Every committed transition writes an
// event in the same transaction, and the `state` personality presents
// each event at least once into the tenant's `_state/0` stack as
// `@state.*` (see docs/advanced/protocols/state.md).
//
//	create      WITH machine, id, state, data?
//	            → {ok:true, record}; a record already there answers
//	              error.code=txco_state_exists with `current`. No event.
//	get         WITH machine, id
//	            → {found:true, record} or {found:false}.
//	transition  WITH machine, id, from, to, expected_version, data?
//	            → {ok:true, record, event_id}. Both `from` and
//	              `expected_version` are required and both must match;
//	              a miss answers error.code=txco_state_conflict with
//	              error.reason = "version" | "state" and `current`, so a
//	              stale actor (a timer armed at waiting@14 waking
//	              waiting@17) is refused, not applied. A missing record
//	              answers txco_state_not_found. Omit `data` to keep the
//	              stored data; any value given (even `{}`) replaces it.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx),
// never a mutable _txc.* field, and `machine` plays the role a KV
// namespace does, so there is no namespace argument. The event's cause
// (which run committed it) is read from the pinned context — source,
// stack, rid, continuation run — never from arguments.
//
// Output lands under `into` (default `_state`); errors as
// `<into>.error.{code,message}` with a nil Go error, so a conflict is
// ordinary control flow: `WHEN ._state.ok == true` or
// `WHEN ._state.error.code == "txco_state_conflict"`.

import (
	"context"
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	chstate "github.com/loremlabs/thanks-computer/chassis/state"
)

type stateDeps struct {
	store *chstate.Store // nil ⇒ every op answers txco_state_disabled
	// wake nudges this node's state dispatcher after a commit so the event
	// is presented at once rather than at the next poll; nil when this
	// node runs no dispatcher. Sends never block.
	wake chan<- struct{}
}

func stateErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// stateErrFrom maps a store error to the envelope by its code. An exists
// or conflict error also carries the record as it is (`<into>.current`),
// and a conflict says which check failed (`<into>.error.reason`).
func stateErrFrom(into string, err error) event.Payload {
	code := chstate.ErrorCode(err)
	if code == "" {
		code = "txco_state_store"
	}
	pl := stateErr(into, code, err.Error())
	switch e := err.(type) {
	case *chstate.ExistsError:
		pl.Raw, _ = sjson.SetRaw(pl.Raw, into+".current", stateRecordJSON(e.Current))
	case *chstate.ConflictError:
		pl.Raw, _ = sjson.Set(pl.Raw, into+".error.reason", e.Reason)
		pl.Raw, _ = sjson.SetRaw(pl.Raw, into+".current", stateRecordJSON(e.Current))
	}
	return pl
}

// statePrelude is the common head of every handler: meta, into, tenant,
// store.
func statePrelude(ctx context.Context, d stateDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = intoPath(meta, "_state")
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, stateErr(into, "txco_state_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", nil, into, stateErr(into, "txco_state_disabled", "no state store on this node (the 'state' personality is not active, or the store failed to open at boot; see --state-store)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

// stateRecordJSON renders a record: {machine, id, state, version, data,
// created_at, updated_at}. The tenant is implicit.
func stateRecordJSON(r chstate.Record) string {
	b := jsonx.NewObject()
	b.Set("machine", r.Machine)
	b.Set("id", r.ID)
	b.Set("state", r.State)
	b.Set("version", r.Version)
	data := string(r.Data)
	if data == "" {
		data = "{}"
	}
	b.SetRaw("data", data)
	b.Set("created_at", r.CreatedAt.UTC().Format(chstate.AtLayout))
	b.Set("updated_at", r.UpdatedAt.UTC().Format(chstate.AtLayout))
	return b.String()
}

// stateData lifts WITH `data`: present and non-null → the raw JSON
// verbatim (objects, arrays and scalars alike); absent or null → nil.
func stateData(meta []byte) json.RawMessage {
	if dv := gjson.GetBytes(meta, "data"); dv.Exists() && dv.Type != gjson.Null {
		return json.RawMessage(dv.Raw)
	}
	return nil
}

// stateCause is the transition's provenance, from trusted context only.
func stateCause(ctx context.Context, in []byte) chstate.Cause {
	rid, _ := ctx.Value(config.CtxKeyRid).(string)
	if rid == "" {
		rid = gjson.GetBytes(in, "_txc.rid").String()
	}
	return chstate.Cause{
		Source: processor.SourceScope(ctx),
		Stack:  processor.StackScope(ctx),
		Trace:  rid,
		Run:    processor.RunScope(ctx),
	}
}

// stateCreate stores a record at version 1. Result at `into`:
// {ok:true, record}.
func stateCreate(ctx context.Context, d stateDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := statePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	rec, err := d.store.Create(ctx, chstate.CreateReq{
		Tenant:  tenant,
		Machine: gjson.GetBytes(meta, "machine").String(),
		ID:      gjson.GetBytes(meta, "id").String(),
		State:   gjson.GetBytes(meta, "state").String(),
		Data:    stateData(meta),
	})
	if err != nil {
		return stateErrFrom(into, err), nil
	}
	resp := jsonx.NewObject()
	resp.Set(into+".ok", true)
	resp.SetRaw(into+".record", stateRecordJSON(rec))
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// stateGet reads a record. Result at `into`: {found:true, record} or
// {found:false}. A missing record is not an error.
func stateGet(ctx context.Context, d stateDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := statePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	rec, err := d.store.Get(ctx, tenant, gjson.GetBytes(meta, "machine").String(), gjson.GetBytes(meta, "id").String())
	if err != nil {
		if chstate.ErrorCode(err) == "txco_state_not_found" {
			resp := jsonx.NewObject()
			resp.Set(into+".found", false)
			return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
		}
		return stateErrFrom(into, err), nil
	}
	resp := jsonx.NewObject()
	resp.Set(into+".found", true)
	resp.SetRaw(into+".record", stateRecordJSON(rec))
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// stateTransition is the compare-and-swap. Result at `into`:
// {ok:true, record, event_id}.
func stateTransition(ctx context.Context, d stateDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := statePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	ev := gjson.GetBytes(meta, "expected_version")
	if !ev.Exists() {
		return stateErr(into, "txco_state_invalid_arg", "expected_version is required"), nil
	}
	if ev.Type != gjson.Number && ev.Type != gjson.String {
		return stateErr(into, "txco_state_invalid_arg", "expected_version must be a number"), nil
	}
	rec, tev, err := d.store.Transition(ctx, chstate.TransitionReq{
		Tenant:          tenant,
		Machine:         gjson.GetBytes(meta, "machine").String(),
		ID:              gjson.GetBytes(meta, "id").String(),
		From:            gjson.GetBytes(meta, "from").String(),
		To:              gjson.GetBytes(meta, "to").String(),
		ExpectedVersion: ev.Int(),
		Data:            stateData(meta),
		Cause:           stateCause(ctx, in),
	})
	if err != nil {
		return stateErrFrom(into, err), nil
	}
	if d.wake != nil {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	resp := jsonx.NewObject()
	resp.Set(into+".ok", true)
	resp.SetRaw(into+".record", stateRecordJSON(rec))
	resp.Set(into+".event_id", tev.ID)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}
