package imap

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// newTestStore opens a per-test temp-file SQLite DB (cgo sqlite + :memory:
// shared-cache is finicky across connections; a temp file works
// everywhere), applies the schema, and pins the clock.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "imap.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.now = func() time.Time { return clk }
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// Idempotent: a second EnsureSchema on an existing file must be a no-op.
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema (again): %v", err)
	}
	return s, &clk
}

func TestUpsertAccountCreatesINBOXAndIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	created, err := s.UpsertAccount(ctx, "acme", "Paris@Example.COM", "hash1", "", nil)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	a, ok, err := s.GetAccount(ctx, "paris@example.com")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if a.Tenant != "acme" || a.Username != "paris@example.com" || a.PwHash != "hash1" || a.Status != StatusActive {
		t.Errorf("account = %+v", a)
	}
	if string(a.Policy) != "{}" {
		t.Errorf("policy default = %q, want {}", a.Policy)
	}
	mb, ok, err := s.GetMailbox(ctx, "acme", "paris@example.com", "inbox")
	if err != nil || !ok {
		t.Fatalf("INBOX: ok=%v err=%v", ok, err)
	}
	if mb.Name != "INBOX" || mb.UIDNext != 1 || mb.UIDValidity == 0 || !mb.Subscribed {
		t.Errorf("INBOX = %+v", mb)
	}

	// Update: empty pwHash keeps the hash; status + policy change.
	created, err = s.UpsertAccount(ctx, "acme", "paris@example.com", "", StatusDisabled, json.RawMessage(`{"append":"observe"}`))
	if err != nil || created {
		t.Fatalf("update: created=%v err=%v", created, err)
	}
	a, _, _ = s.GetAccount(ctx, "paris@example.com")
	if a.PwHash != "hash1" || a.Status != StatusDisabled || string(a.Policy) != `{"append":"observe"}` {
		t.Errorf("after update = %+v", a)
	}
	// A new password replaces the hash.
	if _, err := s.UpsertAccount(ctx, "acme", "paris@example.com", "hash2", "", nil); err != nil {
		t.Fatal(err)
	}
	a, _, _ = s.GetAccount(ctx, "paris@example.com")
	if a.PwHash != "hash2" {
		t.Errorf("pw_hash = %q, want hash2", a.PwHash)
	}
	// Still exactly one INBOX.
	mbs, _ := s.ListMailboxes(ctx, "acme", "paris@example.com")
	if len(mbs) != 1 {
		t.Errorf("mailboxes = %d, want 1", len(mbs))
	}
}

func TestUpsertAccountRefusesCrossTenantUsername(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", "paris@example.com", "h", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertAccount(ctx, "other", "paris@example.com", "h", "", nil); err != ErrUsernameTaken {
		t.Errorf("cross-tenant upsert err = %v, want ErrUsernameTaken", err)
	}
	if _, err := s.UpsertAccount(ctx, "acme", "new@example.com", "", "", nil); err == nil {
		t.Error("create without password should fail")
	}
	if _, err := s.UpsertAccount(ctx, "acme", "x@example.com", "h", "weird", nil); err == nil {
		t.Error("bad status should fail")
	}
}

