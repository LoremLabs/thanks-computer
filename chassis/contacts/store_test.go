package contacts

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// newTestStore opens a per-test temp-file SQLite DB with the production
// DSN shape (WAL + busy timeout + immediate write lock), applies the schema
// twice (idempotence), and pins the clock.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contacts.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.SetClock(func() time.Time { return clk })
	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("ensure schema #%d: %v", i, err)
		}
	}
	return s, &clk
}

func vcf(uid, fn, email, rev string) []byte {
	return []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nN:" + fn + ";;;;\r\nEMAIL;TYPE=INTERNET:" + email +
		"\r\nREV:" + rev + "\r\nEND:VCARD\r\n")
}

func mustBook(t *testing.T, s *Store, name string) Addressbook {
	t.Helper()
	ab, _, err := s.EnsureAddressbook(context.Background(), Addressbook{Tenant: "acme", Username: "paris@example.com", Name: name, DisplayName: "Senders"})
	if err != nil {
		t.Fatal(err)
	}
	return ab
}

func TestAccounts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	created, err := s.UpsertAccount(ctx, "acme", "Paris@Example.COM", "hash1", "", nil)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	a, ok, err := s.GetAccount(ctx, "paris@example.com")
	if err != nil || !ok || a.Tenant != "acme" || a.PwHash != "hash1" || a.Status != StatusActive || string(a.Policy) != "{}" {
		t.Fatalf("account = %+v ok=%v err=%v", a, ok, err)
	}
	if c, err := s.UpsertAccount(ctx, "acme", "paris@example.com", "", StatusDisabled, nil); err != nil || c {
		t.Fatalf("update: created=%v err=%v", c, err)
	}
	a, _, _ = s.GetAccount(ctx, "paris@example.com")
	if a.PwHash != "hash1" || a.Status != StatusDisabled {
		t.Errorf("after update: %+v", a)
	}
	if _, err := s.UpsertAccount(ctx, "other", "paris@example.com", "h", "", nil); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("cross-tenant upsert err = %v, want ErrUsernameTaken", err)
	}
	if _, err := s.UpsertAccount(ctx, "acme", "new@example.com", "", "", nil); err == nil {
		t.Error("create without a password must fail")
	}
	if _, err := s.UpsertAccount(ctx, "acme", "x@example.com", "h", "weird", nil); err == nil {
		t.Error("bad status must fail")
	}
}

func TestAddressbooksEnsureUpdateRemoveResurrect(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ab, created, err := s.EnsureAddressbook(ctx, Addressbook{Tenant: "acme", Username: "paris@example.com", Name: "senders", DisplayName: "Paris senders", Policy: []byte(`{"put":"stack"}`)})
	if err != nil || !created || ab.ID == "" || ab.DisplayName != "Paris senders" || ab.SyncToken != 0 {
		t.Fatalf("ensure: %+v created=%v err=%v", ab, created, err)
	}
	ab2, created, err := s.EnsureAddressbook(ctx, Addressbook{Tenant: "acme", Username: "paris@example.com", Name: "senders", Description: "who may write"})
	if err != nil || created || ab2.ID != ab.ID || ab2.DisplayName != "Paris senders" || ab2.Description != "who may write" || string(ab2.Policy) != `{"put":"stack"}` {
		t.Fatalf("re-ensure: %+v created=%v err=%v", ab2, created, err)
	}
	if _, _, err := s.EnsureAddressbook(ctx, Addressbook{Tenant: "acme", Username: "paris@example.com", Name: "Bad Name"}); err == nil {
		t.Error("bad name must fail")
	}
	list, err := s.ListAddressbooks(ctx, "acme", "paris@example.com")
	if err != nil || len(list) != 1 || list[0].Name != "senders" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	dn := "Renamed"
	if err := s.SetAddressbookProps(ctx, ab.ID, &dn, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetAddressbookByID(ctx, ab.ID); got.DisplayName != "Renamed" || got.Description != "who may write" {
		t.Errorf("after proppatch: %+v", got)
	}
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "a.vcf", UID: "a@x", VCard: vcf("a@x", "A", "a@x.io", "20260905T120000Z")}, PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RemoveAddressbook(ctx, ab.ID); err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.GetAddressbook(ctx, "acme", "paris@example.com", "senders"); ok {
		t.Error("removed book still live")
	}
	objs, _ := s.ListObjects(ctx, ab.ID, ListOpts{IncludeDeleted: true})
	if len(objs) != 1 || !objs[0].Deleted {
		t.Errorf("objects after remove = %+v", objs)
	}
	ab3, created, err := s.EnsureAddressbook(ctx, Addressbook{Tenant: "acme", Username: "paris@example.com", Name: "senders"})
	if err != nil || !created || ab3.ID != ab.ID {
		t.Fatalf("resurrect: %+v created=%v err=%v", ab3, created, err)
	}
	if live, _ := s.ListObjects(ctx, ab.ID, ListOpts{}); len(live) != 0 {
		t.Errorf("resurrected book has live objects: %+v", live)
	}
}

