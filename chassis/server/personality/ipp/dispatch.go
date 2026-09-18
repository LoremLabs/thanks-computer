package ipp

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

const (
	leaseBatch        = 50
	purgeEveryNPasses = 300
	markTimeout       = 5 * time.Second
)

// dispatchLoop hands committed jobs to the bus. It wakes on a nudge (a job
// just committed on THIS node — the normal path, milliseconds) and on a
// ticker (jobs committed elsewhere, retries, recovery).
func (c *Controller) dispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	c.pass(ctx) // anything committed before the last shutdown
	passes := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.nudge:
			c.pass(ctx)
		case <-ticker.C:
			c.pass(ctx)
			if passes++; passes >= purgeEveryNPasses {
				passes = 0
				if n, err := c.store.Purge(ctx, c.retention); err == nil && n > 0 {
					c.pu.Logger.Info("ipp: purged finished jobs", zap.Int64("count", n))
				}
			}
		}
	}
}

// pass is one sweep: recover, then lease and deliver. Errors are logged and
// the pass returns — the next one retries.
func (c *Controller) pass(ctx context.Context) {
	if admission.IsDraining() {
		// A draining node takes no new work onto its bus. Committed jobs
		// stay committed; another node, or this one after the deploy,
		// delivers them.
		return
	}
	if _, err := c.store.ReleaseStaleLeases(ctx, c.leaseStale); err != nil && ctx.Err() == nil {
		c.pu.Logger.Warn("ipp: release stale leases failed", zap.String("err", err.Error()))
	}
	if n, err := c.store.FailExhausted(ctx, c.maxAttempts); err == nil && n > 0 {
		c.pu.Logger.Warn("ipp: jobs failed after exhausting delivery attempts", zap.Int64("count", n))
		c.noteJob("undeliverable")
	}
	// A `receiving` row is either a Create-Job waiting for its document or an
	// upload in progress, and the row cannot tell which. So the cutoff is the
	// advertised wait PLUS the longest a legal upload can take (the read
	// deadline spool.go sets for a document of the maximum size) — never
	// shorter, or a slow but healthy upload would be failed under the client.
	if _, err := c.store.ReapReceiving(ctx, c.receiveTimeout+bodyBudget(c.maxBytes)); err != nil && ctx.Err() == nil {
		c.pu.Logger.Warn("ipp: reap receiving failed", zap.String("err", err.Error()))
	}
	jobs, err := c.store.LeaseCommitted(ctx, c.nodeID, leaseBatch)
	if err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("ipp: lease committed failed", zap.String("err", err.Error()))
		}
		return
	}
	for _, j := range jobs {
		c.deliver(ctx, j)
	}
}

// deliver puts one job's envelope on the bus. THE JOB ENDS HERE: the moment
// the bus accepts the envelope the row is `delivered` and IPP reports the
// job completed. Nobody waits for the run — a stack may take a second or
// three days, may fail, may need a human; a print job that reported
// "aborted" because a pony was still thinking would be a lie about a
// document that was handed over perfectly well.
func (c *Controller) deliver(ctx context.Context, j chipp.Job) {
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(j, rid, c.nodeID, c.now())

	// The envelope's context IS the run's context, so it must outlive this
	// function: chassis lifetime + the ordinary run ceiling, NOT the handoff
	// timeout below.
	runCtx, cancelRun := context.WithTimeout(c.ctx, c.runTimeout)
	runCtx = context.WithValue(runCtx, config.CtxKeyRid, rid)
	// Buffered and unread on purpose: the bus sends the run's single reply
	// under a select, so a reader is not required. The goroutine below only
	// releases the run context's timer when the run ends.
	resCh := make(chan event.Payload, 1)
	env := event.PackageJSON(runCtx, payload, resCh, "ipp")

	handoff, cancelHandoff := context.WithTimeout(ctx, c.dispatchTimeout)
	defer cancelHandoff()
	select {
	case c.pu.Bus <- env:
		go func() {
			select {
			case <-resCh:
			case <-runCtx.Done():
			}
			cancelRun()
		}()
		// Own context: a shutdown racing the handoff must not lose the mark
		// (a lost mark re-delivers after the lease goes stale — at-least-once
		// across a crash; stacks that care key on @ipp.job_id).
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
		err := c.store.MarkDelivered(mctx, j.ID, rid)
		cancel()
		if err != nil {
			c.pu.Logger.Warn("ipp: mark delivered failed", zap.String("job_id", j.ID), zap.String("err", err.Error()))
		}
		c.noteJob("delivered")
		c.pu.Logger.Info("ipp job delivered", zap.String("tenant", j.Tenant), zap.String("printer", j.Printer),
			zap.String("job_id", j.ID), zap.String("rid", rid), zap.Int("attempt", j.Attempts))
	case <-handoff.Done():
		// The bus did not take it. It never ran: hand the job back.
		cancelRun()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
		_ = c.store.ReleaseLease(rctx, j.ID)
		cancel()
		c.noteJob("bus_timeout")
	}
}

// buildEnvelope renders a committed job as the `_ipp/0` envelope. Pure, so
// the shape is unit-testable. Every `_txc.ipp.*` fact is head-stamped and
// read-only to the stack.
//
// What is absent is deliberate: no copies, media, sides or quality. They
// are properties of paper. And no bytes — the document crosses the stack
// boundary by reference (`document.sha256`, bare hex, exactly what
// `txco://blob/get sha256=` takes), so a 400 KB page and a 400 MB report
// make the same size envelope.
func buildEnvelope(j chipp.Job, rid, node string, now time.Time) string {
	b := jsonx.NewObject()
	b.Set("_txc.src", "ipp")
	b.Set("_txc.rid", rid)
	b.Set("_ts", now.UTC().Format(time.RFC3339))
	// Trusted: the zone's tenant, resolved by the head. detect-tenant turns
	// it into the route to `_ipp/0`.
	b.Set("_txc.ipp.tenant", j.Tenant)
	if j.ClientIP != "" {
		b.Set("_txc.client.ip", j.ClientIP)
	}
	b.Set("_txc.ipp.host", j.Host)
	b.Set("_txc.ipp.printer", j.Printer)
	// The URI the client configured: through the shared front door the path
	// carries the tenant's handle, so it is stored, not rebuilt. (A row from
	// before the column existed has the plain form.)
	uriPath := j.URIPath
	if uriPath == "" {
		uriPath = PathPrefix + "/" + j.Printer
	}
	b.Set("_txc.ipp.printer_uri", "ipps://"+j.Host+uriPath)
	b.Set("_txc.ipp.job_id", j.ID)
	b.Set("_txc.ipp.job_number", j.Number)
	b.Set("_txc.ipp.requesting_user", j.RequestingUser) // client-claimed: untrusted
	b.Set("_txc.ipp.job_name", j.JobName)
	b.Set("_txc.ipp.document.sha256", j.SHA256)
	b.Set("_txc.ipp.document.size", j.Size)
	b.Set("_txc.ipp.document.format", j.DocumentFormat)
	if j.DocumentName != "" {
		b.Set("_txc.ipp.document.name", j.DocumentName)
	}
	b.Set("_txc.ipp.submitted_at", j.CreatedAt.UTC().Format(time.RFC3339))
	b.Set("_txc.ipp.attempt", j.Attempts)
	b.Set("_txc.ipp.node", node)
	return b.String()
}
