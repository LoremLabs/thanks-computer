package outlet_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const dsn = "fake://db.internal.example:5432/crm?password=hunter2"

type decls struct {
	mu sync.Mutex
	m  map[string]*outlet.Decl
	h  map[string]string
}

func (d *decls) Lookup(_ context.Context, tenant, stack, name string) (*outlet.Decl, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if tenant != "t1" || stack != "support" {
		return nil, "", outlet.ErrNotDeclared
	}
	dl, ok := d.m[name]
	if !ok {
		return nil, "", outlet.ErrNotDeclared
	}
	h := d.h[name]
	if h == "" {
		h = "h1"
	}
	return dl, h, nil
}

type secretsSrc struct {
	mu      sync.Mutex
	values  map[string]string
	version int
	calls   int
}

func (s *secretsSrc) MaterializeForOpSlug(_ context.Context, tenant, stack, name string) ([]byte, *secrets.SecretMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	v, ok := s.values[name]
	if !ok {
		return nil, nil, secrets.ErrSecretNotFound
	}
	return []byte(v), &secrets.SecretMetadata{SecretID: "sid-" + name, VersionNo: s.version}, nil
}

type env struct {
	rt   *Runtime
	drv  *outlettest.Driver
	dec  *decls
	sec  *secretsSrc
	logs *observer.ObservedLogs
	now  time.Time
}

type Runtime = outlet.Runtime

func newEnv(t *testing.T, guard egress.Guard) *env {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	e := &env{
		drv:  &outlettest.Driver{Columns: []string{"id", "name"}, Rows: [][]any{{1, "Alice"}, {2, "Bob"}}, RowsAffected: 1},
		dec:  &decls{m: map[string]*outlet.Decl{"crm": {Driver: "fake", Secret: "CRM_DSN", Access: outlet.AccessWrite}, "ro": {Driver: "fake", Secret: "CRM_DSN", Access: outlet.AccessRead}}, h: map[string]string{}},
		sec:  &secretsSrc{values: map[string]string{"CRM_DSN": dsn}, version: 1},
		logs: logs,
		now:  time.Unix(1_700_000_000, 0),
	}
	e.rt = outlet.NewRuntime(outlet.Deps{
		Lookup: func(name string) (outlet.Driver, bool) {
			if name == "fake" {
				return e.drv, true
			}
			return nil, false
		},
		Guard:   guard,
		Secrets: e.sec,
		Decls:   e.dec,
		Logger:  zap.New(core),
		Limits:  outlet.Limits{MaxRows: 100, MaxBytes: 1 << 20, PoolMaxConns: 2, IdleClose: time.Minute},
		Now:     func() time.Time { return e.now },
	})
	t.Cleanup(e.rt.Close)
	return e
}

func call(e *env, op string) outlet.Outcome {
	return e.rt.Call(context.Background(), outlet.Call{Tenant: "t1", Stack: "support", Outlet: "crm", Op: op, SQL: "SELECT 1"})
}

func TestCallReturnsRows(t *testing.T) {
	e := newEnv(t, nil)
	out := call(e, outlet.OpQuery)
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	if out.Driver != "fake" || out.Result.Count != 2 || string(out.Result.RowsJSON) != `[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]` {
		t.Fatalf("result: %+v %s", out.Result, out.Result.RowsJSON)
	}
	if e.drv.LastDSN() != dsn {
		t.Fatalf("driver should have seen the DSN once: %q", e.drv.LastDSN())
	}
	out = call(e, outlet.OpExec)
	if out.Err != nil || out.Result.RowsAffected != 1 || out.Result.Count != 0 || string(out.Result.RowsJSON) != "[]" {
		t.Fatalf("exec: %+v %v", out.Result, out.Err)
	}
	if e.drv.Opens.Load() != 1 || e.rt.OpenPools() != 1 {
		t.Fatalf("one pool expected: opens=%d pools=%d", e.drv.Opens.Load(), e.rt.OpenPools())
	}
}

