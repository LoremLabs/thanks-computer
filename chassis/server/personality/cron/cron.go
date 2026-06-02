package cron

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

type CronController struct {
	ctx      context.Context
	pu       *processor.Unit
	shutdown chan bool
	wg       sync.WaitGroup
	tickN    uint64 // monotonic tick counter since boot (stamped as _txc.cron.tick)
}

func NewController(ctx context.Context, pu *processor.Unit) *CronController {

	cc := &CronController{
		ctx:      ctx,
		pu:       pu,
		shutdown: make(chan bool),
	}

	return cc
}

func (cc *CronController) Start() {

	// stats.Record(wac.ctx, metrics.ServerRestarts.M(1))
	if strings.Contains(cc.pu.Conf.Personalities, "cron") {

		ctx, cronCancel := context.WithCancel(cc.ctx)
		cc.ctx = ctx

		go func() {
			cc.pu.Logger.Info("cron controller started")
			cc.wg.Add(1)

			period := cc.pu.Conf.CronPeriod
			if period <= 0 {
				period = 1 // minimum tick time, 1 second
			}

			for {

				select {
				case <-time.After(time.Duration(period) * time.Second):
					tick := atomic.AddUint64(&cc.tickN, 1)
					cc.wg.Add(1)
					go func() {
						defer cc.wg.Done()
						cc.fire(time.Now(), tick, period)
					}()
				case doshutdown := <-cc.shutdown:

					if doshutdown {
						cronCancel()
						cc.wg.Done()
						return
					}
				}
			}
		}()
	}
}

// fire builds the shared cron envelope for this tick and dispatches it:
// the legacy system-wide `job="default"` event (unchanged — backward
// compat with ingress.cron.jobs), plus one fan-out event per tenant
// that has authored a `_cron` stack (the opt-in subscription). Each
// dispatch is independent (its own rid/ResCh); a slow tenant never
// delays another or the next tick.
func (cc *CronController) fire(now time.Time, tick uint64, maxTime int) {
	min := now.Minute()

	// Base payload with the wall-clock fields a stack's visible txcl
	// WHEN can compare (txcl has no arithmetic/time funcs, so the
	// useful mod buckets are precomputed here — mechanism only).
	base, _ := sjson.Set("", "_txc.src", "cron")
	base, _ = sjson.Set(base, "_ts", now.Format(time.RFC3339))
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

	// Legacy: one system-wide tick keyed by job name "default" (kept
	// exactly as before for ingress.cron.jobs operators).
	def, _ := sjson.Set(base, "_txc.cron.job", "default")
	cc.dispatch(def, maxTime)

	// Fan-out: one tick per tenant that opted in by authoring a `_cron`
	// stack. detectTenantBody routes `src=cron`+`cron.tenant` to that
	// tenant's `_cron/0` (the sanctioned _sys→tenant re-tenant path).
	for _, slug := range cc.subscribers() {
		p, _ := sjson.Set(base, "_txc.cron.tenant", slug)
		p, _ = sjson.Set(p, "_txc.cron.job", "_cron")
		cc.dispatch(p, maxTime)
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

// dispatch sends one cron envelope onto the bus with its own rid +
// response channel, and drains the response on a tracked goroutine so
// the tick loop is never blocked by a slow stack.
func (cc *CronController) dispatch(payload string, maxTime int) {
	rid := hxid.NewTimeSort().String()
	payload, _ = sjson.Set(payload, "_txc.rid", rid)

	ctx, cancel := context.WithTimeout(cc.ctx, time.Duration(maxTime)*time.Second)
	ctx = context.WithValue(ctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload)
	envelope := event.PackageJSON(ctx, payload, resCh, "cron")

	cc.wg.Add(1)
	go func() {
		defer cc.wg.Done()
		defer cancel()
		cc.pu.Bus <- envelope
		select {
		case res := <-resCh:
			// A denied cron tick has no client to reject; surface it as
			// a warning so a suspended/drained tenant's skipped ticks
			// are visible (the customer stack already didn't run).
			if status, reason, ok := admission.Denied(res.Raw); ok {
				cc.pu.Logger.Warn("cron tick denied by admission",
					zap.String("rid", rid),
					zap.Int("status", status),
					zap.String("reason", reason))
			}
			// The tick stays visible via the Info "usage" line
			// (src=cron, tenant, rid, status, duration); this full
			// envelope dump is debug-only trace.
			if cc.pu.Logger.Core().Enabled(zap.DebugLevel) {
				cc.pu.Logger.Debug("cron res", zap.String("response", res.Raw))
			}
		case <-ctx.Done():
			cc.pu.Logger.Info("cron response timeout")
		case <-cc.ctx.Done():
			cc.pu.Logger.Info("cron response shutdown")
		}
	}()
}

func (cc *CronController) Stop() {
	if strings.Contains(cc.pu.Conf.Personalities, "cron") {
		cc.pu.Logger.Info("calling cron controller stop")

		// shut down workers
		cc.shutdown <- true
		cc.wg.Wait()
		cc.pu.Logger.Info("cron controller stopped")
	}
}
