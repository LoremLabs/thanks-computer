package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// fakeConnections records what the https op asks and answers what the test
// says.
type fakeConnections struct {
	last   atomic.Value // connection.UseInput
	result connection.UseResult
	err    *connection.Error
	client *http.Client
	used   atomic.Int64
}

func (f *fakeConnections) Use(_ context.Context, in connection.UseInput) (connection.UseResult, *connection.Error) {
	f.last.Store(in)
	if f.err != nil {
		return connection.UseResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeConnections) HTTPClient(*url.URL) *http.Client {
	f.used.Add(1)
	return f.client
}

func TestExecHTTPConnection(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	fake := &fakeConnections{result: connection.UseResult{Header: "authorization", Value: "Bearer tok", Token: "tok"}, client: &http.Client{Timeout: 2 * time.Second}}
	pu.Connections = fake
	ctx := WithCapAuthority(WithTenant(context.Background(), "acme"), "service:pony-a", "api.github.read")
	op := operation.Operation{
		Stack: "tools-api/canary", Input: `{}`,
		Meta:      `{"method":"GET","into":"_ares","secrets":{"headers":{"authorization":{"connection":"github"}}}}`,
		Resonator: &resonator.Resonator{Exec: srv.URL + "/repos"},
	}
	payload, err := pu.ExecHTTP(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth.Load().(string) != "Bearer tok" || !gjson.Get(payload.Raw, "_ares.ok").Bool() || fake.used.Load() != 1 {
		t.Errorf("payload %s auth %q client used %d", payload.Raw, gotAuth.Load(), fake.used.Load())
	}
	in := fake.last.Load().(connection.UseInput)
	if in.Tenant != "acme" || in.Stack != "tools-api/canary" || in.Provider != "github" || in.Principal != "service:pony-a" || in.Capability != "api.github.read" || in.URL.Path != "/repos" {
		t.Errorf("asked %+v", in)
	}

	// The op's own format wins over the consumer's.
	gotAuth.Store("")
	op.Meta = `{"method":"GET","into":"_ares","secrets":{"headers":{"authorization":{"connection":"github","format":"token {}"}}}}`
	if _, err := pu.ExecHTTP(ctx, op); err != nil || gotAuth.Load().(string) != "token tok" {
		t.Errorf("format: %q %v", gotAuth.Load(), err)
	}

	// A refusal is data under `into`, and nothing is sent.
	gotAuth.Store("")
	fake.err = &connection.Error{Code: connection.CodeMissing, Message: "connect GitHub first"}
	payload, err = pu.ExecHTTP(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(payload.Raw, "_ares.error.code").String() != "txco_connection_missing" || gjson.Get(payload.Raw, "_ares.error.connection").String() != "github" || gotAuth.Load().(string) != "" {
		t.Errorf("refusal: %s (sent %q)", payload.Raw, gotAuth.Load())
	}
	// Without a principal pinned, the service decides (here the fake); the
	// op still passes empty pins through, never invents them.
	fake.err = nil
	if _, err := pu.ExecHTTP(WithTenant(context.Background(), "acme"), op); err != nil {
		t.Fatal(err)
	}
	if in := fake.last.Load().(connection.UseInput); in.Principal != "" || in.Capability != "" {
		t.Errorf("pins invented: %+v", in)
	}
	// A connection on a body path is refused; no node without the service.
	op.Meta = `{"method":"POST","into":"_ares","secrets":{"body":{"token":{"connection":"github"}}}}`
	payload, _ = pu.ExecHTTP(ctx, op)
	if gjson.Get(payload.Raw, "_ares.error.code").String() != "txco_connection_invalid_arg" {
		t.Errorf("body path: %s", payload.Raw)
	}
	pu.Connections = nil
	op.Meta = `{"method":"GET","secrets":{"headers":{"authorization":{"connection":"github"}}}}`
	payload, _ = pu.ExecHTTP(ctx, op)
	if gjson.Get(payload.Raw, "_connection.error.code").String() != "txco_connection_unavailable" {
		t.Errorf("no service: %s", payload.Raw)
	}
}

func TestCapAuthorityPins(t *testing.T) {
	ctx := context.Background()
	if p, c := CapAuthorityFrom(ctx); p != "" || c != "" {
		t.Error("nothing pinned")
	}
	ctx = WithCapAuthority(ctx, "service:pony-a", "api.github.read")
	if p, c := CapAuthorityFrom(ctx); p != "service:pony-a" || c != "api.github.read" {
		t.Error("pinned")
	}
	// An incomplete pin changes nothing.
	if p, _ := CapAuthorityFrom(WithCapAuthority(ctx, "", "x")); p != "service:pony-a" {
		t.Error("an empty principal must not replace the pin")
	}
}
