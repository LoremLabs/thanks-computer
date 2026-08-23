// Package blobseed is the BLOBS/ store-seed Materializer: it reconciles a
// stack's BLOBS/** tree (chassis/storeseed) into the tenant's blob name
// index (chassis/blob). The tree IS the hierarchy — BLOBS/faqs/house-01.doc
// seeds the blob name "faqs/house-01.doc".
//
// Unlike the NDJSON kinds it never sees pack BYTES: the CLI streamed each
// file to the content-addressed store before the draft referenced it, so a
// row here is {path, hash} and reconcile only writes pointers. Size comes
// from one GetReader probe (closed unread) per newly-pointed hash.
//
// Ownership + drift (the git model). Every row this materializer writes
// carries seeded_by = the stack and seeded_sha = the content the tree
// shipped. A runtime blob/put keeps both and moves only sha256, so a seeded
// name the runtime repointed is DRIFT — a remote that moved past your last
// push. `txco data apply` refuses to push over drift (pull first), so here:
//
//   - a name whose tree file is UNCHANGED (hash == seeded_sha) but has
//     drifted is left alone — the runtime edit stands;
//   - a name whose tree file CHANGED is re-asserted — the tree's new content
//     wins and the name is the pack's again;
//   - delete-missing unlinks only this stack's seeded names, and keeps a
//     drifted one (a runtime-edited document survives a tree drop);
//   - Scope.Force (`--force`) overrides all three: the tree wins outright.
//
// Permissions and filename on a seeded name are preserved across re-seeds
// (v1 packs declare none; a runtime put may have set some — never silently
// declassify).
package blobseed

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// Materializer reconciles BLOBS/ rows into a blob.Index.
type Materializer struct {
	ix     blob.Index
	fcas   filecas.Store
	shared bool
	now    func() time.Time
}

// New builds the blob Materializer. shared declares whether the INDEX's
// backing KV is fleet-shared (redis) — reconciled once on the origin — or
// per-node (boltdb). fcas may be nil (a node without a filecas), in which
// case Reconcile errors rather than seeding pointers to bytes it can't see.
func New(ix blob.Index, fcas filecas.Store, shared bool) *Materializer {
	return &Materializer{ix: ix, fcas: fcas, shared: shared, now: time.Now}
}

func (m *Materializer) Kind() string { return storeseed.KindBlob }
func (m *Materializer) Shared() bool { return m.shared }

// Reconcile makes the tenant's seeded names for scope.Stack match packs
// under the drift rules above. packs may contain the EmptyTree marker alone
// (tree removed) — that runs just the delete pass. Errors are aggregated so
// one bad row doesn't skip the rest; the caller logs them (activation is
// unaffected) and the next apply retries.
func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	if m.ix == nil {
		return errors.New("blob index not configured")
	}
	desired := map[string]string{} // name → sha256
	var errs []error
	for _, p := range packs {
		if p.Path == "" {
			continue // EmptyTree marker
		}
		if p.Kind != storeseed.KindBlob {
			errs = append(errs, fmt.Errorf("%s: not a blob row", p.Path))
			continue
		}
		if err := blob.ValidName(p.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Path, err))
			continue
		}
		if !blob.ValidSha256(p.Hash) {
			errs = append(errs, fmt.Errorf("%s: row carries no content hash", p.Path))
			continue
		}
		desired[p.Name] = p.Hash
	}
	if len(desired) > 0 && m.fcas == nil {
		return errors.New("no filecas on this node; cannot verify seeded blob bytes")
	}

	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)
	now := m.now().UTC().Truncate(time.Second)

	for _, name := range names {
		if err := m.seedOne(ctx, scope, name, desired[name], now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	// Delete-missing over THIS stack's seeded names only.
	page, err := m.ix.ListNames(ctx, scope.Tenant, blob.ListOpts{})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list names: %w", err))...)
	}
	for _, row := range page.Names {
		if row.SeededBy != scope.Stack {
			continue
		}
		if _, keep := desired[row.Name]; keep {
			continue
		}
		// Re-read right before deleting: a runtime put between the list and
		// this delete is drift, and drift survives a drop unless forced.
		cur, found, gerr := m.ix.GetName(ctx, scope.Tenant, row.Name)
		if gerr != nil {
			errs = append(errs, fmt.Errorf("%s: re-read: %w", row.Name, gerr))
			continue
		}
		if !found || cur.SeededBy != scope.Stack {
			continue
		}
		if cur.Drifted() && !scope.Force {
			continue // the tree dropped a document the runtime edited — keep it
		}
		if derr := m.ix.DeleteName(ctx, scope.Tenant, row.Name); derr != nil {
			errs = append(errs, fmt.Errorf("%s: delete stale: %w", row.Name, derr))
		}
	}
	return errors.Join(errs...)
}

func (m *Materializer) seedOne(ctx context.Context, scope storeseed.Scope, name, hash string, now time.Time) error {
	prior, hasPrior, err := m.ix.GetName(ctx, scope.Tenant, name)
	if err != nil {
		return err
	}
	same := hasPrior && prior.SHA256 == hash
	if same && prior.SeededBy == scope.Stack && prior.SeededSHA == hash {
		return nil // already the desired pointer with the bookkeeping — no churn
	}
	if hasPrior && !same && !scope.Force && prior.Drifted() && prior.SeededSHA == hash {
		// The tree did not change this file; the runtime did. The push that
		// carried this version was not forced, so the runtime edit stands.
		return nil
	}

	var size int64
	ct := blob.DefaultContentType(name)
	if same {
		size = prior.Size
		if prior.ContentType != "" {
			ct = prior.ContentType
		}
	} else {
		// One probe per newly-pointed hash: the bytes must be resident (the
		// CLI streamed them before the draft referenced them) and the size
		// comes from the reader, not a read. Closed unread.
		rc, n, gerr := filecas.GetReader(ctx, m.fcas, hash)
		if gerr != nil {
			if errors.Is(gerr, filecas.ErrNotFound) {
				return fmt.Errorf("bytes %s not resident in the CAS", hash)
			}
			return gerr
		}
		_ = rc.Close()
		size = n
	}
	if _, err := m.ix.PutShaIfAbsent(ctx, scope.Tenant, blob.ShaRow{
		SHA256: hash, Size: size, ContentType: ct, FirstSeen: now,
	}); err != nil {
		return err
	}
	row := blob.NameRow{
		Name:        name,
		SHA256:      hash,
		Size:        size,
		ContentType: ct,
		SeededBy:    scope.Stack,
		SeededSHA:   hash,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if hasPrior {
		row.Filename = prior.Filename
		row.Permissions = prior.Permissions
		if !prior.CreatedAt.IsZero() {
			row.CreatedAt = prior.CreatedAt
		}
		if same {
			row.UpdatedAt = prior.UpdatedAt // adopting bookkeeping isn't a content change
		}
	}
	return m.ix.PutName(ctx, scope.Tenant, row)
}
