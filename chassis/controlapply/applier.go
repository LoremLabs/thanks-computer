// Package controlapply is the consume side of the control-plane contract: a
// background controller that polls a feed.Source for control events and
// applies them to the local databases so a fleet stays in sync.
//
// Model (internal docs/todo-architecture-saas-fleet.md §3.1): an event is a
// notification + a content-addressed artifact ref + checksum. The applier
// fetches the artifact, verifies it, and upserts the affected local rows in
// one transaction, advancing the control-version cursor. It is inert unless
// a feed source is configured (--feed-source != nop) so single-node
// behaviour is unchanged.
package controlapply

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/admin"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// cursorVar is the varvals key holding the highest applied ControlVersion.
// It is the same key chassis/snapshot stamps on a bootstrapped DB, so a
// freshly-bootstrapped node resumes from the snapshot's position.
const cursorVar = "txco-control-version"

const tsLayout = "2006-01-02T15:04:05.000Z"

// Controller is the background applier. Lifecycle mirrors the continuation
// sweeper (Start/Stop, ticker, ctx.Done, errors logged not fatal).
type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	admin    *admin.Controller
	src      feed.Source
	astore   artifact.Store
	shutdown chan bool
	wg       sync.WaitGroup
}

func NewController(ctx context.Context, pu *processor.Unit,
	adminCtrl *admin.Controller, src feed.Source, astore artifact.Store) *Controller {
	return &Controller{
		ctx:      ctx,
		pu:       pu,
		admin:    adminCtrl,
		src:      src,
		astore:   astore,
		shutdown: make(chan bool),
	}
}

