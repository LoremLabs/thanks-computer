package notebook

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// newTestStore opens a per-test temp-file SQLite DB (cgo sqlite + :memory:
// shared-cache is finicky across connections; a temp file works
// everywhere), applies the schema, and pins the clock.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	// The production DSN shape (sqlite.go): WAL + busy timeout + an
	// immediate write lock at BEGIN, so the concurrency tests below behave
	// like a real node.
	path := filepath.Join(t.TempDir(), "notebook.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.now = func() time.Time { return clk }
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// Idempotent: a second EnsureSchema on an existing file must be a no-op.
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema (again): %v", err)
	}
	return s, &clk
}

var testRef = Ref{Tenant: "acme", Namespace: "core", Name: "task/42"}

func mustAppend(t *testing.T, s *Store, ref Ref, typ, key string) AppendResult {
	t.Helper()
	r, err := s.Append(context.Background(), AppendReq{Ref: ref, Type: typ, ObjectKey: key,
		Data: json.RawMessage(`{"k":` + fmt.Sprintf("%q", key) + `}`)})
	if err != nil {
		t.Fatalf("append %s/%q: %v", typ, key, err)
	}
	return r
}

func count(t *testing.T, s *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(s.rb(q), args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func seqsOf(r ReadResult) []int64 {
	out := make([]int64, 0, len(r.Entries))
	for _, e := range r.Entries {
		out = append(out, e.Seq)
	}
	return out
}

func wantSeqs(t *testing.T, label string, r ReadResult, want ...int64) {
	t.Helper()
	got := seqsOf(r)
	if len(got) != len(want) {
		t.Fatalf("%s: seqs = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: seqs = %v, want %v", label, got, want)
		}
	}
	if r.Count != len(want) {
		t.Errorf("%s: count = %d, want %d", label, r.Count, len(want))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("%s: not ascending: %v", label, got)
		}
	}
}

func headOf(t *testing.T, s *Store, ref Ref) Head {
	t.Helper()
	h, ok, err := s.Stat(context.Background(), ref)
	if err != nil || !ok {
		t.Fatalf("stat %s: ok=%v err=%v", ref.ID(), ok, err)
	}
	return h
}

func TestEnsureSchemaIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table','index')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		have[n] = true
	}
	for _, want := range []string{"notebooks", "notebook_entries", "notebooks_name_idx",
		"notebook_entries_at_idx", "notebook_entries_key_idx", "notebook_entries_exp_idx"} {
		if !have[want] {
			t.Errorf("missing %s in sqlite_master", want)
		}
	}
}

func TestFirstAppendAutoCreatesHead(t *testing.T) {
	s, clk := newTestStore(t)
	t0 := *clk
	r := mustAppend(t, s, testRef, "task.created", "")
	if r.Seq != 1 || !r.At.Equal(t0) || r.Existed {
		t.Fatalf("first append = %+v", r)
	}
	h := headOf(t, s, testRef)
	if h.HighSeq != 1 || h.Generation <= 0 || h.TTL != 0 || !h.CreatedAt.Equal(t0) || !h.UpdatedAt.Equal(t0) || h.Name != "task/42" {
		t.Errorf("head = %+v", h)
	}
	var ttl sql.NullInt64
	if err := s.db.QueryRow(`SELECT ttl_secs FROM notebooks WHERE notebook_id = ?`, testRef.ID()).Scan(&ttl); err != nil || ttl.Valid {
		t.Errorf("ttl_secs = %+v err=%v, want NULL", ttl, err)
	}
	*clk = t0.Add(time.Second)
	if r2 := mustAppend(t, s, testRef, "task.started", ""); r2.Seq != 2 {
		t.Errorf("second seq = %d", r2.Seq)
	}
	h2 := headOf(t, s, testRef)
	if !h2.CreatedAt.Equal(t0) || !h2.UpdatedAt.Equal(*clk) || h2.Generation != h.Generation {
		t.Errorf("head after second = %+v", h2)
	}
}

