package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// storedBytes is what a repository may offer the staging so it can hold
// the repository's size cap: how many bytes its objects take today.
type storedBytes interface {
	StoredBytes(ctx context.Context) (int64, error)
}

// NewStaging is a memory staging area over any Repo: the pack's objects
// are kept in memory until Commit, reads fall through to the repository.
// The caps are enforced as the parser writes: an object past
// MaxObjectBytes or a count past MaxObjects fails the write with the
// matching error.
func NewStaging(repo Repo, limits Limits) Staging {
	return &memStaging{repo: repo, mem: memory.NewStorage(), limits: limits}
}

type memStaging struct {
	repo   Repo
	mem    *memory.Storage
	limits Limits
	count  int
}

func (s *memStaging) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (s *memStaging) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if s.limits.MaxObjectBytes > 0 && o.Size() > s.limits.MaxObjectBytes {
		return plumbing.ZeroHash, fmt.Errorf("%w: %d bytes > %d", ErrObjectTooLarge, o.Size(), s.limits.MaxObjectBytes)
	}
	if s.limits.MaxObjects > 0 && s.count >= s.limits.MaxObjects {
		return plumbing.ZeroHash, fmt.Errorf("%w: more than %d", ErrTooManyObjects, s.limits.MaxObjects)
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

func (s *memStaging) AddAlternate(string) error { return ErrAlternates }

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

func (s *memStaging) Commit(ctx context.Context, keep []plumbing.Hash) error {
	// Which of the kept objects are new, and how big they are together.
	var fresh []plumbing.EncodedObject
	var add int64
	for _, h := range keep {
		o, err := s.mem.EncodedObject(plumbing.AnyObject, h)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue // already in the repository
		}
		if err != nil {
			return err
		}
		fresh = append(fresh, o)
		add += o.Size()
	}
	if s.limits.MaxRepoBytes > 0 {
		if sb, ok := s.repo.(storedBytes); ok {
			have, err := sb.StoredBytes(ctx)
			if err != nil {
				return err
			}
			if have+add > s.limits.MaxRepoBytes {
				return fmt.Errorf("%w: %d + %d bytes > %d", ErrRepoTooLarge, have, add, s.limits.MaxRepoBytes)
			}
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	if many, ok := s.repo.(interface {
		SetEncodedObjects([]plumbing.EncodedObject) ([]plumbing.Hash, error)
	}); ok {
		_, err := many.SetEncodedObjects(fresh)
		return err
	}
	for _, o := range fresh {
		if _, err := s.repo.SetEncodedObject(o); err != nil {
			return err
		}
	}
	return nil
}