func TestCallRefusals(t *testing.T) {
	e := newEnv(t, nil)
	if out := e.rt.Call(context.Background(), outlet.Call{Tenant: "t1", Stack: "support", Outlet: "nope", Op: "query", SQL: "SELECT 1"}); out.Err == nil || out.Err.Code != outlet.CodeNotDeclared {
		t.Fatalf("undeclared: %v", out.Err)
	}
	if out := e.rt.Call(context.Background(), outlet.Call{Tenant: "t2", Stack: "support", Outlet: "crm", Op: "query", SQL: "SELECT 1"}); out.Err == nil || out.Err.Code != outlet.CodeNotDeclared {
		t.Fatalf("other tenant: %v", out.Err)
	}
	if out := e.rt.Call(context.Background(), outlet.Call{Tenant: "t1", Stack: "support", Outlet: "ro", Op: "exec", SQL: "SELECT 1"}); out.Err == nil || out.Err.Code != outlet.CodeInvalidRequest {
		t.Fatalf("exec on read: %v", out.Err)
	}
	delete(e.sec.values, "CRM_DSN")
	if out := call(e, outlet.OpQuery); out.Err == nil || out.Err.Code != outlet.CodeMissingSecret || !strings.Contains(out.Err.Message, "CRM_DSN") {
		t.Fatalf("missing secret: %v", out.Err)
	}
	if e.drv.Opens.Load() != 0 {
		t.Fatal("nothing should have opened")
	}
	// No secret store at all is also missing_secret, said differently.
	e2 := newEnv(t, nil)
	e2.rt = outlet.NewRuntime(outlet.Deps{Lookup: func(string) (outlet.Driver, bool) { return e2.drv, true }, Decls: e2.dec})
	if out := call(e2, outlet.OpQuery); out.Err == nil || out.Err.Code != outlet.CodeMissingSecret || !strings.Contains(out.Err.Message, "no secret store") {
		t.Fatalf("no store: %v", out.Err)
	}
}

func TestSingleFlightOpen(t *testing.T) {
	e := newEnv(t, nil)
	e.drv.OpenDelay = 30 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out := call(e, outlet.OpQuery); out.Err != nil {
				t.Errorf("call: %v", out.Err)
			}
		}()
	}
	wg.Wait()
	if e.drv.Opens.Load() != 1 {
		t.Fatalf("concurrent first calls must open once, got %d", e.drv.Opens.Load())
	}
}

func TestFailedOpenNotCached(t *testing.T) {
	e := newEnv(t, nil)
	e.drv.FailOpen = outlet.NewError(outlet.CodeConnectFailed, "outlet could not be opened")
	if out := call(e, outlet.OpQuery); out.Err == nil || out.Err.Code != outlet.CodeConnectFailed {
		t.Fatalf("want connect_failed: %v", out.Err)
	}
	if e.rt.OpenPools() != 0 {
		t.Fatal("a failed open must not stay in the pool set")
	}
	e.drv.FailOpen = nil
	if out := call(e, outlet.OpQuery); out.Err != nil {
		t.Fatalf("retry: %v", out.Err)
	}
	if e.drv.Opens.Load() != 2 {
		t.Fatalf("opens = %d", e.drv.Opens.Load())
	}
	// An unclassified error from the driver is reported with a fixed message.
	e.drv.FailQuery = errors.New("pq: connection to db.internal.example failed for user app")
	out := call(e, outlet.OpQuery)
	if out.Err == nil || out.Err.Code != outlet.CodeUnavailable || strings.Contains(out.Err.Message, "internal.example") {
		t.Fatalf("unclassified: %v", out.Err)
	}
}

func TestRotationAndRedeployReplacePool(t *testing.T) {
	e := newEnv(t, nil)
	call(e, outlet.OpQuery)
	e.sec.mu.Lock()
	e.sec.version = 2
	e.sec.mu.Unlock()
	if out := call(e, outlet.OpQuery); out.Err != nil {
		t.Fatal(out.Err)
	}
	if e.drv.Opens.Load() != 2 || e.drv.Closes.Load() != 1 || e.rt.OpenPools() != 1 {
		t.Fatalf("rotation: opens=%d closes=%d pools=%d", e.drv.Opens.Load(), e.drv.Closes.Load(), e.rt.OpenPools())
	}
	e.dec.mu.Lock()
	e.dec.h["crm"] = "h2"
	e.dec.mu.Unlock()
	call(e, outlet.OpQuery)
	if e.drv.Opens.Load() != 3 || e.drv.Closes.Load() != 2 || e.rt.OpenPools() != 1 {
		t.Fatalf("redeploy: opens=%d closes=%d pools=%d", e.drv.Opens.Load(), e.drv.Closes.Load(), e.rt.OpenPools())
	}
}

func TestIdleClose(t *testing.T) {
	e := newEnv(t, nil)
	call(e, outlet.OpQuery)
	e.rt.Sweep()
	if e.rt.OpenPools() != 1 {
		t.Fatal("a fresh pool must survive a sweep")
	}
	e.now = e.now.Add(2 * time.Minute)
	e.rt.Sweep()
	if e.rt.OpenPools() != 0 || e.drv.Closes.Load() != 1 {
		t.Fatalf("idle pool should close: pools=%d closes=%d", e.rt.OpenPools(), e.drv.Closes.Load())
	}
	if out := call(e, outlet.OpQuery); out.Err != nil || e.drv.Opens.Load() != 2 {
		t.Fatalf("reopen after idle close: %v opens=%d", out.Err, e.drv.Opens.Load())
	}
}

