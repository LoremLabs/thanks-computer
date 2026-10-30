package gitstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenSQLiteFile(filepath.Join(t.TempDir(), "git.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// The schema is idempotent: a second boot finds it and keeps the rows.
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	c = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestRepos(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r, created, err := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "pony", Principal: "pony:pony", CreatedBy: "ranch", Quotas: Quotas{MaxRepoBytes: 1 << 20}})
	if err != nil || !created || !strings.HasPrefix(r.ID, "rp_") || r.Head != "refs/heads/main" || r.PolicyPush != "stack" || r.PolicyContent != "text" {
		t.Fatalf("create: %+v created=%v err=%v", r, created, err)
	}
	again, created, err := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "pony", Principal: "someone:else"})
	if err != nil || created || again.ID != r.ID || again.Principal != "pony:pony" {
		t.Fatalf("second create should return the first: %+v created=%v err=%v", again, created, err)
	}
	if got, found, err := s.GetRepo(ctx, "acme", "pony"); err != nil || !found || got.Quotas.MaxRepoBytes != 1<<20 {
		t.Fatalf("get: %+v %v %v", got, found, err)
	}
	if _, found, _ := s.GetRepo(ctx, "other", "pony"); found {
		t.Fatal("another tenant sees the repository")
	}
	if _, _, err := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "zebra"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListRepos(ctx, "acme")
	if err != nil || len(list) != 2 || list[0].Name != "pony" || list[1].Name != "zebra" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := s.RemoveRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepo(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	if _, found, _ := s.GetRepo(ctx, "acme", "pony"); found {
		t.Fatal("removed repository still found")
	}
}

func TestRepoPolicy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r, _, err := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "pony", PolicyContent: "stack"})
	if err != nil || r.PolicyContent != "stack" {
		t.Fatalf("create with a content policy: %+v %v", r, err)
	}
	// Each half alone; "" leaves the other as it is.
	if err := s.SetRepoPolicy(ctx, r.ID, "", "text"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetRepo(ctx, "acme", "pony"); got.PolicyContent != "text" || got.PolicyPush != "stack" {
		t.Fatalf("after content → text: %+v", got)
	}
	if err := s.SetRepoPolicy(ctx, r.ID, "open", ""); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetRepo(ctx, "acme", "pony"); got.PolicyPush != "open" || got.PolicyContent != "text" {
		t.Fatalf("after push → open: %+v", got)
	}
	if err := s.SetRepoPolicy(ctx, "rp_nope", "stack", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}

// A store made before policy_content existed gains the column on the next
// EnsureSchema, with 'text' for every row it already held.
func TestRepoPolicyColumnAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "git.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE repos (
		id TEXT PRIMARY KEY, tenant TEXT NOT NULL, name TEXT NOT NULL, principal TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '', policy_push TEXT NOT NULL DEFAULT 'stack', head TEXT NOT NULL DEFAULT 'refs/heads/main',
		max_object_bytes BIGINT NOT NULL DEFAULT 0, max_pack_bytes BIGINT NOT NULL DEFAULT 0, max_repo_bytes BIGINT NOT NULL DEFAULT 0,
		max_objects BIGINT NOT NULL DEFAULT 0, created_at TEXT NOT NULL, UNIQUE (tenant, name))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO repos (id, tenant, name, created_at) VALUES ('rp_old', 'acme', 'pony', '2026-10-28T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := OpenSQLiteFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, found, err := s.GetRepo(ctx, "acme", "pony")
	if err != nil || !found || got.PolicyContent != "text" || got.PolicyPush != "stack" {
		t.Fatalf("old row after the schema grew: %+v %v %v", got, found, err)
	}
}