func TestAppendSemantics(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", "paris@example.com", "h", "", nil); err != nil {
		t.Fatal(err)
	}
	mb, _, _ := s.GetMailbox(ctx, "acme", "paris@example.com", "INBOX")

	var got []Change
	s.SetOnChange(func(c Change) { got = append(got, c) })

	msg := func(key, sha string) Message {
		return Message{ObjectKey: key, Kind: KindRecord, SHA256: sha, FormatVersion: 1, Size: 10,
			Flags: []string{`\seen`, "$Hello", `\Seen`}, Subject: "hi", Envelope: json.RawMessage(`{"Subject":"hi"}`)}
	}
	r1, err := s.AppendMessage(ctx, mb.ID, msg("k1", "aaa"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.UID != 1 || r1.Noop || r1.Replaced || r1.UIDValidity != mb.UIDValidity {
		t.Errorf("first append = %+v", r1)
	}
	// Same key, same sha → noop, same uid, no change event.
	before := len(got)
	r2, err := s.AppendMessage(ctx, mb.ID, msg("k1", "aaa"))
	if err != nil || !r2.Noop || r2.UID != 1 {
		t.Errorf("noop append = %+v err=%v", r2, err)
	}
	if len(got) != before {
		t.Errorf("noop emitted %d changes", len(got)-before)
	}
	// Different key → uid 2.
	r3, _ := s.AppendMessage(ctx, mb.ID, msg("k2", "bbb"))
	if r3.UID != 2 {
		t.Errorf("second key uid = %d, want 2", r3.UID)
	}
	// Same key k1, different sha → old uid 1 expunged, new uid 3.
	got = nil
	r4, err := s.AppendMessage(ctx, mb.ID, msg("k1", "ccc"))
	if err != nil {
		t.Fatal(err)
	}
	if !r4.Replaced || r4.ReplacedUID != 1 || r4.UID != 3 {
		t.Errorf("replace = %+v", r4)
	}
	if len(got) != 2 || got[0].Kind != ChangeExpunge || got[0].UID != 1 || got[0].Seq != 1 ||
		got[1].Kind != ChangeAppend || got[1].UID != 3 || got[1].Total != 2 || got[1].Seq != 2 {
		t.Errorf("changes = %+v", got)
	}
	heads, _ := s.ListMessageHeads(ctx, mb.ID)
	if len(heads) != 2 || heads[0].UID != 2 || heads[1].UID != 3 {
		t.Errorf("heads = %+v", heads)
	}
	// Flags normalised: system flag canonical case, deduped, sorted.
	if strings.Join(heads[0].Flags, ",") != `$Hello,\Seen` {
		t.Errorf("flags = %v", heads[0].Flags)
	}
	// uidnext stored, never derived: after removing the tail the next
	// append still gets 4.
	if ok, err := s.RemoveMessage(ctx, mb.ID, 3); err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	r5, _ := s.AppendMessage(ctx, mb.ID, msg("", "ddd"))
	if r5.UID != 4 {
		t.Errorf("uid after remove = %d, want 4", r5.UID)
	}
	// Internal date defaults to the clock.
	m, ok, _ := s.GetMessage(ctx, mb.ID, 4)
	if !ok || !m.InternalDate.Equal(*clk) || m.Kind != KindRecord || m.SHA256 != "ddd" {
		t.Errorf("row = %+v", m)
	}
	// Flags are the mutable surface.
	got = nil
	fl, err := s.SetFlags(ctx, mb.ID, 4, []string{`\Seen`, `\Answered`})
	if err != nil || strings.Join(fl, ",") != `\Answered,\Seen` {
		t.Errorf("SetFlags = %v err=%v", fl, err)
	}
	if len(got) != 1 || got[0].Kind != ChangeFlags || got[0].UID != 4 || got[0].Seq != 2 {
		t.Errorf("flag change = %+v", got)
	}
	m, _, _ = s.GetMessage(ctx, mb.ID, 4)
	if !HasFlag(m.Flags, `\seen`) {
		t.Errorf("flags not persisted: %v", m.Flags)
	}
	if _, err := s.SetFlags(ctx, mb.ID, 99, nil); err == nil {
		t.Error("SetFlags on a missing uid should fail")
	}
	mb2, _, _ := s.GetMailboxByID(ctx, mb.ID)
	if mb2.UIDNext != 5 || mb2.ModSeq == 0 {
		t.Errorf("mailbox after appends = %+v", mb2)
	}
}

func TestMailboxHelpers(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", "p@example.com", "h", "", nil); err != nil {
		t.Fatal(err)
	}
	mb, created, err := s.EnsureMailbox(ctx, "acme", "p@example.com", "/Brain//Knowledge/")
	if err != nil || !created || mb.Name != "Brain/Knowledge" {
		t.Fatalf("ensure = %+v created=%v err=%v", mb, created, err)
	}
	_, created, _ = s.EnsureMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge")
	if created {
		t.Error("second ensure should not create")
	}
	if _, _, err := s.EnsureMailbox(ctx, "acme", "p@example.com", "   "); err == nil {
		t.Error("empty name should fail")
	}
	if err := s.SetSubscribed(ctx, mb.ID, false); err != nil {
		t.Fatal(err)
	}
	mbs, _ := s.ListMailboxes(ctx, "acme", "p@example.com")
	if len(mbs) != 2 || mbs[0].Name != "Brain/Knowledge" || mbs[0].Subscribed || mbs[1].Name != "INBOX" {
		t.Errorf("list = %+v", mbs)
	}
	if _, ok, _ := s.GetMailboxByRole(ctx, "acme", "p@example.com", "knowledge"); ok {
		t.Error("no role assigned yet")
	}
	if NormalizeFlags([]string{`\Recent`, "", " x "}) == nil || len(NormalizeFlags([]string{`\Recent`})) != 0 {
		t.Error("\\Recent must be dropped")
	}
}

func TestGetAccountByLocalPart(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, u := range []string{"paris@a.example.com", "rome@a.example.com", "rome@b.example.com", "paris_x@a.example.com"} {
		if _, err := s.UpsertAccount(ctx, "acme", u, "h", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	a, n, err := s.GetAccountByLocalPart(ctx, "Paris")
	if err != nil || n != 1 || a.Username != "paris@a.example.com" {
		t.Errorf("unique = %+v n=%d err=%v", a, n, err)
	}
	if _, n, _ := s.GetAccountByLocalPart(ctx, "rome"); n != 2 {
		t.Errorf("ambiguous n = %d, want 2", n)
	}
	if _, n, _ := s.GetAccountByLocalPart(ctx, "nobody"); n != 0 {
		t.Errorf("missing n = %d", n)
	}
	if _, n, _ := s.GetAccountByLocalPart(ctx, "paris@a.example.com"); n != 0 {
		t.Errorf("a full address is not a local part: n = %d", n)
	}
}
