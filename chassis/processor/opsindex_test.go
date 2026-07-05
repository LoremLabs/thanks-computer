package processor

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/operation"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
)

// newFileBackedUnit builds a Unit whose opstack lives in a FILE-backed
// SQLite, so the test can open a SECOND handle onto identical data.
// Pinning that second handle in ctx makes currentOpsIndex miss
// (pointer != Dbc.Snapshot()) and drives the original SQL path — the
// parity oracle. The DB schema mirrors newTestUnit's.
func newFileBackedUnit(t testing.TB) (*Unit, *sql.DB) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "opstack.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatalf("sqlite open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	db := open()
	if _, err := db.Exec(`CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, tenant_id TEXT, UNIQUE(stack, scope, txcl, tenant_id));
		CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT, revoked_at TEXT);`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	pu := &Unit{
		Conf:   config.Config{Environment: "test", OpTimeout: "1s", OpTimeoutMax: "10m"},
		Logger: zap.NewNop(),
		Dbc:    &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()},
		Mc:     &metrics.Metrics{Tracer: tp.Tracer("test")},
	}
	return pu, open() // second, independent handle onto the same rows
}

// seedOp inserts one rule row. tenantID is `any` so tests can seed the
// untenanted bucket with a typed nil (`(*string)(nil)`).
func seedIdxOp(t testing.TB, db *sql.DB, tenantID any, stack string, scope int, name, txcl string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, ?, '', '')`,
		tenantID, stack, scope, name, txcl); err != nil {
		t.Fatalf("seed op (%v,%s,%d,%s): %v", tenantID, stack, scope, name, err)
	}
}

func seedIdxTenant(t testing.TB, db *sql.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, '')`, id, slug, slug); err != nil {
		t.Fatalf("seed tenant %s: %v", slug, err)
	}
}

// opKey reduces an Operation to its lookup-relevant identity (OpID is
// intentionally fresh per call on both paths, Resonator presence differs
// by design — the index pre-parses).
func opKeys(ops []operation.Operation) []string {
	keys := make([]string, 0, len(ops))
	for _, op := range ops {
		keys = append(keys, op.Stack+"|"+strconv.Itoa(op.Scope)+"|"+op.Name+"|"+op.Txcl+"|"+op.MockRes)
	}
	return keys
}

