package drive_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/drive/filestore"
)

// newTestStore opens a per-test temp-file SQLite index with the production
// DSN shape, a file object store in a temp dir, applies the schema twice
// (idempotence), and pins the clock.
func newTestStore(t *testing.T) (*drive.Store, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "drive.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	objects, err := filestore.New(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	clk := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := drive.NewStore(db, registry.SQLite, objects)
	s.SetClock(func() time.Time { return clk })
	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("ensure schema #%d: %v", i, err)
		}
	}
	return s, &clk
}

func newColl(t *testing.T, s *drive.Store) drive.Collection {
	t.Helper()
	c, created, err := s.EnsureCollection(context.Background(), "tnt_a", "docs")
	if err != nil || !created {
		t.Fatalf("EnsureCollection: created=%v err=%v", created, err)
	}
	return c
}

func put(t *testing.T, s *drive.Store, collID, p, body string) drive.PutResult {
	t.Helper()
	r, err := s.Put(context.Background(), collID, p, strings.NewReader(body), int64(len(body)), drive.PutOpts{})
	if err != nil {
		t.Fatalf("Put %s: %v", p, err)
	}
	return r
}

func read(t *testing.T, s *drive.Store, collID, p string) (string, drive.Resource) {
	t.Helper()
	rc, res, err := s.Open(context.Background(), collID, p)
	if err != nil {
		t.Fatalf("Open %s: %v", p, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b), res
}

// recSink records mutations; withTx makes it transactional (the row is
// "written" inside the tx and rolled back with it).
type recSink struct {
	withTx bool
	tx     []drive.Mutation
	post   []drive.Mutation
}

func (r *recSink) Enqueue(_ context.Context, m drive.Mutation) error {
	r.post = append(r.post, m)
	return nil
}

type recTxSink struct{ *recSink }

func (r recTxSink) EnqueueTx(_ context.Context, tx *sql.Tx, m drive.Mutation) error {
	if tx == nil {
		return errors.New("nil tx")
	}
	r.tx = append(r.tx, m)
	return nil
}

func TestCollectionsAndAccounts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	if !strings.HasPrefix(c.ID, "dc_") || c.Tenant != "tnt_a" || c.Name != "docs" {
		t.Fatalf("collection = %+v", c)
	}
	again, created, err := s.EnsureCollection(ctx, "tnt_a", "docs")
	if err != nil || created || again.ID != c.ID {
		t.Fatalf("second Ensure: %+v created=%v err=%v", again, created, err)
	}
	if _, _, err := s.EnsureCollection(ctx, "tnt_a", "bad name"); err == nil {
		t.Fatal("bad name accepted")
	}
	got, ok, err := s.GetCollection(ctx, "tnt_a", "docs")
	if err != nil || !ok || got.ID != c.ID {
		t.Fatalf("GetCollection: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.GetCollection(ctx, "tnt_b", "docs"); ok {
		t.Fatal("other tenant sees the collection")
	}
	list, err := s.ListCollections(ctx, "tnt_a")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCollections: %d %v", len(list), err)
	}

	// Accounts: create, update, tenant fence, collection ownership.
	created, err = s.UpsertAccount(ctx, "tnt_a", "Alice", "h1", "", c.ID)
	if err != nil || !created {
		t.Fatalf("UpsertAccount: created=%v err=%v", created, err)
	}
	a, ok, err := s.GetAccount(ctx, "alice")
	if err != nil || !ok || a.PwHash != "h1" || a.Status != drive.StatusActive || a.CollectionID != c.ID {
		t.Fatalf("GetAccount: %+v ok=%v err=%v", a, ok, err)
	}
	if _, err := s.UpsertAccount(ctx, "tnt_a", "alice", "", drive.StatusDisabled, ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	a, _, _ = s.GetAccount(ctx, "alice")
	if a.PwHash != "h1" || a.Status != drive.StatusDisabled {
		t.Fatalf("update kept nothing: %+v", a)
	}
	cb, _, err := s.EnsureCollection(ctx, "tnt_b", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertAccount(ctx, "tnt_b", "alice", "h2", "", cb.ID); !errors.Is(err, drive.ErrUsernameTaken) {
		t.Fatalf("cross-tenant upsert: %v", err)
	}
	if _, err := s.UpsertAccount(ctx, "tnt_b", "bob", "h2", "", c.ID); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("other tenant's collection accepted: %v", err)
	}
	if _, err := s.UpsertAccount(ctx, "tnt_a", "carol", "", "", c.ID); err == nil {
		t.Fatal("create without password accepted")
	}

	// Delete: refuses non-empty, force tombstones.
	put(t, s, c.ID, "a.txt", "x")
	if _, err := s.DeleteCollection(ctx, "tnt_a", "docs", false); !errors.Is(err, drive.ErrNotEmpty) {
		t.Fatalf("DeleteCollection non-empty: %v", err)
	}
	if ok, err := s.DeleteCollection(ctx, "tnt_a", "docs", true); err != nil || !ok {
		t.Fatalf("DeleteCollection force: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.GetCollection(ctx, "tnt_a", "docs"); ok {
		t.Fatal("deleted collection still live")
	}
	if _, ok, _ := s.Stat(ctx, c.ID, "a.txt"); ok {
		t.Fatal("resource of deleted collection still live")
	}
	// Same name again is a NEW collection.
	c2, created, err := s.EnsureCollection(ctx, "tnt_a", "docs")
	if err != nil || !created || c2.ID == c.ID {
		t.Fatalf("recreate: %+v created=%v err=%v", c2, created, err)
	}
}

func TestRootAndBadPaths(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)

	root, ok, err := s.Stat(ctx, c.ID, "/")
	if err != nil || !ok || !root.IsDir() || root.ResourceID != c.ID || root.ModSeq != 0 {
		t.Fatalf("root: %+v ok=%v err=%v", root, ok, err)
	}
	if _, ok, _ := s.Stat(ctx, "dc_nope", ""); ok {
		t.Fatal("root of a missing collection exists")
	}
	if _, err := s.Put(ctx, c.ID, "/", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("put root: %v", err)
	}
	if _, err := s.Put(ctx, c.ID, "notes/../x", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("put dotdot: %v", err)
	}
	if _, err := s.Put(ctx, "dc_nope", "x", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("put into missing collection: %v", err)
	}
	if _, err := s.Mkdir(ctx, c.ID, ""); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, _, err := s.Stat(ctx, c.ID, "a//b"); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("stat bad path: %v", err)
	}
	r := put(t, s, c.ID, "/hello.txt/", "hello")
	if !strings.HasPrefix(r.ResourceID, "dr_") || !r.Created || r.Noop || r.Size != 5 || r.ETag != drive.ETagOf([]byte("hello")) || r.ModSeq != 1 {
		t.Fatalf("put: %+v", r)
	}
	if got, ok, _ := s.Stat(ctx, c.ID, "hello.txt"); !ok || got.Path != "hello.txt" {
		t.Fatalf("trimmed path: %+v ok=%v", got, ok)
	}
}

