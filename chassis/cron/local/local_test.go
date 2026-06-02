package local

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cron"
)

func open(t *testing.T, maxInflight int) cron.Queue {
	t.Helper()
	q, err := cron.Open("local", cron.Config{MaxInflight: maxInflight})
	if err != nil {
		t.Fatalf("open local: %v", err)
	}
	return q
}

// TestLocalEnqueueDispatches: an enqueued job is handed to fn exactly once.
func TestLocalEnqueueDispatches(t *testing.T) {
	q := open(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 1)
	go func() { _ = q.Work(ctx, func(_ context.Context, j cron.Job) error { got <- j.Tenant; return nil }) }()

	if err := q.Enqueue(ctx, cron.Job{Tenant: "acme"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case tn := <-got:
		if tn != "acme" {
			t.Fatalf("dispatched tenant %q, want acme", tn)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job never dispatched")
	}
}

// TestLocalConcurrencyCapHonored: no more than MaxInflight fn calls run at once,
// and every enqueued job runs exactly once.
func TestLocalConcurrencyCapHonored(t *testing.T) {
	const max = 2
	const jobs = 6
	q := open(t, max)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var inflight, peak, ran int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(jobs)

	go func() {
		_ = q.Work(ctx, func(_ context.Context, _ cron.Job) error {
			cur := atomic.AddInt64(&inflight, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			<-release // hold the slot until the test releases
			atomic.AddInt64(&inflight, -1)
			atomic.AddInt64(&ran, 1)
			wg.Done()
			return nil
		})
	}()

	// Enqueue from a goroutine: with the cap held, only `max` jobs are
	// in-flight and the buffer is small, so the later Enqueue calls block
	// until workers free up (on release) — exactly the backpressure we want.
	for i := 0; i < jobs; i++ {
		go func() { _ = q.Enqueue(ctx, cron.Job{}) }()
	}

	// Give the pool time to saturate to the cap, then assert it never exceeded it.
	time.Sleep(200 * time.Millisecond)
	if p := atomic.LoadInt64(&peak); p > max {
		t.Fatalf("peak inflight = %d, want <= %d", p, max)
	}
	close(release)
	wg.Wait()
	if r := atomic.LoadInt64(&ran); r != jobs {
		t.Fatalf("ran = %d, want %d (each job exactly once)", r, jobs)
	}
}

// TestLocalEnqueueCancelled: with a full buffer and no worker draining,
// Enqueue returns the ctx error rather than blocking forever.
func TestLocalEnqueueCancelled(t *testing.T) {
	q := open(t, 1) // buffer == 1, no Work() running
	ctx, cancel := context.WithCancel(context.Background())

	// First enqueue fills the buffer.
	if err := q.Enqueue(ctx, cron.Job{}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Second would block; cancel and assert it returns the ctx error.
	cancel()
	if err := q.Enqueue(ctx, cron.Job{}); err == nil {
		t.Fatal("enqueue on full buffer with cancelled ctx returned nil error")
	}
}
