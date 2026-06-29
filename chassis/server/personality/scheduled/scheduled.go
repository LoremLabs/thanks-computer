// Package scheduled is the chassis `scheduled` inlet: a per-period poll loop
// that drains the scheduled_events store (chassis/scheduled) and fires due
// events into their tenant's `_scheduled/0` stack.
//
// Unlike cron (which fans out one tick per tenant every period), the
// scheduler is table-driven: each pass reclaims stale claims, atomically
// CLAIMS due rows (so exactly one fleet node fires each — the claim is the
// coordination), dispatches the stored payload onto the bus, and marks the
// row done/failed. The bus re-entry mirrors cron: detectTenantBody routes
// `_txc.src=scheduled` + `_txc.scheduled.tenant` into the tenant's
// `_scheduled/0` (the sanctioned _sys→tenant pin).
package scheduled

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	sched "github.com/loremlabs/thanks-computer/chassis/scheduled"
)

const (
	// claimBatch bounds how many due rows one poll pass claims, so a backlog
	// drains over several passes instead of one giant burst.
	claimBatch = 200
	// fireDispatchTimeout caps a single event's pipeline run (it may send an
	// email). Generous; the at-most-once bias on response timeout is in fire().
	fireDispatchTimeout = 60 * time.Second
	// purgeEveryNPasses runs the retention delete every Nth poll pass so it
	// isn't on every tick (cheap, but pointless to run each period).
	purgeEveryNPasses = 30
)

// Controller is the scheduled inlet. It satisfies the server's
// Start()/Stop() controller contract.
type Controller struct {
	ctx    context.Context
	pu     *processor.Unit
	store  *sched.Store
	nodeID string // this chassis's identity, stamped as _txc.scheduled.node

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewController builds the scheduled controller around an opened store. A nil
// store (scheduled personality not active / no --db-scheduled-dsn) yields an
// inert controller whose Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, store *sched.Store) *Controller {
	return &Controller{
		ctx:    ctx,
		pu:     pu,
		store:  store,
		nodeID: resolveNodeID(pu.Conf.Fqdn),
	}
}

// resolveNodeID picks a stable identity for THIS chassis to stamp on fired
// events + record as the claim owner. Mirrors cron.resolveNodeID: prefer the
// operator-set FQDN, fall back to the OS hostname, then "local".
func resolveNodeID(fqdn string) string {
	if fqdn != "" {
		return fqdn
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

func (c *Controller) enabled() bool {
	return strings.Contains(c.pu.Conf.Personalities, "scheduled") && c.store != nil
}

// Start launches the poll loop. No-op when the scheduled personality is
// disabled or no store was opened (data-plane-only chassis / tests).
func (c *Controller) Start() {
	if !c.enabled() {
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	c.ctx, c.cancel = ctx, cancel

	period := c.pu.Conf.ScheduledPeriod
	if period <= 0 {
		period = 20
	}
	maxInflight := c.pu.Conf.ScheduledMaxInflight
	if maxInflight <= 0 {
		maxInflight = 32
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pu.Logger.Info("scheduled poller started",
			zap.Int("period", period), zap.String("node", c.nodeID))
		ticker := time.NewTicker(time.Duration(period) * time.Second)
		defer ticker.Stop()

		c.poll(ctx, maxInflight) // fire anything already due at boot
		passes := 0
		for {
			select {
			case <-ticker.C:
				c.poll(ctx, maxInflight)
				if passes++; passes >= purgeEveryNPasses {
					passes = 0
					c.purge(ctx)
				}
			case <-ctx.Done():
				c.pu.Logger.Info("scheduled poller stopped")
				return
			}
		}
	}()
}

func (c *Controller) Stop() {
	if !c.enabled() {
		return
	}
	c.pu.Logger.Info("calling scheduled controller stop")
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	c.pu.Logger.Info("scheduled controller stopped")
}

// poll is one pass: reclaim stale claims, claim due rows, fire them (bounded
// fan-out). Errors are logged (unless we're shutting down) and the pass
// returns — the next tick retries.
func (c *Controller) poll(ctx context.Context, maxInflight int) {
	stale := time.Duration(c.pu.Conf.ScheduledStaleAfter) * time.Second
	if stale <= 0 {
		stale = 600 * time.Second
	}
	if n, err := c.store.ReclaimStale(ctx, stale); err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("scheduled reclaim-stale failed", zap.Error(err))
		}
	} else if n > 0 {
		c.pu.Logger.Info("scheduled reclaimed stale claims", zap.Int64("count", n))
	}

	claimed, err := c.store.ClaimDue(ctx, c.nodeID, claimBatch)
	if err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("scheduled claim-due failed", zap.Error(err))
		}
		return
	}
	if len(claimed) == 0 {
		return
	}

	sem := make(chan struct{}, maxInflight)
	var wg sync.WaitGroup
	for _, ev := range claimed {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ev sched.Claimed) {
			defer wg.Done()
			defer func() { <-sem }()
			c.fire(ctx, ev)
		}(ev)
	}
	wg.Wait()
}

