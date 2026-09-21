package blevestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/search/blevestore"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

func eq(field string, v any) search.Filter {
	return search.Filter{Conditions: []search.Condition{{Field: field, Op: vector.OpEq, Value: v}}}
}

// A small log that uses every op, including the two whose meaning depends on
// WHEN they run: an update and a delete by filter, each followed by an upsert
// of a record the filter would have matched.
func testLog() []search.Mutation {
	item := func(id, text, audience string) search.Item {
		return search.Item{ID: id, Text: text, Metadata: map[string]any{"audience": audience, "doc": id[:1]}}
	}
	return []search.Mutation{
		{Op: search.OpEnsure},
		{Op: search.OpUpsert, Items: []search.Item{item("a0", "pool pump manual", "team"), item("a1", "pool opening hours", "team"), item("b0", "parking gate code", "anyone")}},
		{Op: search.OpUpdate, Filter: eq("audience", "team"), Change: search.Change{Merge: map[string]any{"audience": "anyone"}}},
		{Op: search.OpUpsert, Items: []search.Item{item("c0", "pool rota for staff", "team")}}, // after the update: stays "team"
		{Op: search.OpDelete, Select: search.Selector{Filter: eq("doc", "b")}},
		{Op: search.OpUpsert, Items: []search.Item{item("b1", "parking permits", "anyone")}}, // after the delete: stays
		{Op: search.OpUpsert, Items: []search.Item{item("a0", "pool pump manual, second edition", "anyone")}},
	}
}

func state(t *testing.T, s search.Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, probe := range []struct {
		q string
		f search.Filter
	}{{"pool", search.Filter{}}, {"pool", eq("audience", "anyone")}, {"pool", eq("audience", "team")}, {"parking", search.Filter{}}, {"second edition", search.Filter{}}} {
		hits, err := s.Query(context.Background(), "acme", "docs", probe.q, 20, probe.f)
		if err != nil {
			t.Fatalf("Query(%q): %v", probe.q, err)
		}
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		out[fmt.Sprintf("%s|%v", probe.q, probe.f.Conditions)] = ids
	}
	return out
}

