package admin

// Store-seed reconcile: the consumer half of the declarative VECTORS/ + KV/
// channel (chassis/storeseed). The CLI collects the packs, the producer
// (control_publish.go) ships them as CAS-backed stack_files, and this file
// reads them back for a version and (P2+) reconciles them into the runtime
// stores. The reconcile hook runs best-effort AFTER the activation tx commits
// — never inside it — because it does I/O against external stores (the vector
// store, the KV store) and must not pin the single runtime DB connection or
// roll back an otherwise-good activation. The pack bytes are durable in the CAS,
// so a failed reconcile is retried on the next apply/reload.

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// ReconcileStorePacks loads the version's CHANGED VECTORS/ + KV/ packs and
// reconciles them into the runtime stores. It is invoked AFTER the activation tx
// commits, from both the control-plane admin handler (origin=true) and the
// data-plane applier (origin=false, in package controlapply — hence exported).
//
// Change-driven: only packs whose content-hash differs from the prior active
// version (priorVersion) are reconciled. A code-only deploy (`txco apply`)
// carries every pack forward unchanged → empty diff → ZERO store I/O (not even a
// CAS fetch). priorVersion <= 0, or any diff failure, falls back to reconciling
// every pack (safe: reconcile is idempotent; correct for a first activation or a
// freshly-bootstrapped node whose store is empty).
//
// Best-effort by contract: it logs and swallows every error so a slow or failing
// store never stalls or rolls back a deploy — the pack bytes are durable in the
// CAS, so a missed reconcile is retried on the next apply/reload.
func (c *Controller) ReconcileStorePacks(ctx context.Context, tenantID, stack string, version, priorVersion int64, origin bool) {
	if c.storeReconciler == nil {
		return
	}
	changed, canDiff := c.changedPackPaths(ctx, tenantID, stack, version, priorVersion)
	if canDiff && len(changed) == 0 {
		return // nothing changed since the prior active version — no data work
	}
	var filter map[string]struct{} // nil ⇒ reconcile every pack
	if canDiff {
		filter = changed
	}
	packs, err := c.loadStorePacks(ctx, tenantID, stack, version, filter)
	if err != nil {
		c.pu.Logger.Warn("store-seed: load packs failed (activation unaffected)",
			zap.String("tenant", tenantID), zap.String("stack", stack),
			zap.Int64("version", version), zap.String("err", err.Error()))
		return
	}
	if len(packs) == 0 {
		return
	}
	scope := storeseed.Scope{Tenant: tenantID, Stack: stack, Version: version}
	if err := c.storeReconciler.Reconcile(ctx, scope, packs, origin); err != nil {
		c.pu.Logger.Warn("store-seed: reconcile failed (activation unaffected; retried next apply)",
			zap.String("tenant", tenantID), zap.String("stack", stack),
			zap.Int64("version", version), zap.Bool("origin", origin),
			zap.Int("packs", len(packs)), zap.String("err", err.Error()))
		return
	}
	c.pu.Logger.Info("store-seed: reconciled",
		zap.String("tenant", tenantID), zap.String("stack", stack),
		zap.Int64("version", version), zap.Bool("origin", origin),
		zap.Int("packs", len(packs)))
}

// loadStorePacks loads the store-seed packs (VECTORS/, KV/) attached to the
// named stack version, resolving each pack's bytes: inline from stack_files on
// the control plane, or from the shared CAS by fingerprint on a data-plane node
// (where applyStackActivated blanks the content column). Returns one RawPack per
// pack file, in path order. A pack whose bytes cannot be resolved is a hard
// error — a reconcile must never silently seed a partial or empty collection.
func (c *Controller) loadStorePacks(
	ctx context.Context, tenantID, stack string, versionNumber int64, onlyPaths map[string]struct{},
) ([]storeseed.RawPack, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, `
		SELECT sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stack_versions sv ON sf.version_id = sv.version_id
		  JOIN stacks s          ON sv.stack_id = s.stack_id
		 WHERE s.tenant_id = ?
		   AND s.name = ?
		   AND sv.version_number = ?
		 ORDER BY sf.path`,
		tenantID, stack, versionNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	emptyHash := sha256Hex("")
	var packs []storeseed.RawPack
	for rows.Next() {
		var path, content, hash string
		if err := rows.Scan(&path, &content, &hash); err != nil {
			return nil, err
		}
		if !storeseed.IsPackPath(path) {
			continue
		}
		// onlyPaths (when non-nil) restricts to the changed packs — so the
		// unchanged ones aren't even resolved from the CAS.
		if onlyPaths != nil {
			if _, ok := onlyPaths[path]; !ok {
				continue
			}
		}
		if hash == "" {
			hash = sha256Hex(content)
		}
		// Data-plane nodes carry the pack as a fingerprint with a blanked
		// content column (mirrors loadVersionFiles' FILES/ resolution): fetch
		// the bytes from the shared CAS. An empty hash means a genuinely-empty
		// pack, not a stripped one — leave it (it yields zero items).
		if content == "" && hash != emptyHash {
			if c.fcas == nil {
				return nil, fmt.Errorf("store-seed %s: fingerprint-only pack but no filecas on this node", path)
			}
			b, gerr := c.fcas.Get(ctx, hash)
			if gerr != nil {
				return nil, fmt.Errorf("store-seed %s (cas %s): %w", path, hash, gerr)
			}
			content = string(b)
		}
		pk, ok := storeseed.NewRawPack(path, []byte(content))
		if !ok {
			// validateStackFilePath should have rejected this at upload; treat a
			// slipped-through malformed pack path as a hard error, not a silent skip.
			return nil, fmt.Errorf("store-seed: malformed pack path %q", path)
		}
		packs = append(packs, pk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return packs, nil
}

// changedPackPaths returns the set of pack paths whose content-hash differs
// between `version` and `priorVersion` (new or modified packs). canDiff is false
// when no diff is possible (priorVersion <= 0, or a query failed) — the caller
// then reconciles every pack. This is a cheap stack_files hash comparison; it
// never touches the CAS, so the common "code deploy, nothing changed" path costs
// two small queries and no store I/O.
func (c *Controller) changedPackPaths(
	ctx context.Context, tenantID, stack string, version, priorVersion int64,
) (map[string]struct{}, bool) {
	if priorVersion <= 0 || priorVersion == version {
		return nil, false
	}
	cur, err := c.packHashes(ctx, tenantID, stack, version)
	if err != nil {
		return nil, false
	}
	prev, err := c.packHashes(ctx, tenantID, stack, priorVersion)
	if err != nil {
		return nil, false
	}
	changed := make(map[string]struct{})
	for path, h := range cur {
		if prev[path] != h {
			changed[path] = struct{}{}
		}
	}
	return changed, true
}

// packHashes returns path→content_hash for the version's store-seed packs.
func (c *Controller) packHashes(
	ctx context.Context, tenantID, stack string, version int64,
) (map[string]string, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, `
		SELECT sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stack_versions sv ON sf.version_id = sv.version_id
		  JOIN stacks s          ON sv.stack_id = s.stack_id
		 WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?
		   AND (sf.path LIKE 'VECTORS/%' OR sf.path LIKE 'KV/%')`,
		tenantID, stack, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, content, hash string
		if err := rows.Scan(&path, &content, &hash); err != nil {
			return nil, err
		}
		if hash == "" {
			hash = sha256Hex(content)
		}
		out[path] = hash
	}
	return out, rows.Err()
}
