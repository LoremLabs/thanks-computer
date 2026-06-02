package cron

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	cronq "github.com/loremlabs/thanks-computer/chassis/cron"
	_ "github.com/loremlabs/thanks-computer/chassis/cron/local" // registers the "local" backend
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newLocalQueue opens the in-process "local" cron queue for tests.
func newLocalQueue(t *testing.T) cronq.Queue {
	t.Helper()
	q, err := cronq.Open("local", cronq.Config{MaxInflight: 4, Period: 1})
	if err != nil {
		t.Fatalf("open local queue: %v", err)
	}
	return q
}

// recordingQueue is a fake cron.Queue that records Enqueue'd jobs, so a
// scheduler tick can be asserted in isolation from the worker (and without
// timing flakiness). Work just blocks until ctx is cancelled.
type recordingQueue struct {
	mu   sync.Mutex
	jobs []cronq.Job
}

func (q *recordingQueue) Name() string { return "recording" }

func (q *recordingQueue) Enqueue(_ context.Context, j cronq.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	return nil
}

func (q *recordingQueue) Work(ctx context.Context, _ func(context.Context, cronq.Job) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (q *recordingQueue) snapshot() []cronq.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]cronq.Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// TestCronTickFiresEvent verifies the cron personality posts an envelope to
// the bus on each tick with src="cron" and a well-formed payload, and that
// Stop() shuts the goroutines down cleanly.
//
// Uses CronPeriod=1 because the controller floors anything <= 0 to 1 second
// (see cron.go), so this is the fastest a real cron tick can be; spread is
// also 0 at period<=1.
func TestCronTickFiresEvent(t *testing.T) {
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "cron",
			CronPeriod:    1,
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}

	cc := NewController(context.Background(), pu, newLocalQueue(t))
	cc.Start()

	// First tick should land within ~1s (wall-clock aligned to the next
	// whole second); allow 3s for slow CI.
	select {
	case env := <-bus:
		if env.Src != "cron" {
			t.Errorf("envelope.Src = %q, want \"cron\"", env.Src)
		}
		if got := gjson.Get(env.Payload.Raw, "_txc.src").String(); got != "cron" {
			t.Errorf("payload _txc.src = %q, want \"cron\"", got)
		}
		if got := gjson.Get(env.Payload.Raw, "_ts").String(); got == "" {
			t.Errorf("payload _ts is empty, want RFC3339 timestamp")
		}
		// The dispatch stamps the firing node and carries the bucket, so a
		// trace can show which chassis fired which bucket.
		if got := gjson.Get(env.Payload.Raw, "_txc.cron.node").String(); got == "" {
			t.Errorf("payload _txc.cron.node is empty, want the firing chassis id")
		}
		if got := gjson.Get(env.Payload.Raw, "_txc.cron.bucket").String(); got == "" {
			t.Errorf("payload _txc.cron.bucket is empty, want the wall-clock bucket")
		}
		// Unblock the dispatch goroutine which is waiting on resCh.
		env.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
	case <-time.After(3 * time.Second):
		t.Fatal("no cron tick envelope on bus within 3s")
	}

	// Stop should return promptly and not hang.
	done := make(chan struct{})
	go func() {
		cc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cron Stop() hung")
	}
}