func TestPutRoundTrip(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)

	r := put(t, s, c.ID, "hello.txt", "hello")
	if !r.Created || r.ModSeq != 1 || r.ETag != drive.ETagOf([]byte("hello")) {
		t.Fatalf("put: %+v", r)
	}
	body, res := read(t, s, c.ID, "hello.txt")
	if body != "hello" || res.ResourceID != r.ResourceID || res.ContentType != "text/plain; charset=utf-8" || res.Depth != 1 || res.ParentPath != "" {
		t.Fatalf("read: %q %+v", body, res)
	}

	// Same bytes: noop, no modseq bump, no new version.
	*clk = clk.Add(time.Minute)
	r2, err := s.Put(ctx, c.ID, "hello.txt", strings.NewReader("hello"), 5, drive.PutOpts{})
	if err != nil || !r2.Noop || r2.ModSeq != 1 || r2.ResourceID != r.ResourceID {
		t.Fatalf("noop put: %+v %v", r2, err)
	}
	// New bytes: same id, new etag, bumped modseq.
	r3, err := s.Put(ctx, c.ID, "hello.txt", strings.NewReader("hello!"), 6, drive.PutOpts{ContentType: "text/x-custom"})
	if err != nil || r3.Created || r3.Noop || r3.ModSeq != 2 || r3.ResourceID != r.ResourceID || r3.ETag == r.ETag {
		t.Fatalf("replace put: %+v %v", r3, err)
	}
	body, res = read(t, s, c.ID, "hello.txt")
	if body != "hello!" || res.ContentType != "text/x-custom" || res.Size != 6 {
		t.Fatalf("after replace: %q %+v", body, res)
	}
	coll, _, _ := s.GetCollectionByID(ctx, c.ID)
	if coll.SyncToken != 2 || coll.BytesUsed != 6 || coll.ResourceCount != 1 {
		t.Fatalf("counters: %+v", coll)
	}

	// Size mismatch both ways.
	if _, err := s.Put(ctx, c.ID, "short.txt", strings.NewReader("abc"), 5, drive.PutOpts{}); !errors.Is(err, drive.ErrSizeMismatch) {
		t.Fatalf("short body: %v", err)
	}
	if _, err := s.Put(ctx, c.ID, "long.txt", strings.NewReader("abcdef"), 5, drive.PutOpts{}); !errors.Is(err, drive.ErrSizeMismatch) {
		t.Fatalf("long body: %v", err)
	}
	if _, ok, _ := s.Stat(ctx, c.ID, "short.txt"); ok {
		t.Fatal("failed put left a row")
	}
	// Zero-byte file.
	z := put(t, s, c.ID, "empty", "")
	if z.Size != 0 || z.ETag != drive.ETagOf(nil) {
		t.Fatalf("empty: %+v", z)
	}
	if body, _ := read(t, s, c.ID, "empty"); body != "" {
		t.Fatalf("empty read: %q", body)
	}
	// Unicode + spaces, NFD folds to NFC.
	put(t, s, c.ID, "café menu.txt", "u")
	if _, ok, err := s.Stat(ctx, c.ID, "café menu.txt"); err != nil || !ok {
		t.Fatalf("NFC stat: ok=%v err=%v", ok, err)
	}
}

