package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
)

// StoreRepos is the durable repository loader: the index in
// chassis/gitstore, the objects' content in the content-addressed store.
// An object's content is stored under the sha256 of the content alone
// (no git header), so a blob's key is its file's own hash — the same key
// the drive's etag and the blob plane use for the same bytes — and the
// git type and size live on the index row, from which the header is
// rebuilt when the object is served.
type StoreRepos struct {
	st     *gitstore.Store
	fcas   filecas.Store
	limits Limits
}

// NewStoreRepos makes the loader. limits are the head's defaults; a
// repository's own quotas, where set, override them.
func NewStoreRepos(st *gitstore.Store, fcas filecas.Store, limits Limits) *StoreRepos {
	return &StoreRepos{st: st, fcas: fcas, limits: limits}
}

// Store exposes the index (the ops).
func (l *StoreRepos) Store() *gitstore.Store { return l.st }

// Limits reports the defaults.
func (l *StoreRepos) Limits() Limits { return l.limits }

// Open finds a tenant's repository. The returned Repo carries ctx for the
// go-git storer calls, which take none: it is request-scoped.
func (l *StoreRepos) Open(ctx context.Context, tenant, name string) (Repo, bool, error) {
	row, found, err := l.st.GetRepo(ctx, tenant, name)
	if err != nil || !found {
		return nil, found, err
	}
	return l.wrap(ctx, row), true, nil
}

// Create makes a repository (idempotent: created is false when it already
// existed) and opens it.
func (l *StoreRepos) Create(ctx context.Context, row gitstore.Repo) (Repo, bool, error) {
	row, created, err := l.st.CreateRepo(ctx, row)
	if err != nil {
		return nil, false, err
	}
	return l.wrap(ctx, row), created, nil
}

func (l *StoreRepos) wrap(ctx context.Context, row gitstore.Repo) *storeRepo {
	lim := l.limits
	if q := row.Quotas; q.MaxObjectBytes > 0 {
		lim.MaxObjectBytes = q.MaxObjectBytes
	}
	if q := row.Quotas; q.MaxPackBytes > 0 {
		lim.MaxPackBytes = q.MaxPackBytes
	}
	if q := row.Quotas; q.MaxRepoBytes > 0 {
		lim.MaxRepoBytes = q.MaxRepoBytes
	}
	if q := row.Quotas; q.MaxObjects > 0 {
		lim.MaxObjects = int(q.MaxObjects)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &storeRepo{ctx: ctx, st: l.st, fcas: l.fcas, row: row, limits: lim}
}

type storeRepo struct {
	ctx    context.Context
	st     *gitstore.Store
	fcas   filecas.Store
	row    gitstore.Repo
	limits Limits
}

func (r *storeRepo) Meta() Meta {
	return Meta{ID: r.row.ID, Tenant: r.row.Tenant, Name: r.row.Name, Principal: r.row.Principal,
		CreatedBy: r.row.CreatedBy, PolicyPush: r.row.PolicyPush}
}

// Limits reports the caps in force for this repository.
func (r *storeRepo) Limits() Limits { return r.limits }

func (r *storeRepo) Head() plumbing.ReferenceName { return plumbing.ReferenceName(r.row.Head) }

func (r *storeRepo) Refs(ctx context.Context) ([]Ref, error) {
	rows, err := r.st.ListRefs(ctx, r.row.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(rows))
	for _, ref := range rows {
		out = append(out, Ref{Name: plumbing.ReferenceName(ref.Name), Hash: plumbing.NewHash(ref.OID)})
	}
	return out, nil
}

func (r *storeRepo) UpdateRefs(ctx context.Context, updates []RefUpdate, by Actor) error {
	ups := make([]gitstore.RefUpdate, 0, len(updates))
	for _, u := range updates {
		ups = append(ups, gitstore.RefUpdate{Name: u.Name.String(), Old: u.Old.String(), New: u.New.String()})
	}
	err := r.st.CompareAndSetRefs(ctx, r.row.ID, ups, gitstore.Actor{Principal: by.Principal, Credential: by.Credential, Source: by.Source, Stack: by.Stack})
	var moved *gitstore.ErrRefMoved
	if errors.As(err, &moved) {
		return &ErrRefMoved{Name: plumbing.ReferenceName(moved.Name), Current: plumbing.NewHash(moved.Current)}
	}
	return err
}

func (r *storeRepo) Stage() Staging { return NewStaging(r, r.limits) }

// StoredBytes is the repository's size today (the cap's base).
func (r *storeRepo) StoredBytes(ctx context.Context) (int64, error) {
	_, bytes, err := r.st.ObjectStats(ctx, r.row.ID)
	return bytes, err
}

// ── the object storer ────────────────────────────────────────────────────

func (r *storeRepo) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (r *storeRepo) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	hs, err := r.SetEncodedObjects([]plumbing.EncodedObject{o})
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return hs[0], nil
}

