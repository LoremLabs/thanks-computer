package processor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// stubRunner is a compute.Runner that returns fixed bytes (or an error),
// standing in for a real engine so the processor wiring can be tested without
// a wasm toolchain.
type stubRunner struct {
	out  string
	err  error
	seen []byte // captures the input handed to the engine
}

func (s *stubRunner) Run(_ context.Context, _ compute.Ref, input []byte) ([]byte, error) {
	s.seen = input
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.out), nil
}

func computeOp(ref, input string) operation.Operation {
	return operation.Operation{
		Stack:     "site",
		Scope:     100,
		Name:      "c",
		Resonator: &resonator.Resonator{Exec: ref},
		Input:     input,
	}
}

func TestExecComputeNoRuntime(t *testing.T) {
	pu, _ := newTestUnit(t)
	// pu.Computes left nil.
	_, err := pu.ExecCompute(context.Background(), computeOp("compute://sha256/abc", `{}`))
	if err == nil {
		t.Fatal("ExecCompute with nil runtime = nil error, want loud failure")
	}
}

func TestExecComputeMalformedRef(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{out: `{}`}
	_, err := pu.ExecCompute(context.Background(), computeOp("compute://bad", `{}`))
	if err == nil {
		t.Fatal("ExecCompute with malformed ref = nil error, want error")
	}
}

func TestExecComputeStubSuccess(t *testing.T) {
	pu, _ := newTestUnit(t)
	sr := &stubRunner{out: `{"computed":true}`}
	pu.Computes = sr
	p, err := pu.ExecCompute(context.Background(), computeOp("compute://sha256/abc", `{"in":1}`))
	if err != nil {
		t.Fatalf("ExecCompute: %v", err)
	}
	if !gjson.Get(p.Raw, "computed").Bool() {
		t.Fatalf("payload = %q, want computed:true", p.Raw)
	}
	if p.Type != event.JSON {
		t.Fatalf("payload type = %v, want JSON", p.Type)
	}
	// ABI v2: the engine sees { input, meta, env }. input is the op envelope;
	// meta carries the trace identity.
	seen := string(sr.seen)
	if gjson.Get(seen, "input.in").Int() != 1 {
		t.Fatalf("engine input.in = %q, want 1", seen)
	}
	if gjson.Get(seen, "meta.stack").String() != "site" ||
		gjson.Get(seen, "meta.scope").Int() != 100 ||
		gjson.Get(seen, "meta.name").String() != "c" ||
		gjson.Get(seen, "meta.op").String() != "site/100/c" {
		t.Fatalf("engine meta wrong: %q", seen)
	}
}

// TestExecComputeEnvFromMeta: ctx.env is sourced from the op's WITH-clause
// config channel (op.Meta), so a compute reads the same config an http worker
// would receive in its meta.
func TestExecComputeEnvFromMeta(t *testing.T) {
	pu, _ := newTestUnit(t)
	sr := &stubRunner{out: `{}`}
	pu.Computes = sr
	op := computeOp("compute://sha256/abc", `{}`)
	op.Meta = `{"region":"us","model":"fast"}`
	if _, err := pu.ExecCompute(context.Background(), op); err != nil {
		t.Fatalf("ExecCompute: %v", err)
	}
	seen := string(sr.seen)
	if gjson.Get(seen, "env.region").String() != "us" || gjson.Get(seen, "env.model").String() != "fast" {
		t.Fatalf("env not sourced from op.Meta: %q", seen)
	}
}

// TestExecComputeSecretsFromBag: ctx.secrets is populated by name from the
// per-op SecretBag — the same materialized secrets an http:// op would splice
// into its request.
func TestExecComputeSecretsFromBag(t *testing.T) {
	pu, _ := newTestUnit(t)
	sr := &stubRunner{out: `{}`}
	pu.Computes = sr
	op := computeOp("compute://sha256/abc", `{}`)
	op.Secrets.Set("STRIPE_API_KEY", []byte("sk_live_xyz"))
	if _, err := pu.ExecCompute(context.Background(), op); err != nil {
		t.Fatalf("ExecCompute: %v", err)
	}
	if got := gjson.Get(string(sr.seen), "secrets.STRIPE_API_KEY").String(); got != "sk_live_xyz" {
		t.Fatalf("secrets.STRIPE_API_KEY = %q, want sk_live_xyz; stdin=%q", got, sr.seen)
	}
}