func TestPreconditionsAndLimits(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	r := put(t, s, c.ID, "f", "one")

	cases := []struct {
		name string
		opts drive.PutOpts
		want error
	}{
		{"create-only on existing", drive.PutOpts{IfNoneMatch: "*"}, drive.ErrPrecondition},
		{"if-match wrong", drive.PutOpts{IfMatch: "nope"}, drive.ErrPrecondition},
		{"if-none-match same", drive.PutOpts{IfNoneMatch: r.ETag}, drive.ErrPrecondition},
		{"if-match right", drive.PutOpts{IfMatch: r.ETag}, nil},
		{"if-match star", drive.PutOpts{IfMatch: "*"}, nil},
	}
	for _, tc := range cases {
		_, err := s.Put(ctx, c.ID, "f", strings.NewReader("two"), 3, tc.opts)
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: err=%v want %v", tc.name, err, tc.want)
		}
		// Reset content for the next case (same bytes ⇒ same etag).
		put(t, s, c.ID, "f", "one")
	}
	if _, err := s.Put(ctx, c.ID, "missing", strings.NewReader("x"), 1, drive.PutOpts{IfMatch: "*"}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("if-match on missing: %v", err)
	}
	if _, err := s.Delete(ctx, c.ID, "f", drive.DeleteOpts{IfMatch: "nope"}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("delete if-match: %v", err)
	}

	s.SetLimits(drive.Limits{MaxFileBytes: 4, MaxCollectionBytes: 10, MaxResources: 3})
	if _, err := s.Put(ctx, c.ID, "big", strings.NewReader("12345"), 5, drive.PutOpts{}); !errors.Is(err, drive.ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	put(t, s, c.ID, "g", "1234")
	put(t, s, c.ID, "h", "12") // f(3) + g(4) + h(2) = 9 of 10
	if _, err := s.Put(ctx, c.ID, "i", strings.NewReader("12"), 2, drive.PutOpts{}); !errors.Is(err, drive.ErrQuota) {
		t.Fatalf("bytes quota: %v", err)
	}
	// Replacing f (3) with 4 bytes: 10 of 10, allowed; the resource cap (3)
	// is already met, so a NEW file is refused even when tiny.
	if _, err := s.Put(ctx, c.ID, "f", strings.NewReader("1234"), 4, drive.PutOpts{}); err != nil {
		t.Fatalf("replace within quota: %v", err)
	}
	if _, err := s.Mkdir(ctx, c.ID, "d"); !errors.Is(err, drive.ErrQuota) {
		t.Fatalf("resource quota: %v", err)
	}
}

func TestDirectoriesListDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)

	if _, err := s.Put(ctx, c.ID, "d/x", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrNoParent) {
		t.Fatalf("put without parent: %v", err)
	}
	if _, err := s.Mkdir(ctx, c.ID, "d/e"); !errors.Is(err, drive.ErrNoParent) {
		t.Fatalf("mkdir without parent: %v", err)
	}
	d, err := s.Mkdir(ctx, c.ID, "d")
	if err != nil || !d.IsDir() || d.ModSeq != 1 {
		t.Fatalf("mkdir: %+v %v", d, err)
	}
	if _, err := s.Mkdir(ctx, c.ID, "d"); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("mkdir twice: %v", err)
	}
	put(t, s, c.ID, "d/x", "x")
	if _, err := s.Mkdir(ctx, c.ID, "d/x"); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("mkdir over file: %v", err)
	}
	if _, err := s.Put(ctx, c.ID, "d", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrIsDirectory) {
		t.Fatalf("put over dir: %v", err)
	}
	if _, err := s.Put(ctx, c.ID, "d/x/y", strings.NewReader("x"), 1, drive.PutOpts{}); !errors.Is(err, drive.ErrNotDirectory) {
		t.Fatalf("put under file: %v", err)
	}
	if _, _, err := s.Open(ctx, c.ID, "d"); !errors.Is(err, drive.ErrIsDirectory) {
		t.Fatalf("open dir: %v", err)
	}
	if _, _, err := s.Open(ctx, c.ID, "nope"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	s.Mkdir(ctx, c.ID, "d/e")
	put(t, s, c.ID, "d/e/y", "yy")
	put(t, s, c.ID, "top", "t")

	names := func(rs []drive.Resource) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Path)
		}
		return strings.Join(out, ",")
	}
	rs, _ := s.List(ctx, c.ID, drive.ListOpts{})
	if names(rs) != "d,top" {
		t.Fatalf("root children: %s", names(rs))
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Path: "d"})
	if names(rs) != "d/e,d/x" {
		t.Fatalf("d children: %s", names(rs))
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Recursive: true})
	if names(rs) != "d,d/e,d/e/y,d/x,top" {
		t.Fatalf("recursive: %s", names(rs))
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Path: "d", Recursive: true, Limit: 2})
	if names(rs) != "d/e,d/e/y" {
		t.Fatalf("recursive d limit 2: %s", names(rs))
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Path: "d", Recursive: true, After: "d/e/y"})
	if names(rs) != "d/x" {
		t.Fatalf("after cursor: %s", names(rs))
	}
	// LIKE metacharacters in a path do not widen the match.
	s.Mkdir(ctx, c.ID, "d_")
	put(t, s, c.ID, "d_/z", "z")
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Path: "d", Recursive: true})
	if names(rs) != "d/e,d/e/y,d/x" {
		t.Fatalf("escaped like: %s", names(rs))
	}

	coll, _, _ := s.GetCollectionByID(ctx, c.ID)
	before := coll.SyncToken
	// Delete the subtree: one modseq for all, tombstones visible with
	// IncludeDeleted + since, counters back down.
	del, err := s.Delete(ctx, c.ID, "d", drive.DeleteOpts{})
	if err != nil || !del.Deleted || del.ModSeq != before+1 || !del.IsDir() {
		t.Fatalf("delete: %+v %v", del, err)
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Recursive: true})
	if names(rs) != "d_,d_/z,top" {
		t.Fatalf("after delete: %s", names(rs))
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{Recursive: true, SinceModSeq: before, IncludeDeleted: true})
	if names(rs) != "d,d/e,d/e/y,d/x" {
		t.Fatalf("since+deleted: %s", names(rs))
	}
	for _, r := range rs {
		if !r.Deleted || r.ModSeq != before+1 {
			t.Fatalf("tombstone: %+v", r)
		}
	}
	coll, _, _ = s.GetCollectionByID(ctx, c.ID)
	if coll.ResourceCount != 3 || coll.BytesUsed != 2 { // d_, d_/z(1), top(1)
		t.Fatalf("counters after delete: %+v", coll)
	}
	if _, err := s.Delete(ctx, c.ID, "d", drive.DeleteOpts{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s.Delete(ctx, c.ID, "", drive.DeleteOpts{}); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("delete root: %v", err)
	}
	// A new file at the deleted path is a NEW resource.
	old := del.ResourceID
	nr := put(t, s, c.ID, "d", "file now")
	if nr.ResourceID == old || !nr.Created {
		t.Fatalf("recreate reused identity: %+v", nr)
	}
	// By-id addressing.
	got, ok, err := s.StatByID(ctx, c.ID, nr.ResourceID)
	if err != nil || !ok || got.Path != "d" {
		t.Fatalf("StatByID: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.StatByID(ctx, c.ID, old); ok {
		t.Fatal("StatByID sees tombstone")
	}
	if _, err := s.DeleteByID(ctx, c.ID, nr.ResourceID, drive.DeleteOpts{}); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
}

func TestMoveAndCopy(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	s.Mkdir(ctx, c.ID, "src")
	s.Mkdir(ctx, c.ID, "src/sub")
	f := put(t, s, c.ID, "src/sub/f.txt", "content")
	put(t, s, c.ID, "src/g.txt", "gg")
	s.Mkdir(ctx, c.ID, "dst")
	put(t, s, c.ID, "dst/taken", "t")
	sub, _, _ := s.Stat(ctx, c.ID, "src/sub")

	// Cycle / self / missing parent / no overwrite.
	for _, tc := range []struct {
		from, to string
		want     error
	}{
		{"src", "src", drive.ErrCycle},
		{"src", "src/sub/in", drive.ErrCycle},
		{"src", "nope/x", drive.ErrNoParent},
		{"src/g.txt", "dst/taken", drive.ErrExists},
		{"missing", "dst/x", drive.ErrNotFound},
		{"", "dst/x", drive.ErrBadPath},
		{"src", "", drive.ErrBadPath},
	} {
		if _, err := s.Move(ctx, c.ID, tc.from, tc.to, false); !errors.Is(err, tc.want) {
			t.Errorf("Move %s→%s: %v want %v", tc.from, tc.to, err, tc.want)
		}
		if _, err := s.Copy(ctx, c.ID, tc.from, tc.to, false); !errors.Is(err, tc.want) {
			t.Errorf("Copy %s→%s: %v want %v", tc.from, tc.to, err, tc.want)
		}
	}

	// Rename a file: same id, same etag, new path.
	mvFile, err := s.Move(ctx, c.ID, "src/g.txt", "dst/h.txt", false)
	if err != nil || mvFile.Path != "dst/h.txt" || mvFile.ParentPath != "dst" {
		t.Fatalf("move file: %+v %v", mvFile, err)
	}
	if _, ok, _ := s.Stat(ctx, c.ID, "src/g.txt"); ok {
		t.Fatal("source still there")
	}
	if body, res := read(t, s, c.ID, "dst/h.txt"); body != "gg" || res.ResourceID != mvFile.ResourceID {
		t.Fatalf("moved file: %q %+v", body, res)
	}
	// Move a directory: descendants follow, ids kept, depth/parent fixed.
	mv, err := s.Move(ctx, c.ID, "src", "dst/moved", false)
	if err != nil || mv.Path != "dst/moved" {
		t.Fatalf("move dir: %+v %v", mv, err)
	}
	got, ok, _ := s.Stat(ctx, c.ID, "dst/moved/sub/f.txt")
	if !ok || got.ResourceID != f.ResourceID || got.Depth != 4 || got.ParentPath != "dst/moved/sub" || got.ETag != f.ETag {
		t.Fatalf("descendant after move: %+v ok=%v", got, ok)
	}
	got, _, _ = s.Stat(ctx, c.ID, "dst/moved/sub")
	if got.ResourceID != sub.ResourceID || got.ParentPath != "dst/moved" || got.Depth != 3 {
		t.Fatalf("subdir after move: %+v", got)
	}
	if rs, _ := s.List(ctx, c.ID, drive.ListOpts{Path: "src", Recursive: true}); len(rs) != 0 {
		t.Fatalf("old subtree lingers: %d", len(rs))
	}
	// Overwrite: the destination subtree is deleted first.
	s.Mkdir(ctx, c.ID, "other")
	put(t, s, c.ID, "other/deep", "d")
	if _, err := s.Move(ctx, c.ID, "dst/h.txt", "other", true); err != nil {
		t.Fatalf("move over dir with overwrite: %v", err)
	}
	if got, _, _ := s.Stat(ctx, c.ID, "other"); got.IsDir() || got.ResourceID != mvFile.ResourceID || got.Size != 2 {
		t.Fatalf("overwrite result: %+v", got)
	}
	if _, ok, _ := s.Stat(ctx, c.ID, "other/deep"); ok {
		t.Fatal("overwritten subtree survived")
	}

	// Copy a directory: new ids everywhere, same etags, bytes readable,
	// source untouched, counters up.
	before, _, _ := s.GetCollectionByID(ctx, c.ID)
	cp, err := s.Copy(ctx, c.ID, "dst/moved", "copy", false)
	if err != nil || cp.Path != "copy" || cp.ResourceID == mv.ResourceID || !cp.IsDir() {
		t.Fatalf("copy dir: %+v %v", cp, err)
	}
	got, ok, _ = s.Stat(ctx, c.ID, "copy/sub/f.txt")
	if !ok || got.ResourceID == f.ResourceID || got.ETag != f.ETag || got.Depth != 3 {
		t.Fatalf("copied descendant: %+v ok=%v", got, ok)
	}
	if body, _ := read(t, s, c.ID, "copy/sub/f.txt"); body != "content" {
		t.Fatalf("copied bytes: %q", body)
	}
	if body, _ := read(t, s, c.ID, "dst/moved/sub/f.txt"); body != "content" {
		t.Fatalf("source bytes after copy: %q", body)
	}
	after, _, _ := s.GetCollectionByID(ctx, c.ID)
	if after.ResourceCount != before.ResourceCount+3 || after.BytesUsed != before.BytesUsed+7 {
		t.Fatalf("counters after copy: %+v → %+v", before, after)
	}
	// Copies are independent: rewrite one, the other keeps its bytes.
	put(t, s, c.ID, "copy/sub/f.txt", "changed")
	if body, _ := read(t, s, c.ID, "dst/moved/sub/f.txt"); body != "content" {
		t.Fatalf("copy shared bytes with source: %q", body)
	}
	// Copy a file with overwrite over a file.
	if _, err := s.Copy(ctx, c.ID, "copy/sub/f.txt", "dst/taken", true); err != nil {
		t.Fatalf("copy file overwrite: %v", err)
	}
	if body, _ := read(t, s, c.ID, "dst/taken"); body != "changed" {
		t.Fatalf("overwritten copy: %q", body)
	}
}

func TestSinkEvents(t *testing.T) {
	for _, withTx := range []bool{false, true} {
		t.Run(map[bool]string{false: "post-commit", true: "transactional"}[withTx], func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			rec := &recSink{}
			if withTx {
				s.SetSink(recTxSink{rec})
			} else {
				s.SetSink(rec)
			}
			c := newColl(t, s)
			events := func() []drive.Mutation {
				if withTx {
					return rec.tx
				}
				return rec.post
			}

			r := put(t, s, c.ID, "a", "1")
			put(t, s, c.ID, "a", "1") // noop: no event
			put(t, s, c.ID, "a", "2")
			s.Mkdir(ctx, c.ID, "d")
			put(t, s, c.ID, "d/x", "x")
			s.Move(ctx, c.ID, "d", "e", false)
			s.Copy(ctx, c.ID, "e", "f", false)
			s.Delete(ctx, c.ID, "e", drive.DeleteOpts{})
			if _, err := s.Put(ctx, c.ID, "z", strings.NewReader("ab"), 5, drive.PutOpts{}); err == nil {
				t.Fatal("size mismatch accepted")
			}

			want := []string{
				drive.EventResourceCreated, drive.EventResourceUpdated, // a
				drive.EventResourceCreated, // d
				drive.EventResourceCreated, // d/x
				drive.EventResourceMoved,   // d → e (one event)
				drive.EventResourceCreated, // copy e → f (one event)
				drive.EventResourceDeleted, // e (one event)
			}
			got := events()
			if len(got) != len(want) {
				t.Fatalf("events: got %d want %d: %+v", len(got), len(want), got)
			}
			for i, m := range got {
				if m.Event != want[i] {
					t.Errorf("event %d = %s want %s", i, m.Event, want[i])
				}
				if m.Tenant != "tnt_a" || m.CollectionID != c.ID || m.Collection != "docs" || m.ModSeq == 0 || m.ResourceID == "" {
					t.Errorf("event %d facts: %+v", i, m)
				}
			}
			if got[1].ResourceID != r.ResourceID || got[1].ETag != drive.ETagOf([]byte("2")) || got[1].Size != 1 {
				t.Errorf("update event: %+v", got[1])
			}
			if got[4].FromPath != "d" || got[4].Path != "e" || got[4].Kind != drive.KindDir {
				t.Errorf("move event: %+v", got[4])
			}
			if got[6].Kind != drive.KindDir || got[6].Path != "e" {
				t.Errorf("delete event: %+v", got[6])
			}
			// Keys are unique per mutation even for one resource.
			seen := map[string]bool{}
			for _, m := range got {
				k := drive.IdempotencyKey(m)
				if seen[k] {
					t.Errorf("duplicate key %s", k)
				}
				seen[k] = true
				if !strings.HasPrefix(k, "drive:dr_") {
					t.Errorf("key %s", k)
				}
			}
			if withTx && len(rec.post) != 0 {
				t.Errorf("transactional sink also called post-commit: %d", len(rec.post))
			}
		})
	}
}

