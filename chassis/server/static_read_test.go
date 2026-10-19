package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// scriptedStore answers each Get with the next step of its script: bytes,
// an error, or a hang until the caller's context ends.
type scriptedStore struct {
	mu    sync.Mutex
	steps []func(ctx context.Context) ([]byte, error)
	calls int
}

func (s *scriptedStore) Get(ctx context.Context, _ string) ([]byte, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	return s.steps[i](ctx)
}
func (s *scriptedStore) Put(context.Context, string, []byte) error    { return nil }
func (s *scriptedStore) Exists(context.Context, string) (bool, error) { return true, nil }
func (s *scriptedStore) Name() string                                 { return "scripted" }
func (s *scriptedStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func serves(b string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(b), nil }
}
func fails(err error) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return nil, err }
}
func hangs(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStaticReadRetriesOnceThenServes(t *testing.T) {
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){fails(errors.New("connection reset")), serves("x")}}
	b, err := boundedReads{Store: s, budget: time.Second}.Get(context.Background(), "h")
	if err != nil || string(b) != "x" || s.count() != 2 {
		t.Fatalf("body %q err %v calls %d", b, err, s.count())
	}
}

func TestStaticReadBudgetCutsAHungRead(t *testing.T) {
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){hangs}}
	start := time.Now()
	_, err := boundedReads{Store: s, budget: 30 * time.Millisecond}.Get(context.Background(), "h")
	took := time.Since(start)
	var re *staticReadError
	if !errors.As(err, &re) || re.Attempts != 2 || !re.Timeout {
		t.Fatalf("err = %#v", err)
	}
	if s.count() != 2 || took > time.Second {
		t.Fatalf("calls %d took %s", s.count(), took)
	}
}

func TestStaticReadNotFoundIsNotRetried(t *testing.T) {
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){fails(filecas.ErrNotFound)}}
	_, err := boundedReads{Store: s, budget: time.Second}.Get(context.Background(), "h")
	if !errors.Is(err, filecas.ErrNotFound) || s.count() != 1 {
		t.Fatalf("err %v calls %d", err, s.count())
	}
}

// The op's deadline wins over the budget: no second attempt once it has
// passed, and the answer comes back while the op can still use it.
func TestStaticReadStopsAtTheOpDeadline(t *testing.T) {
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){hangs}}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := boundedReads{Store: s, budget: 10 * time.Second}.Get(ctx, "h")
	var re *staticReadError
	if !errors.As(err, &re) || re.Attempts != 1 || !re.Timeout || s.count() != 1 {
		t.Fatalf("err %#v calls %d", err, s.count())
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %s", took)
	}
}

// staticCASIndex is a static index with one tenant file, /app.js in stack web of
// tenant acme, whose bytes live in the file store under hash.
func staticCASIndex(t *testing.T) (*static.Index, string) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	content := "console.log(1)"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	for _, q := range []string{
		tenantSchemaDDL,
		`INSERT INTO tenants VALUES('tnt_a','acme','acme','t',NULL)`,
		`INSERT INTO stacks VALUES('s_a','tnt_a','web',7,'t')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES(7,'FILES/app.js',?,?)`, content, hash); err != nil {
		t.Fatal(err)
	}
	ix := static.NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}
	return ix, hash
}

// A file that is there but whose bytes the store did not give back is a
// 503 nothing keeps, never the app's 404, and the trace says so.
func TestStaticResultBodyStoreFailureIs503(t *testing.T) {
	ix, hash := staticCASIndex(t)
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){hangs}}
	tr := &captureTracer{}
	ctx := trace.WithContext(context.Background(), tr)
	start := time.Now()
	env := staticResultBody(ctx, ix, boundedReads{Store: s, budget: 20 * time.Millisecond}, reqInTenant(t, "/app.js", "web", "acme", ""))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %s", took)
	}
	want := map[string]string{
		"_txc.web.res.status":                  "503",
		"_txc.web.res.headers.cache-control.0": "no-store",
		"_txc.web.res.headers.retry-after.0":   "1",
		"_txc.web.res.headers.content-type.0":  "text/plain; charset=utf-8",
		"_txc.halt":                            "true",
	}
	for path, v := range want {
		if got := gjson.Get(env, path).String(); got != v {
			t.Fatalf("%s = %q, want %q: %s", path, got, v, env)
		}
	}
	if body(t, env) != "503 the file could not be read in time; try again\n" {
		t.Fatalf("body %q", body(t, env))
	}
	ev := tr.last()
	if ev.Event != "static.unavailable" || ev.Fields["stack"] != "web" || ev.Fields["hash"] != hash[:16] ||
		ev.Fields["attempts"] != 2 || ev.Fields["timeout"] != true {
		t.Fatalf("event = %s %v", ev.Event, ev.Fields)
	}

	// A plain store error, no timeout: the same answer.
	s = &scriptedStore{steps: []func(context.Context) ([]byte, error){fails(errors.New("bucket unreachable"))}}
	env = staticResultBody(context.Background(), ix, boundedReads{Store: s, budget: time.Second}, reqInTenant(t, "/app.js", "web", "acme", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 503 || s.count() != 2 {
		t.Fatalf("calls %d env %s", s.count(), env)
	}
}

// One slow attempt and a good second one: the file, as if nothing happened.
func TestStaticResultBodyServesOnRetry(t *testing.T) {
	ix, _ := staticCASIndex(t)
	s := &scriptedStore{steps: []func(context.Context) ([]byte, error){hangs, serves("console.log(1)")}}
	env := staticResultBody(context.Background(), ix, boundedReads{Store: s, budget: 20 * time.Millisecond}, reqInTenant(t, "/app.js", "web", "acme", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 200 || body(t, env) != "console.log(1)" || s.count() != 2 {
		t.Fatalf("calls %d env %s", s.count(), env)
	}
}
