package ipp

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite", Config{DBPath: filepath.Join(t.TempDir(), "ipp.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, tenant, printer string) Job {
	t.Helper()
	j, err := s.CreateJob(context.Background(), NewJob{Tenant: tenant, Printer: printer, JobName: "doc", Host: "ipp.example.com"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return j
}

const sha = "7c92a1b5e0d3f4a6b8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"

func TestJobNumbersAreMonotonicPerTenant(t *testing.T) {
	s := newTestStore(t)
	for want := int64(1); want <= 3; want++ {
		if j := mustCreate(t, s, "acme", "research"); j.Number != want || j.State != StateReceiving {
			t.Fatalf("acme job: number=%d state=%s, want %d receiving", j.Number, j.State, want)
		}
	}
	// A different tenant starts its own sequence; printers share the tenant's.
	if j := mustCreate(t, s, "beta", "research"); j.Number != 1 {
		t.Fatalf("beta first job number = %d", j.Number)
	}
	if j := mustCreate(t, s, "acme", "expenses"); j.Number != 4 {
		t.Fatalf("acme second printer job number = %d, want 4 (job-id is unique per tenant)", j.Number)
	}
}

func TestLifecycleReceivingCommittedDelivered(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	j := mustCreate(t, s, "acme", "research")

	ok, err := s.SetCommitted(ctx, j.ID, sha, 4096, "application/pdf", "report.pdf")
	if err != nil || !ok {
		t.Fatalf("SetCommitted: %v %v", ok, err)
	}
	// Committing twice is a no-op (the row already left `receiving`).
	if ok, _ := s.SetCommitted(ctx, j.ID, sha, 1, "x", ""); ok {
		t.Fatal("second SetCommitted succeeded")
	}

	leased, err := s.LeaseCommitted(ctx, "node-a", 10)
	if err != nil || len(leased) != 1 || leased[0].ID != j.ID || leased[0].Attempts != 1 {
		t.Fatalf("LeaseCommitted: %+v %v", leased, err)
	}
	if leased[0].State != StateCommitted {
		t.Fatalf("a lease must not be a state: %s", leased[0].State)
	}
	// A leased row is not leased again.
	if again, _ := s.LeaseCommitted(ctx, "node-b", 10); len(again) != 0 {
		t.Fatalf("leased twice: %+v", again)
	}

	if err := s.MarkDelivered(ctx, j.ID, "rid-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, "acme", "research", j.Number)
	if err != nil || got.State != StateDelivered || got.Rid != "rid-1" || got.SHA256 != sha ||
		got.Size != 4096 || got.DocumentName != "report.pdf" || got.DeliveredAt.IsZero() {
		t.Fatalf("delivered job: %+v %v", got, err)
	}
	// Terminal: nothing moves it again.
	_ = s.MarkFailed(ctx, j.ID, "too late")
	if got, _ := s.GetJobByID(ctx, j.ID); got.State != StateDelivered {
		t.Fatalf("delivered job was moved to %s", got.State)
	}
}

func TestCancelGuards(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// receiving → canceled; the in-flight upload then cannot commit.
	a := mustCreate(t, s, "acme", "research")
	if j, ok, err := s.Cancel(ctx, "acme", "research", a.Number); err != nil || !ok || j.State != StateCanceled {
		t.Fatalf("cancel receiving: %+v %v %v", j, ok, err)
	}
	if ok, _ := s.SetCommitted(ctx, a.ID, sha, 1, "application/pdf", ""); ok {
		t.Fatal("a canceled job committed")
	}
	// Canceling again: not an error, not a transition.
	if j, ok, err := s.Cancel(ctx, "acme", "research", a.Number); err != nil || ok || j.State != StateCanceled {
		t.Fatalf("re-cancel: %+v %v %v", j, ok, err)
	}

	// committed under a lease (mid-handoff) → refused.
	b := mustCreate(t, s, "acme", "research")
	_, _ = s.SetCommitted(ctx, b.ID, sha, 1, "application/pdf", "")
	if _, err := s.LeaseCommitted(ctx, "node-a", 1); err != nil {
		t.Fatal(err)
	}
	if j, ok, _ := s.Cancel(ctx, "acme", "research", b.Number); ok || j.State != StateCommitted {
		t.Fatalf("canceled a leased job: %+v", j)
	}
	// delivered → refused.
	_ = s.MarkDelivered(ctx, b.ID, "rid")
	if j, ok, _ := s.Cancel(ctx, "acme", "research", b.Number); ok || j.State != StateDelivered {
		t.Fatalf("canceled a delivered job: %+v", j)
	}
	// Another tenant / printer cannot see it.
	if _, _, err := s.Cancel(ctx, "beta", "research", b.Number); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-tenant cancel: %v", err)
	}
}

func TestLeaseRecoveryAndExhaustion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	j := mustCreate(t, s, "acme", "research")
	_, _ = s.SetCommitted(ctx, j.ID, sha, 1, "application/pdf", "")
	if l, _ := s.LeaseCommitted(ctx, "dead-node", 1); len(l) != 1 {
		t.Fatal("lease")
	}
	// The node dies. Before the stale window nothing moves…
	if n, _ := s.ReleaseStaleLeases(ctx, 10*time.Minute); n != 0 {
		t.Fatalf("released a fresh lease: %d", n)
	}
	// …after it, the lease clears and another node takes the job.
	now = now.Add(11 * time.Minute)
	if n, _ := s.ReleaseStaleLeases(ctx, 10*time.Minute); n != 1 {
		t.Fatalf("stale lease not released: %d", n)
	}
	l, _ := s.LeaseCommitted(ctx, "node-b", 1)
	if len(l) != 1 || l[0].Attempts != 2 {
		t.Fatalf("re-lease: %+v", l)
	}
	// The bus never accepts: hand it back, and once attempts run out the job
	// fails — the ONLY way delivery fails.
	if err := s.ReleaseLease(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.FailExhausted(ctx, 3); n != 0 {
		t.Fatalf("failed with attempts to spare: %d", n)
	}
	if n, _ := s.FailExhausted(ctx, 2); n != 1 {
		t.Fatalf("exhausted job not failed: %d", n)
	}
	if got, _ := s.GetJobByID(ctx, j.ID); got.State != StateFailed || got.StateReason == "" {
		t.Fatalf("exhausted job: %+v", got)
	}
}

func TestReapListCountPurge(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	abandoned := mustCreate(t, s, "acme", "research") // Create-Job, document never sent
	live := mustCreate(t, s, "acme", "research")
	_, _ = s.SetCommitted(ctx, live.ID, sha, 1, "application/pdf", "")

	if n, _ := s.CountReceiving(ctx, "acme"); n != 1 {
		t.Fatalf("CountReceiving = %d", n)
	}
	if n, _ := s.CountQueued(ctx, "acme", "research"); n != 2 {
		t.Fatalf("CountQueued = %d", n)
	}

	now = now.Add(20 * time.Minute)
	if n, _ := s.ReapReceiving(ctx, 10*time.Minute); n != 1 {
		t.Fatalf("ReapReceiving = %d", n)
	}
	if got, _ := s.GetJobByID(ctx, abandoned.ID); got.State != StateFailed {
		t.Fatalf("abandoned job: %s", got.State)
	}

	active, _ := s.ListJobs(ctx, "acme", "research", false, 0)
	done, _ := s.ListJobs(ctx, "acme", "research", true, 0)
	if len(active) != 1 || active[0].ID != live.ID || len(done) != 1 || done[0].ID != abandoned.ID {
		t.Fatalf("ListJobs: active=%+v done=%+v", active, done)
	}

	// Purge removes only terminal rows past retention.
	now = now.Add(8 * 24 * time.Hour)
	if n, _ := s.Purge(ctx, 7*24*time.Hour); n != 1 {
		t.Fatalf("Purge = %d", n)
	}
	if _, err := s.GetJobByID(ctx, live.ID); err != nil {
		t.Fatalf("purge removed a live job: %v", err)
	}
}

// Competing dispatchers must partition the committed set: every job leased
// exactly once.
func TestLeaseCommittedIsExclusive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	const jobs = 40
	for i := 0; i < jobs; i++ {
		j := mustCreate(t, s, "acme", "research")
		if ok, err := s.SetCommitted(ctx, j.ID, sha, 1, "application/pdf", ""); !ok || err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				l, err := s.LeaseCommitted(ctx, "node", 5)
				if err != nil {
					t.Errorf("lease: %v", err)
					return
				}
				if len(l) == 0 {
					return
				}
				mu.Lock()
				for _, j := range l {
					seen[j.ID]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(seen) != jobs {
		t.Fatalf("leased %d distinct jobs, want %d", len(seen), jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s leased %d times", id, n)
		}
	}
}
