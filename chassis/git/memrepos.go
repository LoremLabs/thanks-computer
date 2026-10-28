package git

import (
	"context"
	"sort"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// MemRepos is the in-memory repository loader: every (tenant, name) is a
// repository, made on first touch, kept until the process ends. It served
// the prototype and serves the tests; the durable loader is StoreRepos.
type MemRepos struct {
	mu     sync.Mutex
	repos  map[string]*memRepo
	limits Limits
}

// NewMemRepos makes an empty loader. limits bound each push's staging.
func NewMemRepos(limits Limits) *MemRepos {
	return &MemRepos{repos: map[string]*memRepo{}, limits: limits}
}

// Open returns the tenant's repository, making it when absent.
func (m *MemRepos) Open(_ context.Context, tenant, name string) (Repo, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenant + "/" + name
	r, ok := m.repos[key]
	if !ok {
		r = &memRepo{
			EncodedObjectStorer: memory.NewStorage(),
			meta:                Meta{ID: "mem:" + key, Tenant: tenant, Name: name, PolicyPush: "open"},
			refs:                map[plumbing.ReferenceName]plumbing.Hash{},
			head:                plumbing.NewBranchReferenceName("main"),
			limits:              m.limits,
		}
		m.repos[key] = r
	}
	return r, true, nil
}

type memRepo struct {
	storer.EncodedObjectStorer // the objects

	mu     sync.RWMutex
	meta   Meta
	refs   map[plumbing.ReferenceName]plumbing.Hash
	head   plumbing.ReferenceName
	limits Limits
}

func (r *memRepo) Meta() Meta                   { return r.meta }
func (r *memRepo) Head() plumbing.ReferenceName { return r.head }

func (r *memRepo) Refs(context.Context) ([]Ref, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Ref, 0, len(r.refs))
	for n, h := range r.refs {
		out = append(out, Ref{Name: n, Hash: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *memRepo) UpdateRefs(_ context.Context, updates []RefUpdate, _ Actor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range updates {
		cur, ok := r.refs[u.Name]
		if !ok {
			cur = plumbing.ZeroHash
		}
		if cur != u.Old {
			return &ErrRefMoved{Name: u.Name, Current: cur}
		}
	}
	for _, u := range updates {
		if u.New.IsZero() {
			delete(r.refs, u.Name)
		} else {
			r.refs[u.Name] = u.New
		}
	}
	return nil
}

func (r *memRepo) Stage() Staging { return NewStaging(r, r.limits) }

// StoredBytes sums the objects' sizes (the repository cap).
func (r *memRepo) StoredBytes(context.Context) (int64, error) {
	it, err := r.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return 0, err
	}
	var n int64
	err = it.ForEach(func(o plumbing.EncodedObject) error {
		n += o.Size()
		return nil
	})
	return n, err
}
