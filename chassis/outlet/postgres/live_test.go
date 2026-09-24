package postgres

// Live tests against a real PostgreSQL. Gated on TXCO_TEST_PG_DSN; a local
// runner is
//
//	docker run --rm -d --name txco-pg -e POSTGRES_PASSWORD=pw -p 5432:5432 postgres:16
//	TXCO_TEST_PG_DSN='postgres://postgres:pw@localhost:5432/postgres?sslmode=disable' go test ./chassis/outlet/postgres/ -run Live -v

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

const liveTable = "txco_outlet_live"

type live struct {
	t    *testing.T
	dsn  string
	raw  *pgx.Conn
	conn outlet.Conn
}

func newLive(t *testing.T) *live {
	t.Helper()
	dsn := os.Getenv("TXCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TXCO_TEST_PG_DSN not set; skipping the live Postgres outlet test")
	}
	ctx := context.Background()
	raw, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close(ctx) })
	for _, s := range []string{
		`DROP TABLE IF EXISTS ` + liveTable,
		`CREATE TABLE ` + liveTable + ` (id int8 PRIMARY KEY, email text UNIQUE, name text, plan text, meta jsonb, created timestamptz)`,
		`INSERT INTO ` + liveTable + ` VALUES (1,'alice@example.com','Alice','team','{"n":9007199254740993}','2026-09-24T10:00:00Z'),
		  (2,'bob@example.com','Bob','free',NULL,'2026-09-24T11:00:00Z'), (3,'carol@example.com','Carol','free','[]','2026-09-24T12:00:00Z')`,
	} {
		if _, err := raw.Exec(ctx, s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = raw.Exec(context.Background(), `DROP TABLE IF EXISTS `+liveTable) })
	c, err := Driver{}.Open(ctx, outlet.OpenParams{DSN: []byte(dsn), Pool: outlet.PoolLimits{MaxConns: 2, IdleTimeout: 10 * time.Second}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &live{t: t, dsn: dsn, raw: raw, conn: c}
}

func (l *live) planOf(id int) string {
	var plan string
	if err := l.raw.QueryRow(context.Background(), `SELECT plan FROM `+liveTable+` WHERE id = $1`, id).Scan(&plan); err != nil {
		l.t.Fatal(err)
	}
	return plan
}

func code(err error) string {
	var oe *outlet.Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	return "<nil or unclassified: " + err.Error() + ">"
}

func TestLiveReadAndWrite(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	res, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT id, name, plan FROM ` + liveTable + ` WHERE email = $1`, Args: []any{"alice@example.com"}, MaxRows: 100, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 || strings.Join(res.Columns, ",") != "id,name,plan" || gjson.GetBytes(res.RowsJSON, "0.name").String() != "Alice" || gjson.GetBytes(res.RowsJSON, "0.id").Int() != 1 {
		t.Fatalf("read: %+v %s", res, res.RowsJSON)
	}

	res, err = l.conn.Exec(ctx, outlet.Request{SQL: `UPDATE ` + liveTable + ` SET plan = $1 WHERE id = $2`, Args: []any{"pro", int64(2)}, MaxRows: 100, MaxBytes: 1 << 20})
	if err != nil || res.RowsAffected != 1 || res.Columns != nil || string(res.RowsJSON) != "[]" {
		t.Fatalf("exec: %+v %v", res, err)
	}
	if l.planOf(2) != "pro" {
		t.Fatal("update did not commit")
	}
	res, err = l.conn.Exec(ctx, outlet.Request{SQL: `UPDATE ` + liveTable + ` SET plan = $1 WHERE id = $2 RETURNING id, plan`, Args: []any{"team", int64(2)}, MaxRows: 100, MaxBytes: 1 << 20})
	if err != nil || res.RowsAffected != 1 || res.Count != 1 || gjson.GetBytes(res.RowsJSON, "0.plan").String() != "team" {
		t.Fatalf("exec returning: %+v %v", res, err)
	}

	// No RETURNING, no rows, but a statement that matched nothing.
	res, err = l.conn.Exec(ctx, outlet.Request{SQL: `DELETE FROM ` + liveTable + ` WHERE id = 99`, MaxRows: 100, MaxBytes: 1 << 20})
	if err != nil || res.RowsAffected != 0 {
		t.Fatalf("no-op exec: %+v %v", res, err)
	}
}

func TestLiveQueryRefusesMutation(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE ` + liveTable + ` SET plan = 'x' WHERE id = 1`,
		`WITH w AS (UPDATE ` + liveTable + ` SET plan = 'y' WHERE id = 1 RETURNING id) SELECT * FROM w`,
		`WITH i AS (INSERT INTO ` + liveTable + ` (id) VALUES (50) RETURNING id) SELECT id FROM i`,
	} {
		_, err := l.conn.Query(ctx, outlet.Request{SQL: sql, MaxRows: 100, MaxBytes: 1 << 20})
		var oe *outlet.Error
		if !errors.As(err, &oe) || oe.Code != outlet.CodeQueryFailed || oe.SQLState != "25006" {
			t.Fatalf("%s: want query_failed/25006, got %v", sql, err)
		}
	}
	if l.planOf(1) != "team" {
		t.Fatal("a refused query changed a row")
	}
}