// TestCronSubscribers verifies the opt-in: only non-revoked tenants
// that authored a `_cron` stack are returned, distinct, and a chassis
// with no dbcache (or no subscribers) yields none (off by default).
func TestCronSubscribers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`)
	mustExec(`CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INT, name TEXT)`)
	mustExec(`INSERT INTO tenants VALUES ('tnt_acme','acme',NULL),('tnt_beta','beta',NULL),('tnt_gone','gone','2020-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO ops VALUES
		('tnt_acme','_cron',100,'heartbeat'),
		('tnt_acme','_cron',0,'gate'),
		('tnt_acme','web',100,'hello'),
		('tnt_beta','web',100,'hello'),
		('tnt_gone','_cron',100,'heartbeat')`)

	// subscribers() never touches the queue; nil is fine here.
	cc := NewController(context.Background(), &processor.Unit{
		Conf:   config.Config{Personalities: "cron"},
		Logger: zap.NewNop(),
		Dbc:    &dbcache.DbCache{Db: db, Source: db},
	}, nil)

	got := cc.subscribers()
	if len(got) != 1 || got[0] != "acme" {
		t.Fatalf("subscribers = %v, want [acme] (beta has no _cron; gone is revoked; acme distinct)", got)
	}

	// No dbcache wired → no subscribers (off by default, no panic).
	ccNil := NewController(context.Background(), &processor.Unit{
		Conf: config.Config{Personalities: "cron"}, Logger: zap.NewNop(),
	}, nil)
	if s := ccNil.subscribers(); s != nil {
		t.Fatalf("nil dbcache subscribers = %v, want nil", s)
	}
}

// TestCronDisabledByPersonalityFlag verifies that omitting "cron" from the
// Personalities config makes Start() a no-op (no goroutine, no bus traffic).
func TestCronDisabledByPersonalityFlag(t *testing.T) {
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "web,tcp", // no "cron"
			CronPeriod:    1,
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}

	cc := NewController(context.Background(), pu, newLocalQueue(t))
	cc.Start()

	// Wait long enough that a tick *would* have fired if the personality
	// were active, then assert the bus is still empty.
	select {
	case env := <-bus:
		t.Errorf("cron fired despite being disabled: got envelope %+v", env)
	case <-time.After(1500 * time.Millisecond):
		// Expected.
	}

	// Stop is also a no-op when disabled — verify it doesn't hang or panic.
	done := make(chan struct{})
	go func() {
		cc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() hung when cron was disabled")
	}
}

// TestAlignDelay verifies ticks align to real wall-clock period boundaries,
// independent of boot time.
func TestAlignDelay(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 6, 2, h, m, s, 0, time.UTC) }
	cases := []struct {
		name   string
		period int
		now    time.Time
		want   time.Duration
	}{
		{"minute", 60, at(15, 24, 37), 23 * time.Second},        // → 15:25:00
		{"five-min", 300, at(15, 24, 37), 23 * time.Second},     // → 15:25:00
		{"hour", 3600, at(15, 24, 37), 2123 * time.Second},      // → 16:00:00
		{"exact-boundary", 60, at(15, 25, 0), 60 * time.Second}, // already on it → full period
	}
	for _, c := range cases {
		if got := alignDelay(c.now, c.period); got != c.want {
			t.Errorf("%s: alignDelay = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSpreadOffsetDeterministic verifies the per-tenant spread is stable,
// in range, and disabled at sub-second periods.
func TestSpreadOffsetDeterministic(t *testing.T) {
	if a, b := spreadOffset("acme", 60), spreadOffset("acme", 60); a != b {
		t.Fatalf("not deterministic: %v != %v", a, b)
	}
	if d := spreadOffset("anything", 1); d != 0 {
		t.Fatalf("period<=1 spread = %v, want 0", d)
	}
	if d := spreadOffset("acme", 60); d < 0 || d >= 60*time.Second {
		t.Fatalf("offset %v out of [0,60s)", d)
	}
}

// TestScheduleTickBucketAndFanout verifies one tick enqueues exactly the
// legacy `default` job plus one job per `_cron` subscriber, all sharing the
// tick's bucket, with payloads frozen at the tick instant (so the spread
// delay can't change what a tenant's WHEN sees).
func TestScheduleTickBucketAndFanout(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INT, name TEXT)`,
		`INSERT INTO tenants VALUES ('tnt_acme','acme',NULL),('tnt_beta','beta',NULL)`,
		`INSERT INTO ops VALUES ('tnt_acme','_cron',100,'a'),('tnt_beta','_cron',100,'b')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	rq := &recordingQueue{}
	cc := NewController(context.Background(), &processor.Unit{
		Conf:   config.Config{Personalities: "cron"},
		Logger: zap.NewNop(),
		Dbc:    &dbcache.DbCache{Db: db, Source: db},
	}, rq)

	now := time.Date(2026, 6, 2, 15, 24, 37, 0, time.UTC)
	// period=1 → spreadOffset is 0 → all enqueues happen synchronously.
	cc.scheduleTick(context.Background(), now, 7, 1)

	jobs := rq.snapshot()
	if len(jobs) != 3 {
		t.Fatalf("enqueued %d jobs, want 3 (1 default + 2 tenants): %+v", len(jobs), jobs)
	}

	wantBucket := "2026-06-02T15:24:37Z" // period=1 → truncated to the second, RFC3339
	defaults := 0
	tenants := map[string]bool{}
	for _, j := range jobs {
		if j.Bucket != wantBucket {
			t.Errorf("job %+v bucket = %q, want %q", j, j.Bucket, wantBucket)
		}
		// The bucket is also stamped into the payload so cron traces carry it.
		if got := gjson.Get(j.Payload, "_txc.cron.bucket").String(); got != wantBucket {
			t.Errorf("job %s/%s payload bucket = %q, want %q", j.Job, j.Tenant, got, wantBucket)
		}
		if got := gjson.Get(j.Payload, "_ts").String(); got != now.Format(time.RFC3339) {
			t.Errorf("job %s/%s _ts = %q, want %q", j.Job, j.Tenant, got, now.Format(time.RFC3339))
		}
		if got := gjson.Get(j.Payload, "_txc.cron.minute").Int(); got != 24 {
			t.Errorf("job %s/%s minute = %d, want 24", j.Job, j.Tenant, got)
		}
		switch j.Job {
		case "default":
			defaults++
			if j.Tenant != "" {
				t.Errorf("default job has tenant %q, want empty", j.Tenant)
			}
		case "_cron":
			tenants[j.Tenant] = true
			if got := gjson.Get(j.Payload, "_txc.cron.tenant").String(); got != j.Tenant {
				t.Errorf("payload tenant %q != job tenant %q", got, j.Tenant)
			}
		default:
			t.Errorf("unexpected job name %q", j.Job)
		}
	}
	if defaults != 1 {
		t.Errorf("default jobs = %d, want 1", defaults)
	}
	if !tenants["acme"] || !tenants["beta"] || len(tenants) != 2 {
		t.Errorf("tenant fan-out = %v, want {acme, beta}", tenants)
	}
}
