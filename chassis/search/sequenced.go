package search

import "context"

// The interfaces in this file are OPTIONAL. A backend that keeps its index on
// local disk may implement them; the op layer never asks for them. They exist
// for whoever runs such a backend as a replaceable projection: applying an
// ordered log of mutations exactly once, and copying a collection out and back
// in as a unit. Nothing here knows where a log or a copy lives.

// Sequenced applies mutations that carry a position in some ordered log.
type Sequenced interface {
	// ApplyAt applies m to the collection unless the collection has already
	// applied a mutation at seq or later, in which case it does nothing and
	// reports applied == false. The position is stored in the same write as
	// the mutation's last change, so after a crash a collection is never
	// ahead of, or behind, the position it reports. A replay of the same log
	// from any earlier point therefore converges on the same records.
	//
	// OpEnsure creates the collection, OpDrop removes it, and the other ops
	// need it to exist (CollectionNotFoundError otherwise). count is what the
	// matching Store method would have returned.
	ApplyAt(ctx context.Context, tenant, collection string, seq uint64, m Mutation) (count int, applied bool, err error)

	// AppliedSeq reports the position of the last mutation the collection
	// applied. found is false when the collection is not on this node's disk.
	AppliedSeq(ctx context.Context, tenant, collection string) (seq uint64, found bool, err error)
}

// Manifest describes one snapshot of one collection.
type Manifest struct {
	Tenant          string `json:"tenant"`
	Collection      string `json:"collection"`
	AppliedSeq      uint64 `json:"applied_seq"`
	Records         uint64 `json:"records"`
	AnalyzerVersion string `json:"analyzer_version"`
	ScoringModel    string `json:"scoring_model"`
	Engine          string `json:"engine"`
	CreatedAt       string `json:"created_at"`
}

// ManifestFile is the manifest's name inside a snapshot directory.
const ManifestFile = "manifest.json"

// Snapshotter copies a collection out of a live store, and back in.
type Snapshotter interface {
	// StagingDir is a scratch directory on the same filesystem as the store,
	// so that Install can move a snapshot into place instead of copying it.
	StagingDir() (string, error)

	// Snapshot writes a complete, self-contained copy of the collection into
	// dir, which must not exist. The collection stays open and writable
	// meanwhile. The copy is opened and checked before Snapshot returns, and
	// the manifest records what it actually holds, not what was asked for.
	Snapshot(ctx context.Context, tenant, collection, dir string) (Manifest, error)

	// Install makes the snapshot at dir (as written by Snapshot) this store's
	// copy of the collection, replacing any it has. dir is consumed.
	Install(ctx context.Context, tenant, collection, dir string) (Manifest, error)
}
