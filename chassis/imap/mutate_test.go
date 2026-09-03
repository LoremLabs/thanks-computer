package imap

import (
	"context"
	"encoding/json"
	"testing"
)

func seedAccount(t *testing.T, s *Store) Mailbox {
	t.Helper()
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", "p@example.com", "h", "", nil); err != nil {
		t.Fatal(err)
	}
	inbox, _, _ := s.GetMailbox(ctx, "acme", "p@example.com", "INBOX")
	return inbox
}

func TestMailboxTree(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s)

	mb, err := s.CreateMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge", "knowledge", []string{`\Archive`, `\Archive`, ""}, json.RawMessage(`{"append":"stack"}`))
	if err != nil || mb.Role != "knowledge" || len(mb.Attrs) != 1 || string(mb.Policy) != `{"append":"stack"}` {
		t.Fatalf("create = %+v err=%v", mb, err)
	}
	if _, err := s.CreateMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge", "", nil, nil); err != ErrMailboxExists {
		t.Errorf("dup create err = %v", err)
	}
	if _, err := s.CreateMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge/Old", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Rename the subtree; role and id survive.
	ren, err := s.RenameMailbox(ctx, "acme", "p@example.com", "Brain", "Mind")
	if err != ErrMailboxNotFound {
		t.Errorf("renaming an implicit parent should be not-found, got %v", err)
	}
	ren, err = s.RenameMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge", "Mind/Docs")
	if err != nil || ren.ID != mb.ID || ren.Name != "Mind/Docs" || ren.Role != "knowledge" {
		t.Fatalf("rename = %+v err=%v", ren, err)
	}
	mbs, _ := s.ListMailboxes(ctx, "acme", "p@example.com")
	names := []string{}
	for _, m := range mbs {
		names = append(names, m.Name)
	}
	if got := join(names); got != "INBOX,Mind/Docs,Mind/Docs/Old" {
		t.Errorf("names after rename = %s", got)
	}
	if _, err := s.RenameMailbox(ctx, "acme", "p@example.com", "Mind/Docs", "Mind/Docs/Deeper"); err == nil {
		t.Error("rename under itself should fail")
	}
	if _, err := s.RenameMailbox(ctx, "acme", "p@example.com", "INBOX", "X"); err != ErrINBOX {
		t.Errorf("rename INBOX err = %v", err)
	}
	if _, err := s.DeleteMailbox(ctx, "acme", "p@example.com", "INBOX"); err != ErrINBOX {
		t.Errorf("delete INBOX err = %v", err)
	}
	if _, err := s.DeleteMailbox(ctx, "acme", "p@example.com", "Mind/Docs"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetMailbox(ctx, "acme", "p@example.com", "Mind/Docs"); ok {
		t.Error("deleted mailbox still live")
	}
	if _, ok, _ := s.GetMailbox(ctx, "acme", "p@example.com", "Mind/Docs/Old"); !ok {
		t.Error("child should survive a parent delete")
	}
	// The name is free again.
	if _, err := s.CreateMailbox(ctx, "acme", "p@example.com", "Mind/Docs", "", nil, nil); err != nil {
		t.Errorf("recreate after delete: %v", err)
	}
	role := "archive"
	up, err := s.UpdateMailbox(ctx, mb.ID, &role, nil, nil)
	if err != ErrMailboxNotFound {
		t.Errorf("update of a deleted mailbox err = %v (%+v)", err, up)
	}
}

