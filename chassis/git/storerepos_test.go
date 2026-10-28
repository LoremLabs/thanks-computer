package git

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
)

func newStoreRepos(t *testing.T, limits Limits) (*StoreRepos, filecas.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := gitstore.OpenSQLiteFile(filepath.Join(dir, "git.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs, err := filestore.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return NewStoreRepos(st, fs, limits), fs
}

func blob(content string) plumbing.EncodedObject {
	o := &plumbing.MemoryObject{}
	o.SetType(plumbing.BlobObject)
	o.SetSize(int64(len(content)))
	_, _ = o.Write([]byte(content))
	return o
}

func read(t *testing.T, o plumbing.EncodedObject) string {
	t.Helper()
	rd, err := o.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStoreReposRoundTripAndFence(t *testing.T) {
	ctx := context.Background()
	l, fs := newStoreRepos(t, Limits{MaxObjectBytes: 1 << 20, MaxObjects: 100, MaxRepoBytes: 1 << 20})
	if _, found, err := l.Open(ctx, "acme", "pony"); err != nil || found {
		t.Fatalf("a repository nobody made is found: %v %v", found, err)
	}
	repo, created, err := l.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "pony", Principal: "pony:pony", CreatedBy: "ranch"})
	if err != nil || !created {
		t.Fatal(err)
	}
	if m := repo.Meta(); m.Tenant != "acme" || m.Name != "pony" || m.Principal != "pony:pony" || m.PolicyPush != "stack" {
		t.Fatalf("meta: %+v", m)
	}
	// A push's worth of objects through the staging.
	st := repo.Stage()
	h1, err := st.SetEncodedObject(blob("# Mission\n"))
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := st.SetEncodedObject(blob("dropped\n"))
	if len(st.Staged()) != 2 {
		t.Fatalf("staged: %v", st.Staged())
	}
	if err := repo.HasEncodedObject(h1); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the repository sees a staged object before Commit: %v", err)
	}
	if err := st.HasEncodedObject(h1); err != nil {
		t.Fatalf("the staging does not see its own object: %v", err)
	}
	if err := st.Commit(ctx, []plumbing.Hash{h1}); err != nil {
		t.Fatal(err)
	}
	o, err := repo.EncodedObject(plumbing.BlobObject, h1)
	if err != nil || read(t, o) != "# Mission\n" || o.Type() != plumbing.BlobObject || o.Size() != 10 {
		t.Fatalf("round trip: %v %+v", err, o)
	}
	if _, err := repo.EncodedObject(plumbing.CommitObject, h1); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the wrong type should be not-found: %v", err)
	}
	if err := repo.HasEncodedObject(h2); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatal("the object outside keep was stored")
	}
	if n, err := repo.EncodedObjectSize(h1); err != nil || n != 10 {
		t.Fatalf("size: %d %v", n, err)
	}
	// The content sits in the CAS under the sha256 of the bytes alone.
	if ok, _ := fs.Exists(ctx, "7b1e5b7d7d93f6d2de3e3fa3a0d5f6b9a7d0b2c1a1c7d2f1e1b4a5c6d7e8f9a0"); ok {
		t.Fatal("unexpected key")
	}
	// Another tenant's repository of the same name sees nothing.
	other, _, err := l.Create(ctx, gitstore.Repo{Tenant: "other", Name: "pony"})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.HasEncodedObject(h1); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatal("the tenant fence leaks")
	}
	// The same bytes in the other repository: one more index row, the same CAS key.
	st2 := other.Stage()
	h1b, _ := st2.SetEncodedObject(blob("# Mission\n"))
	if h1b != h1 {
		t.Fatal("same content, different oid")
	}
	if err := st2.Commit(ctx, []plumbing.Hash{h1b}); err != nil {
		t.Fatal(err)
	}
	if err := other.HasEncodedObject(h1); err != nil {
		t.Fatal("the second repository does not see its own copy")
	}
	// Refs through the seam.
	if err := repo.UpdateRefs(ctx, []RefUpdate{{Name: "refs/heads/main", Old: plumbing.ZeroHash, New: h1}}, Actor{Principal: "pony:pony", Source: "push"}); err != nil {
		t.Fatal(err)
	}
	refs, _ := repo.Refs(ctx)
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" || refs[0].Hash != h1 {
		t.Fatalf("refs: %+v", refs)
	}
	err = repo.UpdateRefs(ctx, []RefUpdate{{Name: "refs/heads/main", Old: plumbing.ZeroHash, New: h2}}, Actor{})
	var moved *ErrRefMoved
	if !errors.As(err, &moved) || moved.Current != h1 {
		t.Fatalf("moved: %v", err)
	}
	// The iterator lists what the index holds.
	it, err := repo.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	_ = it.ForEach(func(plumbing.EncodedObject) error { n++; return nil })
	if n != 1 {
		t.Fatalf("iterated %d objects, want 1", n)
	}
}

func TestStoreReposRepoCap(t *testing.T) {
	ctx := context.Background()
	l, _ := newStoreRepos(t, Limits{MaxRepoBytes: 32})
	repo, _, err := l.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "pony"})
	if err != nil {
		t.Fatal(err)
	}
	st := repo.Stage()
	h1, _ := st.SetEncodedObject(blob("twenty bytes of text"))
	if err := st.Commit(ctx, []plumbing.Hash{h1}); err != nil {
		t.Fatal(err)
	}
	st = repo.Stage()
	h2, _ := st.SetEncodedObject(blob("another twenty bytes"))
	if err := st.Commit(ctx, []plumbing.Hash{h2}); !errors.Is(err, ErrRepoTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
	if err := repo.HasEncodedObject(h2); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatal("the refused object was stored")
	}
	// A repository's own quota overrides the default.
	big, _, _ := l.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "big", Quotas: gitstore.Quotas{MaxRepoBytes: 1 << 20}})
	st = big.Stage()
	h3, _ := st.SetEncodedObject(blob("a much larger allowance for this one, by its own quota row"))
	if err := st.Commit(ctx, []plumbing.Hash{h3}); err != nil {
		t.Fatal(err)
	}
}