// fire dispatches one claimed event onto the bus, re-tenanting it into the
// tenant's `_scheduled/0`, and records the terminal state:
//   - response received  → MarkDone
//   - response timeout    → MarkFailed (it WAS dispatched; at-most-once bias —
//     don't risk a double-fire by leaving it claimable)
//   - couldn't even reach the bus (shutdown) → leave 'claimed' so the stale
//     reclaim retries it later (it never ran)
func (c *Controller) fire(ctx context.Context, ev sched.Claimed) {
	rid := hxid.NewTimeSort().String()
	now := time.Now().UTC().Format(time.RFC3339)

	payload := "{}"
	payload, _ = sjson.Set(payload, "_txc.src", "scheduled")
	payload, _ = sjson.Set(payload, "_txc.scheduled.tenant", ev.Tenant)
	payload, _ = sjson.Set(payload, "_txc.scheduled.idempotency_key", ev.IdempotencyKey)
	payload, _ = sjson.Set(payload, "_txc.scheduled.event_id", ev.ID)
	payload, _ = sjson.Set(payload, "_txc.scheduled.node", c.nodeID)
	payload, _ = sjson.Set(payload, "_txc.scheduled.fired_at", now)
	payload, _ = sjson.SetRaw(payload, "_txc.scheduled.payload", payloadOrEmpty(ev.Payload))
	payload, _ = sjson.Set(payload, "_ts", now)
	payload, _ = sjson.Set(payload, "_txc.rid", rid)

	dctx, cancel := context.WithTimeout(ctx, fireDispatchTimeout)
	defer cancel()
	dctx = context.WithValue(dctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(dctx, payload, resCh, "scheduled")

	select {
	case c.pu.Bus <- envelope:
	case <-dctx.Done():
		// Never reached the bus (shutdown / bus stopped) — it didn't run.
		// Leave the row 'claimed'; ReclaimStale will hand it to another pass.
		c.pu.Logger.Warn("scheduled dispatch bus timeout",
			zap.String("event_id", ev.ID), zap.String("tenant", ev.Tenant))
		return
	}

	select {
	case res := <-resCh:
		if status, reason, ok := admission.Denied(res.Raw); ok {
			c.pu.Logger.Warn("scheduled event denied by admission",
				zap.String("event_id", ev.ID), zap.String("tenant", ev.Tenant),
				zap.Int("status", status), zap.String("reason", reason))
		}
		if err := c.store.MarkDone(dctx, ev.ID); err != nil {
			c.pu.Logger.Warn("scheduled mark-done failed",
				zap.String("event_id", ev.ID), zap.Error(err))
			return
		}
		c.pu.Logger.Info("scheduled event fired",
			zap.String("event_id", ev.ID), zap.String("tenant", ev.Tenant),
			zap.String("idempotency_key", ev.IdempotencyKey), zap.String("rid", rid))
	case <-dctx.Done():
		// Dispatched but the response didn't drain in time. It probably ran;
		// fail it terminally rather than risk re-firing (at-most-once bias).
		c.pu.Logger.Warn("scheduled response timeout",
			zap.String("event_id", ev.ID), zap.String("tenant", ev.Tenant))
		if err := c.store.MarkFailed(context.Background(), ev.ID); err != nil {
			c.pu.Logger.Warn("scheduled mark-failed failed",
				zap.String("event_id", ev.ID), zap.Error(err))
		}
	}
}

// purge runs the retention delete for terminal rows. Best-effort; logged.
func (c *Controller) purge(ctx context.Context) {
	retention := time.Duration(c.pu.Conf.ScheduledRetention) * time.Second
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if n, err := c.store.Purge(ctx, retention); err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("scheduled purge failed", zap.Error(err))
		}
	} else if n > 0 {
		c.pu.Logger.Info("scheduled purged terminal events", zap.Int64("count", n))
	}
}

// payloadOrEmpty returns the stored payload as a raw JSON object, defaulting
// to {} (the store validates JSON on enqueue, so this is belt-and-suspenders).
func payloadOrEmpty(p []byte) string {
	if len(p) == 0 {
		return "{}"
	}
	return string(p)
}