func newBleve(t *testing.T) *blevestore.Store {
	t.Helper()
	s, err := blevestore.New(filepath.Join(t.TempDir(), "search"), 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSequencedInterfaces(t *testing.T) {
	var s search.Store = newBleve(t)
	if _, ok := s.(search.Sequenced); !ok {
		t.Fatal("blevestore.Store is not search.Sequenced")
	}
	if _, ok := s.(search.Snapshotter); !ok {
		t.Fatal("blevestore.Store is not search.Snapshotter")
	}
}

// Replaying a log, whole or from the middle, any number of times, converges on
// the same records and the same cursor.
func TestApplyAtIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newBleve(t)
	log := testLog()
	if _, found, _ := s.AppliedSeq(ctx, "acme", "docs"); found {
		t.Fatal("AppliedSeq found a collection that does not exist")
	}
	for i, m := range log {
		if _, applied, err := s.ApplyAt(ctx, "acme", "docs", uint64(100+i), m); err != nil || !applied {
			t.Fatalf("ApplyAt %d: applied=%v err=%v", i, applied, err)
		}
	}
	want := state(t, s)
	if !reflect.DeepEqual(want["pool|[{audience eq team}]"], []string{"c0"}) {
		t.Fatalf("the update was not evaluated in order: %v", want)
	}
	if !reflect.DeepEqual(want["parking|[]"], []string{"b1"}) {
		t.Fatalf("the delete was not evaluated in order: %v", want)
	}
	for round := 0; round < 2; round++ {
		for i, m := range log {
			if n, applied, err := s.ApplyAt(ctx, "acme", "docs", uint64(100+i), m); err != nil || applied || n != 0 {
				t.Fatalf("replay of %d: n=%d applied=%v err=%v", i, n, applied, err)
			}
		}
	}
	if got := state(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("a replay changed the records:\n got %v\nwant %v", got, want)
	}
	if seq, found, err := s.AppliedSeq(ctx, "acme", "docs"); err != nil || !found || seq != uint64(100+len(log)-1) {
		t.Fatalf("AppliedSeq = %d, %v, %v", seq, found, err)
	}
	// A mutation for a collection nobody ensured is an error, not a create.
	var nf *search.CollectionNotFoundError
	if _, _, err := s.ApplyAt(ctx, "acme", "never", 1, log[1]); !errors.As(err, &nf) {
		t.Fatalf("ApplyAt to a missing collection: %T %v", err, err)
	}
}

// The recovery path: a snapshot taken mid-log, installed into an empty store,
// then the WHOLE log replayed over it. The result is the source's.
func TestSnapshotInstallReplay(t *testing.T) {
	ctx := context.Background()
	src, dst := newBleve(t), newBleve(t)
	log := testLog()
	const cut = 4
	for i, m := range log[:cut] {
		if _, _, err := src.ApplyAt(ctx, "acme", "docs", uint64(100+i), m); err != nil {
			t.Fatalf("ApplyAt %d: %v", i, err)
		}
	}
	stage, err := dst.StagingDir() // the DESTINATION's staging dir: Install moves, it does not copy
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(stage, "snap-1")
	man, err := src.Snapshot(ctx, "acme", "docs", snap)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if man.AppliedSeq != 100+cut-1 || man.Records != 4 || man.Collection != "docs" || man.Tenant != "acme" {
		t.Fatalf("manifest = %+v", man)
	}
	if _, err := src.Snapshot(ctx, "acme", "docs", snap); err == nil {
		t.Fatal("Snapshot into an existing directory: want an error")
	}
	for i, m := range log[cut:] {
		if _, _, err := src.ApplyAt(ctx, "acme", "docs", uint64(100+cut+i), m); err != nil {
			t.Fatalf("ApplyAt: %v", err)
		}
	}

	got, err := dst.Install(ctx, "acme", "docs", snap)
	if err != nil || got.AppliedSeq != man.AppliedSeq {
		t.Fatalf("Install: %+v, %v", got, err)
	}
	var applied int
	for i, m := range log {
		_, ok, err := dst.ApplyAt(ctx, "acme", "docs", uint64(100+i), m)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if ok {
			applied++
		}
	}
	if applied != len(log)-cut {
		t.Errorf("replay applied %d mutations, want %d (the snapshot covers the rest)", applied, len(log)-cut)
	}
	if a, b := state(t, src), state(t, dst); !reflect.DeepEqual(a, b) {
		t.Fatalf("restored store differs:\n src %v\n dst %v", a, b)
	}
	// Install replaces a collection that is already there and open.
	snap2 := filepath.Join(stage, "snap-2")
	if _, err := src.Snapshot(ctx, "acme", "docs", snap2); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := dst.Install(ctx, "acme", "docs", snap2); err != nil {
		t.Fatalf("Install over an open collection: %v", err)
	}
	if a, b := state(t, src), state(t, dst); !reflect.DeepEqual(a, b) {
		t.Fatalf("after re-install:\n src %v\n dst %v", a, b)
	}
	// A directory that is not a snapshot is refused, and nothing is lost.
	if _, err := dst.Install(ctx, "acme", "docs", filepath.Join(stage, "nothing-here")); err == nil {
		t.Fatal("Install of a missing snapshot: want an error")
	}
	if a, b := state(t, src), state(t, dst); !reflect.DeepEqual(a, b) {
		t.Fatal("a refused Install damaged the collection")
	}
}

// A snapshot taken while writes keep landing is consistent: its cursor names
// exactly the records it holds.
func TestSnapshotUnderWrites(t *testing.T) {
	ctx := context.Background()
	src, dst := newBleve(t), newBleve(t)
	if _, _, err := src.ApplyAt(ctx, "acme", "docs", 1, search.Mutation{Op: search.OpEnsure}); err != nil {
		t.Fatal(err)
	}
	const total = 120
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 2; i <= total; i++ {
			m := search.Mutation{Op: search.OpUpsert, Items: []search.Item{{ID: fmt.Sprintf("r%03d", i), Text: "steady stream of records"}}}
			if _, _, err := src.ApplyAt(ctx, "acme", "docs", uint64(i), m); err != nil {
				t.Errorf("ApplyAt %d: %v", i, err)
				return
			}
		}
	}()
	stage, _ := dst.StagingDir()
	for n := 0; n < 3; n++ {
		dir := filepath.Join(stage, fmt.Sprintf("s%d", n))
		man, err := src.Snapshot(ctx, "acme", "docs", dir)
		if err != nil {
			t.Fatalf("Snapshot %d: %v", n, err)
		}
		// One record per sequence from 2 on: the cursor and the count must agree.
		if want := uint64(0); man.AppliedSeq >= 2 {
			want = man.AppliedSeq - 1
			if man.Records != want {
				t.Errorf("snapshot %d: cursor %d but %d records (want %d)", n, man.AppliedSeq, man.Records, want)
			}
		}
		if _, err := dst.Install(ctx, "acme", "docs", dir); err != nil {
			t.Fatalf("Install %d: %v", n, err)
		}
	}
	wg.Wait()
}

func TestDropThenEnsureThroughApplyAt(t *testing.T) {
	ctx := context.Background()
	s := newBleve(t)
	steps := []search.Mutation{
		{Op: search.OpEnsure},
		{Op: search.OpUpsert, Items: []search.Item{{ID: "a", Text: "first life"}}},
		{Op: search.OpDrop},
		{Op: search.OpEnsure},
		{Op: search.OpUpsert, Items: []search.Item{{ID: "b", Text: "second life"}}},
	}
	for i, m := range steps {
		if _, _, err := s.ApplyAt(ctx, "acme", "docs", uint64(i+1), m); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if hits, _ := s.Query(ctx, "acme", "docs", "life", 5, search.Filter{}); len(hits) != 1 || hits[0].ID != "b" {
		t.Fatalf("after drop and re-create: %+v", hits)
	}
}
