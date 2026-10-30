// Package git is the chassis's git protocol core: the storage seam the
// `git` personality (chassis/server/personality/git) and the txco://repo/*
// ops share, with two implementations of it — repositories in memory (the
// prototype's, and the tests') and repositories over the chassis's index
// (chassis/gitstore) and content-addressed store (chassis/filecas).
//
// What a repository IS here: go-git's object storer (the packfile parser
// writes it, the encoder and the reachability walk read it), a list of
// refs, and ONE write that moves refs — all of them or none, each against
// the value the writer last saw — plus a staging area a push fills before
// anything is kept. The protocol framing, the policy lanes and the ops sit
// above this seam; a second transport (the parked SSH facade for stacks)
// would sit above it too.
package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Limits are the caps a push is held to. Zero means "no cap" for the byte
// and count limits.
type Limits struct {
	// MaxPackBytes caps one request body (a push's pack, a fetch's haves).
	MaxPackBytes int64
	// MaxObjectBytes caps one object (a file's bytes).
	MaxObjectBytes int64
	// MaxObjects caps the objects in one pack.
	MaxObjects int
	// MaxRepoBytes caps a repository's stored objects, the new ones included.
	MaxRepoBytes int64
	// TextOnly refuses a blob that is not valid UTF-8 (or holds a NUL), a
	// symlink, an executable and a submodule.
	TextOnly bool
}

// Meta is what the head and the ops know about a repository besides its
// objects and refs.
type Meta struct {
	ID        string
	Tenant    string
	Name      string
	Principal string
	CreatedBy string
	// PolicyPush is "stack" (a push is accepted only when the tenant's
	// `_git` stack answers ok) or "open" (no lane).
	PolicyPush string
	// PolicyContent is "text" (a blob that is not text is refused by the
	// head, --git-text-only) or "stack" (the `_git` stack decides from the
	// `text`/`type` facts; a symlink, an executable and a submodule are
	// still refused).
	PolicyContent string
}

// Repo is one repository as the protocol sees it.
//
// Objects are looked up by id in THIS repository only: a store shared
// across tenants (the content-addressed store) is reached through the
// repository's own index and never probed on a client's behalf, so one
// tenant can never learn what another holds.
type Repo interface {
	storer.EncodedObjectStorer

	// Meta describes the repository.
	Meta() Meta
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
// anything (authorization happened before the move).
type Actor struct {
	Principal  string
	Credential string
	// Source is "push" or "op".
	Source string
	// Stack is the stack whose op moved the ref ("" for a push).
	Stack string
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
	// refs the push moves); the rest are dropped. ErrRepoTooLarge when the
	// repository would pass its cap.
	Commit(ctx context.Context, keep []plumbing.Hash) error
}

// RepoLoader opens a tenant's repository by name. found is false when the
// tenant has no repository of that name.
type RepoLoader interface {
	Open(ctx context.Context, tenant, name string) (repo Repo, found bool, err error)
}

// The errors a staging answers past the caps; the push maps them to a
// one-line reason the client prints.
var (
	ErrObjectTooLarge = errors.New("object over the size cap")
	ErrTooManyObjects = errors.New("too many objects in the pack")
	ErrRepoTooLarge   = errors.New("repository over its size cap")
	ErrAlternates     = errors.New("alternates are not supported")
)
