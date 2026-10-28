package git

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Repo is one repository as the protocol sees it: its objects (go-git's
// storer interface, which the packfile parser writes and the encoder
// reads), its refs, and the one write that moves refs — all of them or
// none, each against the value the writer last saw.
//
// Objects are looked up by id in THIS repository only: a store shared
// across tenants (the chassis's content-addressed store, G1) is reached
// through the repository's own index and never probed on a client's
// behalf, so one tenant can never learn what another holds.
type Repo interface {
	storer.EncodedObjectStorer

	// Head is the ref HEAD points at (refs/heads/main): what a clone checks
	// out, and the branch a push may not delete.
	Head() plumbing.ReferenceName
	// Refs lists every ref, sorted by name.
	Refs(ctx context.Context) ([]Ref, error)
	// UpdateRefs moves refs by compare-and-swap: every update's Old must be
	// the ref's current value (plumbing.ZeroHash for a ref that does not
	// exist), or nothing moves and the first mismatch is returned as
	// *ErrRefMoved. A zero New deletes the ref.
	UpdateRefs(ctx context.Context, updates []RefUpdate, by Actor) error
	// Stage opens a staging area for one push: the pack's objects land
	// there, are checked, and only the reachable ones are kept.
	Stage() Staging
}

// Ref is one ref and the object it names.
type Ref struct {
	Name plumbing.ReferenceName
	Hash plumbing.Hash
}

// RefUpdate is one command of a push, or one application-side move.
type RefUpdate struct {
	Name plumbing.ReferenceName
	Old  plumbing.Hash
	New  plumbing.Hash
}

// Actor is who moved a ref: the principal and credential a push signed in
// with, or the stack that called an op. Recorded, never trusted for
// anything (authorisation happened before the move).
type Actor struct {
	Principal  string
	Credential string
	// Source is "push" or "op".
	Source string
}

// ErrRefMoved is the compare-and-swap failure: the ref is not at Old.
type ErrRefMoved struct {
	Name    plumbing.ReferenceName
	Current plumbing.Hash
}

func (e *ErrRefMoved) Error() string {
	return fmt.Sprintf("ref %s moved: now %s", e.Name, e.Current)
}

// Staging holds one push's objects until they are kept. Reads fall through
// to the repository, so a thin pack's deltas against objects the
// repository already has resolve, and the reachability walk sees both.
type Staging interface {
	storer.EncodedObjectStorer

	// Staged lists the objects the pack delivered.
	Staged() []plumbing.Hash
	// Commit keeps exactly the listed objects (those reachable from the
	// refs the push moves); the rest are dropped.
	Commit(ctx context.Context, keep []plumbing.Hash) error
}

// RepoLoader opens a tenant's repository by name. found is false when the
// tenant has no repository of that name.
type RepoLoader interface {
	Open(ctx context.Context, tenant, name string) (repo Repo, found bool, err error)
}
