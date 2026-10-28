package git

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// MemRepos is the in-memory repository loader: every (tenant, name) is a
// repository, made on first touch, kept until the process ends. It is the
// prototype's store [G0] and the tests' — the durable store over the
// content-addressed store is G1's.
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
	refs   map[plumbing.ReferenceName]plumbing.Hash
	head   plumbing.ReferenceName
	limits Limits
}

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

func (r *memRepo) Stage() Staging {
	return &memStaging{repo: r, mem: memory.NewStorage(), limits: r.limits}
}

// errObjectTooLarge and errTooManyObjects are what the staging answers the
// packfile parser past the caps; the push maps them to a one-line reason.
var (
	errObjectTooLarge  = errors.New("object over the size cap")
	errTooManyObjects  = errors.New("too many objects in the pack")
	errAlternatesUnsup = errors.New("alternates are not supported")
)

// memStaging is a memory staging area over any Repo: new objects are kept
// in mem, reads fall through to the repository.
type memStaging struct {
	repo   Repo
	mem    *memory.Storage
	limits Limits
	count  int
}

func (s *memStaging) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (s *memStaging) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if s.limits.MaxObjectBytes > 0 && o.Size() > s.limits.MaxObjectBytes {
		return plumbing.ZeroHash, fmt.Errorf("%w: %d bytes > %d", errObjectTooLarge, o.Size(), s.limits.MaxObjectBytes)
	}
	if s.limits.MaxObjects > 0 && s.count >= s.limits.MaxObjects {
		return plumbing.ZeroHash, fmt.Errorf("%w: more than %d", errTooManyObjects, s.limits.MaxObjects)
	}
	h, err := s.mem.SetEncodedObject(o)
	if err == nil {
		s.count++
	}
	return h, err
}

func (s *memStaging) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	o, err := s.mem.EncodedObject(t, h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return s.repo.EncodedObject(t, h)
	}
	return o, err
}

func (s *memStaging) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return s.mem.IterEncodedObjects(t)
}

func (s *memStaging) HasEncodedObject(h plumbing.Hash) error {
	if err := s.mem.HasEncodedObject(h); err == nil {
		return nil
	}
	return s.repo.HasEncodedObject(h)
}

func (s *memStaging) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	n, err := s.mem.EncodedObjectSize(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return s.repo.EncodedObjectSize(h)
	}
	return n, err
}

func (s *memStaging) AddAlternate(string) error { return errAlternatesUnsup }

func (s *memStaging) Staged() []plumbing.Hash {
	var out []plumbing.Hash
	it, err := s.mem.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return nil
	}
	_ = it.ForEach(func(o plumbing.EncodedObject) error {
		out = append(out, o.Hash())
		return nil
	})
	return out
}

func (s *memStaging) Commit(_ context.Context, keep []plumbing.Hash) error {
	for _, h := range keep {
		o, err := s.mem.EncodedObject(plumbing.AnyObject, h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue // already in the repository
		}
		if err != nil {
			return err
		}
		if _, err := s.repo.SetEncodedObject(o); err != nil {
			return err
		}
	}
	return nil
}
