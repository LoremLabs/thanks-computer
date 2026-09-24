package processor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

const outletTestDSN = "fake://db.example.net:5432/crm?password=s3cr3t-hunter2"

// outletTestDecls declares `crm` (write) and `ro` (read) for tenant acme,
// stack site — and nothing for anyone else.
type outletTestDecls struct{}

func (outletTestDecls) Lookup(_ context.Context, tenant, stack, name string) (*outlet.Decl, string, error) {
	if tenant != "acme" || stack != "site" {
		return nil, "", outlet.ErrNotDeclared
	}
	switch name {
	case "crm":
		return &outlet.Decl{Driver: "fake", Secret: "CRM_DSN", Access: outlet.AccessWrite}, "h1", nil
	case "ro":
		return &outlet.Decl{Driver: "fake", Secret: "CRM_DSN", Access: outlet.AccessRead}, "h1", nil
	}
	return nil, "", outlet.ErrNotDeclared
}

type outletTestSecrets struct{ dsn string }

func (s *outletTestSecrets) MaterializeForOpSlug(_ context.Context, tenant, stack, name string) ([]byte, *secrets.SecretMetadata, error) {
	if s.dsn == "" || name != "CRM_DSN" {
		return nil, nil, secrets.ErrSecretNotFound
	}
	return []byte(s.dsn), &secrets.SecretMetadata{SecretID: "sid", VersionNo: 1}, nil
}

func newOutletUnit(t *testing.T, guard egress.Guard) (*Unit, *outlettest.Driver, *outletTestSecrets) {
	t.Helper()
	pu, _ := newTestUnit(t)
	drv := &outlettest.Driver{Columns: []string{"id", "name"}, Rows: [][]any{{1, "Alice"}}, RowsAffected: 1}
	sec := &outletTestSecrets{dsn: outletTestDSN}
	pu.Outlets = outlet.NewRuntime(outlet.Deps{
		Lookup:  func(string) (outlet.Driver, bool) { return drv, true },
		Guard:   guard,
		Secrets: sec,
		Decls:   outletTestDecls{},
		Limits:  outlet.Limits{MaxRows: 100, MaxBytes: 1 << 20, PoolMaxConns: 2},
	})
	t.Cleanup(pu.Outlets.Close)
	return pu, drv, sec
}

func outletOp(exec, meta string) operation.Operation {
	return operation.Operation{
		Stack: "site", Scope: 100, Name: "o",
		Resonator: &resonator.Resonator{Exec: exec},
		Input:     `{}`,
		Meta:      meta,
	}
}

func outletCtx(rec *recordingTracer) context.Context {
	ctx := WithTenant(context.Background(), "acme")
	if rec != nil {
		ctx = trace.WithContext(ctx, rec)
	}
	return ctx
}