func TestDeclTimeoutAndCeilings(t *testing.T) {
	e := newEnv(t, nil)
	e.dec.m["crm"].Timeout = 20
	e.drv.QueryDelay = 300 * time.Millisecond
	out := call(e, outlet.OpQuery)
	if out.Err == nil || out.Err.Code != outlet.CodeTimeout {
		t.Fatalf("decl timeout: %v", out.Err)
	}
	e.drv.QueryDelay = 0
	e.dec.m["crm"].Timeout = 0
	e.dec.m["crm"].MaxRows = 1
	out = call(e, outlet.OpQuery)
	if out.Err == nil || out.Err.Code != outlet.CodeResultTooLarge || out.Result != nil || out.Err.Rows != 1 {
		t.Fatalf("row ceiling: %+v %+v", out.Err, out.Result)
	}
	e.dec.m["crm"].MaxRows = 1000 // may not raise the node ceiling of 100
	e.drv.Rows = make([][]any, 101)
	for i := range e.drv.Rows {
		e.drv.Rows[i] = []any{i, "x"}
	}
	if out = call(e, outlet.OpQuery); out.Err == nil || out.Err.Code != outlet.CodeResultTooLarge {
		t.Fatalf("node ceiling: %v", out.Err)
	}
}

func TestPrivateAddressRefusedAndNothingLeaks(t *testing.T) {
	guard, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, guard)
	e.sec.values["CRM_DSN"] = "fake://10.0.0.5:5432/crm?password=hunter2"
	out := call(e, outlet.OpQuery)
	if out.Err == nil || out.Err.Code != outlet.CodeConnectFailed {
		t.Fatalf("private DSN under private policy: %v", out.Err)
	}
	for _, s := range []string{"10.0.0.5", "hunter2"} {
		if strings.Contains(out.Err.Message, s) {
			t.Fatalf("error leaks %q: %v", s, out.Err)
		}
		for _, l := range e.logs.All() {
			if strings.Contains(l.Message, s) || strings.Contains(fmt.Sprint(l.ContextMap()), s) {
				t.Fatalf("log leaks %q: %v", s, l)
			}
		}
	}
	if e.rt.OpenPools() != 0 {
		t.Fatal("a refused open must not be cached")
	}
	// A public address passes the same guard.
	e.sec.values["CRM_DSN"] = "fake://8.8.8.8:5432/crm"
	if out := call(e, outlet.OpQuery); out.Err != nil {
		t.Fatalf("public DSN: %v", out.Err)
	}
}

func TestCloseShutsEverything(t *testing.T) {
	e := newEnv(t, nil)
	call(e, outlet.OpQuery)
	e.rt.Close()
	if e.drv.Closes.Load() != 1 || e.rt.OpenPools() != 0 {
		t.Fatalf("close: closes=%d pools=%d", e.drv.Closes.Load(), e.rt.OpenPools())
	}
	if out := call(e, outlet.OpQuery); out.Err == nil || out.Err.Code != outlet.CodeUnavailable {
		t.Fatalf("after close: %v", out.Err)
	}
}

// The node's --outlet-egress applies when a declaration says nothing; a
// declaration's own egress wins either way, and the driver is handed the
// node's relays.
func TestEgressDefaultAndOverride(t *testing.T) {
	e := newEnv(t, nil)
	relays := []string{"100.64.0.9:1080"}
	e.rt = outlet.NewRuntime(outlet.Deps{
		Lookup:  func(string) (outlet.Driver, bool) { return e.drv, true },
		Secrets: e.sec, Decls: e.dec,
		Egress: outlet.EgressConfig{Default: outlet.EgressRelay, Relays: relays},
	})
	t.Cleanup(e.rt.Close)
	out := call(e, outlet.OpQuery)
	if out.Err != nil || out.Egress != outlet.EgressRelay {
		t.Fatalf("node default relay: egress=%q err=%v", out.Egress, out.Err)
	}
	if got := e.drv.LastEgress(); got.Mode != outlet.EgressRelay || len(got.Relays) != 1 || got.Relays[0] != relays[0] {
		t.Fatalf("driver handed %+v", got)
	}
	e.dec.mu.Lock()
	e.dec.m["crm"].Egress = outlet.EgressDirect
	e.dec.h["crm"] = "h-direct"
	e.dec.mu.Unlock()
	out = call(e, outlet.OpQuery)
	if out.Err != nil || out.Egress != outlet.EgressDirect || e.drv.LastEgress().Mode != outlet.EgressDirect {
		t.Fatalf("declared direct must override the node default: egress=%q driver=%+v err=%v", out.Egress, e.drv.LastEgress(), out.Err)
	}
	if e.drv.Opens.Load() != 2 {
		t.Fatalf("a different egress is a different pool: opens=%d", e.drv.Opens.Load())
	}
}
