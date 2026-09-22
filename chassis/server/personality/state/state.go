// Package state is the chassis `state` inlet: the dispatcher that presents
// committed state transitions (chassis/state's transactional outbox) into
// their tenant's `_state/0` stack, at least once each.
//
// It is the scheduled poller's shape — reclaim stale claims, atomically
// CLAIM due events (the claim is the fleet coordination), fire them onto
// the bus, record an outcome — with one difference that is the point of
// it: a claim closes at ACCEPTANCE, not completion. The processor tells
// the inlet (Envelope.Accepted) the moment an event has been routed into
// its tenant and admitted, before the tenant's stack runs, and the event
// is `done` right there. A long `_state` run never holds a delivery
// claim open, and a response timeout is never read as "delivered": an
// event that was not accepted goes back to the queue.
//
// The bus re-entry mirrors scheduled: detectTenantBody routes
// `_txc.src=state` + `_txc.state.tenant` into the tenant's `_state/0`.
package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	chstate "github.com/loremlabs/thanks-computer/chassis/state"
)

// SubscriptionStack is the tenant-level stack that receives state events.
// Its existence (an active version) is the subscription.
const SubscriptionStack = "_state"

const (
	// claimBatch bounds how many due events one pass claims.
	claimBatch = 200
	// purgeEveryNPasses runs the retention delete every Nth pass.
	purgeEveryNPasses = 120
	// lookupTimeout bounds the subscription query against the mirror.
	lookupTimeout = 5 * time.Second
	// storeTimeout bounds each outcome write. Outcomes use their own
	// context so a shutdown racing an acceptance cannot lose the mark.
	storeTimeout = 10 * time.Second
	// deniedBackoff is how long a suspended/disabled tenant's event waits
	// before it is offered again. Denials do not consume attempts, so this
	// is the whole rate.
	deniedBackoff = time.Hour
	// rateLimitedBackoff is the floor for a 429 with no retry_after.
	rateLimitedBackoff = 5 * time.Second
	// retryBase is the first backoff for a counted retry (accept timeout);
	// it doubles per attempt up to retryCap.
	retryBase = 5 * time.Second
	retryCap  = time.Hour
)

// Controller is the state inlet. It satisfies the server's Start()/Stop()
// controller contract.
type Controller struct {
	ctx    context.Context
	pu     *processor.Unit
	store  *chstate.Store
	nodeID string
	// wake is nudged by txco://state/transition after a commit on this
	// node, so the event is presented at once instead of next period.
	wake <-chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Seams for tests.
	subscribed func(ctx context.Context, tenant string) (bool, error)
	now        func() time.Time
}

// NewController builds the state controller around an opened store. A nil
// store (state personality not active) yields an inert controller whose
// Start is a no-op. wake may be nil.
func NewController(ctx context.Context, pu *processor.Unit, store *chstate.Store, wake <-chan struct{}) *Controller {
	c := &Controller{
		ctx:    ctx,
		pu:     pu,
		store:  store,
		nodeID: resolveNodeID(pu.Conf.Fqdn),
		wake:   wake,
		now:    func() time.Time { return time.Now().UTC() },
	}
	c.subscribed = c.snapshotSubscribed
	return c
}

// resolveNodeID picks a stable identity for THIS chassis (the claim owner
// and `@state.node`): the operator-set FQDN, else the OS hostname, else
// "local" — cron's rule.
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
	return c.pu.Conf.HasPersonality("state") && c.store != nil
}

// --- knobs, with the config defaults as fallbacks ------------------------

func secs(v, dflt int) time.Duration {
	if v <= 0 {
		return time.Duration(dflt) * time.Second
	}
	return time.Duration(v) * time.Second
}

func (c *Controller) period() time.Duration        { return secs(c.pu.Conf.StatePeriod, 5) }
func (c *Controller) acceptTimeout() time.Duration { return secs(c.pu.Conf.StateAcceptTimeout, 30) }
func (c *Controller) runTimeout() time.Duration    { return secs(c.pu.Conf.StateRunTimeout, 600) }
func (c *Controller) staleAfter() time.Duration    { return secs(c.pu.Conf.StateStaleAfter, 300) }
func (c *Controller) retention() time.Duration     { return secs(c.pu.Conf.StateRetention, 30*24*3600) }
func (c *Controller) maxInflight() int {
	if c.pu.Conf.StateMaxInflight <= 0 {
		return 32
	}
	return c.pu.Conf.StateMaxInflight
}
func (c *Controller) maxAttempts() int {
	if c.pu.Conf.StateMaxAttempts <= 0 {
		return 5
	}
	return c.pu.Conf.StateMaxAttempts
}