func TestPutObjectNoopReplaceByUIDAndPreconditions(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	ab := mustBook(t, s, "senders")

	r1, err := s.PutObject(ctx, ab.ID, Object{Name: "bob.vcf", UID: "bob@x", FN: "Bob", Addresses: []string{"bob@x.io"}, VCard: vcf("bob@x", "Bob", "bob@x.io", "20260905T120000Z")}, PutOpts{ByUID: true})
	if err != nil || !r1.Created || r1.Noop || r1.ModSeq != 1 || r1.Name != "bob.vcf" || r1.ETag == "" {
		t.Fatalf("create: %+v err=%v", r1, err)
	}
	// Same content, new REV ⇒ noop, etag and modseq unchanged.
	*clk = clk.Add(time.Hour)
	r2, err := s.PutObject(ctx, ab.ID, Object{Name: "ignored.vcf", UID: "bob@x", VCard: vcf("bob@x", "Bob", "bob@x.io", "20260905T130000Z")}, PutOpts{ByUID: true})
	if err != nil || !r2.Noop || r2.ETag != r1.ETag || r2.ModSeq != 1 || r2.Name != "bob.vcf" {
		t.Fatalf("noop: %+v err=%v", r2, err)
	}
	if got, _, _ := s.GetAddressbookByID(ctx, ab.ID); got.SyncToken != 1 {
		t.Errorf("sync token moved on a noop: %d", got.SyncToken)
	}
	// Changed content by uid ⇒ same name, new etag, modseq 2, facts replaced.
	r3, err := s.PutObject(ctx, ab.ID, Object{Name: "ignored.vcf", UID: "bob@x", FN: "Robert", Addresses: []string{"rob@x.io"}, VCard: vcf("bob@x", "Robert", "rob@x.io", "20260905T130000Z")}, PutOpts{ByUID: true})
	if err != nil || r3.Created || r3.Noop || r3.ETag == r1.ETag || r3.ModSeq != 2 || r3.Name != "bob.vcf" {
		t.Fatalf("replace: %+v err=%v", r3, err)
	}
	o, ok, _ := s.GetObject(ctx, ab.ID, "bob.vcf")
	if !ok || o.FN != "Robert" || len(o.Addresses) != 1 || o.Addresses[0] != "rob@x.io" || o.ETag != r3.ETag || o.Kind != "individual" || o.Version != "3.0" {
		t.Errorf("stored object = %+v", o)
	}
	// A client put under a different name with the same uid ⇒ conflict.
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "other.vcf", UID: "bob@x", VCard: vcf("bob@x", "X", "x@x.io", "20260905T130000Z")}, PutOpts{}); !errors.Is(err, ErrUIDConflict) {
		t.Errorf("uid conflict err = %v", err)
	}
	// Preconditions.
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "bob.vcf", UID: "bob@x", VCard: vcf("bob@x", "Y", "y@x.io", "1")}, PutOpts{IfNoneMatch: "*"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-none-match * on existing err = %v", err)
	}
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "bob.vcf", UID: "bob@x", VCard: vcf("bob@x", "Y", "y@x.io", "1")}, PutOpts{IfMatch: "stale"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-match stale err = %v", err)
	}
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "new.vcf", UID: "new@x", VCard: vcf("new@x", "N", "n@x.io", "1")}, PutOpts{IfMatch: "*"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-match * on missing err = %v", err)
	}
	r4, err := s.PutObject(ctx, ab.ID, Object{Name: "bob.vcf", UID: "bob@x", VCard: vcf("bob@x", "Y", "y@x.io", "1")}, PutOpts{IfMatch: r3.ETag})
	if err != nil || r4.Noop || r4.ModSeq != 3 {
		t.Fatalf("if-match current: %+v err=%v", r4, err)
	}
	// Delete, tombstone, resurrect under the same name with a new uid.
	etag, found, err := s.DeleteObject(ctx, ab.ID, "bob.vcf")
	if err != nil || !found || etag != r4.ETag {
		t.Fatalf("delete: etag=%s found=%v err=%v", etag, found, err)
	}
	if _, found, _ := s.DeleteObject(ctx, ab.ID, "bob.vcf"); found {
		t.Error("second delete found the tombstone")
	}
	dead, _ := s.ListObjects(ctx, ab.ID, ListOpts{IncludeDeleted: true, SinceModSeq: 3})
	if len(dead) != 1 || !dead[0].Deleted || dead[0].ModSeq != 4 {
		t.Errorf("tombstone = %+v", dead)
	}
	r5, err := s.PutObject(ctx, ab.ID, Object{Name: "bob.vcf", UID: "fresh@x", VCard: vcf("fresh@x", "F", "f@x.io", "1")}, PutOpts{IfNoneMatch: "*"})
	if err != nil || !r5.Created || r5.ModSeq != 5 || r5.UID != "fresh@x" {
		t.Fatalf("resurrect: %+v err=%v", r5, err)
	}
	if o, ok, _ := s.GetObjectByUID(ctx, ab.ID, "fresh@x"); !ok || o.Deleted || o.Name != "bob.vcf" {
		t.Errorf("resurrected = %+v ok=%v", o, ok)
	}
	if _, err := s.PutObject(ctx, "ab_missing", Object{Name: "a.vcf", UID: "a", VCard: []byte("x")}, PutOpts{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing book err = %v", err)
	}
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "../x", UID: "a", VCard: []byte("x")}, PutOpts{}); err == nil {
		t.Error("bad resource name accepted")
	}
	some, _ := s.ListObjects(ctx, ab.ID, ListOpts{Names: []string{"bob.vcf", "missing.vcf"}})
	if len(some) != 1 || some[0].Name != "bob.vcf" {
		t.Errorf("by names = %+v", some)
	}
}