// TestOpsIndexParityWithSQLPath probes every interesting (stack, scope)
// through both the index path (Unit's own snapshot) and the SQL path
// (ctx pinned to a second handle over the same file) and requires
// identical results: floor semantics, parent-prefix peeling, tenant
// isolation, end-of-stack, untenanted bucket.
func TestOpsIndexParityWithSQLPath(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)

	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxTenant(t, pu.Dbc.Db, "t-b", "bravo")

	// alpha: sparse ladder incl. multi-op scope + child stack that
	// inherits from its parent prefix.
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 1000, "session", `WHEN .never == "x" EXEC "txco://noop"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 1000, "intake", `WHEN .also == "y" EXEC "txco://noop"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 2200, "lib", `EXEC "txco://noop"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 900000, "spa-404", `EMIT .ok = true`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "website", 100, "parent", `EXEC "txco://noop"`)
	// bravo owns a same-named stack — isolation must hold on both paths.
	seedIdxOp(t, pu.Dbc.Db, "t-b", "www", 500, "bravo-own", `EXEC "txco://noop"`)
	// untenanted bucket (tenant_id IS NULL).
	seedIdxOp(t, pu.Dbc.Db, (*string)(nil), "legacy", 0, "old", `EXEC "txco://noop"`)

	probes := []struct {
		tenant, stage string
	}{
		{"alpha", "www/0"},            // floor → 1000, two ops
		{"alpha", "www/1000"},         // exact
		{"alpha", "www/1001"},         // floor → 2200
		{"alpha", "www/2201"},         // floor → 900000
		{"alpha", "www/900001"},       // past the end → empty
		{"alpha", "website/canary/0"}, // parent-prefix peel → website/100
		{"alpha", "nosuch/0"},         // nothing anywhere → empty
		{"bravo", "www/0"},            // bravo sees only its own www
		{"alpha", "legacy/0"},         // tenant set, untenanted rows invisible
		{"", "legacy/0"},              // empty tenant → IS NULL bucket
	}

	for _, p := range probes {
		t.Run(p.tenant+"_"+p.stage, func(t *testing.T) {
			idxCtx := WithTenant(context.Background(), p.tenant)
			gotIdx, err := pu.OpsForStage(idxCtx, p.stage)
			if err != nil {
				t.Fatalf("index path: %v", err)
			}
			// Sanity: the index path really ran (current snapshot, no wildcard).
			if pu.opsIdx.Load() == nil {
				t.Fatal("index was never built — test is not exercising the index path")
			}

			sqlCtx := context.WithValue(WithTenant(context.Background(), p.tenant), ctxKeyOpstackSnap, sqlHandle)
			gotSQL, err := pu.OpsForStage(sqlCtx, p.stage)
			if err != nil {
				t.Fatalf("sql path: %v", err)
			}

			if !reflect.DeepEqual(opKeys(gotIdx), opKeys(gotSQL)) {
				t.Fatalf("parity mismatch for %s %s:\n index: %v\n sql:   %v",
					p.tenant, p.stage, opKeys(gotIdx), opKeys(gotSQL))
			}
			// Index path must come pre-parsed (that's the point).
			for _, op := range gotIdx {
				if op.Resonator == nil {
					t.Errorf("index path returned unparsed op %s/%d %s", op.Stack, op.Scope, op.Name)
				}
			}
		})
	}
}

// TestOpsIndexPerRequestIsolation hammers one cached ladder from many
// goroutines, mutating every per-request field on the returned copies,
// and asserts no bleed between requests and fresh OpIDs each call.
// Run with -race: it is also the shared-Resonator safety check.
func TestOpsIndexPerRequestIsolation(t *testing.T) {
	pu, _ := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 100, "one", `WHEN .go == "yes" EXEC "txco://noop"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 100, "two", `EMIT .hello = "world"`)

	ctx := WithTenant(context.Background(), "alpha")

	var wg sync.WaitGroup
	seen := make(chan string, 64*2)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				ops, err := pu.OpsForStage(ctx, "www/0")
				if err != nil {
					t.Errorf("OpsForStage: %v", err)
					return
				}
				if len(ops) != 2 {
					t.Errorf("got %d ops, want 2", len(ops))
					return
				}
				for j := range ops {
					if ops[j].Input != "" || ops[j].Output != "" || ops[j].Meta != "" {
						t.Errorf("per-request state leaked into fresh copy: %+v", ops[j])
						return
					}
					// Concurrent WHEN evaluation over the SHARED parsed
					// resonator (the -race target).
					ops[j].Resonator.WhenMatches(fmt.Sprintf(`{"go":"yes","g":%d,"i":%d}`, g, i))
					// Dirty every per-request field on our copy.
					ops[j].Input = fmt.Sprintf(`{"g":%d,"i":%d}`, g, i)
					ops[j].Output = "out"
					ops[j].Meta = "meta"
					seen <- ops[j].OpID
				}
			}
		}(g)
	}
	wg.Wait()
	close(seen)

	ids := map[string]bool{}
	for id := range seen {
		if id == "" {
			t.Fatal("empty OpID on index-path op")
		}
		if ids[id] {
			t.Fatalf("OpID %s returned twice — copies are being shared across requests", id)
		}
		ids[id] = true
	}
}