// Start launches the poll loop. No-op when the personality is off or no
// store was opened.
func (c *Controller) Start() {
	if !c.enabled() {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.ctx, c.cancel = ctx, cancel

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pu.Logger.Info("state dispatcher started",
			zap.Duration("period", c.period()), zap.String("node", c.nodeID))
		ticker := time.NewTicker(c.period())
		defer ticker.Stop()

		passes := 0
		more := c.poll(ctx) // present anything already due at boot
		for {
			if more {
				// The last pass claimed something: go again now, so the
				// next version of a record follows the one just accepted
				// without waiting a period.
				if ctx.Err() != nil {
					return
				}
				more = c.poll(ctx)
				continue
			}
			select {
			case <-ticker.C:
				more = c.poll(ctx)
				if passes++; passes >= purgeEveryNPasses {
					passes = 0
					c.purge(ctx)
				}
			case <-c.wake:
				more = c.poll(ctx)
			case <-ctx.Done():
				c.pu.Logger.Info("state dispatcher stopped")
				return
			}
		}
	}()
}

func (c *Controller) Stop() {
	if !c.enabled() {
		return
	}
	c.pu.Logger.Info("calling state controller stop")
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	c.pu.Logger.Info("state controller stopped")
}

// poll is one pass: reclaim stale claims, claim due events, fire them
// (bounded fan-out). It reports whether it claimed anything, so the loop
// can go straight round again.
func (c *Controller) poll(ctx context.Context) bool {
	// Draining: claim nothing new. An event claimed now would be presented
	// by a node that is leaving; left unclaimed, another node presents it.
	if admission.IsDraining() {
		return false
	}
	if n, err := c.store.ReclaimStale(ctx, c.staleAfter(), c.maxAttempts()); err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("state reclaim-stale failed", zap.Error(err))
		}
	} else if n > 0 {
		c.pu.Logger.Info("state reclaimed stale claims", zap.Int64("count", n))
	}

	claimed, err := c.store.ClaimDue(ctx, c.nodeID, claimBatch)
	if err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("state claim-due failed", zap.Error(err))
		}
		return false
	}
	if len(claimed) == 0 {
		return false
	}

	sem := make(chan struct{}, c.maxInflight())
	var wg sync.WaitGroup
	for _, ev := range claimed {
		select {
		case <-ctx.Done():
			wg.Wait()
			return false
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ev chstate.Event) {
			defer wg.Done()
			defer func() { <-sem }()
			c.fire(ctx, ev)
		}(ev)
	}
	wg.Wait()
	return true
}

// buildEnvelope is the event as the tenant's `_state/0` sees it:
// `@state.*` and `@state.cause.*`, all chassis-stamped under `_txc` so a
// mailed-in or posted envelope cannot mint them.
func buildEnvelope(ev chstate.Event, rid, node string, now time.Time) string {
	payload := "{}"
	payload, _ = sjson.Set(payload, "_txc.src", "state")
	payload, _ = sjson.Set(payload, "_txc.state.tenant", ev.Tenant)
	payload, _ = sjson.Set(payload, "_txc.state.event_id", ev.ID)
	payload, _ = sjson.Set(payload, "_txc.state.machine", ev.Machine)
	payload, _ = sjson.Set(payload, "_txc.state.id", ev.RecordID)
	payload, _ = sjson.Set(payload, "_txc.state.version", ev.Version)
	payload, _ = sjson.Set(payload, "_txc.state.from", ev.From)
	payload, _ = sjson.Set(payload, "_txc.state.to", ev.To)
	payload, _ = sjson.Set(payload, "_txc.state.attempt", ev.Attempts+1)
	payload, _ = sjson.Set(payload, "_txc.state.node", node)
	payload, _ = sjson.Set(payload, "_txc.state.fired_at", now.UTC().Format(time.RFC3339))
	payload, _ = sjson.Set(payload, "_txc.state.committed_at", ev.CreatedAt.UTC().Format(time.RFC3339Nano))
	payload, _ = sjson.Set(payload, "_txc.state.cause.source", ev.Cause.Source)
	payload, _ = sjson.Set(payload, "_txc.state.cause.stack", ev.Cause.Stack)
	payload, _ = sjson.Set(payload, "_txc.state.cause.trace", ev.Cause.Trace)
	payload, _ = sjson.Set(payload, "_txc.state.cause.run", ev.Cause.Run)
	payload, _ = sjson.Set(payload, "_ts", now.UTC().Format(time.RFC3339))
	payload, _ = sjson.Set(payload, "_txc.rid", rid)
	return payload
}