func TestBatchIsAtomic(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ab := mustBook(t, s, "senders")
	if _, err := s.PutObject(ctx, ab.ID, Object{Name: "ann.vcf", UID: "ann@x", VCard: vcf("ann@x", "Ann", "ann@x.io", "1")}, PutOpts{}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Batch(ctx, ab.ID,
		[]Object{
			{Name: "ann.vcf", UID: "ann@x", VCard: vcf("ann@x", "Ann", "ann@x.io", "2")},  // noop
			{Name: "bob.vcf", UID: "bob@x", VCard: vcf("bob@x", "Bob", "bob@x.io", "1")},  // created
			{Name: "carol.vcf", UID: "cy@x", VCard: vcf("cy@x", "Carol", "cy@x.io", "1")}, // created
		},
		[]string{"nobody@x"})
	if err != nil || res.Noop != 1 || res.Created != 2 || res.Updated != 0 || res.Deleted != 0 || res.Missing != 1 || len(res.Puts) != 3 {
		t.Fatalf("batch 1: %+v err=%v", res, err)
	}
	if got, _, _ := s.GetAddressbookByID(ctx, ab.ID); got.SyncToken != 3 {
		t.Errorf("sync token after batch = %d, want 3", got.SyncToken)
	}
	res, err = s.Batch(ctx, ab.ID,
		[]Object{{Name: "x.vcf", UID: "bob@x", VCard: vcf("bob@x", "Robert", "bob@x.io", "3")}}, // updated in place
		[]string{"ann@x", "cy@x"})
	if err != nil || res.Updated != 1 || res.Deleted != 2 || res.Missing != 0 {
		t.Fatalf("batch 2: %+v err=%v", res, err)
	}
	live, _ := s.ListObjects(ctx, ab.ID, ListOpts{})
	if len(live) != 1 || live[0].Name != "bob.vcf" || live[0].UID != "bob@x" {
		t.Errorf("after batch 2: %+v", live)
	}
	// A bad entry rolls everything back and names itself.
	_, err = s.Batch(ctx, ab.ID,
		[]Object{
			{Name: "dan.vcf", UID: "dan@x", VCard: vcf("dan@x", "Dan", "dan@x.io", "1")},
			{Name: "bad name", UID: "e@x", VCard: vcf("e@x", "E", "e@x.io", "1")},
		}, nil)
	var be *BatchError
	if !errors.As(err, &be) || be.Index != 1 || be.Op != "put" {
		t.Fatalf("batch 3 err = %v", err)
	}
	if _, ok, _ := s.GetObjectByUID(ctx, ab.ID, "dan@x"); ok {
		t.Error("a failed batch wrote an object")
	}
	if got, _, _ := s.GetAddressbookByID(ctx, ab.ID); got.SyncToken != 6 {
		t.Errorf("sync token after failed batch = %d, want 6", got.SyncToken)
	}
	if _, err := s.Batch(ctx, ab.ID, nil, []string{""}); err == nil {
		t.Error("empty delete uid accepted")
	}
}

func TestSameContentIgnoresRevAndProdid(t *testing.T) {
	a := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//a//EN\r\nREV:20260905T120000Z\r\nNOTE:a long note that is folded\r\n  across lines\r\nEND:VCARD\r\n")
	b := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//b//EN\r\nREV:20260905T130000Z\r\nNOTE:a long note that is folded across lines\r\nEND:VCARD\r\n")
	if !SameContent(a, b) {
		t.Error("REV/PRODID/folding must not count")
	}
	if SameContent(a, []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nNOTE:other\r\nEND:VCARD\r\n")) {
		t.Error("different content reported same")
	}
}