func TestObjects(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r, _, _ := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "pony"})
	objs := []Object{
		{OID: a, Type: "blob", Size: 10, SHA256: strings.Repeat("1", 64)},
		{OID: b, Type: "tree", Size: 20, SHA256: strings.Repeat("2", 64)},
	}
	if err := s.PutObjects(ctx, r.ID, objs); err != nil {
		t.Fatal(err)
	}
	// The same id again is a no-op, not an error.
	if err := s.PutObjects(ctx, r.ID, objs[:1]); err != nil {
		t.Fatal(err)
	}
	o, found, err := s.GetObject(ctx, r.ID, strings.ToUpper(a))
	if err != nil || !found || o.Type != "blob" || o.Size != 10 || o.Inline != nil {
		t.Fatalf("get: %+v %v %v", o, found, err)
	}
	if _, found, _ := s.GetObject(ctx, "rp_other", a); found {
		t.Fatal("another repository sees the object: the tenant fence leaks")
	}
	n, bytes, err := s.ObjectStats(ctx, r.ID)
	if err != nil || n != 2 || bytes != 30 {
		t.Fatalf("stats: %d %d %v", n, bytes, err)
	}
	trees, err := s.ListObjects(ctx, r.ID, "tree")
	if err != nil || len(trees) != 1 || trees[0].OID != b {
		t.Fatalf("list trees: %+v %v", trees, err)
	}
	if err := s.RemoveRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.ObjectStats(ctx, r.ID); n != 0 {
		t.Fatal("objects survive the repository's removal")
	}
}

func TestRefsCompareAndSet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r, _, _ := s.CreateRepo(ctx, Repo{Tenant: "acme", Name: "pony"})
	by := Actor{Principal: "pony:pony", Credential: "crd_1", Source: "push"}

	// Create from nothing.
	if err := s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{{Name: "refs/heads/main", Old: "", New: a}}, by); err != nil {
		t.Fatal(err)
	}
	// Create again: moved.
	err := s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{{Name: "refs/heads/main", Old: ZeroOID, New: b}}, by)
	var moved *ErrRefMoved
	if !errors.As(err, &moved) || moved.Name != "refs/heads/main" || moved.Current != a {
		t.Fatalf("create over an existing ref: %v", err)
	}
	// A stale old: moved, and the sibling did not move either.
	err = s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{
		{Name: "refs/heads/propose/x", Old: ZeroOID, New: c},
		{Name: "refs/heads/main", Old: b, New: c},
	}, by)
	if !errors.As(err, &moved) || moved.Current != a {
		t.Fatalf("stale old: %v", err)
	}
	if _, found, _ := s.GetRef(ctx, r.ID, "refs/heads/propose/x"); found {
		t.Fatal("the sibling moved despite the refused update")
	}
	// A correct update of both.
	if err := s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{
		{Name: "refs/heads/propose/x", Old: ZeroOID, New: c},
		{Name: "refs/heads/main", Old: a, New: b},
	}, Actor{Principal: "ranch", Source: "op", Stack: "loop"}); err != nil {
		t.Fatal(err)
	}
	refs, err := s.ListRefs(ctx, r.ID)
	if err != nil || len(refs) != 2 || refs[0].Name != "refs/heads/main" || refs[0].OID != b || refs[1].OID != c {
		t.Fatalf("refs: %+v %v", refs, err)
	}
	// Delete with the right old.
	if err := s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{{Name: "refs/heads/propose/x", Old: c, New: ZeroOID}}, by); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetRef(ctx, r.ID, "refs/heads/propose/x"); found {
		t.Fatal("deleted ref still there")
	}
	// Delete a missing ref: moved (current is zero).
	err = s.CompareAndSetRefs(ctx, r.ID, []RefUpdate{{Name: "refs/heads/propose/x", Old: c, New: ZeroOID}}, by)
	if !errors.As(err, &moved) || moved.Current != ZeroOID {
		t.Fatalf("delete missing: %v", err)
	}
	// The log has every move, newest first.
	log, err := s.RefLog(ctx, r.ID, 10)
	if err != nil || len(log) != 4 {
		t.Fatalf("log: %d entries %v", len(log), err)
	}
	if log[0].Name != "refs/heads/propose/x" || log[0].New != ZeroOID || log[0].Principal != "pony:pony" {
		t.Fatalf("newest log entry: %+v", log[0])
	}
	var sawOp bool
	for _, e := range log {
		if e.Source == "op" && e.Stack == "loop" {
			sawOp = true
		}
	}
	if !sawOp {
		t.Fatal("the op's move is not in the log")
	}
}