// storeCtx is the context for an outcome write: never the run's, never
// the poll's — a shutdown racing an acceptance must not lose the mark.
func storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

// retryBackoff is the wait before a counted re-presentation: 5s, 10s,
// 20s, … capped at an hour. attempt is the presentation that just failed
// (1-based).
func retryBackoff(attempt int) time.Duration {
	d := retryBase
	for i := 1; i < attempt && d < retryCap; i++ {
		d *= 2
	}
	if d > retryCap {
		d = retryCap
	}
	return d
}

// fire presents one claimed event and records an outcome:
//
//	accepted (routed + admitted, before the stack runs) → done
//	admission denied                                    → pending again, uncounted
//	response with no acceptance (never left _sys)       → skipped
//	bus refused the handoff                             → pending again, uncounted
//	nothing within the accept timeout                   → pending again, counted
//	shutdown mid-wait                                   → left claimed; ReclaimStale
func (c *Controller) fire(ctx context.Context, ev chstate.Event) {
	log := c.pu.Logger.With(zap.String("event_id", ev.ID), zap.String("tenant", ev.Tenant),
		zap.String("machine", ev.Machine), zap.String("id", ev.RecordID), zap.Int64("version", ev.Version))

	// The subscription: an active _state stack on a live tenant. Without
	// one the event is skipped for good — not replayed if a stack appears
	// later (the doc's contract: presentation needs a subscriber now).
	ok, err := c.subscribed(ctx, ev.Tenant)
	if err != nil {
		log.Warn("state subscription check failed; will retry", zap.Error(err))
		c.retry(ev, retryBase, "subscription check failed: "+err.Error(), false)
		return
	}
	if !ok {
		sctx, cancel := storeCtx()
		defer cancel()
		if err := c.store.MarkSkipped(sctx, ev.ID, "no active _state stack"); err != nil {
			log.Warn("state mark-skipped failed", zap.Error(err))
		}
		log.Info("state event skipped: no active _state stack")
		return
	}

	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(ev, rid, c.nodeID, c.now())

	// The envelope's context IS the run's context, so it must outlive
	// this function: chassis lifetime + the run ceiling, not the accept
	// wait below (ipp's rule).
	runCtx, cancelRun := context.WithTimeout(c.ctx, c.runTimeout())
	runCtx = context.WithValue(runCtx, config.CtxKeyRid, rid)
	resCh := make(chan event.Payload, 1)
	accepted := make(chan event.Acceptance, 1)
	env := event.PackageJSON(runCtx, payload, resCh, "state")
	env.Accepted = accepted

	wait := time.NewTimer(c.acceptTimeout())
	defer wait.Stop()

	select {
	case c.pu.Bus <- env:
	case <-wait.C:
		cancelRun()
		log.Warn("state dispatch: bus refused the handoff")
		c.retry(ev, retryBase, "bus refused handoff", false)
		return
	case <-ctx.Done():
		cancelRun()
		return // shutting down: left claimed for ReclaimStale
	}
	// Release the run's timer when the run ends, whenever that is.
	go func() {
		select {
		case <-resCh:
		case <-runCtx.Done():
		}
		cancelRun()
	}()

	select {
	case a := <-accepted:
		c.accept(log, ev, rid, a)
	case res := <-resCh:
		// A run that was accepted answers after its acceptance; both may
		// be ready, so acceptance wins the tie.
		select {
		case a := <-accepted:
			c.accept(log, ev, rid, a)
			return
		default:
		}
		if status, reason, ok := admission.Denied(res.Raw); ok {
			backoff := deniedBackoff
			if status == 429 {
				backoff = rateLimitedBackoff
				if ra := gjson.Get(res.Raw, "_txc.admission.retry_after").Int(); ra > 0 && time.Duration(ra)*time.Second > backoff {
					backoff = time.Duration(ra) * time.Second
				}
			}
			log.Warn("state event denied by admission; will retry",
				zap.Int("status", status), zap.String("reason", reason), zap.Duration("backoff", backoff))
			c.retry(ev, backoff, "admission denied: "+reason, false)
			return
		}
		// Answered without ever leaving _sys: the tenant vanished between
		// the subscription check and boot, or boot did not route it.
		sctx, cancel := storeCtx()
		defer cancel()
		if err := c.store.MarkSkipped(sctx, ev.ID, "run ended in _sys without acceptance"); err != nil {
			log.Warn("state mark-skipped failed", zap.Error(err))
		}
		log.Warn("state event skipped: the run was never accepted for the tenant", zap.String("rid", rid))
	case <-wait.C:
		// Neither signal: the boot stage is wedged or the node is
		// overloaded. Counted — a late acceptance means the stack sees this
		// event twice, which at-least-once allows and @state.attempt shows.
		log.Warn("state event not accepted within the accept timeout; will retry",
			zap.Duration("timeout", c.acceptTimeout()), zap.Int("attempt", ev.Attempts+1))
		c.retry(ev, retryBackoff(ev.Attempts+1), "accept timeout", true)
	case <-ctx.Done():
		return // shutting down: left claimed for ReclaimStale
	}
}

