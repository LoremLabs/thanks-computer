package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	chstate "github.com/loremlabs/thanks-computer/chassis/state"
)

// The state handler tests call the handlers the way ExecCore does: trusted
// tenant (and source/stack/rid) on ctx + WITH meta, against the bundled
// sqlite backend in t.TempDir.

func newStateDeps(t *testing.T) stateDeps {
	t.Helper()
	st, err := chstate.Open("sqlite", chstate.Config{DBPath: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return stateDeps{store: st, wake: make(chan struct{}, 1)}
}

type stateHandler func(context.Context, stateDeps, []byte) (event.Payload, error)

// callState runs a handler for tenant with the given WITH meta. ctxMod, if
// set, decorates the context (source, stack, rid). A Go error from a
// handler fails the test: state errors must be envelope-surfaced.
func callState(t *testing.T, fn stateHandler, d stateDeps, tenant, metaJSON string, ctxMod func(context.Context) context.Context) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if ctxMod != nil {
		ctx = ctxMod(ctx)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	in := `{"_txc":{"op":"demo/100/state","rid":"rid_from_envelope"}}`
	pl, err := fn(ctx, d, []byte(in))
	if err != nil {
		t.Fatalf("state handler returned a Go error (must be envelope-surfaced): %v", err)
	}
	return pl.Raw
}

func stWantCode(t *testing.T, raw, code string) {
	t.Helper()
	if got := gjson.Get(raw, "_state.error.code").String(); got != code {
		t.Fatalf("_state.error.code = %q, want %q (raw %s)", got, code, raw)
	}
}

func stWantOK(t *testing.T, raw string) {
	t.Helper()
	if e := gjson.Get(raw, "_state.error"); e.Exists() {
		t.Fatalf("unexpected _state.error: %s", e.Raw)
	}
	if !gjson.Get(raw, "_state.ok").Bool() {
		t.Fatalf("_state.ok not true: %s", raw)
	}
}

func TestStateCreateGetExists(t *testing.T) {
	d := newStateDeps(t)
	raw := callState(t, stateCreate, d, "acme", `{"machine":"onepony.task","id":"paris:t1","state":"working","data":{"owner":"a"}}`, nil)
	stWantOK(t, raw)
	rec := gjson.Get(raw, "_state.record")
	if rec.Get("machine").String() != "onepony.task" || rec.Get("id").String() != "paris:t1" ||
		rec.Get("state").String() != "working" || rec.Get("version").Int() != 1 ||
		rec.Get("data.owner").String() != "a" || rec.Get("created_at").String() == "" {
		t.Fatalf("record = %s", rec.Raw)
	}
	// Exists: the code, and the record as it is.
	raw = callState(t, stateCreate, d, "acme", `{"machine":"onepony.task","id":"paris:t1","state":"other"}`, nil)
	stWantCode(t, raw, "txco_state_exists")
	if gjson.Get(raw, "_state.current.state").String() != "working" || gjson.Get(raw, "_state.current.version").Int() != 1 {
		t.Fatalf("current = %s", gjson.Get(raw, "_state.current").Raw)
	}
	// Get: found, and not found (not an error).
	raw = callState(t, stateGet, d, "acme", `{"machine":"onepony.task","id":"paris:t1"}`, nil)
	if !gjson.Get(raw, "_state.found").Bool() || gjson.Get(raw, "_state.record.version").Int() != 1 {
		t.Fatalf("get = %s", raw)
	}
	raw = callState(t, stateGet, d, "acme", `{"machine":"onepony.task","id":"nope"}`, nil)
	if gjson.Get(raw, "_state.found").Bool() || gjson.Get(raw, "_state.error").Exists() || gjson.Get(raw, "_state.record").Exists() {
		t.Fatalf("get missing = %s", raw)
	}
	// Default data.
	raw = callState(t, stateCreate, d, "acme", `{"machine":"onepony.task","id":"t2","state":"working"}`, nil)
	if gjson.Get(raw, "_state.record.data").Raw != `{}` {
		t.Fatalf("default data = %s", gjson.Get(raw, "_state.record.data").Raw)
	}
	// Invalid arguments answer in-band.
	stWantCode(t, callState(t, stateCreate, d, "acme", `{"machine":"Bad","id":"x","state":"s"}`, nil), "txco_state_invalid_arg")
	d.store.SetMaxDataBytes(8)
	stWantCode(t, callState(t, stateCreate, d, "acme", `{"machine":"m","id":"x","state":"s","data":{"k":"0123456789"}}`, nil), "txco_state_invalid_arg")
	stWantCode(t, callState(t, stateCreate, d, "acme", `{"machine":"m","id":"x","state":"s","data":[1,2,3`, nil), "txco_state_invalid_arg")
}

func TestStateTransitionCauseAndWake(t *testing.T) {
	d := newStateDeps(t)
	callState(t, stateCreate, d, "acme", `{"machine":"m","id":"t1","state":"working"}`, nil)
	withCause := func(ctx context.Context) context.Context {
		ctx = processor.WithSource(ctx, "web")
		ctx = processor.WithStack(ctx, "web/canary")
		return context.WithValue(ctx, config.CtxKeyRid, "rid_from_ctx")
	}
	raw := callState(t, stateTransition, d, "acme",
		`{"machine":"m","id":"t1","from":"working","to":"waiting","expected_version":1,"data":{"wake":"soon"}}`, withCause)
	stWantOK(t, raw)
	if gjson.Get(raw, "_state.record.version").Int() != 2 || gjson.Get(raw, "_state.record.state").String() != "waiting" ||
		gjson.Get(raw, "_state.record.data.wake").String() != "soon" {
		t.Fatalf("record = %s", gjson.Get(raw, "_state.record").Raw)
	}
	evID := gjson.Get(raw, "_state.event_id").String()
	if !strings.HasPrefix(evID, "stev_") {
		t.Fatalf("event_id = %q", evID)
	}
	ev, err := d.store.GetEvent(context.Background(), evID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Cause != (chstate.Cause{Source: "web", Stack: "web/canary", Trace: "rid_from_ctx"}) {
		t.Fatalf("cause = %+v", ev.Cause)
	}
	if len(d.wake) != 1 {
		t.Fatalf("dispatcher not nudged")
	}
	// Trace falls back to the envelope's rid; expected_version as a string
	// is fine; omitted data keeps; null data keeps; {} replaces.
	raw = callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"ready","expected_version":"2"}`, nil)
	stWantOK(t, raw)
	if gjson.Get(raw, "_state.record.data.wake").String() != "soon" {
		t.Fatalf("omitted data did not keep: %s", raw)
	}
	ev, _ = d.store.GetEvent(context.Background(), gjson.Get(raw, "_state.event_id").String())
	if ev.Cause.Trace != "rid_from_envelope" || ev.Cause.Source != "" {
		t.Fatalf("fallback cause = %+v", ev.Cause)
	}
	raw = callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"ready","to":"ready","expected_version":3,"data":null}`, nil)
	stWantOK(t, raw)
	if gjson.Get(raw, "_state.record.data.wake").String() != "soon" {
		t.Fatalf("null data did not keep: %s", raw)
	}
	raw = callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"ready","to":"done","expected_version":4,"data":{}}`, nil)
	stWantOK(t, raw)
	if gjson.Get(raw, "_state.record.data").Raw != `{}` || gjson.Get(raw, "_state.record.version").Int() != 5 {
		t.Fatalf("{} did not replace: %s", raw)
	}
	// A nil wake channel (no dispatcher on this node) is fine.
	d2 := stateDeps{store: d.store}
	stWantOK(t, callState(t, stateTransition, d2, "acme", `{"machine":"m","id":"t1","from":"done","to":"done","expected_version":5}`, nil))
}

func TestStateTransitionErrors(t *testing.T) {
	d := newStateDeps(t)
	callState(t, stateCreate, d, "acme", `{"machine":"m","id":"t1","state":"working"}`, nil)
	stWantOK(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"working","to":"waiting","expected_version":1}`, nil))

	// Stale version: the code, the reason, and the record as it is.
	raw := callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"ready","expected_version":1}`, nil)
	stWantCode(t, raw, "txco_state_conflict")
	if gjson.Get(raw, "_state.error.reason").String() != "version" || gjson.Get(raw, "_state.current.version").Int() != 2 ||
		gjson.Get(raw, "_state.current.state").String() != "waiting" {
		t.Fatalf("version conflict = %s", raw)
	}
	if gjson.Get(raw, "_state.ok").Exists() || gjson.Get(raw, "_state.event_id").Exists() {
		t.Fatalf("conflict must not look like success: %s", raw)
	}
	// Wrong from.
	raw = callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"working","to":"ready","expected_version":2}`, nil)
	stWantCode(t, raw, "txco_state_conflict")
	if gjson.Get(raw, "_state.error.reason").String() != "state" {
		t.Fatalf("state conflict = %s", raw)
	}
	// Not found; missing / bad expected_version; bad names.
	stWantCode(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"nope","from":"a","to":"b","expected_version":1}`, nil), "txco_state_not_found")
	stWantCode(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"ready"}`, nil), "txco_state_invalid_arg")
	stWantCode(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"ready","expected_version":true}`, nil), "txco_state_invalid_arg")
	stWantCode(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"ready","expected_version":0}`, nil), "txco_state_invalid_arg")
	stWantCode(t, callState(t, stateTransition, d, "acme", `{"machine":"m","id":"t1","from":"waiting","to":"","expected_version":2}`, nil), "txco_state_invalid_arg")
	// Nothing above moved the record.
	if raw = callState(t, stateGet, d, "acme", `{"machine":"m","id":"t1"}`, nil); gjson.Get(raw, "_state.record.version").Int() != 2 {
		t.Fatalf("record moved: %s", raw)
	}
	// Another tenant cannot see or move it.
	raw = callState(t, stateGet, d, "other", `{"machine":"m","id":"t1"}`, nil)
	if gjson.Get(raw, "_state.found").Bool() {
		t.Fatalf("tenant leak: %s", raw)
	}
	stWantCode(t, callState(t, stateTransition, d, "other", `{"machine":"m","id":"t1","from":"waiting","to":"x","expected_version":2}`, nil), "txco_state_not_found")
}

