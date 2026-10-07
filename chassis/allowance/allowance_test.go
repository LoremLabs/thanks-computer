package allowance

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kvtools/valkeyrie"

	"github.com/loremlabs/thanks-computer/chassis/kv"
	boltdb "github.com/loremlabs/thanks-computer/chassis/kv/boltstore"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

func newStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	st := NewStore(kv.New(s, 0, 0))
	st.now = func() time.Time { return now }
	return st
}

func TestWindow(t *testing.T) {
	at := time.Date(2026, 12, 31, 23, 15, 0, 0, time.FixedZone("x", -5*3600)) // 2027-01-01 04:15 UTC
	cases := []struct {
		per        Period
		id         string
		start, end time.Time
	}{
		{Hour, "h2027010104", time.Date(2027, 1, 1, 4, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC)},
		{Day, "d20270101", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)},
		{Month, "m202701", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		id, start, end := c.per.Window(at)
		if id != c.id || !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("%s: got (%s, %s, %s), want (%s, %s, %s)", c.per, id, start, end, c.id, c.start, c.end)
		}
	}
	// December rolls into the next year.
	if _, _, end := Month.Window(time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)); !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("December window ends %s, want 2027-01-01", end)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"scout": true, "pony-scout": true, "a.b_c-1": true, "9lives": true,
		"": false, "Scout": false, "-x": false, ".x": false, "a/b": false, "a b": false, "a@b": false,
		string(make([]byte, 65)): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDefValidate(t *testing.T) {
	if err := (Def{Name: "scout", Fuel: 500, Per: Day}).Validate(); err != nil {
		t.Errorf("valid def: %v", err)
	}
	for _, d := range []Def{
		{Name: "", Fuel: 1, Per: Day},
		{Name: "scout", Fuel: 0, Per: Day},
		{Name: "scout", Fuel: 1, Per: "week"},
	} {
		if d.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want an error", d)
		}
	}
}

func TestStoreSetGetListDelete(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, time.Now())
	for _, d := range []Def{{"b-pony", 20, Hour}, {"a-pony", 10, Day}} {
		if err := s.Set(ctx, "acme", d); err != nil {
			t.Fatalf("Set %s: %v", d.Name, err)
		}
	}
	if err := s.Set(ctx, "other", Def{"a-pony", 99, Month}); err != nil {
		t.Fatal(err)
	}
	d, found, err := s.Def(ctx, "acme", "a-pony")
	if err != nil || !found || d.Fuel != 10 || d.Per != Day {
		t.Fatalf("Def = %+v, %v, %v", d, found, err)
	}
	defs, next, err := s.List(ctx, "acme", "", 0)
	if err != nil || next != "" || len(defs) != 2 || defs[0].Name != "a-pony" || defs[1].Name != "b-pony" {
		t.Fatalf("List = %+v, %q, %v (another tenant's allowance must not show)", defs, next, err)
	}
	if err := s.Delete(ctx, "acme", "a-pony"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Def(ctx, "acme", "a-pony"); found {
		t.Error("deleted definition still found")
	}
	if _, found, _ := s.Def(ctx, "other", "a-pony"); !found {
		t.Error("deleting acme's allowance removed another tenant's")
	}
}

func TestStoreStatusAndCharge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 17, 30, 0, 0, time.UTC)
	s := newStore(t, now)

	// Undefined: metered by day, never exhausted.
	if err := s.Charge(ctx, "acme", "scout", 70); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx, "acme", "scout")
	if err != nil {
		t.Fatal(err)
	}
	if st.Defined || st.Used != 70 || st.Per != Day || st.Exhausted() {
		t.Fatalf("undefined status = %+v", st)
	}
	if want := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC); !st.ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %s, want %s", st.ResetsAt, want)
	}

	// Defined by the hour: its own window, fresh.
	if err := s.Set(ctx, "acme", Def{"scout", 100, Hour}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []int64{60, 50} {
		if err := s.Charge(ctx, "acme", "scout", f); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = s.Status(ctx, "acme", "scout")
	if !st.Defined || st.Used != 110 || st.Remaining() != 0 || !st.Exhausted() {
		t.Fatalf("hour status = %+v, want used 110 of 100, exhausted", st)
	}
	if want := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC); !st.ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %s, want %s", st.ResetsAt, want)
	}

	// The next hour starts at zero.
	s.now = func() time.Time { return now.Add(time.Hour) }
	st, _ = s.Status(ctx, "acme", "scout")
	if st.Used != 0 || st.Remaining() != 100 {
		t.Errorf("next hour status = %+v, want a fresh window", st)
	}
}

type recSink struct {
	mu     sync.Mutex
	events []usage.UsageEvent
	closed bool
}

func (r *recSink) WriteEvent(ev usage.UsageEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}
func (r *recSink) Name() string                { return "rec" }
func (r *recSink) Close(context.Context) error { r.closed = true; return nil }

func TestTeeChargesBillableAllowanceFuel(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, time.Now())
	next := &recSink{}
	tee := NewTee(next, s, nil)
	for _, ev := range []usage.UsageEvent{
		{Tenant: "acme", Allowance: "scout", Fuel: 40, Billable: true},
		{Tenant: "acme", Allowance: "scout", Fuel: 2, Billable: true},
		{Tenant: "acme", Allowance: "scout", Fuel: 1000, Billable: false}, // a side line: not charged
		{Tenant: "acme", Fuel: 500, Billable: true},                       // no allowance
		{Tenant: "acme", Allowance: "Bad/Name", Fuel: 9, Billable: true},  // never a key
	} {
		tee.WriteEvent(ev)
	}
	if err := tee.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(next.events) != 5 || !next.closed {
		t.Fatalf("next sink got %d events, closed=%v; want all 5 forwarded and closed", len(next.events), next.closed)
	}
	if tee.Name() != "rec" {
		t.Errorf("Name = %q, want the wrapped sink's", tee.Name())
	}
	st, err := s.Status(ctx, "acme", "scout")
	if err != nil || st.Used != 42 {
		t.Fatalf("scout used = %d (%v), want 42", st.Used, err)
	}
}
