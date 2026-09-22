package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite", Config{DBPath: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, tenant, machine, id, st string) Record {
	t.Helper()
	rec, err := s.Create(context.Background(), CreateReq{Tenant: tenant, Machine: machine, ID: id, State: st})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return rec
}

func TestCreateGetExists(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	rec, err := s.Create(ctx, CreateReq{Tenant: "acme", Machine: "onepony.task", ID: "pony:t1", State: "working",
		Data: json.RawMessage(`{"owner":"a"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Version != 1 || rec.State != "working" || string(rec.Data) != `{"owner":"a"}` {
		t.Fatalf("created = %+v", rec)
	}
	if rec.CreatedAt.IsZero() || !rec.CreatedAt.Equal(rec.UpdatedAt) {
		t.Fatalf("timestamps = %v / %v", rec.CreatedAt, rec.UpdatedAt)
	}
	got, err := s.Get(ctx, "acme", "onepony.task", "pony:t1")
	if err != nil || got.Version != 1 || got.State != "working" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// Twice: exists, carrying the current record, and no event.
	_, err = s.Create(ctx, CreateReq{Tenant: "acme", Machine: "onepony.task", ID: "pony:t1", State: "other"})
	var ex *ExistsError
	if !errors.As(err, &ex) || ex.Current.State != "working" || ErrorCode(err) != "txco_state_exists" {
		t.Fatalf("second create = %v", err)
	}
	evs, _ := s.EventsForRecord(ctx, "acme", "onepony.task", "pony:t1")
	if len(evs) != 0 {
		t.Fatalf("creation wrote %d events, want 0", len(evs))
	}
	// Empty data stores {}.
	rec2 := mustCreate(t, s, "acme", "onepony.task", "pony:t2", "working")
	if string(rec2.Data) != `{}` {
		t.Fatalf("default data = %s", rec2.Data)
	}
	// Not found.
	_, err = s.Get(ctx, "acme", "onepony.task", "nope")
	if ErrorCode(err) != "txco_state_not_found" {
		t.Fatalf("get missing = %v", err)
	}
}

func TestTransitionBumpsVersionAndWritesOneEvent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, "acme", "onepony.task", "t1", "working")
	cause := Cause{Source: "web", Stack: "web", Trace: "rid_1", Run: "run_9"}
	rec, ev, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "onepony.task", ID: "t1",
		From: "working", To: "waiting", ExpectedVersion: 1, Data: json.RawMessage(`{"wake":"soon"}`), Cause: cause})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if rec.Version != 2 || rec.State != "waiting" || string(rec.Data) != `{"wake":"soon"}` {
		t.Fatalf("after = %+v", rec)
	}
	if ev.Version != 2 || ev.From != "working" || ev.To != "waiting" || ev.Cause != cause || ev.ID[:5] != "stev_" {
		t.Fatalf("event = %+v", ev)
	}
	evs, err := s.EventsForRecord(ctx, "acme", "onepony.task", "t1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	got := evs[0]
	if got.ID != ev.ID || got.Status != StatusPending || got.Attempts != 0 || got.Cause != cause || got.NextAttemptAt.IsZero() {
		t.Fatalf("stored event = %+v", got)
	}
	// Nil data keeps the stored data; {} replaces it.
	rec, _, err = s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "onepony.task", ID: "t1",
		From: "waiting", To: "ready", ExpectedVersion: 2})
	if err != nil || string(rec.Data) != `{"wake":"soon"}` || rec.Version != 3 {
		t.Fatalf("keep data: %+v %v", rec, err)
	}
	rec, _, err = s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "onepony.task", ID: "t1",
		From: "ready", To: "ready", ExpectedVersion: 3, Data: json.RawMessage(`{}`)})
	if err != nil || string(rec.Data) != `{}` || rec.Version != 4 {
		t.Fatalf("replace data / self-transition: %+v %v", rec, err)
	}
}

func TestTransitionConflicts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, "acme", "m", "t1", "working")
	for v := int64(1); v < 10; v++ { // working@1 → … → waiting@10
		to := "working"
		if v == 9 {
			to = "waiting"
		}
		if _, _, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: to, ExpectedVersion: v}); err != nil {
			t.Fatalf("v%d: %v", v, err)
		}
	}
	cur, _ := s.Get(ctx, "acme", "m", "t1")
	if cur.State != "waiting" || cur.Version != 10 {
		t.Fatalf("setup = %+v", cur)
	}
	// The stale actor: a timer armed at waiting@7 must not wake waiting@10.
	_, _, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "waiting", To: "ready", ExpectedVersion: 7})
	var c *ConflictError
	if !errors.As(err, &c) || c.Reason != "version" || c.Current.Version != 10 || ErrorCode(err) != "txco_state_conflict" {
		t.Fatalf("stale version = %v", err)
	}
	// Right version, wrong state.
	_, _, err = s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: "ready", ExpectedVersion: 10})
	if !errors.As(err, &c) || c.Reason != "state" || c.Current.State != "waiting" {
		t.Fatalf("wrong from = %v", err)
	}
	// Both wrong: version wins the explanation.
	_, _, err = s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: "ready", ExpectedVersion: 3})
	if !errors.As(err, &c) || c.Reason != "version" {
		t.Fatalf("both wrong = %v", err)
	}
	// Not found.
	_, _, err = s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "nope", From: "a", To: "b", ExpectedVersion: 1})
	if ErrorCode(err) != "txco_state_not_found" {
		t.Fatalf("missing = %v", err)
	}
	// Nothing above wrote an event or moved the record.
	evs, _ := s.EventsForRecord(ctx, "acme", "m", "t1")
	if len(evs) != 9 {
		t.Fatalf("events = %d, want 9", len(evs))
	}
	if cur, _ = s.Get(ctx, "acme", "m", "t1"); cur.Version != 10 || cur.State != "waiting" {
		t.Fatalf("record moved: %+v", cur)
	}
}

// TestTransitionRace: 20 goroutines CAS the same version; exactly one
// wins, and exactly one event row exists for the new version.
func TestTransitionRace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, "acme", "m", "t1", "working")
	for v := int64(1); v < 7; v++ {
		if _, _, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: "working", ExpectedVersion: v}); err != nil {
			t.Fatalf("v%d: %v", v, err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: "waiting", ExpectedVersion: 7})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case ErrorCode(err) == "txco_state_conflict":
				conflicts++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 19 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	evs, _ := s.EventsForRecord(ctx, "acme", "m", "t1")
	n := 0
	for _, e := range evs {
		if e.Version == 8 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("events at v8 = %d, want 1", n)
	}
}

// TestTransitionAtomicWithoutSeam: make the event INSERT fail (a row
// already at (record, v+1)) and check the record did not move — the
// UPDATE and the INSERT are one transaction.
func TestTransitionAtomicWithoutSeam(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, "acme", "m", "t1", "working")
	now := formatAt(s.now())
	if _, err := s.DB().Exec(`INSERT INTO state_events (event_id, tenant, machine, record_id, version, from_state, to_state, created_at, next_attempt_at)
		VALUES ('stev_fake', 'acme', 'm', 't1', 2, 'x', 'y', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := s.Transition(ctx, TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "working", To: "waiting", ExpectedVersion: 1})
	if ErrorCode(err) != "txco_state_store" {
		t.Fatalf("want a store error, got %v", err)
	}
	cur, _ := s.Get(ctx, "acme", "m", "t1")
	if cur.Version != 1 || cur.State != "working" {
		t.Fatalf("record moved without its event: %+v", cur)
	}
}

func TestTenantIsolation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, "a", "m", "t1", "working")
	if _, err := s.Get(ctx, "b", "m", "t1"); ErrorCode(err) != "txco_state_not_found" {
		t.Fatalf("tenant b read a's record: %v", err)
	}
	if _, _, err := s.Transition(ctx, TransitionReq{Tenant: "b", Machine: "m", ID: "t1", From: "working", To: "x", ExpectedVersion: 1}); ErrorCode(err) != "txco_state_not_found" {
		t.Fatalf("tenant b moved a's record: %v", err)
	}
	// Same (machine, id) is a different record per tenant.
	mustCreate(t, s, "b", "m", "t1", "other")
	ra, _ := s.Get(ctx, "a", "m", "t1")
	rb, _ := s.Get(ctx, "b", "m", "t1")
	if ra.State != "working" || rb.State != "other" {
		t.Fatalf("a=%s b=%s", ra.State, rb.State)
	}
}

func TestValidation(t *testing.T) {
	s := openTest(t)
	s.SetMaxDataBytes(16)
	ctx := context.Background()
	bad := []CreateReq{
		{Tenant: "", Machine: "m", ID: "x", State: "s"},
		{Tenant: "a", Machine: "Upper", ID: "x", State: "s"},
		{Tenant: "a", Machine: "1abc", ID: "x", State: "s"},
		{Tenant: "a", Machine: "m", ID: "", State: "s"},
		{Tenant: "a", Machine: "m", ID: "a/b", State: "s"},
		{Tenant: "a", Machine: "m", ID: "a\x01b", State: "s"},
		{Tenant: "a", Machine: "m", ID: "x", State: ""},
		{Tenant: "a", Machine: "m", ID: "x", State: "has space"},
		{Tenant: "a", Machine: "m", ID: "x", State: "s", Data: json.RawMessage(`{not json`)},
		{Tenant: "a", Machine: "m", ID: "x", State: "s", Data: json.RawMessage("\"\xff\"")},
		{Tenant: "a", Machine: "m", ID: "x", State: "s", Data: json.RawMessage(`{"k":"0123456789abcdef"}`)},
	}
	for i, req := range bad {
		if _, err := s.Create(ctx, req); ErrorCode(err) != "txco_state_invalid_arg" {
			t.Errorf("case %d: %v", i, err)
		}
	}
	mustCreate(t, s, "a", "m", "x", "s")
	if _, _, err := s.Transition(ctx, TransitionReq{Tenant: "a", Machine: "m", ID: "x", From: "s", To: "t", ExpectedVersion: 0}); ErrorCode(err) != "txco_state_invalid_arg" {
		t.Errorf("version 0: %v", err)
	}
	if _, _, err := s.Transition(ctx, TransitionReq{Tenant: "a", Machine: "m", ID: "x", From: "s", To: "", ExpectedVersion: 1}); ErrorCode(err) != "txco_state_invalid_arg" {
		t.Errorf("empty to: %v", err)
	}
	// Good ids and states.
	for _, id := range []string{"pony:t_1", "a b", "héllo", "x.y-z"} {
		if err := ValidID(id); err != nil {
			t.Errorf("id %q: %v", id, err)
		}
	}
	for _, st := range []string{"working", "Waiting", "done.ok", "v1-x"} {
		if err := ValidState("state", st); err != nil {
			t.Errorf("state %q: %v", st, err)
		}
	}
}

// --- the dispatcher's side ------------------------------------------------

func seedEvent(t *testing.T, s *Store, tenant, machine, id string, from, to string) Event {
	t.Helper()
	cur, err := s.Get(context.Background(), tenant, machine, id)
	if err != nil {
		cur = mustCreate(t, s, tenant, machine, id, from)
	}
	_, ev, err := s.Transition(context.Background(), TransitionReq{Tenant: tenant, Machine: machine, ID: id, From: cur.State, To: to, ExpectedVersion: cur.Version})
	if err != nil {
		t.Fatalf("seed transition: %v", err)
	}
	return ev
}

func TestClaimOutcomes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	ev := seedEvent(t, s, "acme", "m", "t1", "working", "waiting")

	won, err := s.ClaimDue(ctx, "node-a", 10)
	if err != nil || len(won) != 1 || won[0].ID != ev.ID || won[0].Status != StatusClaimed || won[0].ClaimedBy != "node-a" {
		t.Fatalf("claim = %+v, %v", won, err)
	}
	if again, _ := s.ClaimDue(ctx, "node-b", 10); len(again) != 0 {
		t.Fatalf("claimed twice: %+v", again)
	}
	// Retry without consuming: pending again, due after the backoff, no attempt.
	if err := s.Retry(ctx, ev.ID, time.Minute, "admission denied", false, 5); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetEvent(ctx, ev.ID)
	if got.Status != StatusPending || got.Attempts != 0 || got.ClaimedBy != "" || got.LastError != "admission denied" || !got.NextAttemptAt.Equal(clock.Add(time.Minute)) {
		t.Fatalf("after retry = %+v", got)
	}
	if early, _ := s.ClaimDue(ctx, "node-a", 10); len(early) != 0 {
		t.Fatalf("claimed before its backoff: %+v", early)
	}
	clock = clock.Add(2 * time.Minute)
	if won, _ = s.ClaimDue(ctx, "node-a", 10); len(won) != 1 {
		t.Fatalf("not due after backoff: %+v", won)
	}
	// Retry consuming an attempt.
	if err := s.Retry(ctx, ev.ID, 0, "accept timeout", true, 5); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetEvent(ctx, ev.ID); got.Status != StatusPending || got.Attempts != 1 {
		t.Fatalf("after counted retry = %+v", got)
	}
	// Done: terminal, with the rid; a second outcome is a no-op.
	s.ClaimDue(ctx, "node-a", 10)
	if err := s.MarkDone(ctx, ev.ID, "rid_x"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetEvent(ctx, ev.ID)
	if got.Status != StatusDone || got.DeliveredRid != "rid_x" || got.DeliveredAt.IsZero() || got.LastError != "" {
		t.Fatalf("after done = %+v", got)
	}
	_ = s.Retry(ctx, ev.ID, 0, "late", true, 5)
	if got, _ = s.GetEvent(ctx, ev.ID); got.Status != StatusDone {
		t.Fatalf("done reopened: %+v", got)
	}
	// Skipped.
	ev2 := seedEvent(t, s, "acme", "m", "t2", "a", "b")
	s.ClaimDue(ctx, "node-a", 10)
	if err := s.MarkSkipped(ctx, ev2.ID, "no _state stack"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetEvent(ctx, ev2.ID); got.Status != StatusSkipped || got.LastError != "no _state stack" {
		t.Fatalf("after skipped = %+v", got)
	}
}

func TestAttemptCapAndReclaim(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	ev := seedEvent(t, s, "acme", "m", "t1", "a", "b")

	// Reclaim: a claim older than staleAfter goes back to pending with an
	// attempt consumed and the same event id; a fresh one is left alone.
	s.ClaimDue(ctx, "node-dead", 10)
	if n, _ := s.ReclaimStale(ctx, 5*time.Minute, 3); n != 0 {
		t.Fatalf("reclaimed a fresh claim")
	}
	clock = clock.Add(6 * time.Minute)
	if n, _ := s.ReclaimStale(ctx, 5*time.Minute, 3); n != 1 {
		t.Fatalf("reclaim = %d, want 1", n)
	}
	got, _ := s.GetEvent(ctx, ev.ID)
	if got.Status != StatusPending || got.Attempts != 1 || got.ClaimedBy != "" {
		t.Fatalf("after reclaim = %+v", got)
	}
	won, _ := s.ClaimDue(ctx, "node-a", 10)
	if len(won) != 1 || won[0].ID != ev.ID || won[0].Attempts != 1 {
		t.Fatalf("re-presented = %+v", won)
	}
	// The cap: attempts 1 → 2 (pending) → 3 (dead at max 3).
	_ = s.Retry(ctx, ev.ID, 0, "timeout", true, 3)
	if got, _ = s.GetEvent(ctx, ev.ID); got.Status != StatusPending || got.Attempts != 2 {
		t.Fatalf("attempt 2 = %+v", got)
	}
	s.ClaimDue(ctx, "node-a", 10)
	_ = s.Retry(ctx, ev.ID, 0, "timeout", true, 3)
	if got, _ = s.GetEvent(ctx, ev.ID); got.Status != StatusDead || got.Attempts != 3 {
		t.Fatalf("attempt 3 = %+v", got)
	}
	if won, _ = s.ClaimDue(ctx, "node-a", 10); len(won) != 0 {
		t.Fatalf("dead event claimed: %+v", won)
	}
	// Reclaim also kills at the cap.
	ev2 := seedEvent(t, s, "acme", "m", "t2", "a", "b")
	_, _ = s.DB().Exec(`UPDATE state_events SET attempts = 2 WHERE event_id = ?`, ev2.ID)
	s.ClaimDue(ctx, "node-dead", 10)
	clock = clock.Add(6 * time.Minute)
	s.ReclaimStale(ctx, 5*time.Minute, 3)
	if got, _ = s.GetEvent(ctx, ev2.ID); got.Status != StatusDead {
		t.Fatalf("reclaim past cap = %+v", got)
	}
}

// TestPerRecordOrder: version 9 is not presented while version 8 is
// pending or claimed, and is released when 8 reaches a terminal status.
// Other records are unaffected.
func TestPerRecordOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ev8 := seedEvent(t, s, "acme", "m", "t1", "a", "b") // v2
	ev9 := seedEvent(t, s, "acme", "m", "t1", "b", "c") // v3
	other := seedEvent(t, s, "acme", "m", "t2", "a", "b")

	won, _ := s.ClaimDue(ctx, "n", 10)
	ids := map[string]bool{}
	for _, e := range won {
		ids[e.ID] = true
	}
	if !ids[ev8.ID] || ids[ev9.ID] || !ids[other.ID] {
		t.Fatalf("first claim = %v", ids)
	}
	// Still held while v8 is claimed, and while it is pending again.
	if again, _ := s.ClaimDue(ctx, "n", 10); len(again) != 0 {
		t.Fatalf("v9 presented while v8 claimed: %+v", again)
	}
	_ = s.Retry(ctx, ev8.ID, time.Hour, "denied", false, 5)
	if again, _ := s.ClaimDue(ctx, "n", 10); len(again) != 0 {
		t.Fatalf("v9 presented while v8 pending: %+v", again)
	}
	// Released at v8's terminal status.
	_, _ = s.DB().Exec(`UPDATE state_events SET next_attempt_at = created_at WHERE event_id = ?`, ev8.ID)
	won, _ = s.ClaimDue(ctx, "n", 10)
	if len(won) != 1 || won[0].ID != ev8.ID {
		t.Fatalf("v8 re-claim = %+v", won)
	}
	_ = s.MarkDone(ctx, ev8.ID, "rid")
	won, _ = s.ClaimDue(ctx, "n", 10)
	if len(won) != 1 || won[0].ID != ev9.ID {
		t.Fatalf("v9 after v8 done = %+v", won)
	}
	// A dead earlier version also releases the next.
	ev10 := seedEvent(t, s, "acme", "m", "t1", "c", "d")
	_ = s.Retry(ctx, ev9.ID, 0, "x", true, 1) // dead
	won, _ = s.ClaimDue(ctx, "n", 10)
	if len(won) != 1 || won[0].ID != ev10.ID {
		t.Fatalf("v10 after v9 dead = %+v", won)
	}
}

func TestPurgeKeepsPending(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return clock })
	done := seedEvent(t, s, "acme", "m", "t1", "a", "b")
	pending := seedEvent(t, s, "acme", "m", "t2", "a", "b")
	s.ClaimDue(ctx, "n", 1) // t1's (earliest next_attempt_at ties: version order within the record only, so claim both then re-open t2)
	won, _ := s.ClaimDue(ctx, "n", 10)
	for _, e := range won {
		_ = s.Retry(ctx, e.ID, 0, "", false, 5)
	}
	_ = s.MarkDone(ctx, done.ID, "rid")
	if got, _ := s.GetEvent(ctx, done.ID); got.Status != StatusDone {
		// The single-claim above may have taken t2; normalise.
		_ = s.MarkDone(ctx, done.ID, "rid")
	}
	clock = clock.Add(31 * 24 * time.Hour)
	n, err := s.Purge(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvent(ctx, pending.ID); err != nil {
		t.Fatalf("pending event purged: %v", err)
	}
	if _, err := s.GetEvent(ctx, done.ID); ErrorCode(err) != "txco_state_not_found" {
		t.Fatalf("done event kept (purged %d): %v", n, err)
	}
}

// TestSchemaIdempotent: EnsureSchema twice on the same file, and on a
// bare DB handle with the SQLite dialect.
func TestSchemaIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open("sqlite", Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := NewStore(db, registry.SQLite).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("nope", Config{}); err == nil {
		t.Fatal("unknown backend must fail")
	}
}