func TestExecOutletQueryRows(t *testing.T) {
	pu, drv, _ := newOutletUnit(t, nil)
	rec := &recordingTracer{}
	pl, err := pu.ExecOutlet(outletCtx(rec), outletOp("outlet://crm/query", `{"sql":"SELECT id, name FROM customers WHERE email = $1","args":["a@example.com"],"into":"_crm"}`))
	if err != nil {
		t.Fatalf("outlet failures must be data, got Go error: %v", err)
	}
	if !gjson.Get(pl.Raw, "_crm.ok").Bool() {
		t.Fatalf("ok: %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "_crm.rows.0.name").String(); got != "Alice" {
		t.Fatalf("rows: %s", pl.Raw)
	}
	if gjson.Get(pl.Raw, "_crm.count").Int() != 1 || gjson.Get(pl.Raw, "_crm.columns.1").String() != "name" || gjson.Get(pl.Raw, "_crm.outlet").String() != "crm" {
		t.Fatalf("shape: %s", pl.Raw)
	}
	if gjson.Get(pl.Raw, "_crm.rows_affected").Exists() {
		t.Fatalf("a query never reports rows_affected: %s", pl.Raw)
	}
	if drv.Queries.Load() != 1 || drv.LastDSN() != outletTestDSN {
		t.Fatalf("driver saw queries=%d dsn=%q", drv.Queries.Load(), drv.LastDSN())
	}
	if len(rec.events) != 1 || rec.events[0].Event != "outlet.completion" {
		t.Fatalf("events: %+v", rec.events)
	}
	f := rec.events[0].Fields
	if f["outlet"] != "crm" || f["driver"] != "fake" || f["operation"] != "query" || f["rows"] != 1 || len(f["statement_sha256"].(string)) != 16 {
		t.Fatalf("event fields: %+v", f)
	}
	for _, leak := range []string{"hunter2", "db.example.net", "SELECT id"} {
		if strings.Contains(pl.Raw, leak) || strings.Contains(pl.Meta, leak) && leak != "SELECT id" {
			t.Fatalf("%q leaked into the payload: %s", leak, pl.Raw)
		}
		for k, v := range f {
			if s, ok := v.(string); ok && strings.Contains(s, leak) {
				t.Fatalf("%q leaked into trace field %s", leak, k)
			}
		}
	}
}

func TestExecOutletExecShapes(t *testing.T) {
	pu, drv, _ := newOutletUnit(t, nil)
	pl, _ := pu.ExecOutlet(outletCtx(nil), outletOp("outlet://crm/exec", `{"sql":"UPDATE customers SET plan = $1 WHERE id = $2","args":["team",1]}`))
	if !gjson.Get(pl.Raw, "_outlet.ok").Bool() || gjson.Get(pl.Raw, "_outlet.rows_affected").Int() != 1 || gjson.Get(pl.Raw, "_outlet.columns").Exists() {
		t.Fatalf("exec without RETURNING: %s", pl.Raw)
	}
	drv.ExecReturns = true
	pl, _ = pu.ExecOutlet(outletCtx(nil), outletOp("outlet://crm/exec", `{"sql":"UPDATE customers SET plan = $1 WHERE id = $2 RETURNING id, name","args":["team",1]}`))
	if gjson.Get(pl.Raw, "_outlet.rows_affected").Int() != 1 || gjson.Get(pl.Raw, "_outlet.rows.0.name").String() != "Alice" || gjson.Get(pl.Raw, "_outlet.count").Int() != 1 {
		t.Fatalf("exec with RETURNING: %s", pl.Raw)
	}
	if drv.Execs.Load() != 2 || drv.Queries.Load() != 0 {
		t.Fatalf("driver calls: execs=%d queries=%d", drv.Execs.Load(), drv.Queries.Load())
	}
}

func TestExecOutletRefusalsAreData(t *testing.T) {
	pu, drv, sec := newOutletUnit(t, nil)
	code := func(exec, meta string) string {
		t.Helper()
		pl, err := pu.ExecOutlet(outletCtx(nil), outletOp(exec, meta))
		if err != nil {
			t.Fatalf("%s: Go error %v", exec, err)
		}
		into := "_outlet"
		if m := gjson.Get(meta, "into"); m.Exists() && !strings.HasPrefix(m.String(), "_txc") {
			into = m.String()
		}
		if gjson.Get(pl.Raw, into+".ok").Bool() {
			t.Fatalf("%s %s: expected a failure, got %s", exec, meta, pl.Raw)
		}
		return gjson.Get(pl.Raw, into+".error.code").String()
	}
	if c := code("outlet://crm/query", `{"sql":"SELECT 1","into":"_txc.tenant"}`); c != outlet.CodeInvalidRequest {
		t.Fatalf("reserved into: %s", c)
	}
	if c := code("outlet://crm:5432/query", `{"sql":"SELECT 1"}`); c != outlet.CodeInvalidRequest {
		t.Fatalf("port in target: %s", c)
	}
	if c := code("outlet://crm/query", `{}`); c != outlet.CodeInvalidRequest {
		t.Fatalf("no sql: %s", c)
	}
	if c := code("outlet://crm/query", `{"sql":"SELECT 1","args":{"a":1}}`); c != outlet.CodeInvalidRequest {
		t.Fatalf("bad args: %s", c)
	}
	if c := code("outlet://nope/query", `{"sql":"SELECT 1"}`); c != outlet.CodeNotDeclared {
		t.Fatalf("undeclared: %s", c)
	}
	if c := code("outlet://ro/exec", `{"sql":"SELECT 1"}`); c != outlet.CodeInvalidRequest {
		t.Fatalf("exec on read: %s", c)
	}
	if drv.Opens.Load() != 0 {
		t.Fatal("no refusal above should have opened a pool")
	}
	sec.dsn = ""
	if c := code("outlet://crm/query", `{"sql":"SELECT 1"}`); c != outlet.CodeMissingSecret {
		t.Fatalf("missing secret: %s", c)
	}
	sec.dsn = outletTestDSN

	// The outlet belongs to the dispatching op's stack. Another stack
	// doesn't see it; the app's own inlet sub-stack does.
	op := outletOp("outlet://crm/query", `{"sql":"SELECT 1"}`)
	op.Stack = "other"
	pl, _ := pu.ExecOutlet(outletCtx(nil), op)
	if gjson.Get(pl.Raw, "_outlet.error.code").String() != outlet.CodeNotDeclared {
		t.Fatalf("other stack: %s", pl.Raw)
	}
	op.Stack = "site/_mail"
	pl, _ = pu.ExecOutlet(outletCtx(nil), op)
	if !gjson.Get(pl.Raw, "_outlet.ok").Bool() {
		t.Fatalf("inlet sub-stack shares the app's outlets: %s", pl.Raw)
	}

	pu.Outlets = nil
	if c := code("outlet://crm/query", `{"sql":"SELECT 1"}`); c != outlet.CodeUnavailable {
		t.Fatalf("no runtime: %s", c)
	}
}

func TestExecOutletPrivateDSNRefused(t *testing.T) {
	guard, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pu, _, sec := newOutletUnit(t, guard)
	sec.dsn = "fake://10.0.0.5:5432/crm?password=hunter2"
	rec := &recordingTracer{}
	pl, _ := pu.ExecOutlet(outletCtx(rec), outletOp("outlet://crm/query", `{"sql":"SELECT 1"}`))
	if gjson.Get(pl.Raw, "_outlet.error.code").String() != outlet.CodeConnectFailed {
		t.Fatalf("want connect_failed: %s", pl.Raw)
	}
	if strings.Contains(pl.Raw, "10.0.0.5") || strings.Contains(pl.Raw, "hunter2") {
		t.Fatalf("DSN leaked: %s", pl.Raw)
	}
	if rec.events[0].Fields["error_code"] != outlet.CodeConnectFailed {
		t.Fatalf("event: %+v", rec.events[0].Fields)
	}
}

// TestExecOutletEndToEnd runs two scopes through Run: 100 queries the
// outlet with a bound envelope value and 200 branches on `<into>.ok`.
func TestExecOutletEndToEnd(t *testing.T) {
	pu, drv, _ := newOutletUnit(t, nil)
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_acme', 'acme', '2026-05-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	lookup := `WHEN .email != "" ` +
		`EXEC "outlet://crm/query" ` +
		`WITH sql = "SELECT id, name FROM customers WHERE email = $1", args = &array(.email), into = "_crm"`
	branch := `WHEN ._crm.ok == true EMIT .greeting = ._crm.rows.0.name`
	for _, o := range []struct {
		scope int
		name  string
		txcl  string
	}{{100, "lookup", lookup}, {200, "branch", branch}} {
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', 'tnt_acme')`,
			"site", o.scope, o.name, o.txcl); err != nil {
			t.Fatal(err)
		}
	}
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"email":"a@example.com","_txc":{"tenant":"acme"}}`, "site/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var pl event.Payload
	select {
	case pl = <-resCh:
	default:
		t.Fatal("no response")
	}
	if got := gjson.Get(pl.Raw, "greeting").String(); got != "Alice" {
		t.Fatalf("scope 200 should have seen the row: %s", pl.Raw)
	}
	if drv.Queries.Load() != 1 {
		t.Fatalf("queries = %d", drv.Queries.Load())
	}
	if strings.Contains(pl.Raw, "hunter2") {
		t.Fatalf("DSN leaked: %s", pl.Raw)
	}
}

// TestExecOutletRotationSwitchesPool drives the real secrets Resolver: a
// rotation changes the secret version, so the next call opens a new pool
// and the old one closes without a restart.
func TestExecOutletRotationSwitchesPool(t *testing.T) {
	pu, drv, _ := newOutletUnit(t, nil)
	if _, err := pu.Dbc.Db.Exec(secretStoreSchema); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := secrets.MintFileMasterKey(keyPath); err != nil {
		t.Fatal(err)
	}
	mk, err := secrets.NewFileMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store := secrets.NewStore(pu.Dbc.Db, mk)
	slugToID := func(ctx context.Context, slug string) (string, error) {
		var id string
		return id, pu.Dbc.Db.QueryRowContext(ctx, `SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug).Scan(&id)
	}
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_acme', 'acme', '2026-05-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.CreateSecret(ctx, "tnt_acme", nil, "CRM_DSN", "", "test", []byte("fake://db1.example.net:5432/crm?password=one")); err != nil {
		t.Fatal(err)
	}
	pu.Outlets = outlet.NewRuntime(outlet.Deps{
		Lookup:  func(string) (outlet.Driver, bool) { return drv, true },
		Secrets: secrets.NewResolver(store, slugToID), // the production type satisfies SecretSource
		Decls:   outletTestDecls{},
		Limits:  outlet.Limits{MaxRows: 100, MaxBytes: 1 << 20},
	})
	t.Cleanup(pu.Outlets.Close)

	call := func() string {
		pl, _ := pu.ExecOutlet(outletCtx(nil), outletOp("outlet://crm/query", `{"sql":"SELECT 1"}`))
		if !gjson.Get(pl.Raw, "_outlet.ok").Bool() {
			t.Fatalf("call failed: %s", pl.Raw)
		}
		return drv.LastDSN()
	}
	if dsn := call(); !strings.HasSuffix(dsn, "password=one") {
		t.Fatalf("first DSN: %q", dsn)
	}
	call()
	if drv.Opens.Load() != 1 {
		t.Fatalf("same version must reuse the pool: opens=%d", drv.Opens.Load())
	}
	if _, err := store.RotateSecret(ctx, "tnt_acme", nil, "CRM_DSN", []byte("fake://db2.example.net:5432/crm?password=two")); err != nil {
		t.Fatal(err)
	}
	if dsn := call(); !strings.HasSuffix(dsn, "password=two") {
		t.Fatalf("rotated DSN: %q", dsn)
	}
	if drv.Opens.Load() != 2 || drv.Closes.Load() != 1 || pu.Outlets.OpenPools() != 1 {
		t.Fatalf("rotation: opens=%d closes=%d pools=%d", drv.Opens.Load(), drv.Closes.Load(), pu.Outlets.OpenPools())
	}
}
