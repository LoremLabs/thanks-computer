// Package controlpublish is the producer half of fleet sync: a
// background pump that drains the control_events_outbox table and
// publishes pending rows via feed.Sink. The lifecycle mirrors
// chassis/controlapply.Controller (Start/Stop, ticker, ctx.Done,
// errors logged not fatal). It is inert unless --feed-sink != nop,
// so single-node behaviour is unchanged.
//
// Producer obligations (see contract §5 + the overlay-repo design
// doc todo-fleet-sync-producer.md):
//   - Admin handlers upload the artifact bytes to the artifact store
//     BEFORE opening the local DB tx.
//   - In the tx, the handler appends a row to control_events_outbox
//     carrying event_id (UUID), event_type, the decomposed fields,
//     and the full canonical payload as payload_json.
//   - The handler commits.
//   - This pump picks the row up asynchronously, publishes via
//     Sink.Append, and writes the broker-assigned ControlVersion
//     back to the row on success.
//
// Crash safety: anything in the outbox WILL be published once the
// broker is reachable. Anything not in the outbox was never accepted.
// Retries reuse the same event_id, so backends with idempotent
// publish semantics (JetStream Nats-Msg-Id, the file Sink's
// filename-as-key) resolve duplicates naturally; the consumer-side
// applied_events table is the load-bearing semantic dedup.
package controlpublish

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

const tsLayout = "2006-01-02T15:04:05.000Z"

// Controller is the background pump. Lifecycle mirrors
// controlapply.Controller.
type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	sink     feed.Sink
	shutdown chan bool
	wg       sync.WaitGroup
}

// NewController returns a pump bound to the chassis unit. sink may
// be nil when feed-sink=nop; enabled() returns false in that case and
// Start/Stop are no-ops.
func NewController(ctx context.Context, pu *processor.Unit, sink feed.Sink) *Controller {
	return &Controller{
		ctx:      ctx,
		pu:       pu,
		sink:     sink,
		shutdown: make(chan bool),
	}
}

func (c *Controller) enabled() bool {
	// Only the admin personality drains the outbox. The outbox is produced only
	// by admin handlers, and gating to a single producer is what keeps the pump
	// safe once the runtime DB is shared Postgres: without it every node with a
	// non-nop feed-sink would drain (and double-publish) the same shared outbox.
	// On per-node SQLite today this is harmless (the admin node still runs it).
	return c.sink != nil && c.pu.Conf.FeedSink != "" && c.pu.Conf.FeedSink != "nop" &&
		strings.Contains(c.pu.Conf.Personalities, "admin")
}

// rb rebinds `?` placeholders for the runtime DB's dialect (identity on
// SQLite). The pump's outbox statements run on c.pu.RuntimeDB, so they use
// its dialect. nil-safe: tests may construct a Unit without a dialect set.
func (c *Controller) rb(q string) string {
	d := c.pu.RuntimeDialect
	if d == nil {
		d = registry.SQLite
	}
	return d.Rebind(q)
}

