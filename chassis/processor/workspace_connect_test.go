package processor

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/attach"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// dialableStub is a stubProvider whose woken computer also implements
// workspace.Dialer, handing back one end of a pipe and keeping the other.
type dialableStub struct {
	*stubProvider
	dialErr error
	far     net.Conn
	seen    workspace.Service
	dials   int
}

func (d *dialableStub) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	return d, nil
}

func (d *dialableStub) DialService(_ context.Context, svc workspace.Service) (net.Conn, error) {
	d.dials++
	d.seen = svc
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	near, far := net.Pipe()
	d.far = far
	return near, nil
}

// connectUnit builds a Unit inside a websocket session run (source pinned,
// session id on the input), with a registry and an "echo" service.
func connectUnit(t *testing.T, prov workspace.Provider) (*Unit, context.Context) {
	t.Helper()
	pu, _ := newTestUnit(t)
	pu.Workspaces = workspace.NewManager(prov, workspace.Limits{}, nil)
	svcs, err := workspace.NewServices([]string{"echo=17777"})
	if err != nil {
		t.Fatal(err)
	}
	pu.Workspaces.SetServices(svcs)
	pu.Attachments = attach.NewRegistry()
	return pu, WithSource(WithTenant(context.Background(), "acme"), "websocket")
}

func connectOp(meta string) operation.Operation {
	op := workspaceOp("workspace://tools/connect", meta)
	op.Input = `{"_txc":{"websocket":{"session":{"id":"s1"}}}}`
	return op
}

func TestExecWorkspaceConnectBindsAService(t *testing.T) {
	d := &dialableStub{stubProvider: &stubProvider{}}
	pu, ctx := connectUnit(t, d)

	p, err := pu.ExecWorkspace(ctx, connectOp(`{"service":"echo","max_duration":"1h","into":"_c"}`))
	if err != nil {
		t.Fatalf("ExecWorkspace: %v", err)
	}
	if !gjson.Get(p.Raw, "_c.connected").Bool() || gjson.Get(p.Raw, "_c.service").String() != "echo" ||
		gjson.Get(p.Raw, "_c.lease.id").String() == "" || gjson.Get(p.Raw, "_c.run").String() == "" {
		t.Fatalf("payload = %s", p.Raw)
	}
	if gjson.Get(p.Raw, "_txc.workspace.provider").String() != "stub" {
		t.Errorf("provenance stamp missing: %s", p.Raw)
	}
	if d.seen.Port != 17777 || d.dials != 1 {
		t.Errorf("dialed %+v ×%d", d.seen, d.dials)
	}
	att, ok := pu.Attachments.Lookup("acme", "s1")
	if !ok || att.Kind != attach.KindService || att.Service != "echo" || att.Conn == nil {
		t.Fatalf("binding = %+v ok=%v", att, ok)
	}
	defer att.Close("test")
	// The Conn is the dialed connection: bytes written through it reach the service.
	go func() { _, _ = att.Conn.Write([]byte("hi")) }()
	buf := make([]byte, 2)
	if _, err := d.far.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("service read = %q err=%v", buf, err)
	}

	// A second connect on the bound session is refused in-band.
	p, _ = pu.ExecWorkspace(ctx, connectOp(`{"service":"echo"}`))
	if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != "bad_request" || !strings.Contains(gjson.Get(p.Raw, "workspace.error.message").String(), "already has an attachment") {
		t.Fatalf("second connect = %s", p.Raw)
	}
	if d.dials != 1 {
		t.Errorf("a refused connect must not dial (dials=%d)", d.dials)
	}
}

func TestExecWorkspaceConnectRefusals(t *testing.T) {
	d := &dialableStub{stubProvider: &stubProvider{}}
	pu, ctx := connectUnit(t, d)

	cases := []struct {
		name string
		ctx  context.Context
		op   operation.Operation
		code string
		msg  string
	}{
		{"not a websocket run", WithTenant(context.Background(), "acme"), connectOp(`{"service":"echo"}`), "bad_request", "inside a WebSocket session run"},
		{"no session id", ctx, workspaceOp("workspace://tools/connect", `{"service":"echo"}`), "bad_request", "inside a WebSocket session run"},
		{"stream", ctx, connectOp(`{"service":"echo","stream":true}`), "bad_request", "stream"},
		{"secrets", ctx, connectOp(`{"service":"echo","secrets":{"env":{"TOKEN":{"secret":"t"}}}}`), "bad_request", "refuses secrets"},
		{"missing service", ctx, connectOp(`{}`), "bad_request", "WITH service is required"},
		{"service not a string", ctx, connectOp(`{"service":5900}`), "bad_request", "WITH service is required"},
		{"unknown service", ctx, connectOp(`{"service":"5900"}`), "bad_request", `unknown service "5900"; this node knows [browser echo]`},
		{"bad duration", ctx, connectOp(`{"service":"echo","max_duration":"soon"}`), "bad_request", "max_duration"},
	}
	loopOp := connectOp(`{"service":"echo"}`)
	loopOp.Resonator.Loop = &resonator.Loop{}
	cases = append(cases, struct {
		name string
		ctx  context.Context
		op   operation.Operation
		code string
		msg  string
	}{"LOOP", ctx, loopOp, "bad_request", "LOOP"})

	for _, c := range cases {
		p, err := pu.ExecWorkspace(c.ctx, c.op)
		if err != nil {
			t.Fatalf("%s: Go error %v (want in-band)", c.name, err)
		}
		if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != c.code {
			t.Errorf("%s: code = %q, want %q (%s)", c.name, got, c.code, p.Raw)
		}
		if got := gjson.Get(p.Raw, "workspace.error.message").String(); !strings.Contains(got, c.msg) {
			t.Errorf("%s: message = %q, want it to contain %q", c.name, got, c.msg)
		}
	}
	if d.dials != 0 {
		t.Errorf("a refused connect dialed %d times", d.dials)
	}
	if _, bound := pu.Attachments.Lookup("acme", "s1"); bound {
		t.Error("a refused connect left a binding")
	}

	// No registry on this node → unsupported.
	pu.Attachments = nil
	p, _ := pu.ExecWorkspace(ctx, connectOp(`{"service":"echo"}`))
	if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != "unsupported" {
		t.Errorf("no registry: code = %q", got)
	}
}

func TestExecWorkspaceConnectProviderFailures(t *testing.T) {
	// A provider with no Dialer answers unsupported, in-band.
	pu, ctx := connectUnit(t, &stubProvider{})
	p, _ := pu.ExecWorkspace(ctx, connectOp(`{"service":"echo"}`))
	if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != "unsupported" {
		t.Fatalf("no Dialer: %s", p.Raw)
	}
	if _, bound := pu.Attachments.Lookup("acme", "s1"); bound {
		t.Error("a failed connect left a binding")
	}

	// A dial that fails keeps its code and leaves nothing bound.
	d := &dialableStub{stubProvider: &stubProvider{}, dialErr: &workspace.Error{Code: "unavailable", Message: "nothing listening on 17777"}}
	pu, ctx = connectUnit(t, d)
	p, _ = pu.ExecWorkspace(ctx, connectOp(`{"service":"echo"}`))
	if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != "unavailable" {
		t.Fatalf("dial failure: %s", p.Raw)
	}
	if _, bound := pu.Attachments.Lookup("acme", "s1"); bound {
		t.Error("a failed dial left a binding")
	}
}