func TestConcurrentAppendsGapless(t *testing.T) {
	s, _ := newTestStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int64]bool{}
	var errs []error
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				r, err := s.Append(context.Background(), AppendReq{Ref: testRef, Type: "t", ObjectKey: fmt.Sprintf("k-%d-%d", g, i)})
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else if seen[r.Seq] {
					errs = append(errs, fmt.Errorf("seq %d allocated twice", r.Seq))
				} else {
					seen[r.Seq] = true
				}
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs[:1])
	}
	for i := int64(1); i <= 160; i++ {
		if !seen[i] {
			t.Fatalf("gap: seq %d never allocated (have %d)", i, len(seen))
		}
	}
	if h := headOf(t, s, testRef); h.HighSeq != 160 {
		t.Errorf("high seq = %d, want 160", h.HighSeq)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebooks`); n != 1 {
		t.Errorf("head rows = %d", n)
	}
}

// Eight writers racing on ONE object_key: exactly one row, every caller
// gets the same {seq, at}, never a leaked unique violation.
func raceSameKey(t *testing.T, s *Store, key string) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fresh int
	var errs []error
	results := map[AppendResult]bool{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Append(context.Background(), AppendReq{Ref: testRef, Type: "t", ObjectKey: key})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if !r.Existed {
				fresh++
			}
			r.Existed = false
			results[r] = true
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if fresh != 1 || len(results) != 1 {
		t.Errorf("fresh=%d distinct results=%v", fresh, results)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebook_entries WHERE notebook_id = ? AND object_key = ?`, testRef.ID(), key); n != 1 {
		t.Errorf("rows for key = %d", n)
	}
}

func TestConcurrentSameObjectKeyOneRow(t *testing.T) {
	s, _ := newTestStore(t)
	mustAppend(t, s, testRef, "t", "other") // head exists
	raceSameKey(t, s, "same")
	if h := headOf(t, s, testRef); h.HighSeq != 2 {
		t.Errorf("high seq = %d, want 2", h.HighSeq)
	}
}

func TestConcurrentFirstAppendSameKey(t *testing.T) {
	s, _ := newTestStore(t)
	raceSameKey(t, s, "first") // exercises ON CONFLICT DO NOTHING + re-lock
	if n := count(t, s, `SELECT COUNT(*) FROM notebooks`); n != 1 {
		t.Errorf("head rows = %d", n)
	}
	if h := headOf(t, s, testRef); h.HighSeq != 1 {
		t.Errorf("high seq = %d, want 1", h.HighSeq)
	}
}