// Start launches the pump goroutine. No-op when disabled.
func (c *Controller) Start() {
	if !c.enabled() {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.ctx = ctx

	period := time.Duration(c.pu.Conf.FeedPollPeriod) * time.Second
	if period <= 0 {
		period = 15 * time.Second
	}

	go func() {
		c.pu.Logger.Info("control-event publisher started",
			zap.String("sink", c.sink.Name()),
			zap.Duration("period", period),
			zap.Int("batch", c.batchSize()))
		c.wg.Add(1)
		// Run once immediately to drain any backlog from before the
		// chassis started — matches the applier's posture.
		c.drainOnce(c.ctx)
		for {
			select {
			case <-time.After(period):
				c.drainOnce(c.ctx)
			case doshutdown := <-c.shutdown:
				if doshutdown {
					cancel()
					c.wg.Done()
					return
				}
			}
		}
	}()
}

// Stop signals shutdown and waits for the pump to drain in-flight
// publishes. No-op when disabled.
func (c *Controller) Stop() {
	if !c.enabled() {
		return
	}
	c.pu.Logger.Info("calling control-event publisher stop")
	c.shutdown <- true
	c.wg.Wait()
	c.pu.Logger.Info("control-event publisher stopped")
}

func (c *Controller) batchSize() int {
	n := c.pu.Conf.FeedSinkBatchSize
	if n <= 0 {
		return 64
	}
	return n
}

// drainOnce runs one pump pass: select pending rows, Append each,
// record success/failure. Failures don't halt the pass — the failed
// row stays pending with attempt_count incremented; subsequent rows
// in the batch still get a chance. Backoff is implicit via the
// tick rate (no per-row backoff in v1; the doc reserves
// --feed-sink-backoff-max for that addition if needed).
func (c *Controller) drainOnce(ctx context.Context) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT id, event_id, payload_json
		   FROM control_events_outbox
		  WHERE published_control_version IS NULL
		  ORDER BY id
		  LIMIT ?`), c.batchSize())
	if err != nil {
		c.pu.Logger.Error("control-event publisher: select pending",
			zap.String("err", err.Error()))
		return
	}
	type pending struct {
		id      int64
		eventID string
		payload []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.eventID, &p.payload); err != nil {
			c.pu.Logger.Error("control-event publisher: scan row",
				zap.String("err", err.Error()))
			_ = rows.Close()
			return
		}
		batch = append(batch, p)
	}
	if err := rows.Close(); err != nil {
		c.pu.Logger.Error("control-event publisher: close rows",
			zap.String("err", err.Error()))
		return
	}
	for _, p := range batch {
		c.publishOne(ctx, p.id, p.eventID, p.payload)
	}
}

func (c *Controller) publishOne(ctx context.Context, rowID int64, eventID string, payload []byte) {
	var ev controlevent.Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		// Outbox row is corrupt — record the failure so it stays
		// visible in the pending set with an explanatory last_error.
		// We don't drop the row; an operator can DELETE it after
		// inspecting.
		c.recordFailure(ctx, rowID, fmt.Errorf("decode payload_json: %w", err))
		return
	}
	// Defense-in-depth: ensure the wire EventID matches the row's
	// canonical event_id column. The handler helper writes both from
	// the same source, but if they drift we want to know loudly.
	if ev.EventID != eventID {
		c.recordFailure(ctx, rowID,
			fmt.Errorf("event_id mismatch: row=%q payload=%q", eventID, ev.EventID))
		return
	}

	out, err := c.sink.Append(ctx, ev)
	if err != nil {
		c.recordFailure(ctx, rowID, err)
		return
	}
	if out.ControlVersion == 0 {
		c.recordFailure(ctx, rowID, fmt.Errorf("sink returned zero control_version"))
		return
	}
	now := time.Now().UTC().Format(tsLayout)
	_, uerr := c.pu.RuntimeDB.ExecContext(ctx,
		c.rb(`UPDATE control_events_outbox
		    SET published_control_version = ?, published_at = ?
		  WHERE id = ?`),
		out.ControlVersion, now, rowID)
	if uerr != nil {
		// Published to the broker but failed to record it locally.
		// On next tick the row is still pending → republish, same
		// event_id → backend dedup returns the same control_version
		// (or the applied_events check protects the consumer
		// regardless). Loud-log so operators see the race.
		c.pu.Logger.Error("control-event publisher: row updated remotely but write-back failed",
			zap.Int64("outbox_id", rowID),
			zap.String("event_id", eventID),
			zap.Uint64("control_version", out.ControlVersion),
			zap.String("err", uerr.Error()))
		return
	}
	c.pu.Logger.Info("control-event published",
		zap.String("event_id", eventID),
		zap.String("type", ev.Type),
		zap.Uint64("control_version", out.ControlVersion))
}

// recordFailure increments attempt_count and stamps last_error +
// last_attempt_at. The row stays in the pending set; the next tick
// will retry. We don't fail the whole drainOnce on this — it's just
// diagnostic bookkeeping.
func (c *Controller) recordFailure(ctx context.Context, rowID int64, err error) {
	now := time.Now().UTC().Format(tsLayout)
	short := err.Error()
	if len(short) > 200 {
		short = short[:200]
	}
	_, uerr := c.pu.RuntimeDB.ExecContext(ctx,
		c.rb(`UPDATE control_events_outbox
		    SET attempt_count = attempt_count + 1,
		        last_error = ?,
		        last_attempt_at = ?
		  WHERE id = ?`),
		short, now, rowID)
	if uerr != nil {
		c.pu.Logger.Error("control-event publisher: failure-bookkeeping update failed",
			zap.Int64("outbox_id", rowID),
			zap.String("publish_err", err.Error()),
			zap.String("update_err", uerr.Error()))
	} else {
		c.pu.Logger.Warn("control-event publish failed (will retry)",
			zap.Int64("outbox_id", rowID),
			zap.String("err", err.Error()))
	}
}

// AppendOutbox is the helper admin handlers call inside their
// existing transactions. Generating event_id is the caller's
// responsibility (chassis/hxid.New is the convention) so callers
// can also return the event_id back to the user (audit log /
// debugging surface) if they want.
//
// payloadJSON is the canonical Event JSON minus ControlVersion (the
// Sink stamps that on publish). Keeping the full doc in the blob
// means new fields land without column migrations.
func AppendOutbox(
	ctx context.Context,
	tx *sql.Tx,
	eventID, eventType, tenantID, stackID string,
	version, baseVersion int64,
	artifactRef, checksum string,
	payloadJSON []byte,
	d registry.Dialect,
) error {
	if eventID == "" {
		return fmt.Errorf("controlpublish: AppendOutbox requires non-empty event_id")
	}
	if eventType == "" {
		return fmt.Errorf("controlpublish: AppendOutbox requires non-empty event_type")
	}
	if len(payloadJSON) == 0 {
		return fmt.Errorf("controlpublish: AppendOutbox requires non-empty payload_json")
	}
	if d == nil {
		d = registry.SQLite
	}
	now := time.Now().UTC().Format(tsLayout)
	_, err := tx.ExecContext(ctx, d.Rebind(`INSERT INTO control_events_outbox
		(event_id, event_type, tenant_id, stack_id, version, base_version,
		 artifact_ref, checksum, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		eventID, eventType,
		nullableString(tenantID), nullableString(stackID),
		nullableInt(version), nullableInt(baseVersion),
		nullableString(artifactRef), nullableString(checksum),
		payloadJSON, now)
	return err
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// DrainForTest exposes the internal drain loop for integration tests
// that drive the pump synchronously instead of relying on the
// ticker. Not part of the public API; do not call from production.
func (c *Controller) DrainForTest(ctx context.Context) { c.drainOnce(ctx) }