func TestCopyMoveExpunge(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	inbox := seedAccount(t, s)
	arch, _ := s.CreateMailbox(ctx, "acme", "p@example.com", "Archive", "", nil, nil)

	for i, k := range []string{"a", "b", "c", "d"} {
		if _, err := s.AppendMessage(ctx, inbox.ID, Message{ObjectKey: k, Kind: KindRecord, SHA256: "sha" + k, Size: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	var got []Change
	s.SetOnChange(func(c Change) { got = append(got, c) })

	// COPY keeps the key when free; a second copy of the same key is keyless.
	r, err := s.CopyMessage(ctx, inbox.ID, 2, arch.ID)
	if err != nil || r.UID != 1 {
		t.Fatalf("copy = %+v err=%v", r, err)
	}
	m, _, _ := s.GetMessage(ctx, arch.ID, 1)
	if m.ObjectKey != "b" || m.SHA256 != "shab" {
		t.Errorf("copied row = %+v", m)
	}
	r, _ = s.CopyMessage(ctx, inbox.ID, 2, arch.ID)
	m, _, _ = s.GetMessage(ctx, arch.ID, r.UID)
	if m.ObjectKey != "" || r.UID != 2 {
		t.Errorf("second copy = %+v row=%+v", r, m)
	}

	// MOVE uids 1 and 3: dest gets 3 and 4; src expunges reported 3 then 1.
	got = nil
	moved, exp, err := s.MoveMessages(ctx, inbox.ID, []uint32{1, 3}, arch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved[1] != 3 || moved[3] != 4 || len(exp) != 2 || exp[0].Seq != 3 || exp[0].UID != 3 || exp[1].Seq != 1 || exp[1].UID != 1 {
		t.Errorf("move = %v exp=%+v", moved, exp)
	}
	heads, _ := s.ListMessageHeads(ctx, inbox.ID)
	if len(heads) != 2 || heads[0].UID != 2 || heads[1].UID != 4 {
		t.Errorf("src after move = %+v", heads)
	}
	// Change order: two dest appends, then src expunges seq 3 then seq 1.
	kinds := ""
	for _, c := range got {
		kinds += string(c.Kind)[0:1]
	}
	if kinds != "aaee" || got[2].Seq != 3 || got[3].Seq != 1 || got[3].Total != 2 {
		t.Errorf("changes = %+v", got)
	}

	// EXPUNGE removes only \Deleted rows, restricted to the uid set.
	if _, err := s.SetFlags(ctx, inbox.ID, 2, []string{`\Deleted`}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetFlags(ctx, inbox.ID, 4, []string{`\Deleted`}); err != nil {
		t.Fatal(err)
	}
	exp, _ = s.Expunge(ctx, inbox.ID, []uint32{4})
	if len(exp) != 1 || exp[0].UID != 4 || exp[0].Seq != 2 {
		t.Errorf("uid expunge = %+v", exp)
	}
	exp, _ = s.Expunge(ctx, inbox.ID, nil)
	if len(exp) != 1 || exp[0].UID != 2 || exp[0].Seq != 1 {
		t.Errorf("expunge = %+v", exp)
	}
	if n, _, _ := s.CountByMailbox(ctx, inbox.ID); n != 0 {
		t.Errorf("inbox count = %d", n)
	}

	// Windowed listing on the archive: 4 rows, limit 2 → next cursor.
	items, next, err := s.ListMessages(ctx, arch.ID, 0, 2, nil)
	if err != nil || len(items) != 2 || next != 2 {
		t.Errorf("window 1 = %d next=%d err=%v", len(items), next, err)
	}
	items, next, _ = s.ListMessages(ctx, arch.ID, next, 2, nil)
	if len(items) != 2 || next != 0 || items[0].UID != 3 {
		t.Errorf("window 2 = %+v next=%d", items, next)
	}
	items, _, _ = s.ListMessages(ctx, arch.ID, 0, 10, []string{`\Flagged`})
	if len(items) != 0 {
		t.Errorf("flag filter = %d", len(items))
	}

	// Reset: new UIDVALIDITY, empty, uidnext back to 1.
	before := arch.UIDValidity
	rs, err := s.ResetMailbox(ctx, arch.ID)
	if err != nil || rs.UIDValidity <= before || rs.UIDNext != 1 {
		t.Errorf("reset = %+v err=%v", rs, err)
	}
	if n, _, _ := s.CountByMailbox(ctx, arch.ID); n != 0 {
		t.Errorf("archive after reset = %d", n)
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func TestRenameSubtreeIgnoresSiblingsAndWildcards(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s)
	for _, n := range []string{"Brain/Knowledge", "Brain/Knowledge/Old", "Brain/Knowledge2", "Brain/Kn_wledge", "Brain/Knowledge/%"} {
		if _, err := s.CreateMailbox(ctx, "acme", "p@example.com", n, "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RenameMailbox(ctx, "acme", "p@example.com", "Brain/Knowledge", "Mind"); err != nil {
		t.Fatal(err)
	}
	mbs, _ := s.ListMailboxes(ctx, "acme", "p@example.com")
	var names []string
	for _, m := range mbs {
		names = append(names, m.Name)
	}
	if got := join(names); got != "Brain/Kn_wledge,Brain/Knowledge2,INBOX,Mind,Mind/%,Mind/Old" {
		t.Errorf("names = %s", got)
	}
}

func TestExpungeNoopDoesNotBumpModSeq(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	inbox := seedAccount(t, s)
	if _, err := s.AppendMessage(ctx, inbox.ID, Message{ObjectKey: "a", Kind: KindRecord, SHA256: "sa", Size: 1}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.GetMailboxByID(ctx, inbox.ID)
	if exp, err := s.Expunge(ctx, inbox.ID, nil); err != nil || len(exp) != 0 {
		t.Fatalf("expunge = %+v err=%v", exp, err)
	}
	after, _, _ := s.GetMailboxByID(ctx, inbox.ID)
	if after.ModSeq != before.ModSeq {
		t.Errorf("no-op expunge bumped modseq %d → %d", before.ModSeq, after.ModSeq)
	}
	if _, err := s.SetFlags(ctx, inbox.ID, 1, []string{`\Deleted`}); err != nil {
		t.Fatal(err)
	}
	if exp, _ := s.Expunge(ctx, inbox.ID, nil); len(exp) != 1 {
		t.Fatalf("expunge = %+v", exp)
	}
	final, _, _ := s.GetMailboxByID(ctx, inbox.ID)
	if final.ModSeq != before.ModSeq+2 {
		t.Errorf("modseq after flags+expunge = %d, want %d", final.ModSeq, before.ModSeq+2)
	}
}