// TestOpsIndexInvalidation covers the three freshness cases: reload
// swap (new handle), in-place write + BumpGen, and a request pinned to
// a superseded handle falling back to SQL against ITS OWN data.
func TestOpsIndexInvalidation(t *testing.T) {
	pu, _ := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 100, "v1", `EMIT .v = "1"`)

	ctx := WithTenant(context.Background(), "alpha")
	ops, err := pu.OpsForStage(ctx, "www/0")
	if err != nil || len(ops) != 1 || ops[0].Name != "v1" {
		t.Fatalf("warmup: ops=%v err=%v", opKeys(ops), err)
	}
	oldHandle := pu.Dbc.Db

	t.Run("in-place write + BumpGen rebuilds", func(t *testing.T) {
		seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 100, "v2", `EMIT .v = "2"`)
		pu.Dbc.BumpGen()
		ops, err := pu.OpsForStage(ctx, "www/0")
		if err != nil {
			t.Fatalf("OpsForStage: %v", err)
		}
		if len(ops) != 2 {
			t.Fatalf("after BumpGen got %d ops, want 2 (stale index served)", len(ops))
		}
	})

	t.Run("reload-style handle swap rebuilds", func(t *testing.T) {
		// Simulate Reload: fresh handle (here: same file, new *sql.DB)
		// with an extra row, swapped into Dbc.Db.
		db2, err := sql.Open("sqlite3", dbFile(t, pu))
		if err != nil {
			t.Fatalf("open second handle: %v", err)
		}
		t.Cleanup(func() { db2.Close() })
		if _, err := db2.Exec(`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES ('t-a','www',100,'v3','EMIT .v = "3"','','')`); err != nil {
			t.Fatalf("seed v3: %v", err)
		}
		pu.Dbc.Mu.Lock()
		pu.Dbc.Db = db2
		pu.Dbc.Mu.Unlock()

		ops, err := pu.OpsForStage(ctx, "www/0")
		if err != nil {
			t.Fatalf("OpsForStage: %v", err)
		}
		if len(ops) != 3 {
			t.Fatalf("after swap got %d ops, want 3 (index pinned to old handle)", len(ops))
		}
	})

	t.Run("request pinned to superseded handle uses SQL on it", func(t *testing.T) {
		// A request that captured the old snapshot mid-reload must
		// keep answering from THAT handle (the continuation-resume
		// guarantee) — via the SQL path, not the (newer) index.
		pinned := context.WithValue(ctx, ctxKeyOpstackSnap, oldHandle)
		ops, err := pu.OpsForStage(pinned, "www/0")
		if err != nil {
			t.Fatalf("OpsForStage: %v", err)
		}
		// The old handle sees the file as it is NOW (shared file in
		// this simulation) — the point is it resolved without error
		// through SQL and not through an index keyed to db2.
		if len(ops) == 0 {
			t.Fatal("pinned-handle request resolved nothing")
		}
		if idx := pu.opsIdx.Load(); idx != nil && idx.snap == oldHandle {
			t.Fatal("index was rebuilt for a superseded handle")
		}
	})
}

// dbFile recovers the temp DB path for the swap simulation.
func dbFile(t testing.TB, pu *Unit) string {
	t.Helper()
	var name string
	if err := pu.Dbc.Db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&name); err != nil {
		t.Fatalf("pragma_database_list: %v", err)
	}
	return name
}

// --- attribution + regression benchmarks -------------------------------
//
// benchSeed approximates the prod shape that triggered the incident: a
// ~96-scope stack (driplit www) inside a ~3200-row multi-tenant ops
// table, walked floor-by-floor to the 900000 fallback exactly as Run's
// natural advancement does (current lookup + getnext look-ahead per
// scope). BenchmarkScopeWalk/sql-c3 is the "3 req/s" case; /indexed-c3
// is the fix.

