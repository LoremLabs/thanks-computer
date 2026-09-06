package conseed_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/conseed"
)

func newStore(t *testing.T) *chcon.Store {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "contacts.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := chcon.NewStore(db, registry.SQLite)
	s.SetClock(func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) })
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func pack(name string, lines ...string) storeseed.RawPack {
	p, ok := storeseed.NewRawPack("CONTACTS/"+name+".jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if !ok {
		panic("bad pack path " + name)
	}
	return p
}

const (
	user = "paris@pony.example.com"
	hdr  = `{"addressbook":{"display_name":"Paris team","description":"seeded"}}`
	bob  = `{"name":"bob.vcf","card":{"fn":"Bob Example","emails":[{"value":"Bob@Example.com"}]}}`
	ann  = `{"name":"ann.vcf","card":{"emails":[{"value":"ann@example.com"}]}}`
	raw  = `{"name":"raw.vcf","vcard":"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:raw-1\r\nFN:Raw\r\nN:Raw;;;;\r\nEMAIL:raw@x.io\r\nEND:VCARD\r\n"}`
)

func TestReconcileOwnsTheAddressbook(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", user, "hash", "", nil); err != nil {
		t.Fatal(err)
	}
	m := conseed.New(s, false)
	scope := storeseed.Scope{Tenant: "acme", Stack: "core", Version: 1}
	if m.Kind() != storeseed.KindContact || m.Shared() {
		t.Fatalf("kind=%s shared=%v", m.Kind(), m.Shared())
	}
	if storeseed.PackName("CONTACTS/"+user+"/team.jsonl") != user+"/team" || storeseed.PackName("CONTACTS/team.jsonl") != "" {
		t.Fatal("PackName does not understand CONTACTS/<username>/<book>.jsonl")
	}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/team", hdr, bob, ann, raw)}); err != nil {
		t.Fatal(err)
	}
	ab, ok, _ := s.GetAddressbook(ctx, "acme", user, "team")
	if !ok || ab.DisplayName != "Paris team" || ab.Description != "seeded" || string(ab.Policy) != `{"put":"deny","delete":"deny"}` {
		t.Fatalf("addressbook = %+v ok=%v", ab, ok)
	}
	objs, _ := s.ListObjects(ctx, ab.ID, chcon.ListOpts{})
	if len(objs) != 3 {
		t.Fatalf("objects = %d", len(objs))
	}
	byName := map[string]chcon.Object{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	if byName["bob.vcf"].UID != "bob.paris@pony.example.com" || byName["bob.vcf"].FN != "Bob Example" || byName["bob.vcf"].Addresses[0] != "bob@example.com" ||
		byName["ann.vcf"].FN != "ann@example.com" || byName["raw.vcf"].UID != "raw-1" || !strings.Contains(string(byName["raw.vcf"].VCard), "FN:Raw\r\n") {
		t.Errorf("objects = %+v", byName)
	}
	token := ab.SyncToken
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/team", hdr, bob, ann, raw)}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetAddressbookByID(ctx, ab.ID); got.SyncToken != token {
		t.Errorf("no-op reconcile moved sync_token %d → %d", token, got.SyncToken)
	}
	// A changed card and a dropped one: updated in place, stale deleted.
	changed := strings.Replace(bob, "Bob Example", "Robert Example", 1)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/team", hdr, changed, raw)}); err != nil {
		t.Fatal(err)
	}
	objs, _ = s.ListObjects(ctx, ab.ID, chcon.ListOpts{})
	if len(objs) != 2 {
		t.Fatalf("after drop: %d objects", len(objs))
	}
	if b, _, _ := s.GetObject(ctx, ab.ID, "bob.vcf"); b.FN != "Robert Example" {
		t.Errorf("updated = %+v", b)
	}
	if _, ok, _ := s.GetObject(ctx, ab.ID, "ann.vcf"); ok {
		t.Error("dropped object still live")
	}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/team", `{"addressbook":{"policy":{"put":"observe"}}}`, changed, raw)}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetAddressbook(ctx, "acme", user, "team"); string(got.Policy) != `{"put":"observe"}` || got.DisplayName != "Paris team" {
		t.Errorf("policy/display after header = %+v", got)
	}
}

func TestReconcileRefusals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", user, "hash", "", nil); err != nil {
		t.Fatal(err)
	}
	m := conseed.New(s, true)
	scope := storeseed.Scope{Tenant: "acme", Stack: "core", Version: 1}
	for name, packs := range map[string][]storeseed.RawPack{
		"unknown account": {pack("ghost@pony.example.com/team", bob)},
		"two headers":     {pack(user+"/team", hdr, hdr, bob)},
		"neither":         {pack(user+"/team", `{"name":"x.vcf"}`)},
		"both":            {pack(user+"/team", `{"name":"x.vcf","vcard":"x","card":{}}`)},
		"no name no uid":  {pack(user+"/team", `{"card":{"fn":"x"}}`)},
		"bad policy":      {pack(user+"/team", `{"addressbook":{"policy":{"fly":"deny"}}}`, bob)},
		"bad card":        {pack(user+"/team", `{"name":"x.vcf","card":{"emails":[{"value":"nope"}]}}`)},
		"bad vcard":       {pack(user+"/team", `{"name":"x.vcf","vcard":"garbage"}`)},
	} {
		if err := m.Reconcile(ctx, scope, packs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := m.Reconcile(ctx, storeseed.Scope{Tenant: "other", Stack: "core"}, []storeseed.RawPack{pack(user+"/team", bob)}); err == nil || !strings.Contains(err.Error(), "another tenant") {
		t.Errorf("cross-tenant err = %v", err)
	}
}