func TestStateScopingAndInto(t *testing.T) {
	d := newStateDeps(t)
	// No tenant; no store.
	stWantCode(t, callState(t, stateCreate, d, "", `{"machine":"m","id":"x","state":"s"}`, nil), "txco_state_no_tenant")
	stWantCode(t, callState(t, stateGet, stateDeps{}, "acme", `{"machine":"m","id":"x"}`, nil), "txco_state_disabled")
	stWantCode(t, callState(t, stateTransition, stateDeps{}, "acme", `{"machine":"m","id":"x","from":"a","to":"b","expected_version":1}`, nil), "txco_state_disabled")
	// into: honoured on success and on error; a chassis path is refused
	// and falls back to the default.
	raw := callState(t, stateCreate, d, "acme", `{"machine":"m","id":"x","state":"s","into":"_mine"}`, nil)
	if !gjson.Get(raw, "_mine.ok").Bool() || gjson.Get(raw, "_state").Exists() {
		t.Fatalf("into on success = %s", raw)
	}
	raw = callState(t, stateCreate, d, "acme", `{"machine":"m","id":"x","state":"s","into":"_mine"}`, nil)
	if gjson.Get(raw, "_mine.error.code").String() != "txco_state_exists" {
		t.Fatalf("into on error = %s", raw)
	}
	raw = callState(t, stateGet, d, "acme", `{"machine":"m","id":"x","into":"_txc.state"}`, nil)
	if !gjson.Get(raw, "_state.found").Bool() || gjson.Get(raw, "_txc").Exists() {
		t.Fatalf("chassis into must fall back: %s", raw)
	}
}