func benchSeed(b *testing.B, db *sql.DB) (scopes []int) {
	seed := func(tenantID, stack string, scope int, name, txcl string) {
		b.Helper()
		if _, err := db.Exec(`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, ?, '', '')`,
			tenantID, stack, scope, name, txcl); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('t-a','alpha','alpha','')`); err != nil {
		b.Fatalf("tenant: %v", err)
	}
	// The walked stack: 96 scopes, ~1.5 ops each, WHENs that never match.
	when := `WHEN @web.req.url.path == "/never/matches" EXEC "txco://noop"`
	for i := 0; i < 96; i++ {
		scope := 1000 + i*100
		scopes = append(scopes, scope)
		seed("t-a", "www", scope, fmt.Sprintf("op%da", i), when)
		if i%2 == 0 {
			seed("t-a", "www", scope, fmt.Sprintf("op%db", i), when)
		}
	}
	scopes = append(scopes, 900000)
	seed("t-a", "www", 900000, "spa-404", `EMIT @halt = true`)
	// Table ballast: other stacks/tenants the LIKE scan must wade through.
	for i := 0; i < 3000; i++ {
		seed("t-a", fmt.Sprintf("ballast%d", i%30), i, fmt.Sprintf("b%d", i), when)
	}
	return scopes
}

// walkOnce mimics Run's advancement queries for one 404: for each
// populated scope, the OpsForStage floor lookup plus the getnext
// look-ahead.
func walkOnce(b *testing.B, pu *Unit, ctx context.Context, scopes []int) {
	b.Helper()
	floor := 0
	for _, s := range scopes {
		ops, err := pu.OpsForStage(ctx, "www/"+strconv.Itoa(floor))
		if err != nil {
			b.Fatalf("OpsForStage: %v", err)
		}
		if len(ops) == 0 || ops[0].Scope != s {
			b.Fatalf("walk landed at %v, want scope %d", opKeys(ops), s)
		}
		if _, err := pu.OpsForStage(ctx, "www/"+strconv.Itoa(s+1)); err != nil { // getnext
			b.Fatalf("getnext: %v", err)
		}
		floor = s + 1
	}
}

func BenchmarkScopeWalk(b *testing.B) {
	for _, mode := range []string{"indexed", "sql"} {
		for _, conc := range []int{1, 3} {
			b.Run(fmt.Sprintf("%s-c%d", mode, conc), func(b *testing.B) {
				db, err := sql.Open("sqlite3", ":memory:")
				if err != nil {
					b.Fatalf("open: %v", err)
				}
				defer db.Close()
				db.SetMaxOpenConns(1) // the dbcache pin — the contended resource
				if _, err := db.Exec(`CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, tenant_id TEXT);
					CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT, revoked_at TEXT);`); err != nil {
					b.Fatalf("schema: %v", err)
				}
				scopes := benchSeed(b, db)

				sr := tracetest.NewSpanRecorder()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
				pu := &Unit{
					Conf:   config.Config{Environment: "test", OpTimeout: "1s", OpTimeoutMax: "10m"},
					Logger: zap.NewNop(),
					Dbc:    &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()},
					Mc:     &metrics.Metrics{Tracer: tp.Tracer("bench")},
				}
				ctx := WithTenant(context.Background(), "alpha")
				if mode == "sql" {
					// Pin a snapshot pointer mismatch so every lookup
					// takes the pre-index SQL path.
					other := pu.Dbc.Db
					pu.Dbc = &dbcache.DbCache{Db: nil, Source: other, Logger: zap.NewNop()}
					ctx = context.WithValue(ctx, ctxKeyOpstackSnap, other)
				}

				b.ResetTimer()
				if conc == 1 {
					for i := 0; i < b.N; i++ {
						walkOnce(b, pu, ctx, scopes)
					}
				} else {
					var wg sync.WaitGroup
					per := b.N / conc
					if per == 0 {
						per = 1
					}
					for g := 0; g < conc; g++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							for i := 0; i < per; i++ {
								walkOnce(b, pu, ctx, scopes)
							}
						}()
					}
					wg.Wait()
				}
			})
		}
	}
}