// SetEncodedObjects stores several objects: every content to the
// content-addressed store, then one index transaction. The staging's
// Commit uses it so a push is one write.
func (r *storeRepo) SetEncodedObjects(objs []plumbing.EncodedObject) ([]plumbing.Hash, error) {
	rows := make([]gitstore.Object, 0, len(objs))
	hashes := make([]plumbing.Hash, 0, len(objs))
	for _, o := range objs {
		rd, err := o.Reader()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rd)
		_ = rd.Close()
		if err != nil {
			return nil, err
		}
		if r.limits.MaxObjectBytes > 0 && int64(len(content)) > r.limits.MaxObjectBytes {
			return nil, fmt.Errorf("%w: %d bytes > %d", ErrObjectTooLarge, len(content), r.limits.MaxObjectBytes)
		}
		oid := plumbing.ComputeHash(o.Type(), content)
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		if err := r.fcas.Put(r.ctx, sha, content); err != nil {
			return nil, fmt.Errorf("git: store object %s: %w", oid, err)
		}
		rows = append(rows, gitstore.Object{OID: oid.String(), Type: o.Type().String(), Size: int64(len(content)), SHA256: sha})
		hashes = append(hashes, oid)
	}
	if err := r.st.PutObjects(r.ctx, r.row.ID, rows); err != nil {
		return nil, err
	}
	return hashes, nil
}

func (r *storeRepo) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	row, found, err := r.st.GetObject(r.ctx, r.row.ID, h.String())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, plumbing.ErrObjectNotFound
	}
	typ, err := plumbing.ParseObjectType(row.Type)
	if err != nil {
		return nil, fmt.Errorf("git: object %s has type %q", h, row.Type)
	}
	if t != plumbing.AnyObject && t != typ {
		return nil, plumbing.ErrObjectNotFound
	}
	content := row.Inline
	if content == nil {
		content, err = r.fcas.Get(r.ctx, row.SHA256)
		if err != nil {
			return nil, fmt.Errorf("git: object %s content: %w", h, err)
		}
	}
	o := &plumbing.MemoryObject{}
	o.SetType(typ)
	o.SetSize(int64(len(content)))
	if _, err := o.Write(content); err != nil {
		return nil, err
	}
	return o, nil
}

func (r *storeRepo) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	typ := ""
	if t != plumbing.AnyObject {
		typ = t.String()
	}
	rows, err := r.st.ListObjects(r.ctx, r.row.ID, typ)
	if err != nil {
		return nil, err
	}
	hashes := make([]plumbing.Hash, 0, len(rows))
	for _, o := range rows {
		hashes = append(hashes, plumbing.NewHash(o.OID))
	}
	return storer.NewEncodedObjectLookupIter(r, t, hashes), nil
}

func (r *storeRepo) HasEncodedObject(h plumbing.Hash) error {
	_, found, err := r.st.GetObject(r.ctx, r.row.ID, h.String())
	if err != nil {
		return err
	}
	if !found {
		return plumbing.ErrObjectNotFound
	}
	return nil
}

func (r *storeRepo) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	row, found, err := r.st.GetObject(r.ctx, r.row.ID, h.String())
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, plumbing.ErrObjectNotFound
	}
	return row.Size, nil
}

func (r *storeRepo) AddAlternate(string) error { return ErrAlternates }
