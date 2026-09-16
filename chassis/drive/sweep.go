package drive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SweepReport counts what one Sweep pass did.
type SweepReport struct {
	// Objects walked in the object store.
	Scanned int
	// Objects deleted because no live row referenced them.
	Orphans int
	// Tombstoned resource rows hard-deleted past retention.
	Tombstones int64
	// Tombstoned collection rows hard-deleted past retention.
	Collections int64
	// Per-object errors logged and skipped (the pass continues).
	Errors int
	// Bytes the deleted orphans held.
	OrphanBytes int64
}

// Sweep is the drive janitor, idempotent and safe to repeat or to run on
// several nodes at once:
//
//  1. Hard-delete tombstoned resources (and collections) whose deleted_at is
//     older than tombstoneRetention — after that a `list since` consumer can
//     no longer learn of the deletion, which is the window it is given.
//  2. Walk the object store and delete every object older than grace that
//     no LIVE row references: superseded versions after a rewrite, the
//     objects of hard-deleted tombstones, and the leftovers of a put or
//     copy whose index transaction failed.
//
// grace protects an in-flight put (bytes land before the row) and an
// in-flight reader of a version that was just superseded: nothing younger
// than grace is touched. Per-object errors are counted and skipped.
func (s *Store) Sweep(ctx context.Context, grace, tombstoneRetention time.Duration) (SweepReport, error) {
	var rep SweepReport
	now := s.now()

	if tombstoneRetention > 0 {
		cutoff := fmtTime(now.Add(-tombstoneRetention))
		res, err := s.db.ExecContext(ctx, s.rb(`DELETE FROM drive_resources WHERE deleted_at IS NOT NULL AND deleted_at < ?`), cutoff)
		if err != nil {
			return rep, fmt.Errorf("drive: purge tombstones: %w", err)
		}
		rep.Tombstones, _ = res.RowsAffected()
		res, err = s.db.ExecContext(ctx, s.rb(`DELETE FROM drive_collections WHERE deleted_at IS NOT NULL AND deleted_at < ?`), cutoff)
		if err != nil {
			return rep, fmt.Errorf("drive: purge collections: %w", err)
		}
		rep.Collections, _ = res.RowsAffected()
	}

	graceCutoff := now.Add(-grace)
	err := s.objects.List(ctx, "", func(key string, info ObjectInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rep.Scanned++
		if !info.ModTime.IsZero() && info.ModTime.After(graceCutoff) {
			return nil
		}
		var one int
		err := s.db.QueryRowContext(ctx, s.rb(`SELECT 1 FROM drive_resources WHERE object_key = ? AND deleted_at IS NULL LIMIT 1`), key).Scan(&one)
		switch {
		case err == nil:
			return nil // referenced by a live row
		case errors.Is(err, sql.ErrNoRows):
		default:
			rep.Errors++
			return nil
		}
		if err := s.objects.Delete(ctx, key); err != nil {
			rep.Errors++
			return nil
		}
		rep.Orphans++
		rep.OrphanBytes += info.Size
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("drive: sweep objects: %w", err)
	}
	return rep, nil
}