func TestLiveTypes(t *testing.T) {
	l := newLive(t)
	res, err := l.conn.Query(context.Background(), outlet.Request{SQL: `SELECT NULL::text AS n, 9007199254740993::int8 AS big, 42::int8 AS small,
		1.50::numeric AS num, '{"a":[1,2],"n":9007199254740993}'::jsonb AS j, '\x0102'::bytea AS b,
		'2026-09-24T10:00:00Z'::timestamptz AS ts, '2026-09-24'::date AS d, '2026-09-24 10:00:00'::timestamp AS naive,
		'00000000-0000-0000-0000-000000000001'::uuid AS u, ARRAY[1,NULL,3]::int4[] AS arr, 'NaN'::float8 AS nan,
		'10.1.2.3/32'::inet AS ip, '1 day 02:00:00'::interval AS iv, true AS t, 'x'::varchar AS v`, MaxRows: 10, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	row := gjson.GetBytes(res.RowsJSON, "0")
	checks := map[string]string{
		"n":     "null",
		"big":   `"9007199254740993"`,
		"small": "42",
		"num":   `"1.50"`,
		"j":     `{"a":[1,2],"n":9007199254740993}`,
		"b":     `"AQI="`,
		"ts":    `"2026-09-24T10:00:00Z"`,
		"d":     `"2026-09-24"`,
		"naive": `"2026-09-24T10:00:00"`,
		"u":     `"00000000-0000-0000-0000-000000000001"`,
		"arr":   `[1,null,3]`,
		"nan":   `"NaN"`,
		"ip":    `"10.1.2.3/32"`,
		"iv":    `"1 day 02:00:00"`,
		"t":     "true",
		"v":     `"x"`,
	}
	for k, want := range checks {
		if got := row.Get(k).Raw; got != want {
			t.Errorf("%s: got %s want %s", k, got, want)
		}
	}
	if strings.Join(res.Columns, ",") != "n,big,small,num,j,b,ts,d,naive,u,arr,nan,ip,iv,t,v" {
		t.Fatalf("columns: %v", res.Columns)
	}
}

func TestLiveArrayArgsAndErrors(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	one, three := int64(1), int64(3)
	res, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT id FROM ` + liveTable + ` WHERE id = ANY($1) ORDER BY id`, Args: []any{[]*int64{&one, nil, &three}}, MaxRows: 10, MaxBytes: 1 << 20})
	if err != nil || res.Count != 2 || gjson.GetBytes(res.RowsJSON, "1.id").Int() != 3 {
		t.Fatalf("ANY($1): %+v %v", res, err)
	}
	a, b := "alice@example.com", "bob@example.com"
	res, err = l.conn.Query(ctx, outlet.Request{SQL: `SELECT count(*) AS c FROM ` + liveTable + ` WHERE email = ANY($1)`, Args: []any{[]*string{&a, &b}}, MaxRows: 10, MaxBytes: 1 << 20})
	if err != nil || gjson.GetBytes(res.RowsJSON, "0.c").Int() != 2 {
		t.Fatalf("text ANY($1): %+v %v", res, err)
	}

	_, err = l.conn.Exec(ctx, outlet.Request{SQL: `INSERT INTO ` + liveTable + ` (id, email) VALUES (9, 'alice@example.com')`, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeConstraint {
		t.Fatalf("duplicate: %v", err)
	}
	_, err = l.conn.Query(ctx, outlet.Request{SQL: `SELECT 1; SELECT 2`, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeQueryFailed {
		t.Fatalf("two statements must be refused by the extended protocol: %v", err)
	}
	_, err = l.conn.Query(ctx, outlet.Request{SQL: `SELECT 1 AS a, 2 AS a`, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeInvalidRequest {
		t.Fatalf("duplicate columns: %v", err)
	}
	_, err = l.conn.Query(ctx, outlet.Request{SQL: `SELECT id FROM ` + liveTable + ` WHERE id = $1`, Args: []any{"not-a-number"}, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeQueryFailed {
		t.Fatalf("unbindable arg: %v", err)
	}
}

func TestLiveCeilings(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	_, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT id FROM ` + liveTable + ` ORDER BY id`, MaxRows: 2, MaxBytes: 1 << 20})
	var oe *outlet.Error
	if !errors.As(err, &oe) || oe.Code != outlet.CodeResultTooLarge || oe.Rows != 2 {
		t.Fatalf("row ceiling: %v", err)
	}
	// The pool survives a row ceiling (the remainder was drained).
	if _, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("after row ceiling: %v", err)
	}

	start := time.Now()
	_, err = l.conn.Query(ctx, outlet.Request{SQL: `SELECT repeat('x', 1000) AS s FROM generate_series(1, 2000000)`, MaxRows: 0, MaxBytes: 64 << 10})
	if !errors.As(err, &oe) || oe.Code != outlet.CodeResultTooLarge || oe.Bytes < 64<<10 {
		t.Fatalf("byte ceiling: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("byte ceiling should abandon the result early, took %v", el)
	}
	if _, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("after byte ceiling: %v", err)
	}

	// exec + RETURNING over the ceiling rolls back.
	_, err = l.conn.Exec(ctx, outlet.Request{SQL: `UPDATE ` + liveTable + ` SET plan = 'rolled' RETURNING id`, MaxRows: 1, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeResultTooLarge {
		t.Fatalf("exec ceiling: %v", err)
	}
	var n int
	if err := l.raw.QueryRow(ctx, `SELECT count(*) FROM `+liveTable+` WHERE plan = 'rolled'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("exec over the ceiling must not commit: n=%d err=%v", n, err)
	}
}

func TestLiveTimeoutCancelAndPool(t *testing.T) {
	l := newLive(t)
	c := l.conn.(*conn)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := l.conn.Query(ctx, outlet.Request{SQL: `SELECT pg_sleep(5)`, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeTimeout {
		t.Fatalf("timeout: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for c.pool.Stat().AcquiredConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := c.pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("a cancelled query must return its connection, acquired=%d", got)
	}
	if _, err := l.conn.Query(context.Background(), outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("after cancel: %v", err)
	}

	// Pool exhaustion: one connection, held, second caller times out.
	small, err := Driver{}.Open(context.Background(), outlet.OpenParams{DSN: []byte(l.dsn), Pool: outlet.PoolLimits{MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	done := make(chan error, 1)
	go func() {
		_, e := small.Query(context.Background(), outlet.Request{SQL: `SELECT pg_sleep(1.5)`, MaxRows: 10, MaxBytes: 1 << 20})
		done <- e
	}()
	time.Sleep(200 * time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	_, err = small.Query(ctx2, outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20})
	if code(err) != outlet.CodeTimeout {
		t.Fatalf("pool wait past the deadline: %v", err)
	}
	if e := <-done; e != nil {
		t.Fatalf("holder: %v", e)
	}
}

func TestLiveAuthAndGuard(t *testing.T) {
	l := newLive(t)
	u, err := url.Parse(l.dsn)
	if err != nil {
		t.Fatal(err)
	}
	// A role that doesn't exist fails authentication under every pg_hba
	// method, including the `trust` a local Homebrew install uses.
	u.User = url.UserPassword("txco_no_such_role", "definitely-wrong-password")
	bad, err := Driver{}.Open(context.Background(), outlet.OpenParams{DSN: []byte(u.String()), Pool: outlet.PoolLimits{MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	_, err = bad.Query(context.Background(), outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20})
	var oe *outlet.Error
	if !errors.As(err, &oe) || oe.Code != outlet.CodeAuthFailed || strings.Contains(oe.Message, u.Hostname()) || strings.Contains(oe.Message, "txco_no_such_role") {
		t.Fatalf("wrong password: %v", err)
	}

	guard, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	priv := "postgres://x:y@127.0.0.1:1/db?sslmode=disable"
	g, err := Driver{}.Open(context.Background(), outlet.OpenParams{DSN: []byte(priv), Guard: guard, Pool: outlet.PoolLimits{MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	_, err = g.Query(context.Background(), outlet.Request{SQL: `SELECT 1`, MaxRows: 10, MaxBytes: 1 << 20})
	if !errors.As(err, &oe) || oe.Code != outlet.CodeConnectFailed || strings.Contains(oe.Message, "127.0.0.1") {
		t.Fatalf("private DSN under private policy: %v", err)
	}
}