func (c *Controller) enabled() bool {
	return c.src != nil && c.pu.Conf.FeedSource != "" && c.pu.Conf.FeedSource != "nop"
}

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
		c.pu.Logger.Info("control-event applier started",
			zap.String("source", c.src.Name()),
			zap.Duration("period", period))
		c.wg.Add(1)
		for {
			select {
			case <-time.After(period):
				c.pollOnce(c.ctx)
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

func (c *Controller) Stop() {
	if !c.enabled() {
		return
	}
	c.pu.Logger.Info("calling control-event applier stop")
	c.shutdown <- true
	c.wg.Wait()
	c.pu.Logger.Info("control-event applier stopped")
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// readCursor returns the highest applied ControlVersion (0 if unset).
func (c *Controller) readCursor(ctx context.Context) (uint64, error) {
	var val string
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT val FROM varvals WHERE var = ?`, cursorVar).Scan(&val)
	if err == sql.ErrNoRows || val == "" {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseUint(val, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("controlapply: bad cursor %q: %w", val, perr)
	}
	return n, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func writeCursor(ctx context.Context, x execer, cv uint64) error {
	_, err := x.ExecContext(ctx,
		`INSERT OR REPLACE INTO varvals (var, val) VALUES (?, ?)`,
		cursorVar, strconv.FormatUint(cv, 10))
	return err
}

// PollOnceForTest exposes the internal poll-and-apply loop for
// integration tests that drive the applier synchronously instead of
// relying on the ticker. Not part of the public API.
func (c *Controller) PollOnceForTest(ctx context.Context) { c.pollOnce(ctx) }

// pollOnce runs one feed poll + apply pass. Any apply failure halts the
// pass (cursor not advanced past the failure); the next tick retries.
func (c *Controller) pollOnce(ctx context.Context) {
	cursor, err := c.readCursor(ctx)
	if err != nil {
		c.pu.Logger.Error("control-event applier: read cursor", zap.String("err", err.Error()))
		return
	}
	events, err := c.src.Poll(ctx, cursor)
	if err != nil {
		c.pu.Logger.Error("control-event applier: poll", zap.String("err", err.Error()))
		return
	}
	acker, ackable := c.src.(feed.Acker)
	for _, ev := range events {
		if err := c.applyOne(ctx, ev, &cursor); err != nil {
			c.pu.Logger.Error("control-event apply halted (will retry)",
				zap.Uint64("control_version", ev.ControlVersion),
				zap.String("type", ev.Type),
				zap.String("err", err.Error()))
			return
		}
		// Ack-after-apply: the local SQLite tx in applyOne has
		// committed (cursor + applied_events + data rows). If the
		// Source needs per-event acknowledgement (e.g. JetStream),
		// confirm now so the broker advances its consumer cursor.
		// An ack failure is loud-logged but does NOT halt the pass —
		// the broker redelivers, the applied_events guard catches
		// the replay, and the apply is a no-op next time.
		if ackable {
			if err := acker.Ack(ctx, ev.EventID); err != nil {
				c.pu.Logger.Warn("control-event ack failed (will replay)",
					zap.String("event_id", ev.EventID),
					zap.Uint64("control_version", ev.ControlVersion),
					zap.String("err", err.Error()))
			}
		}
	}
}

// applyOne validates, checksum-verifies and applies a single event. On
// success it advances *cursor.
//
// Idempotency: the applied_events table is checked FIRST. If the
// event_id is already recorded, we skip the artifact fetch + apply
// entirely and just advance the cursor — saves a network round-trip
// to the artifact store on re-delivery, and prevents double-apply
// regardless of broker-side dedup TTL.
func (c *Controller) applyOne(ctx context.Context, ev controlevent.Event, cursor *uint64) error {
	if ev.ControlVersion <= *cursor {
		return nil // already applied (idempotent by cursor)
	}
	if err := ev.Validate(); err != nil {
		return err
	}

	// Semantic-identity dedup: if we've recorded this event_id,
	// skip the apply, advance the cursor, and move on. This is the
	// load-bearing guard against re-delivery beyond the broker's
	// own dedup window (e.g. JetStream's Nats-Msg-Id sliding window
	// expires before a pump crash recovers).
	seen, err := c.alreadyApplied(ctx, ev.EventID)
	if err != nil {
		return fmt.Errorf("applied_events check: %w", err)
	}
	if seen {
		if err := c.advanceCursorAndMark(ctx, ev); err != nil {
			return err
		}
		*cursor = ev.ControlVersion
		return nil
	}

	var data []byte
	if ev.ArtifactRef != "" {
		d, _, err := c.astore.Get(ctx, ev.ArtifactRef)
		if err != nil {
			return fmt.Errorf("fetch artifact %q: %w", ev.ArtifactRef, err)
		}
		want := strings.TrimPrefix(ev.Checksum, "sha256:")
		if got := sha256Hex(d); got != want {
			return fmt.Errorf("checksum mismatch (event=%s actual=sha256:%s)", ev.Checksum, got)
		}
		data = d
	}

	switch ev.Type {
	case controlevent.TypeStackActivated:
		var art StackActivatedArtifact
		if err := json.Unmarshal(data, &art); err != nil {
			return fmt.Errorf("decode stack artifact: %w", err)
		}
		if err := c.applyStackActivated(ctx, ev, art); err != nil {
			return err
		}
	case controlevent.TypeSystemOpstack:
		// _sys ships with the binary/image; not applied from the feed.
		c.pu.Logger.Info("control-event: system.opstack.updated not applied (ships with binary)",
			zap.Uint64("control_version", ev.ControlVersion))
		if err := c.advanceCursorAndMark(ctx, ev); err != nil {
			return err
		}
	default:
		var art RowsArtifact
		if err := json.Unmarshal(data, &art); err != nil {
			return fmt.Errorf("decode rows artifact: %w", err)
		}
		if err := c.applyRows(ctx, ev, art); err != nil {
			return err
		}
	}

	*cursor = ev.ControlVersion
	return nil
}

// alreadyApplied reports whether event_id is in applied_events.
// Cheap PK lookup against runtime.db.
func (c *Controller) alreadyApplied(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	var seen int
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT 1 FROM applied_events WHERE event_id = ? LIMIT 1`, eventID).Scan(&seen)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// markApplied inserts (event_id, control_version, applied_at) into
// applied_events using INSERT OR IGNORE so a same-tx redundant call
// is a no-op. Called inside the apply transaction for runtime
// events; called as a separate write for auth/system events.
func markApplied(ctx context.Context, x execer, ev controlevent.Event) error {
	if ev.EventID == "" {
		return fmt.Errorf("markApplied: empty event_id")
	}
	_, err := x.ExecContext(ctx,
		`INSERT OR IGNORE INTO applied_events (event_id, control_version, applied_at)
		 VALUES (?, ?, ?)`,
		ev.EventID, ev.ControlVersion,
		time.Now().UTC().Format(tsLayout))
	return err
}

// advanceCursorAndMark writes both the cursor and the applied_events
// row in a single runtime.db tx. Used for no-op event types
// (system.opstack, absent tables) and for the "already-applied"
// short-circuit. Atomicity prevents the cursor advancing without a
// corresponding dedup record.
func (c *Controller) advanceCursorAndMark(ctx context.Context, ev controlevent.Event) error {
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := writeCursor(ctx, tx, ev.ControlVersion); err != nil {
		return err
	}
	if err := markApplied(ctx, tx, ev); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// advanceCursorOnly retained for callers that genuinely don't need
// to mark applied_events (none today — all paths now use
// advanceCursorAndMark so the cursor and dedup record stay in
// lockstep). Kept private + unused so the build flags it if
// reintroduced accidentally.
//
//nolint:unused
func (c *Controller) advanceCursorOnly(ctx context.Context, cv uint64) error {
	return writeCursor(ctx, c.pu.RuntimeDB, cv)
}

// applyStackActivated upserts the activated version's files then reuses the
// admin activation core. Files + cursor commit atomically; dbcache is
// reloaded so the next request sees the new ops.
func (c *Controller) applyStackActivated(ctx context.Context, ev controlevent.Event, art StackActivatedArtifact) error {
	if art.TenantID == "" || art.Stack == "" || art.Version <= 0 {
		return fmt.Errorf("stack artifact missing tenant/stack/version")
	}
	now := time.Now().UTC().Format(tsLayout)

	// The runtime DB is opened with _txlock=immediate, so BeginTx takes the
	// RESERVED write lock up front and this applier serialises against the
	// admin apply path on _busy_timeout instead of failing a read→write
	// upgrade with "database is locked". See openSQLiteOrDie in chassis/app.
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Upsert the stacks row.
	var stackID string
	err = tx.QueryRowContext(ctx,
		`SELECT stack_id FROM stacks WHERE tenant_id = ? AND name = ?`,
		art.TenantID, art.Stack).Scan(&stackID)
	if err == sql.ErrNoRows {
		stackID = "stk_" + hxid.NewTimeSort().String()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES (?, ?, ?, ?)`,
			stackID, art.TenantID, art.Stack, now); err != nil {
			return fmt.Errorf("upsert stack: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("lookup stack: %w", err)
	}

	// Upsert the stack_versions row for this version_number.
	var versionID int64
	err = tx.QueryRowContext(ctx,
		`SELECT version_id FROM stack_versions WHERE stack_id = ? AND version_number = ?`,
		stackID, art.Version).Scan(&versionID)
	if err == sql.ErrNoRows {
		res, ierr := tx.ExecContext(ctx,
			`INSERT INTO stack_versions
			    (stack_id, version_number, parent_version_id, status, created_by, created_at, manifest_hash)
			 VALUES (?, ?, NULL, 'draft', 'controlapply', ?, '')`,
			stackID, art.Version, now)
		if ierr != nil {
			return fmt.Errorf("insert version: %w", ierr)
		}
		versionID, _ = res.LastInsertId()
	} else if err != nil {
		return fmt.Errorf("lookup version: %w", err)
	}

	// Replace the version's files.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM stack_files WHERE version_id = ?`, versionID); err != nil {
		return fmt.Errorf("clear files: %w", err)
	}
	for _, f := range art.Files {
		content := f.Content
		hash := f.ContentHash
		if (strings.HasPrefix(f.Path, "FILES/") || storeseed.IsPackPath(f.Path)) && hash != "" {
			// Fingerprint-only CAS-backed asset (FILES/ static asset or a
			// VECTORS//KV/ store-seed pack): the bytes live in the shared
			// content-addressed store, so don't inline them here — keeps the
			// node's in-memory runtime DB free of tenant file/pack bytes. The
			// static serve path (FILES/) and the store-seed reconciler (packs)
			// resolve them lazily by content_hash.
			content = ""
		} else if hash == "" {
			hash = sha256Hex([]byte(content))
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?, ?, ?, ?)`,
			versionID, f.Path, content, hash); err != nil {
			return fmt.Errorf("insert file %s: %w", f.Path, err)
		}
	}

	// Reuse the admin activation core: materialise ops + flip active_version.
	if err := c.admin.ApplyStackVersion(ctx, tx, art.TenantID, art.Stack, art.Version, now); err != nil {
		return fmt.Errorf("materialise: %w", err)
	}

	if err := writeCursor(ctx, tx, ev.ControlVersion); err != nil {
		return fmt.Errorf("write cursor: %w", err)
	}
	if err := markApplied(ctx, tx, ev); err != nil {
		return fmt.Errorf("mark applied: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after apply failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	// Materialise declarative store-seed packs (VECTORS/, KV/) into this node's
	// runtime stores — post-commit, synchronous (this applier is already a
	// background worker draining events serially). origin=false: a data-plane
	// node reconciles its PER-NODE stores (sqlite-vec) from the CAS-resolved
	// packs but skips SHARED stores (pgvector), which the control plane seeded
	// once. ev.BaseVersion is the prior active version → change-driven reconcile
	// skips unchanged packs. Best-effort: ReconcileStorePacks logs, never fails.
	c.admin.ReconcileStorePacks(ctx, art.TenantID, art.Stack, art.Version, ev.BaseVersion, false)

	c.pu.Logger.Info("control-event applied: stack.activated",
		zap.String("event_id", ev.EventID),
		zap.String("tenant", art.TenantID), zap.String("stack", art.Stack),
		zap.Int64("version", art.Version), zap.Uint64("control_version", ev.ControlVersion))
	return nil
}

// applyRows applies a whitelisted table upsert/delete. runtime-targeted
// changes commit the cursor in the same tx (atomic) and reload dbcache;
// auth-targeted changes commit then advance the cursor separately (safe:
// re-apply is idempotent). An auth event on a node without auth.db, or a
// table absent from the schema, is a logged no-op that still advances.
func (c *Controller) applyRows(ctx context.Context, ev controlevent.Event, art RowsArtifact) error {
	if !tableAllowed(art.DB, art.Table) {
		return fmt.Errorf("table %q in db %q is not syncable", art.Table, art.DB)
	}
	if art.Op != opUpsert && art.Op != opDelete {
		return fmt.Errorf("unknown op %q", art.Op)
	}

	var db *sql.DB
	switch art.DB {
	case dbRuntime:
		db = c.pu.RuntimeDB
	case dbAuth:
		db = c.pu.AuthDB
		if db == nil {
			c.pu.Logger.Info("control-event: auth event skipped (no auth.db on this node)",
				zap.String("table", art.Table), zap.Uint64("control_version", ev.ControlVersion))
			return c.advanceCursorAndMark(ctx, ev)
		}
		// Auth-row fleet sync targets SQLite admin-capable *consumer*
		// nodes (the SQLite-flavoured sqlite_master probe + INSERT OR
		// REPLACE below). A shared-Postgres auth store is the *control
		// plane's* own DB, written through the registry — it is the
		// event producer and runs --feed-source=nop, so this path is
		// never reached in a supported deployment. Refuse loudly rather
		// than run SQLite SQL against Postgres (the applier contract is
		// "loud, retried, never silent divergence").
		if registry.DialectForDSN(c.pu.Conf.DbAuthDsn) == registry.Postgres {
			return fmt.Errorf("control-event: auth-row sync is unsupported against a Postgres auth store "+
				"(table %q, control_version %d) — the Postgres auth DB is the control plane's own "+
				"store; data-plane consumers use a SQLite auth.db", art.Table, ev.ControlVersion)
		}
	default:
		return fmt.Errorf("unknown target db %q", art.DB)
	}

	// Designed-not-shipped tables (e.g. tenant_runtime_state) may be absent.
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name = ? LIMIT 1`,
		art.Table).Scan(&exists); err == sql.ErrNoRows {
		c.pu.Logger.Info("control-event: target table absent; skipping",
			zap.String("db", art.DB), zap.String("table", art.Table),
			zap.Uint64("control_version", ev.ControlVersion))
		return c.advanceCursorAndMark(ctx, ev)
	} else if err != nil {
		return err
	}

	// A runtime-DB target is opened with _txlock=immediate, so BeginTx takes the
	// RESERVED lock up front (see openSQLiteOrDie in chassis/app); other targets
	// stay deferred as before. An explicit Exec("BEGIN IMMEDIATE") here would
	// only error with "within a transaction", so it's gone.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, row := range art.Rows {
		if art.Op == opUpsert {
			if err := upsertRow(ctx, tx, art.Table, row); err != nil {
				return err
			}
		} else {
			if err := deleteRow(ctx, tx, art.Table, art.PK, row); err != nil {
				return err
			}
		}
	}

	runtimeTarget := art.DB == dbRuntime
	if runtimeTarget {
		if err := writeCursor(ctx, tx, ev.ControlVersion); err != nil {
			return err
		}
		if err := markApplied(ctx, tx, ev); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	if runtimeTarget {
		if err := c.pu.Dbc.Reload(); err != nil {
			c.pu.Logger.Warn("dbcache reload after apply failed; FS watcher will retry",
				zap.String("err", err.Error()))
		}
	} else {
		// Auth tx committed; cursor + applied_events live in runtime.db,
		// written together in one runtime tx. Idempotent: a redelivery
		// after a crash between the auth commit and this write will
		// re-INSERT-OR-REPLACE the auth rows (safe per the contract)
		// then arrive here and find applied_events empty → write it.
		if err := c.advanceCursorAndMark(ctx, ev); err != nil {
			return err
		}
	}
	c.pu.Logger.Info("control-event applied: rows",
		zap.String("db", art.DB), zap.String("table", art.Table),
		zap.String("op", art.Op), zap.Int("rows", len(art.Rows)),
		zap.Uint64("control_version", ev.ControlVersion))
	return nil
}

// coerce normalizes a JSON-decoded value for SQLite binding: integral
// float64 → int64, bool → 0/1, a {"$b64":"…"} wrapper → raw []byte (so
// BLOB columns round-trip — JSON has no byte type, and binding the base64
// TEXT would corrupt the stored bytes), everything else unchanged.
//
// The {"$b64":…} convention is how the producer ships binary columns (the
// secret store's nonce/ciphertext/wrapped_dek/dek_nonce). A malformed wrapper
// (bad base64, extra keys) falls through unchanged so upsert fails loudly on
// the type mismatch rather than silently storing garbage.
func coerce(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case map[string]any:
		if len(t) == 1 {
			if enc, ok := t["$b64"].(string); ok {
				if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
					return raw
				}
			}
		}
		return v
	default:
		return v
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func upsertRow(ctx context.Context, tx *sql.Tx, table string, row map[string]any) error {
	cols := sortedKeys(row)
	if len(cols) == 0 {
		return fmt.Errorf("empty row for %s", table)
	}
	ph := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, k := range cols {
		ph[i] = "?"
		args[i] = coerce(row[k])
	}
	q := fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s) VALUES (%s)`,
		table, strings.Join(cols, ","), strings.Join(ph, ","))
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func deleteRow(ctx context.Context, tx *sql.Tx, table string, pk []string, row map[string]any) error {
	if len(pk) == 0 {
		return fmt.Errorf("delete on %s requires pk", table)
	}
	conds := make([]string, len(pk))
	args := make([]any, len(pk))
	for i, k := range pk {
		conds[i] = k + " = ?"
		args[i] = coerce(row[k])
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s`, table, strings.Join(conds, " AND "))
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}
