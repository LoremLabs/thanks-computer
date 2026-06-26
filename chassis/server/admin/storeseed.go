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
// Store key = the tenant SLUG. The runtime keys every store by the slug
// (processor.TenantScope), but the control plane only has the tenant_id in hand;
// keying the reconcile by the id would seed under a different identifier than the
// runtime reads (collection-not-found at search time). storeTenantKey resolves
// it. stack_files queries below stay on tenant_id — that's the durable DB FK.
//
// Change-driven, self-healing: a pack is reconciled when its content-hash differs
// from the prior active version (priorVersion) OR when its target is absent from
// the store. A code-only deploy (`txco apply`) carries packs forward unchanged
// and the targets already exist → empty set → no item reconcile (just a cheap
// presence probe per vector pack). priorVersion <= 0, or any diff failure,
// reconciles every pack (correct for a first activation / a fresh or wiped node).
//
// Best-effort by contract: it logs and swallows every error so a slow or failing
// store never stalls or rolls back a deploy — the pack bytes are durable in the
// CAS, so a missed reconcile is retried on the next apply/reload.
func (c *Controller) ReconcileStorePacks(ctx context.Context, tenantID, stack string, version, priorVersion int64, origin bool) {
	if c.storeReconciler == nil {
		return
	}
	storeKey := c.storeTenantKey(ctx, tenantID)
	var filter map[string]struct{} // nil ⇒ reconcile every pack
	if changed, canDiff := c.changedPackPaths(ctx, tenantID, stack, version, priorVersion); canDiff {
		// Union the content-changed packs with any whose store target is missing
		// (re-key migration, fresh/wiped node) so self-healing beats the skip.
		filter = map[string]struct{}{}
		for p := range changed {
			filter[p] = struct{}{}
		}
		for p := range c.missingVectorPacks(ctx, tenantID, storeKey, stack, version) {
			filter[p] = struct{}{}
		}
		if len(filter) == 0 {
			return // nothing changed AND nothing missing — no data work
		}
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
	scope := storeseed.Scope{Tenant: storeKey, Stack: stack, Version: version}
	if err := c.storeReconciler.Reconcile(ctx, scope, packs, origin); err != nil {
		c.pu.Logger.Warn("store-seed: reconcile failed (activation unaffected; retried next apply)",
			zap.String("tenant", tenantID), zap.String("stack", stack),
			zap.Int64("version", version), zap.Bool("origin", origin),
			zap.Int("packs", len(packs)), zap.String("err", err.Error()))
		return
	}
	c.pu.Logger.Info("store-seed: reconciled",
		zap.String("tenant", tenantID), zap.String("store_key", storeKey),
		zap.String("stack", stack), zap.Int64("version", version),
		zap.Bool("origin", origin), zap.Int("packs", len(packs)))
}

// storeTenantKey resolves the tenant SLUG that keys the vector + KV stores, so
// the control-plane reconcile + inspect endpoints key the same way the runtime
// reads (processor.TenantScope is the slug). On a lookup miss — a test fixture
// with no tenant row, or a not-yet-replicated tenant on a fresh follower — it
// falls back to the id so behaviour stays internally consistent (and the next
// apply/reload, by when the row has replicated, self-heals via missingVectorPacks).
func (c *Controller) storeTenantKey(ctx context.Context, tenantID string) string {
	if c.tenants != nil {
		if t, err := c.tenants.Lookup(ctx, tenantID); err == nil && t != nil && t.Slug != "" {
			return t.Slug
		}
	}
	return tenantID
}

// missingVectorPacks returns the VECTORS/ pack paths whose collection is absent
// from the store under storeKey — so a re-key migration or a fresh/wiped node
// re-seeds even when the pack CONTENT is unchanged (the change-driven diff would
// otherwise skip it). The probe is a single cheap DescribeCollection per vector
// pack (collection metadata, not the items). KV packs have no collection concept
// to probe, so they stay purely change-driven (a fresh node still seeds them on
// first activation, where priorVersion<=0 reconciles everything).
func (c *Controller) missingVectorPacks(ctx context.Context, tenantID, storeKey, stack string, version int64) map[string]struct{} {
	out := map[string]struct{}{}
	if c.vstore == nil {
		return out
	}
	hashes, err := c.packHashes(ctx, tenantID, stack, version)
	if err != nil {
		return out
	}
	for path := range hashes {
		if storeseed.KindForPath(path) != storeseed.KindVector {
			continue
		}
		name := storeseed.PackName(path)
		if name == "" {
			continue
		}
		if _, found, derr := c.vstore.DescribeCollection(ctx, storeKey, name); derr == nil && !found {
			out[path] = struct{}{}
		}
	}
	return out
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