func TestSweep(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	// The sweeper compares object mtimes (real wall clock) with the store
	// clock, so pin the store clock to real time here.
	*clk = time.Now().UTC()
	c := newColl(t, s)
	countObjects := func() int {
		n := 0
		_ = s.Objects().List(ctx, "", func(string, drive.ObjectInfo) error { n++; return nil })
		return n
	}

	put(t, s, c.ID, "a", "v1")
	put(t, s, c.ID, "a", "v2") // supersedes v1's object
	put(t, s, c.ID, "b", "b")
	s.Delete(ctx, c.ID, "b", drive.DeleteOpts{})
	// A failed put (size mismatch) discards its object itself.
	s.Put(ctx, c.ID, "c", strings.NewReader("x"), 3, drive.PutOpts{})
	if n := countObjects(); n != 3 { // a:v1, a:v2, b
		t.Fatalf("objects before sweep: %d", n)
	}

	// Within grace nothing is touched; tombstones within retention stay.
	rep, err := s.Sweep(ctx, time.Hour, 24*time.Hour)
	if err != nil || rep.Orphans != 0 || rep.Tombstones != 0 || rep.Scanned != 3 {
		t.Fatalf("sweep within grace: %+v %v", rep, err)
	}
	// Zero grace an hour later: the superseded version AND the deleted
	// file's bytes go (a tombstone keeps the row for `list since`, not the
	// bytes); the live version stays.
	*clk = clk.Add(time.Hour)
	rep, err = s.Sweep(ctx, 0, 24*time.Hour)
	if err != nil || rep.Orphans != 2 || rep.OrphanBytes != 3 || rep.Tombstones != 0 {
		t.Fatalf("sweep zero grace: %+v %v", rep, err)
	}
	if body, _ := read(t, s, c.ID, "a"); body != "v2" {
		t.Fatalf("live version swept: %q", body)
	}
	rs, _ := s.List(ctx, c.ID, drive.ListOpts{IncludeDeleted: true, Recursive: true})
	if len(rs) != 2 {
		t.Fatalf("tombstone purged early: %+v", rs)
	}
	// Past retention the tombstone row is purged.
	*clk = clk.Add(48 * time.Hour)
	rep, err = s.Sweep(ctx, 0, 24*time.Hour)
	if err != nil || rep.Tombstones != 1 || rep.Orphans != 0 {
		t.Fatalf("sweep past retention: %+v %v", rep, err)
	}
	if n := countObjects(); n != 1 {
		t.Fatalf("objects after sweep: %d", n)
	}
	rs, _ = s.List(ctx, c.ID, drive.ListOpts{IncludeDeleted: true, Recursive: true})
	if len(rs) != 1 || rs[0].Path != "a" {
		t.Fatalf("rows after sweep: %+v", rs)
	}
	// Idempotent.
	rep, _ = s.Sweep(ctx, 0, 24*time.Hour)
	if rep.Orphans != 0 || rep.Tombstones != 0 || rep.Errors != 0 {
		t.Fatalf("second sweep: %+v", rep)
	}
}

func TestLargeStreamingPut(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	r, err := s.Put(ctx, c.ID, "big.bin", bytes.NewReader(body), int64(len(body)), drive.PutOpts{})
	if err != nil || r.Size != int64(len(body)) || r.ETag != drive.ETagOf(body) {
		t.Fatalf("big put: %+v %v", r, err)
	}
	got, res := read(t, s, c.ID, "big.bin")
	if got != string(body) || res.ContentType != "application/octet-stream" {
		t.Fatalf("big read: %d bytes, %s", len(got), res.ContentType)
	}
}