func TestDuplicateObjectKeyReturnsOriginal(t *testing.T) {
	s, clk := newTestStore(t)
	t0 := *clk
	first := mustAppend(t, s, testRef, "email.received", "recv:1")
	*clk = t0.Add(5 * time.Second)
	again, err := s.Append(context.Background(), AppendReq{Ref: testRef, Type: "something.else", ObjectKey: "recv:1", Data: json.RawMessage(`{"ignored":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Existed || again.Seq != first.Seq || !again.At.Equal(t0) {
		t.Errorf("duplicate = %+v, want original %+v with Existed", again, first)
	}
	h := headOf(t, s, testRef)
	if h.HighSeq != 1 || !h.UpdatedAt.Equal(t0) {
		t.Errorf("head after duplicate = %+v", h)
	}
	r, _ := s.Read(context.Background(), ReadReq{Ref: testRef})
	if len(r.Entries) != 1 || r.Entries[0].Type != "email.received" || string(r.Entries[0].Data) != `{"k":"recv:1"}` {
		t.Errorf("stored entry = %+v", r.Entries)
	}
}

// tenEntries appends 10 entries one second apart, type "a" on odd seqs and
// "b" on even, and returns the per-seq timestamps (index = seq).
func tenEntries(t *testing.T, s *Store, clk *time.Time) []time.Time {
	t.Helper()
	at := make([]time.Time, 11)
	for i := 1; i <= 10; i++ {
		typ := "a"
		if i%2 == 0 {
			typ = "b"
		}
		at[i] = *clk
		mustAppend(t, s, testRef, typ, "")
		*clk = clk.Add(time.Second)
	}
	return at
}

func TestReadAscendingAfterSinceTail(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	at := tenEntries(t, s, clk)
	g := headOf(t, s, testRef).Generation
	cur := func(seq int64) Cursor { return Cursor{Generation: g, Seq: seq} }

	r, err := s.Read(ctx, ReadReq{Ref: testRef, After: cur(3)})
	if err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, "after 3", r, 4, 5, 6, 7, 8, 9, 10)
	if !r.Next.IsZero() || r.Truncated || r.Cursor != cur(10) {
		t.Errorf("after 3: next=%+v truncated=%v cursor=%+v", r.Next, r.Truncated, r.Cursor)
	}

	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Since: at[4], Until: at[7]})
	wantSeqs(t, "since 4 until 7", r, 4, 5, 6)

	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 3})
	wantSeqs(t, "tail 3", r, 8, 9, 10)
	if !r.Next.IsZero() || r.Cursor != cur(10) || r.Truncated {
		t.Errorf("tail 3: next=%+v cursor=%+v truncated=%v", r.Next, r.Cursor, r.Truncated)
	}

	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 3, Type: "b"})
	wantSeqs(t, "tail 3 type b", r, 6, 8, 10)

	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 3, Since: at[8]})
	wantSeqs(t, "tail 3 since 8", r, 8, 9, 10)

	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 3, After: cur(9)})
	wantSeqs(t, "tail 3 after 9", r, 10)

	// tail larger than the notebook: nothing clipped, not truncated.
	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 500})
	if r.Count != 10 || r.Truncated {
		t.Errorf("tail 500: count=%d truncated=%v", r.Count, r.Truncated)
	}
	// tail with an explicit limit is capped by it and says so.
	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Tail: 5, Limit: 2})
	wantSeqs(t, "tail 5 limit 2", r, 9, 10)
	if !r.Truncated {
		t.Errorf("tail 5 limit 2 should be truncated")
	}
}

func TestLimitAndNextCursorDrain(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		mustAppend(t, s, testRef, "t", "")
	}
	g := headOf(t, s, testRef).Generation
	cur := func(seq int64) Cursor { return Cursor{Generation: g, Seq: seq} }

	p1, err := s.Read(ctx, ReadReq{Ref: testRef, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, "page 1", p1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	if p1.Next != cur(10) || p1.Cursor != p1.Next || !p1.Truncated {
		t.Errorf("page 1: next=%+v cursor=%+v truncated=%v", p1.Next, p1.Cursor, p1.Truncated)
	}
	p2, _ := s.Read(ctx, ReadReq{Ref: testRef, Limit: 10, After: p1.Next})
	wantSeqs(t, "page 2", p2, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)
	if p2.Next != cur(20) {
		t.Errorf("page 2 next = %+v", p2.Next)
	}
	p3, _ := s.Read(ctx, ReadReq{Ref: testRef, Limit: 10, After: p2.Next})
	wantSeqs(t, "page 3", p3, 21, 22, 23, 24, 25)
	if !p3.Next.IsZero() || p3.Cursor != cur(25) || p3.Truncated {
		t.Errorf("page 3: next=%+v cursor=%+v truncated=%v", p3.Next, p3.Cursor, p3.Truncated)
	}

	// The drain idiom: After = Next until Next is zero.
	var all []int64
	passes := 0
	req := ReadReq{Ref: testRef, Limit: 10}
	for {
		r, err := s.Read(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		passes++
		all = append(all, seqsOf(r)...)
		if r.Next.IsZero() {
			break
		}
		req.After = r.Next
	}
	if passes != 3 || len(all) != 25 || all[0] != 1 || all[24] != 25 {
		t.Errorf("drain: passes=%d n=%d", passes, len(all))
	}

	// Defaults and clamps.
	if r, _ := s.Read(ctx, ReadReq{Ref: testRef}); r.Count != 25 || !r.Next.IsZero() {
		t.Errorf("default limit: count=%d next=%+v", r.Count, r.Next)
	}
	if r, _ := s.Read(ctx, ReadReq{Ref: testRef, Limit: 25}); r.Count != 25 || !r.Next.IsZero() || r.Truncated {
		t.Errorf("exactly-n: count=%d next=%+v truncated=%v", r.Count, r.Next, r.Truncated)
	}
	s.SetLimits(Limits{MaxReadLimit: 5})
	if r, _ := s.Read(ctx, ReadReq{Ref: testRef, Limit: 50}); r.Count != 5 || r.Next != cur(5) {
		t.Errorf("ceiling clamp: count=%d next=%+v", r.Count, r.Next)
	}
}

func TestCursorOpaqueAndGenerationAware(t *testing.T) {
	c := Cursor{Generation: 1234567890123, Seq: 42}
	enc := c.Encode()
	if enc == "" || strings.ContainsAny(enc, "+/=: ") || !strings.HasPrefix(enc, "1") {
		t.Errorf("encoded cursor %q is not opaque/url-safe", enc)
	}
	back, err := ParseCursor(enc)
	if err != nil || back != c {
		t.Errorf("round trip = %+v err=%v", back, err)
	}
	if (Cursor{}).Encode() != "" {
		t.Errorf("zero cursor must encode as empty")
	}
	if z, err := ParseCursor(""); err != nil || !z.IsZero() {
		t.Errorf("parse empty = %+v err=%v", z, err)
	}
	for _, bad := range []string{"12", "42", "2abc", "1!!!", "1" + base64url("nocolon"), "1" + base64url("1:x"), "1" + base64url("0:1"), "1" + base64url("5:0")} {
		if _, err := ParseCursor(bad); ErrorCode(err) != "txco_notebook_invalid_arg" {
			t.Errorf("ParseCursor(%q) err = %v, want invalid_arg", bad, err)
		}
	}

	s, _ := newTestStore(t)
	ctx := context.Background()
	refB := Ref{Tenant: "acme", Namespace: "core", Name: "task/other"}
	mustAppend(t, s, testRef, "t", "")
	mustAppend(t, s, refB, "t", "")
	ra, _ := s.Read(ctx, ReadReq{Ref: testRef})
	if _, err := s.Read(ctx, ReadReq{Ref: refB, After: ra.Cursor}); ErrorCode(err) != "txco_notebook_stale_cursor" {
		t.Errorf("cursor across notebooks err = %v, want stale_cursor", err)
	}
	var se *StaleCursorError
	if _, err := s.Read(ctx, ReadReq{Ref: refB, After: ra.Cursor}); !errors.As(err, &se) {
		t.Errorf("stale cursor should be a *StaleCursorError, got %T", err)
	}
}

func base64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestTypeFilter(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	types := []string{"x", "y", "z"}
	for i := 0; i < 9; i++ {
		mustAppend(t, s, testRef, types[i%3], "")
	}
	g := headOf(t, s, testRef).Generation
	r, _ := s.Read(ctx, ReadReq{Ref: testRef, Type: "y"})
	wantSeqs(t, "type y", r, 2, 5, 8)
	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Type: "y", Limit: 2})
	wantSeqs(t, "type y limit 2", r, 2, 5)
	if r.Next != (Cursor{Generation: g, Seq: 5}) {
		t.Errorf("next = %+v", r.Next)
	}
	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Type: "y", Limit: 2, After: r.Next})
	wantSeqs(t, "type y page 2", r, 8)
	r, _ = s.Read(ctx, ReadReq{Ref: testRef, Type: "nope"})
	if r.Count != 0 || !r.Next.IsZero() || !r.Cursor.IsZero() {
		t.Errorf("unknown type: %+v", r)
	}
}

func TestTTLLazyFilterAndSweepDoesNotRenumber(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	t0 := *clk
	if err := s.SetTTL(ctx, testRef, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	if h := headOf(t, s, testRef); h.TTL != 60*time.Second || h.HighSeq != 0 {
		t.Errorf("head after SetTTL = %+v", h)
	}
	for i := 0; i < 3; i++ {
		mustAppend(t, s, testRef, "t", "")
	}
	*clk = t0.Add(61 * time.Second)
	if r, _ := s.Read(ctx, ReadReq{Ref: testRef}); r.Count != 0 {
		t.Errorf("expired entries visible: %v", seqsOf(r))
	}
	if r := mustAppend(t, s, testRef, "t", ""); r.Seq != 4 {
		t.Errorf("seq after expiry = %d, want 4 (never recycled)", r.Seq)
	}
	for i, want := range []int64{2, 1, 0} {
		n, err := s.Sweep(ctx, *clk, 2)
		if err != nil || n != want {
			t.Errorf("sweep %d = %d err=%v, want %d", i, n, err, want)
		}
	}
	r, _ := s.Read(ctx, ReadReq{Ref: testRef})
	wantSeqs(t, "after sweep", r, 4)
	if h := headOf(t, s, testRef); h.HighSeq != 4 {
		t.Errorf("high seq after sweep = %d", h.HighSeq)
	}

	// Boundary: expires_at == now is invisible AND sweepable.
	t1 := *clk
	if _, err := s.Append(ctx, AppendReq{Ref: testRef, Type: "t", TTL: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	*clk = t1.Add(10 * time.Second)
	r, _ = s.Read(ctx, ReadReq{Ref: testRef})
	wantSeqs(t, "at boundary", r, 4)
	if n, _ := s.Sweep(ctx, *clk, 10); n != 1 {
		t.Errorf("boundary sweep = %d, want 1", n)
	}

	// Per-append TTL overrides the head's 60s.
	t2 := *clk
	if _, err := s.Append(ctx, AppendReq{Ref: testRef, Type: "t", TTL: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	*clk = t2.Add(6 * time.Second)
	r, _ = s.Read(ctx, ReadReq{Ref: testRef})
	wantSeqs(t, "override", r, 4)

	// A duplicate key whose original expired: fresh seq, old row gone.
	t3 := *clk
	if _, err := s.Append(ctx, AppendReq{Ref: testRef, Type: "t", ObjectKey: "k", TTL: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	*clk = t3.Add(6 * time.Second)
	again, err := s.Append(ctx, AppendReq{Ref: testRef, Type: "t", ObjectKey: "k"})
	if err != nil || again.Existed || again.Seq != 8 {
		t.Errorf("append after expired original = %+v err=%v", again, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebook_entries WHERE notebook_id = ? AND object_key = ?`, testRef.ID(), "k"); n != 1 {
		t.Errorf("rows for k = %d", n)
	}
}

func TestSweepBatchesAcrossNotebooks(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	refs := []Ref{{Tenant: "acme", Namespace: "core", Name: "a"}, {Tenant: "acme", Namespace: "core", Name: "b"}, {Tenant: "beta", Namespace: "x", Name: "c"}}
	for _, ref := range refs {
		for i := 0; i < 2; i++ {
			if _, err := s.Append(ctx, AppendReq{Ref: ref, Type: "t", TTL: time.Second}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Append(ctx, AppendReq{Ref: ref, Type: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	*clk = clk.Add(2 * time.Second)
	for _, want := range []int64{5, 1, 0} {
		if n, _ := s.Sweep(ctx, *clk, 5); n != want {
			t.Errorf("sweep = %d, want %d", n, want)
		}
	}
	for _, ref := range refs {
		r, _ := s.Read(ctx, ReadReq{Ref: ref})
		wantSeqs(t, ref.Name, r, 3)
		headOf(t, s, ref)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebooks`); n != 3 {
		t.Errorf("heads = %d", n)
	}
}

func TestDeleteRemovesHeadAndEntries(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		mustAppend(t, s, testRef, "t", "")
	}
	before, _ := s.Read(ctx, ReadReq{Ref: testRef})
	genA := headOf(t, s, testRef).Generation
	ok, err := s.Delete(ctx, testRef.Tenant, testRef.Namespace, testRef.Name)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebook_entries WHERE notebook_id = ?`, testRef.ID()); n != 0 {
		t.Errorf("entries after delete = %d", n)
	}
	if _, ok, _ := s.Stat(ctx, testRef); ok {
		t.Errorf("head survives delete")
	}
	if ok, _ := s.Delete(ctx, testRef.Tenant, testRef.Namespace, testRef.Name); ok {
		t.Errorf("second delete reports true")
	}
	if r, _ := s.Read(ctx, ReadReq{Ref: testRef}); r.Count != 0 || r.Entries == nil {
		t.Errorf("read of missing notebook = %+v", r)
	}
	if r := mustAppend(t, s, testRef, "t", ""); r.Seq != 1 {
		t.Errorf("seq after recreate = %d", r.Seq)
	}
	if h := headOf(t, s, testRef); h.Generation == genA {
		t.Errorf("recreated head kept generation %d", genA)
	}
	if _, err := s.Read(ctx, ReadReq{Ref: testRef, After: before.Cursor}); ErrorCode(err) != "txco_notebook_stale_cursor" {
		t.Errorf("pre-delete cursor err = %v, want stale_cursor", err)
	}
}

func TestListPrefixRange(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"conversation/a", "conversation/b", "task/1", "task/10", "task/2", "task_x"} {
		mustAppend(t, s, Ref{Tenant: "acme", Namespace: "core", Name: name}, "t", "")
	}
	mustAppend(t, s, Ref{Tenant: "acme", Namespace: "core", Name: "task/2"}, "t", "")
	mustAppend(t, s, Ref{Tenant: "acme", Namespace: "other", Name: "task/1"}, "t", "")
	mustAppend(t, s, Ref{Tenant: "beta", Namespace: "core", Name: "task/1"}, "t", "")

	names := func(r ListResult) []string {
		out := []string{}
		for _, h := range r.Notebooks {
			out = append(out, h.Name)
		}
		return out
	}
	p1, err := s.List(ctx, "acme", "core", "task/", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p1); strings.Join(got, ",") != "task/1,task/10" || p1.Next != "task/10" || p1.Count != 2 {
		t.Errorf("page 1 = %v next=%q", got, p1.Next)
	}
	p2, _ := s.List(ctx, "acme", "core", "task/", p1.Next, 2)
	if got := names(p2); strings.Join(got, ",") != "task/2" || p2.Next != "" {
		t.Errorf("page 2 = %v next=%q", got, p2.Next)
	}
	if p2.Notebooks[0].HighSeq != 2 {
		t.Errorf("task/2 high seq = %d", p2.Notebooks[0].HighSeq)
	}
	all, _ := s.List(ctx, "acme", "core", "", "", 10)
	if got := names(all); strings.Join(got, ",") != "conversation/a,conversation/b,task/1,task/10,task/2,task_x" {
		t.Errorf("all = %v", got)
	}
	if other, _ := s.List(ctx, "acme", "other", "", "", 10); len(other.Notebooks) != 1 {
		t.Errorf("other ns = %v", names(other))
	}
	if _, err := s.List(ctx, "a/b", "core", "", "", 10); ErrorCode(err) != "txco_notebook_invalid_arg" {
		t.Errorf("bad tenant err = %v", err)
	}
}

func TestForEachStreamsAndSnapshots(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	s.SetLimits(Limits{MaxReadLimit: 50, DefaultReadLimit: 10})
	for i := 0; i < 120; i++ {
		mustAppend(t, s, testRef, "t", "")
	}
	var got []int64
	if err := s.ForEach(ctx, ReadReq{Ref: testRef}, func(e Entry) error { got = append(got, e.Seq); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 || got[0] != 1 || got[119] != 120 {
		t.Errorf("visited %d (first %d last %d)", len(got), got[0], got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("not ascending/gapless at %d: %v", i, got[i-2:i+1])
		}
	}
	stop := errors.New("stop")
	n := 0
	if err := s.ForEach(ctx, ReadReq{Ref: testRef}, func(Entry) error {
		n++
		if n == 7 {
			return stop
		}
		return nil
	}); err != stop || n != 7 {
		t.Errorf("early stop: n=%d err=%v", n, err)
	}
	// An append during the walk is not visited (snapshot).
	n = 0
	if err := s.ForEach(ctx, ReadReq{Ref: testRef}, func(e Entry) error {
		n++
		if e.Seq == 1 {
			mustAppend(t, s, testRef, "t", "")
		}
		return nil
	}); err != nil || n != 120 {
		t.Errorf("snapshot: n=%d err=%v", n, err)
	}
	n = 0
	if err := s.ForEach(ctx, ReadReq{Ref: Ref{Tenant: "acme", Namespace: "core", Name: "missing"}}, func(Entry) error { n++; return nil }); err != nil || n != 0 {
		t.Errorf("missing: n=%d err=%v", n, err)
	}
	// After + Type + a tail all flow through.
	g := headOf(t, s, testRef).Generation
	n = 0
	if err := s.ForEach(ctx, ReadReq{Ref: testRef, After: Cursor{Generation: g, Seq: 118}}, func(Entry) error { n++; return nil }); err != nil || n != 3 {
		t.Errorf("after 118: n=%d err=%v", n, err)
	}
	n = 0
	if err := s.ForEach(ctx, ReadReq{Ref: testRef, Tail: 4}, func(Entry) error { n++; return nil }); err != nil || n != 4 {
		t.Errorf("tail 4: n=%d err=%v", n, err)
	}
}

func TestInputGuards(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ok := testRef
	cases := []struct {
		label string
		req   AppendReq
		code  string
	}{
		{"reserved segment", AppendReq{Ref: Ref{"acme", "core", "_secret/x"}, Type: "t"}, "txco_notebook_invalid_name"},
		{"dotdot", AppendReq{Ref: Ref{"acme", "core", "a/../b"}, Type: "t"}, "txco_notebook_invalid_name"},
		{"long name", AppendReq{Ref: Ref{"acme", "core", strings.Repeat("a", 251)}, Type: "t"}, "txco_notebook_invalid_name"},
		{"empty name", AppendReq{Ref: Ref{"acme", "core", ""}, Type: "t"}, "txco_notebook_invalid_name"},
		{"tenant with slash", AppendReq{Ref: Ref{"a/b", "core", "x"}, Type: "t"}, "txco_notebook_invalid_arg"},
		{"empty type", AppendReq{Ref: ok, Type: ""}, "txco_notebook_invalid_arg"},
		{"type charset", AppendReq{Ref: ok, Type: "has space"}, "txco_notebook_invalid_arg"},
		{"long type", AppendReq{Ref: ok, Type: strings.Repeat("t", 129)}, "txco_notebook_too_large"},
		{"long key", AppendReq{Ref: ok, Type: "t", ObjectKey: strings.Repeat("k", 513)}, "txco_notebook_too_large"},
		{"bad json", AppendReq{Ref: ok, Type: "t", Data: json.RawMessage(`{bad`)}, "txco_notebook_invalid_arg"},
		{"bad utf8", AppendReq{Ref: ok, Type: "t", Data: json.RawMessage("\"\xff\"")}, "txco_notebook_invalid_arg"},
		{"big data", AppendReq{Ref: ok, Type: "t", Data: json.RawMessage(`"` + strings.Repeat("x", 70000) + `"`)}, "txco_notebook_too_large"},
	}
	for _, c := range cases {
		_, err := s.Append(ctx, c.req)
		if ErrorCode(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.label, err, c.code)
		}
	}
	if _, err := s.Read(ctx, ReadReq{Ref: ok, Since: time.Unix(10, 0), Until: time.Unix(10, 0)}); ErrorCode(err) != "txco_notebook_invalid_arg" {
		t.Errorf("since>=until err = %v", err)
	}
	if _, err := s.Read(ctx, ReadReq{Ref: ok, Tail: -1}); ErrorCode(err) != "txco_notebook_invalid_arg" {
		t.Errorf("negative tail err = %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM notebooks`); n != 0 {
		t.Errorf("a rejected append created a head (%d)", n)
	}
	// Scalar and array data are JSON values too.
	if _, err := s.Append(ctx, AppendReq{Ref: ok, Type: "t", Data: json.RawMessage(`[1,2]`)}); err != nil {
		t.Errorf("array data: %v", err)
	}
	if _, err := s.Append(ctx, AppendReq{Ref: ok, Type: "t", Data: json.RawMessage(`"note"`)}); err != nil {
		t.Errorf("string data: %v", err)
	}
	r, _ := s.Read(ctx, ReadReq{Ref: ok})
	if len(r.Entries) != 2 || string(r.Entries[0].Data) != `[1,2]` || string(r.Entries[1].Data) != `"note"` {
		t.Errorf("stored data = %+v", r.Entries)
	}
}

func TestAtLayoutFixedWidthAndOrdered(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for _, tm := range []time.Time{base, base.Add(time.Nanosecond), base.Add(500 * time.Millisecond), base.In(time.FixedZone("x", 3600))} {
		if got := FormatAt(tm); len(got) != 30 || !strings.HasSuffix(got, "Z") {
			t.Errorf("FormatAt(%v) = %q (len %d)", tm, got, len(got))
		}
		back, err := ParseAt(FormatAt(tm))
		if err != nil || !back.Equal(tm) {
			t.Errorf("round trip %v → %v err=%v", tm, back, err)
		}
	}
	if FormatAt(base.Add(500*time.Millisecond)) <= FormatAt(base) {
		t.Errorf("lexical order != time order")
	}
	if FormatAt(base.Add(time.Nanosecond)) <= FormatAt(base) {
		t.Errorf("nanosecond ordering lost")
	}
	// A caller's coarse `since` normalises through ParseAt and includes the
	// entry stamped at exactly that second.
	s, _ := newTestStore(t)
	mustAppend(t, s, testRef, "t", "")
	since, err := ParseAt("2026-09-03T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Read(context.Background(), ReadReq{Ref: testRef, Since: since})
	wantSeqs(t, "since coarse", r, 1)
}
