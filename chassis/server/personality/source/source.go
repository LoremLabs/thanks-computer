// Package source is the chassis `source` inlet: a per-period poll loop that
// claims due external-source watchers from the tenant_sources runtime table
// (chassis/source), opens each (a remote IMAP mailbox today), reads what is
// new past the source's durable cursor, fires each item into the source's
// `<stack>/_source/0`, and — once an item's run succeeds — acknowledges it at
// the source (marks it read, or moves it aside). It is the outbound mirror of
// the LMTP inlet: mail the tenant already receives elsewhere, pulled in
// without changing their MX or forwarding.
//
// Coordination is the CLAIM, exactly as the scheduled inlet: one conditional
// UPDATE flips a due source idle→claimed and RETURNING says "I won", so a
// single fleet node polls a given mailbox per cycle even when several nodes
// share the runtime DB. There is no leader election.
//
// The mailbox credential is materialized from the encrypted secret store
// inside the claim, handed to the connection's Open, and zeroed the instant
// Open returns — it is never stored on a struct, and the SecretBag machinery
// keeps it out of every envelope, trace, and log by construction.
package source

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/mail"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	src "github.com/loremlabs/thanks-computer/chassis/source"
)

// Controller is the source inlet. It satisfies the Start()/Stop() contract.
type Controller struct {
	ctx    context.Context
	pu     *processor.Unit
	store  *src.Store
	guard  egress.Guard
	nodeID string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewController builds the source controller around the runtime-DB source
// store and the egress guard. A nil store (source personality inactive)
// yields an inert controller whose Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, store *src.Store, guard egress.Guard) *Controller {
	return &Controller{
		ctx:    ctx,
		pu:     pu,
		store:  store,
		guard:  guard,
		nodeID: resolveNodeID(pu.Conf.Fqdn),
	}
}

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
	return c.pu != nil && c.pu.Conf.HasPersonality("source") && c.store != nil
}

// Start launches the poll loop. No-op when the source personality is disabled
// or no store was opened.
func (c *Controller) Start() {
	if !c.enabled() {
		return
	}
	if c.pu.Secrets == nil {
		c.pu.Logger.Warn("source personality enabled but no secret store; sources needing a credential will fail to open")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.ctx, c.cancel = ctx, cancel

	period := c.pu.Conf.SourcePeriod
	if period <= 0 {
		period = 30
	}
	maxInflight := c.pu.Conf.SourceMaxInflight
	if maxInflight <= 0 {
		maxInflight = 8
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pu.Logger.Info("source poller started",
			zap.Int("period", period), zap.String("node", c.nodeID))
		ticker := time.NewTicker(time.Duration(period) * time.Second)
		defer ticker.Stop()
		c.poll(ctx, maxInflight)
		for {
			select {
			case <-ticker.C:
				c.poll(ctx, maxInflight)
			case <-ctx.Done():
				c.pu.Logger.Info("source poller stopped")
				return
			}
		}
	}()
}

func (c *Controller) Stop() {
	if !c.enabled() {
		return
	}
	c.pu.Logger.Info("calling source controller stop")
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	c.pu.Logger.Info("source controller stopped")
}

// poll is one pass: reclaim stale claims, claim due sources, handle each
// (bounded fan-out).
func (c *Controller) poll(ctx context.Context, maxInflight int) {
	stale := time.Duration(c.pu.Conf.SourceStaleAfter) * time.Second
	if stale <= 0 {
		stale = 600 * time.Second
	}
	if n, err := c.store.ReclaimStale(ctx, stale); err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("source reclaim-stale failed", zap.Error(err))
		}
	} else if n > 0 {
		c.pu.Logger.Info("source reclaimed stale claims", zap.Int64("count", n))
	}

	claimed, err := c.store.ClaimDue(ctx, c.nodeID, maxInflight*4)
	if err != nil {
		if ctx.Err() == nil {
			c.pu.Logger.Warn("source claim-due failed", zap.Error(err))
		}
		return
	}
	if len(claimed) == 0 {
		return
	}

	sem := make(chan struct{}, maxInflight)
	var wg sync.WaitGroup
	for _, cl := range claimed {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(cl src.Claimed) {
			defer wg.Done()
			defer func() { <-sem }()
			c.handleSource(ctx, cl)
		}(cl)
	}
	wg.Wait()
}

