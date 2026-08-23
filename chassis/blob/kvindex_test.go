package blob

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
)

func newIndex(t *testing.T) Index {
	t.Helper()
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewKVIndex(kvstore.New(s, 65536, 0))
}

const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestKVIndexNames(t *testing.T) {
	ctx := context.Background()
	ix := newIndex(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if _, found, err := ix.GetName(ctx, "t1", "faqs/house-01.doc"); err != nil || found {
		t.Fatalf("miss: found=%v err=%v", found, err)
	}
	row := NameRow{Name: "faqs/house-01.doc", SHA256: shaA, Size: 12, ContentType: "text/plain",
		Filename: "House 01.doc", Permissions: []string{"blob:guest:read"}, CreatedAt: now, UpdatedAt: now}
	if err := ix.PutName(ctx, "t1", row); err != nil {
		t.Fatal(err)
	}
	got, found, err := ix.GetName(ctx, "t1", "faqs/house-01.doc")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.Name != row.Name || got.SHA256 != shaA || got.Size != 12 || got.Filename != "House 01.doc" ||
		len(got.Permissions) != 1 || !got.CreatedAt.Equal(now) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Nested name with several '/' survives the ':' substitution.
	deep := NameRow{Name: "a/b/c/d.txt", SHA256: shaB, CreatedAt: now, UpdatedAt: now}
	if err := ix.PutName(ctx, "t1", deep); err != nil {
		t.Fatal(err)
	}
	if g, f, _ := ix.GetName(ctx, "t1", "a/b/c/d.txt"); !f || g.Name != "a/b/c/d.txt" {
		t.Fatalf("deep name: found=%v name=%q", f, g.Name)
	}
	// Invalid names never reach the store.
	if err := ix.PutName(ctx, "t1", NameRow{Name: "bad:name", SHA256: shaA}); err == nil {
		t.Fatal("invalid name accepted")
	}
	// Tenant isolation.
	if _, f, _ := ix.GetName(ctx, "t2", "faqs/house-01.doc"); f {
		t.Fatal("tenant leak")
	}
	// Delete.
	if err := ix.DeleteName(ctx, "t1", "faqs/house-01.doc"); err != nil {
		t.Fatal(err)
	}
	if _, f, _ := ix.GetName(ctx, "t1", "faqs/house-01.doc"); f {
		t.Fatal("still present after delete")
	}
	if err := ix.DeleteName(ctx, "t1", "faqs/house-01.doc"); err != nil {
		t.Fatalf("double delete: %v", err)
	}
}

func TestKVIndexList(t *testing.T) {
	ctx := context.Background()
	ix := newIndex(t)
	now := time.Now().UTC()
	// Empty → [] not nil.
	page, err := ix.ListNames(ctx, "t1", ListOpts{})
	if err != nil || page.Names == nil || len(page.Names) != 0 || page.Next != "" {
		t.Fatalf("empty: %+v err=%v", page, err)
	}
	// "a/b" must sort before "a0" by NAME even though the stored keys
	// ("n:a:b" vs "n:a0") order the other way.
	for _, n := range []string{"a0", "a/b", "faqs/z.md", "faqs/a.md", "bookings/2026.csv"} {
		if err := ix.PutName(ctx, "t1", NameRow{Name: n, SHA256: shaA, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(p ListPage) []string {
		var out []string
		for _, r := range p.Names {
			out = append(out, r.Name)
		}
		return out
	}
	page, _ = ix.ListNames(ctx, "t1", ListOpts{})
	if got := names(page); len(got) != 5 || got[0] != "a/b" || got[1] != "a0" || got[2] != "bookings/2026.csv" {
		t.Fatalf("sorted by raw name: %v", got)
	}
	// Prefix.
	page, _ = ix.ListNames(ctx, "t1", ListOpts{Prefix: "faqs/"})
	if got := names(page); len(got) != 2 || got[0] != "faqs/a.md" || got[1] != "faqs/z.md" {
		t.Fatalf("prefix: %v", got)
	}
	// Pagination: 2 per page, follow the cursor.
	var all []string
	cursor := ""
	for pages := 0; ; pages++ {
		p, err := ix.ListNames(ctx, "t1", ListOpts{After: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Names) > 2 {
			t.Fatalf("page over limit: %v", names(p))
		}
		all = append(all, names(p)...)
		if p.Next == "" {
			if pages != 2 {
				t.Fatalf("pages = %d, want 3", pages+1)
			}
			break
		}
		cursor = p.Next
	}
	if len(all) != 5 || all[4] != "faqs/z.md" {
		t.Fatalf("paged walk: %v", all)
	}
	// Other tenant sees nothing.
	if p, _ := ix.ListNames(ctx, "t2", ListOpts{}); len(p.Names) != 0 {
		t.Fatal("tenant leak in list")
	}
}

func TestKVIndexSha(t *testing.T) {
	ctx := context.Background()
	ix := newIndex(t)
	t0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	if _, found, _ := ix.GetSha(ctx, "t1", shaA); found {
		t.Fatal("sha present before put")
	}
	created, err := ix.PutShaIfAbsent(ctx, "t1", ShaRow{SHA256: shaA, Size: 3, ContentType: "text/plain", FirstSeen: t0})
	if err != nil || !created {
		t.Fatalf("first put: created=%v err=%v", created, err)
	}
	created, err = ix.PutShaIfAbsent(ctx, "t1", ShaRow{SHA256: shaA, Size: 3, FirstSeen: t0.Add(time.Hour)})
	if err != nil || created {
		t.Fatalf("second put: created=%v err=%v", created, err)
	}
	row, found, _ := ix.GetSha(ctx, "t1", shaA)
	if !found || row.SHA256 != shaA || !row.FirstSeen.Equal(t0) || row.Size != 3 {
		t.Fatalf("first_seen not preserved: %+v found=%v", row, found)
	}
	// The tenant fence: another tenant does not own it.
	if _, found, _ := ix.GetSha(ctx, "t2", shaA); found {
		t.Fatal("sha ownership leaked across tenants")
	}
	if _, err := ix.PutShaIfAbsent(ctx, "t1", ShaRow{SHA256: "nothex"}); err == nil {
		t.Fatal("malformed sha accepted")
	}
}

func TestNameRowDrifted(t *testing.T) {
	if (NameRow{SHA256: shaA}).Drifted() {
		t.Error("runtime-only row must not drift")
	}
	if (NameRow{SHA256: shaA, SeededSHA: shaA, SeededBy: "s"}).Drifted() {
		t.Error("seeded row at the shipped content must not drift")
	}
	if !(NameRow{SHA256: shaB, SeededSHA: shaA, SeededBy: "s"}).Drifted() {
		t.Error("seeded row repointed at runtime must drift")
	}
}
