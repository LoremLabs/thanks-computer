package cron

import (
	"context"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	cronq "github.com/loremlabs/thanks-computer/chassis/cron"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// workerRetryBackoff is how long the worker waits before re-entering
// queue.Work after a non-cancellation error (e.g. a broker-backed queue
// whose stream isn't provisioned yet). Keeps cron self-healing.
const workerRetryBackoff = 5 * time.Second

type CronController struct {
	ctx      context.Context
	pu       *processor.Unit
	queue    cronq.Queue
	nodeID   string // this chassis's identity, stamped on each dispatch (_txc.cron.node)
	shutdown chan bool
	wg       sync.WaitGroup
	tickN    uint64 // monotonic tick counter since boot (stamped as _txc.cron.tick)
}

// NewController builds the cron controller around an opened cron Queue
// (the scheduler enqueues onto it; the worker drains it). Mirrors
// controlpublish.NewController(ctx, pu, sink).
func NewController(ctx context.Context, pu *processor.Unit, queue cronq.Queue) *CronController {
	return &CronController{
		ctx:      ctx,
		pu:       pu,
		queue:    queue,
		nodeID:   resolveNodeID(pu.Conf.Fqdn),
		shutdown: make(chan bool),
	}
}

// resolveNodeID picks a stable identity for THIS chassis to stamp on cron
// dispatches, so a trace shows which node actually fired a tick (the live
// trace tail fans in from every node with no host column otherwise).
// Prefers the operator-set FQDN; falls back to the OS hostname (distinct
// per container in a fleet); "local" as a last resort. For node
// attribution to distinguish two chassis, they need distinct FQDNs or
// hostnames — the usual case.
func resolveNodeID(fqdn string) string {
	if fqdn != "" {
		return fqdn
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// Start launches the worker (drains the queue, dispatches) and the
// scheduler (wall-clock-aligned ticks, enqueues per target). No-op when
// the cron personality is disabled or no queue is wired (data-plane-only
// chassis / tests).
func (cc *CronController) Start() {
	if !strings.Contains(cc.pu.Conf.Personalities, "cron") {
		return
	}
	if cc.queue == nil {
		return
	}

	ctx, cronCancel := context.WithCancel(cc.ctx)
	cc.ctx = ctx

	period := cc.pu.Conf.CronPeriod
	if period <= 0 {
		period = 1 // minimum tick time, 1 second
	}

	// Worker: drain the queue and dispatch. queue.Work blocks until ctx
	// is cancelled; we re-enter it on a non-cancellation error (with
	// backoff) so a broker queue whose stream isn't provisioned yet
	// self-heals once it appears, instead of permanently disabling cron.
	cc.wg.Add(1)
	go func() {
		defer cc.wg.Done()
		cc.pu.Logger.Info("cron worker started", zap.String("queue", cc.queue.Name()))
		for {
			err := cc.queue.Work(ctx, cc.dispatch)
			if ctx.Err() != nil {
				cc.pu.Logger.Info("cron worker stopped")
				return
			}
			cc.pu.Logger.Warn("cron worker exited; retrying", zap.Error(err))
			select {
			case <-time.After(workerRetryBackoff):
			case <-ctx.Done():
				cc.pu.Logger.Info("cron worker stopped")
				return
			}
		}
	}()

	// Scheduler: wall-clock-aligned ticks → enqueue per target. Each
	// tick's fan-out runs in its own tracked goroutine so a full queue
	// buffer (backpressure) never delays the next tick or shutdown.
	cc.wg.Add(1)
	go func() {
		defer cc.wg.Done()
		cc.pu.Logger.Info("cron scheduler started", zap.Int("period", period))
		for {
			select {
			case <-time.After(alignDelay(time.Now(), period)):
				tick := atomic.AddUint64(&cc.tickN, 1)
				now := time.Now()
				cc.wg.Add(1)
				go func() {
					defer cc.wg.Done()
					cc.scheduleTick(ctx, now, tick, period)
				}()
			case doshutdown := <-cc.shutdown:
				if doshutdown {
					cronCancel()
					return
				}
			}
		}
	}()
}

// alignDelay returns how long to sleep so the next tick fires on the next
// wall-clock period boundary (period=60 → top of the next minute; 300 →
// next :00/:05/:10…), so _txc.cron.minute and the modN buckets land on
// real clock boundaries regardless of boot time. (Periods that don't
// divide the hour, e.g. 7s, align to the Unix-epoch grid — acceptable;
// the common 60/300/900/3600 divide cleanly.)
func alignDelay(now time.Time, period int) time.Duration {
	p := time.Duration(period) * time.Second
	if p <= 0 {
		return time.Second
	}
	next := now.Truncate(p).Add(p)
	d := next.Sub(now)
	if d <= 0 {
		d = p
	}
	return d
}

// spreadOffset returns a deterministic [0, period) second offset for a
// slug so the per-tick fan-out is smeared evenly across the period
// (stampede fix). fnv-1a is stable across processes/nodes (no per-boot
// randomization), so the same tenant always lands in the same slot.
func spreadOffset(slug string, period int) time.Duration {
	if period <= 1 {
		return 0 // sub-second spread not worth it at 1s ticks
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	return time.Duration(h.Sum32()%uint32(period)) * time.Second
}

// scheduleTick builds the shared cron envelope for this tick ONCE and
// enqueues it: the system-wide `job="default"` event when --cron-system-tick
// is set, plus one fan-out event per tenant that authored a `_cron` stack,
// each delayed by its deterministic spread offset. The payload (and thus
// _ts + modN buckets) is frozen here at tick time, BEFORE any spread delay —
// so a tenant's WHEN sees the tick instant, not the delayed dispatch instant.
func (cc *CronController) scheduleTick(ctx context.Context, now time.Time, tick uint64, period int) {
	min := now.Minute()
	// Canonical bucket at SECOND resolution (RFC3339), aligned to the
	// period grid. Second resolution (not minute) so sub-minute periods get
	// distinct buckets — the NATS path uses this as the Nats-Msg-Id dedup
	// key, and a minute-resolution key would collapse every sub-minute tick
	// in a minute into one. At the default 60s period the seconds are :00.
	bucket := now.Truncate(time.Duration(period) * time.Second).UTC().Format(time.RFC3339)

	// Base payload with the wall-clock fields a stack's visible txcl WHEN
	// can compare (txcl has no arithmetic/time funcs, so the useful mod
	// buckets are precomputed here — mechanism only).
	base, _ := sjson.Set("", "_txc.src", "cron")
	base, _ = sjson.Set(base, "_ts", now.Format(time.RFC3339))
	// The canonical wall-clock bucket this tick belongs to — the fleet
	// dedup key, also stamped here so every cron trace carries it (group
	// traces by tenant+bucket to see exactly one fire per bucket).
	base, _ = sjson.Set(base, "_txc.cron.bucket", bucket)
	base, _ = sjson.Set(base, "_txc.cron.minute", min)
	base, _ = sjson.Set(base, "_txc.cron.hour", now.Hour())
	base, _ = sjson.Set(base, "_txc.cron.dom", now.Day())
	base, _ = sjson.Set(base, "_txc.cron.dow", int(now.Weekday())) // Sun=0
	base, _ = sjson.Set(base, "_txc.cron.month", int(now.Month()))
	base, _ = sjson.Set(base, "_txc.cron.year", now.Year())
	base, _ = sjson.Set(base, "_txc.cron.tick", tick)
	base, _ = sjson.Set(base, "_txc.cron.mod5", min%5)
	base, _ = sjson.Set(base, "_txc.cron.mod10", min%10)
	base, _ = sjson.Set(base, "_txc.cron.mod15", min%15)
	base, _ = sjson.Set(base, "_txc.cron.mod30", min%30)
	if cc.pu.Conf.DebugPrivate {
		base, _ = sjson.Set(base, "_txc.flag_private", true)
	}

	// A system-wide tick keyed by job name "default" (no tenant, no
	// spread), for scheduled work hooked in _sys/boot or routed by a
	// "default" cron-job ingress binding. Enabled with --cron-system-tick;
	// the per-tenant fan-out below runs regardless.
	if cc.pu.Conf.CronSystemTick {
		def, _ := sjson.Set(base, "_txc.cron.job", "default")
		cc.enqueue(ctx, cronq.Job{Job: "default", Bucket: bucket, MaxTime: period, Payload: def})
	}

	// Fan-out: one tick per tenant that opted in by authoring a `_cron`
	// stack, smeared across the period by its deterministic offset.
	// detectTenantBody routes `src=cron`+`cron.tenant` to that tenant's
	// `_cron/0` (the sanctioned _sys→tenant re-tenant path).
	for _, slug := range cc.subscribers() {
		p, _ := sjson.Set(base, "_txc.cron.tenant", slug)
		p, _ = sjson.Set(p, "_txc.cron.job", "_cron")
		job := cronq.Job{Tenant: slug, Job: "_cron", Bucket: bucket, MaxTime: period, Payload: p}

		delay := spreadOffset(slug, period)
		if delay <= 0 {
			cc.enqueue(ctx, job)
			continue
		}
		cc.wg.Add(1)
		go func(j cronq.Job, d time.Duration) {
			defer cc.wg.Done()
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				cc.enqueue(ctx, j)
			case <-ctx.Done():
			}
		}(job, delay)
	}
}

// enqueue submits a job to the queue, logging a non-shutdown failure.
func (cc *CronController) enqueue(ctx context.Context, j cronq.Job) {
	if err := cc.queue.Enqueue(ctx, j); err != nil {
		if ctx.Err() != nil {
			return // expected during shutdown; not worth a warning
		}
		cc.pu.Logger.Warn("cron enqueue failed",
			zap.String("tenant", j.Tenant),
			zap.String("job", j.Job),
			zap.String("err", err.Error()))
	}
}

// subscribers returns the slugs of non-revoked tenants that have a
// `_cron` stack, read from the in-memory dbcache snapshot (snapshot
// pointer captured under the lock, queried unlocked — no per-tick disk
// hit, no long-held lock). Empty when nothing opted in (off-by-default)
// or when the dbcache isn't wired (tests / data-plane-less chassis).
func (cc *CronController) subscribers() []string {
	if cc.pu.Dbc == nil {
		return nil
	}
	cc.pu.Dbc.Mu.Lock()
	snap := cc.pu.Dbc.Db
	cc.pu.Dbc.Mu.Unlock()
	if snap == nil {
		return nil
	}
	rows, err := snap.QueryContext(cc.ctx,
		`SELECT DISTINCT t.slug
		   FROM ops o
		   JOIN tenants t ON t.tenant_id = o.tenant_id
		  WHERE o.stack = '_cron' AND t.revoked_at IS NULL`)
	if err != nil {
		cc.pu.Logger.Warn("cron subscribers query failed", zap.String("err", err.Error()))
		return nil
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			cc.pu.Logger.Warn("cron subscribers scan failed", zap.String("err", err.Error()))
			return slugs
		}
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// dispatch is the worker fn handed to queue.Work: it sends one cron
// envelope onto the bus with its own rid + response channel and drains
// the response. By the time it runs, the queue has claimed the job for
// exactly one worker. The bus send is guarded by the dispatch deadline so
// a stopped bus at shutdown can't block the worker forever.
func (cc *CronController) dispatch(workerCtx context.Context, job cronq.Job) error {
	rid := hxid.NewTimeSort().String()
	payload, _ := sjson.Set(job.Payload, "_txc.rid", rid)
	// Stamp the firing chassis: in fleet mode the worker that pulls the
	// job is the one that runs it, so this is the node that actually fired
	// the bucket (visible in the trace; the scheduling node may differ).
	payload, _ = sjson.Set(payload, "_txc.cron.node", cc.nodeID)

	maxTime := job.MaxTime
	if maxTime <= 0 {
		maxTime = 1
	}
	ctx, cancel := context.WithTimeout(workerCtx, time.Duration(maxTime)*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload)
	envelope := event.PackageJSON(ctx, payload, resCh, "cron")

	select {
	case cc.pu.Bus <- envelope:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case res := <-resCh:
		// A denied cron tick has no client to reject; surface it as a
		// warning so a suspended/drained tenant's skipped ticks are
		// visible (the customer stack already didn't run).
		if status, reason, ok := admission.Denied(res.Raw); ok {
			cc.pu.Logger.Warn("cron tick denied by admission",
				zap.String("rid", rid),
				zap.Int("status", status),
				zap.String("reason", reason))
		}
		// The tick stays visible via the Info "usage" line (src=cron,
		// tenant, rid, status, duration); this full envelope dump is
		// debug-only trace.
		if cc.pu.Logger.Core().Enabled(zap.DebugLevel) {
			cc.pu.Logger.Debug("cron res", zap.String("response", res.Raw))
		}
	case <-ctx.Done():
		cc.pu.Logger.Info("cron response timeout")
	}
	return nil
}

func (cc *CronController) Stop() {
	if !strings.Contains(cc.pu.Conf.Personalities, "cron") || cc.queue == nil {
		return
	}
	cc.pu.Logger.Info("calling cron controller stop")

	// shut down scheduler (which cancels the shared ctx → worker + spread
	// timers drain), then wait for every tracked goroutine.
	cc.shutdown <- true
	cc.wg.Wait()
	cc.pu.Logger.Info("cron controller stopped")
}
