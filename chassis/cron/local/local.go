// Package local is the in-process cron Queue: a buffered channel feeding
// a fixed pool of worker goroutines. Single-node default — preserves the
// chassis's "fire and drain per dispatch" behavior, but bounds how many
// dispatches run at once (a fan-out of N slow tenants can't spawn N
// unbounded goroutines). Selected with --cron-queue=local (the default).
package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/cron"
)

// defaultMaxInflight matches config.go's --cron-max-inflight default; used
// only as a floor when an unset/zero Config slips through.
const defaultMaxInflight = 32

func init() {
	cron.Register("local", func(cfg cron.Config) (cron.Queue, error) {
		max := cfg.MaxInflight
		if max <= 0 {
			max = defaultMaxInflight
		}
		return &Queue{
			jobs: make(chan cron.Job, max), // modest buffer: decouple enqueue from pickup
			max:  max,
		}, nil
	})
}

// Queue is the in-process cron Queue.
type Queue struct {
	jobs chan cron.Job
	max  int
}

func (*Queue) Name() string { return "local" }

// Enqueue sends a job onto the buffered channel. Honors ctx so a
// shutting-down scheduler doesn't block forever on a full buffer (a full
// buffer is intended backpressure — the worker pool hasn't drained the
// previous fan-out).
func (q *Queue) Enqueue(ctx context.Context, job cron.Job) error {
	select {
	case q.jobs <- job:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cron/local: enqueue cancelled: %w", ctx.Err())
	}
}

// Work starts q.max worker goroutines, each pulling jobs and calling fn
// synchronously. Receiving a job off the channel is the at-most-one-worker
// guarantee — no separate semaphore, no goroutine-per-job. Work returns
// after ctx is cancelled and every worker has finished its current fn.
func (q *Queue) Work(ctx context.Context, fn func(context.Context, cron.Job) error) error {
	var wg sync.WaitGroup
	for i := 0; i < q.max; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-q.jobs:
					// fn logs its own errors; one bad tick never kills the worker.
					_ = fn(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

var _ cron.Queue = (*Queue)(nil)