func (c *Controller) accept(log *zap.Logger, ev chstate.Event, rid string, a event.Acceptance) {
	if a.Tenant != ev.Tenant {
		// Cannot happen through detectTenantBody, which routes on the
		// stamped tenant; say so loudly if it ever does.
		log.Error("state event accepted for a different tenant than its record",
			zap.String("accepted_tenant", a.Tenant), zap.String("accepted_stack", a.Stack))
	}
	sctx, cancel := storeCtx()
	defer cancel()
	if err := c.store.MarkDone(sctx, ev.ID, rid); err != nil {
		log.Warn("state mark-done failed", zap.Error(err))
		return
	}
	log.Info("state event presented", zap.String("rid", rid), zap.String("stack", a.Stack), zap.Int("attempt", ev.Attempts+1))
}

func (c *Controller) retry(ev chstate.Event, backoff time.Duration, reason string, counted bool) {
	sctx, cancel := storeCtx()
	defer cancel()
	if err := c.store.Retry(sctx, ev.ID, backoff, reason, counted, c.maxAttempts()); err != nil {
		c.pu.Logger.Warn("state retry failed", zap.String("event_id", ev.ID), zap.Error(err))
	}
}

// purge runs the retention delete for terminal rows. Best-effort; logged.
func (c *Controller) purge(ctx context.Context) {
	if n, err := c.store.Purge(ctx, c.retention()); err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("state purge failed", zap.Error(err))
		}
	} else if n > 0 {
		c.pu.Logger.Info("state purged terminal events", zap.Int64("count", n))
	}
}

// snapshotSubscribed asks the mirror whether the tenant is live and has an
// active `_state` stack (the ipp/imap heads' subscription query).
func (c *Controller) snapshotSubscribed(ctx context.Context, slug string) (bool, error) {
	if c.pu == nil || c.pu.Dbc == nil {
		return false, nil
	}
	db := c.pu.Dbc.Snapshot()
	if db == nil {
		return false, nil
	}
	qctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var one int
	err := db.QueryRowContext(qctx, `SELECT 1 FROM stacks s
	                       JOIN tenants t ON t.tenant_id = s.tenant_id
	                      WHERE t.slug = ? AND t.revoked_at IS NULL
	                        AND s.name = ? AND s.active_version IS NOT NULL
	                      LIMIT 1`, slug, SubscriptionStack).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