func TestExecComputePropagatesEngineError(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{err: errors.New("boom")}
	if _, err := pu.ExecCompute(context.Background(), computeOp("compute://sha256/abc", `{}`)); err == nil {
		t.Fatal("ExecCompute = nil error, want engine error propagated")
	}
}

type stubUsage struct{ ev *usage.UsageEvent }

func (s *stubUsage) WriteEvent(e usage.UsageEvent) { s.ev = &e }
func (s *stubUsage) Close(context.Context) error   { return nil }

// TestComputeEmitsUsage: a compute invocation emits a usage event with
// src="compute", the compute's stack/scope/name, byte counts, and status.
func TestComputeEmitsUsage(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{out: `{"ok":true}`}
	us := &stubUsage{}
	pu.Usage = us

	_, err := pu.ExecCompute(context.Background(), computeOp("compute://sha256/abc", `{"a":1}`))
	if err != nil {
		t.Fatalf("ExecCompute: %v", err)
	}
	if us.ev == nil {
		t.Fatal("no usage event emitted")
	}
	if us.ev.Src != "compute" {
		t.Fatalf("usage Src = %q, want compute", us.ev.Src)
	}
	if us.ev.Stack != "site/100/c" {
		t.Fatalf("usage Stack = %q, want site/100/c", us.ev.Stack)
	}
	if us.ev.Status != "ok" {
		t.Fatalf("usage Status = %q, want ok", us.ev.Status)
	}
	if us.ev.BytesIn != len(`{"a":1}`) || us.ev.BytesOut != len(`{"ok":true}`) {
		t.Fatalf("usage bytes in/out = %d/%d, want %d/%d",
			us.ev.BytesIn, us.ev.BytesOut, len(`{"a":1}`), len(`{"ok":true}`))
	}
}

// TestComputeUsageStatusError: a failing compute emits status="error".
func TestComputeUsageStatusError(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{err: errors.New("boom")}
	us := &stubUsage{}
	pu.Usage = us
	_, _ = pu.ExecCompute(context.Background(), computeOp("compute://sha256/abc", `{}`))
	if us.ev == nil || us.ev.Status != "error" {
		t.Fatalf("usage status = %v, want error", us.ev)
	}
}

// TestComputeErrorHaltsRequest: a compute throwing at runtime halts the
// request with an error response (not a silently-dropped op + normal output).
func TestComputeErrorHaltsRequest(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{err: errors.New("boom-from-compute")}
	seedOp(t, pu, "site", 100, "c", `EXEC "compute://sha256/deadbeef"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "site/100", resCh) }()

	var p event.Payload
	select {
	case p = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no response within 5s")
	}
	if p.Type != event.ErrorStr {
		t.Fatalf("payload type = %v, want ErrorStr (halt); raw=%s", p.Type, p.Raw)
	}
	if !strings.Contains(p.Raw, "boom-from-compute") {
		t.Fatalf("error payload missing cause: %s", p.Raw)
	}
}

// TestComputeDispatchMergesAndGoto proves the compute transport flows the
// IDENTICAL post-EXEC path as http://: the compute output is merged into the
// envelope, and a `_txc.goto` in that output drives the same scope-jump
// machinery (advanceAfterScope) — landing on site/200 whose EMIT then runs.
// A passing assertion on BOTH `computed` and `rendered` is the parity proof.
func TestComputeDispatchMergesAndGoto(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{out: `{"computed":true,"_txc":{"goto":"site/200"}}`}

	seedOp(t, pu, "site", 100, "c", `EXEC "compute://sha256/deadbeef"`)
	seedOp(t, pu, "site", 200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "site/100", resCh) }()

	var p event.Payload
	select {
	case p = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no response within 5s")
	}

	if !gjson.Get(p.Raw, "computed").Bool() {
		t.Fatalf("compute output not merged: %s", p.Raw)
	}
	if !gjson.Get(p.Raw, "rendered").Bool() {
		t.Fatalf("_txc.goto from compute output did not drive the jump to site/200: %s", p.Raw)
	}
}