// handleSource opens one claimed source, drains new items into runs, acks the
// ones that succeeded, and releases the claim with the advanced cursor. Every
// exit path releases the claim so a source is never left stuck 'claimed'.
func (c *Controller) handleSource(ctx context.Context, cl src.Claimed) {
	logger := c.pu.Logger.With(
		zap.String("source", cl.SourceID), zap.String("tenant", cl.Tenant),
		zap.String("stack", cl.Stack), zap.String("kind", cl.Kind))

	kindImpl, ok := src.OpenKind(cl.Kind)
	if !ok {
		c.release(ctx, cl, cl.Cursor, errors.New("source: unknown kind "+cl.Kind))
		return
	}
	secretName := gjson.GetBytes(cl.Config, "secret").String()
	onProcessed := gjson.GetBytes(cl.Config, "on_processed").String()

	defaultAct, aerr := src.ParseAction(onProcessed)
	if aerr != nil {
		c.release(ctx, cl, cl.Cursor, aerr)
		return
	}

	// Materialize the credential, hand it to Open, zero it immediately —
	// regardless of whether Open succeeded.
	var cred []byte
	if secretName != "" {
		if c.pu.Secrets == nil {
			c.release(ctx, cl, cl.Cursor, errors.New("source: needs secret "+secretName+" but no secret store is configured"))
			return
		}
		v, _, serr := c.pu.Secrets.MaterializeForOpSlug(ctx, cl.Tenant, cl.Stack, secretName)
		if serr != nil {
			c.release(ctx, cl, cl.Cursor, serr)
			return
		}
		cred = v
	}
	conn, oerr := kindImpl.Open(ctx, src.OpenParams{
		Config: cl.Config, Cred: cred, Guard: c.guard, Logger: logger,
	})
	secrets.Zero(cred)
	if oerr != nil {
		c.release(ctx, cl, cl.Cursor, oerr)
		return
	}
	defer conn.Close()

	batch := c.pu.Conf.SourceBatch
	if batch <= 0 {
		batch = 50
	}
	items, drained, perr := conn.Poll(ctx, cl.Cursor, batch)
	if perr != nil {
		c.release(ctx, cl, cl.Cursor, perr)
		return
	}

	advanced := cl.Cursor
	allOK := true
	fired := 0
	for _, it := range items {
		resp, ok := c.dispatch(ctx, cl, it)
		if !ok {
			allOK = false
			break
		}
		act := defaultAct
		if override := gjson.Get(resp, "_txc.source.res.action").String(); override != "" {
			if a, e := src.ParseAction(override); e == nil {
				act = a
			} else {
				logger.Warn("source: stack proposed an invalid action; using configured default",
					zap.String("proposed", override), zap.Error(e))
			}
		}
		if err := conn.Ack(ctx, it, act); err != nil {
			logger.Warn("source: ack failed; stopping batch", zap.String("key", it.Key), zap.Error(err))
			allOK = false
			break
		}
		advanced = it.Cursor
		fired++
	}
	if allOK && drained != nil {
		advanced = drained
	}
	var relErr error
	if !allOK {
		relErr = errors.New("source: a poll item did not complete; will retry from the last success")
	}
	if fired > 0 {
		logger.Info("source polled", zap.Int("fired", fired), zap.Int("found", len(items)), zap.Bool("clean", allOK))
	}
	c.release(ctx, cl, advanced, relErr)
}

// dispatch fires one item as a run of the source's `<stack>/_source/0` and
// waits for its terminal. Returns the response payload and whether the run
// succeeded (a completed, non-admission-denied response). The message is
// parsed to the shared mail shape so `@source.msg.*` mirrors `@lmtp.msg.*`.
func (c *Controller) dispatch(ctx context.Context, cl src.Claimed, it src.Item) (string, bool) {
	rid := hxid.NewTimeSort().String()
	now := time.Now().UTC().Format(time.RFC3339)

	p := "{}"
	p, _ = sjson.Set(p, "_txc.src", "source")
	p, _ = sjson.Set(p, "_txc.rid", rid)
	p, _ = sjson.Set(p, "_ts", now)
	p, _ = sjson.Set(p, "_txc.source.id", cl.DeclaredID)
	p, _ = sjson.Set(p, "_txc.source.kind", cl.Kind)
	p, _ = sjson.Set(p, "_txc.source.tenant", cl.Tenant)
	p, _ = sjson.Set(p, "_txc.source.stack", cl.Stack)
	p, _ = sjson.Set(p, "_txc.source.pack", cl.Pack)
	p, _ = sjson.Set(p, "_txc.source.node", c.nodeID)
	p, _ = sjson.Set(p, "_txc.source.key", it.Key)
	p, _ = sjson.Set(p, "_txc.source.fetched_at", now)
	if len(it.Meta) > 0 && gjson.ValidBytes(it.Meta) {
		p, _ = sjson.SetRaw(p, "_txc.source.meta", string(it.Meta))
	}
	// The message, in the shared inbound-mail shape (imap kind). `.raw` is the
	// b64 original, exactly like `_txc.lmtp.msg.raw`.
	msgJSON, _ := mail.ParseMessage(it.Raw)
	if msgJSON == "" {
		msgJSON = "{}"
	}
	msgJSON, _ = sjson.Set(msgJSON, "raw", base64.StdEncoding.EncodeToString(it.Raw))
	p, _ = sjson.SetRaw(p, "_txc.source.msg", msgJSON)

	timeout := time.Duration(c.pu.Conf.SourceDispatchTimeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dctx = context.WithValue(dctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(dctx, p, resCh, "source")
	select {
	case c.pu.Bus <- envelope:
	case <-dctx.Done():
		// Never reached the bus (shutdown): it didn't run — treat as failure,
		// don't ack, retry next pass.
		return "", false
	}
	select {
	case res := <-resCh:
		if status, reason, denied := admission.Denied(res.Raw); denied {
			c.pu.Logger.Warn("source run denied by admission",
				zap.String("source", cl.SourceID), zap.String("key", it.Key),
				zap.Int("status", status), zap.String("reason", reason))
			return res.Raw, false
		}
		return res.Raw, true
	case <-dctx.Done():
		// Dispatched but no terminal in time. Do NOT ack — unlike scheduled's
		// at-most-once bias, an un-acked item is simply re-read next pass
		// (at-least-once), which the stack dedups on `_txc.source.key`.
		return "", false
	}
}

func (c *Controller) release(ctx context.Context, cl src.Claimed, cursor src.Cursor, pollErr error) {
	// Use a detached context for the release write so a cancelled poll ctx
	// (shutdown) still frees the claim rather than leaving it stuck.
	rctx := context.WithoutCancel(ctx)
	if err := c.store.Release(rctx, cl.SourceID, cursor, cl.EverySeconds, pollErr); err != nil {
		c.pu.Logger.Warn("source release failed", zap.String("source", cl.SourceID), zap.Error(err))
	}
	if pollErr != nil {
		c.pu.Logger.Warn("source poll ended with error", zap.String("source", cl.SourceID), zap.Error(pollErr))
	}
}
