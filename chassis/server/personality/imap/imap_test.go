package imap

import (
	"bytes"
	"context"
	"database/sql"
	"net/mail"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// fakeAdmission suspends one tenant and admits everyone else.
type fakeAdmission struct{ suspended string }

func (f fakeAdmission) Decide(tenant string) admission.Decision {
	if tenant == f.suspended {
		return admission.Decision{Admit: false, Status: 402, Reason: "suspended"}
	}
	return admission.Decision{Admit: true}
}
func (fakeAdmission) AllowRate(string) (bool, time.Duration)           { return true, 0 }
func (fakeAdmission) AcquireConcurrency(string, *admission.Lease) bool { return true }

type harness struct {
	ctrl  *Controller
	store *chimap.Store
	fcas  filecas.Store
	addr  string
}

func newHarness(t *testing.T, conf config.Config) *harness {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "imap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := chimap.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf.Personalities = "imap"
	conf.IMAPListenAddrs = []string{"127.0.0.1:0"}
	conf.IMAPInsecureAuth = true
	if conf.IMAPLoginRate == 0 {
		conf.IMAPLoginRate = 100
	}
	if conf.IMAPObserveSample == 0 {
		conf.IMAPObserveSample = 1
	}
	if conf.IMAPRespTimeout == "" {
		conf.IMAPRespTimeout = "30s"
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, store)
	ctrl.SetFileCAS(fs)
	ctrl.Start()
	t.Cleanup(func() { ctrl.Stop(); cancel() })
	addrs := ctrl.boundAddrs()
	if len(addrs) < 1 {
		t.Fatalf("bound %v", addrs)
	}
	return &harness{ctrl: ctrl, store: store, fcas: fs, addr: addrs[0]}
}

func (h *harness) account(t *testing.T, tenant, username, password, status string) {
	t.Helper()
	hash, err := chimap.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertAccount(context.Background(), tenant, username, hash, status, nil); err != nil {
		t.Fatal(err)
	}
}

// appendHello projects a record into the account's INBOX the way the
// txco://imap/append op does: build → CAS → row.
func (h *harness) appendHello(t *testing.T, tenant, username, key, subject, text string) chimap.AppendResult {
	t.Helper()
	ctx := context.Background()
	mb, ok, err := h.store.GetMailbox(ctx, tenant, username, "INBOX")
	if err != nil || !ok {
		t.Fatalf("INBOX: %v %v", ok, err)
	}
	rec := &chimap.Record{Headers: map[string]string{"From": "Pony <pony@example.com>", "To": username, "Subject": subject}, Text: text}
	ing, err := BuildRecordMessage(rec, key, domainOf(username), time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), []string{"$Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.fcas.Put(ctx, ing.SHA256, ing.Canonical); err != nil {
		t.Fatal(err)
	}
	res, err := h.store.AppendMessage(ctx, mb.ID, ing.Message)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func dial(t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginAndReadINBOX(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", "paris@example.com", "secret-1", "")
	h.appendHello(t, "acme", "paris@example.com", "hello", "Hello from your pony", "Hi there — your mailbox works.\n")

	c := dial(t, h.addr)
	if err := c.Login("Paris@Example.com", "secret-1").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	ns, err := c.Namespace().Wait()
	if err != nil || len(ns.Personal) != 1 || ns.Personal[0].Delim != '/' {
		t.Errorf("namespace = %+v err=%v", ns, err)
	}
	lst, err := c.List("", "*", nil).Collect()
	if err != nil || len(lst) != 1 || lst[0].Mailbox != "INBOX" {
		t.Fatalf("list = %+v err=%v", lst, err)
	}
	st, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true}).Wait()
	if err != nil || *st.NumMessages != 1 || *st.NumUnseen != 1 || st.UIDNext != 2 {
		t.Fatalf("status = %+v err=%v", st, err)
	}
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil || sel.NumMessages != 1 || sel.UIDNext != 2 || sel.UIDValidity == 0 {
		t.Fatalf("select = %+v err=%v", sel, err)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, RFC822Size: true, InternalDate: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch = %+v err=%v", msgs, err)
	}
	m := msgs[0]
	if m.UID != 1 || m.Envelope == nil || m.Envelope.Subject != "Hello from your pony" || m.Envelope.From[0].Addr() != "pony@example.com" {
		t.Errorf("envelope = %+v", m.Envelope)
	}
	if m.BodyStructure == nil || m.BodyStructure.MediaType() != "text/plain" {
		t.Errorf("bodystructure = %+v", m.BodyStructure)
	}
	raw := m.FindBodySection(&imap.FetchItemBodySection{})
	if int64(len(raw)) != m.RFC822Size {
		t.Errorf("BODY[] %d bytes, RFC822.SIZE %d", len(raw), m.RFC822Size)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("BODY[] not RFC 5322: %v\n%s", err, raw)
	}
	if parsed.Header.Get("Subject") != "Hello from your pony" || !strings.Contains(string(raw), "your mailbox works") {
		t.Errorf("rendered:\n%s", raw)
	}
	// A non-peek BODY[] fetch marks \Seen; the keyword from the append is
	// there too.
	msgs, _ = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if len(msgs) != 1 || !hasFlag(msgs[0].Flags, imap.FlagSeen) || !hasFlag(msgs[0].Flags, "$Hello") {
		t.Errorf("flags after fetch = %v", msgs[0].Flags)
	}
	// STORE persists: a second connection sees the flag.
	if _, err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	c2 := dial(t, h.addr)
	if err := c2.Login("paris@example.com", "secret-1").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, _ = c2.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if len(msgs) != 1 || !hasFlag(msgs[0].Flags, imap.FlagFlagged) || !hasFlag(msgs[0].Flags, imap.FlagSeen) {
		t.Errorf("flags on 2nd conn = %v", msgs[0].Flags)
	}
	st, _ = c2.Status("INBOX", &imap.StatusOptions{NumUnseen: true}).Wait()
	if *st.NumUnseen != 0 {
		t.Errorf("unseen after \\Seen = %d", *st.NumUnseen)
	}
	// Column-backed SEARCH.
	// Column-backed SEARCH (plain `* SEARCH n` without ESEARCH, so count
	// the returned set rather than reading Count).
	sd, err := c2.Search(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "pony"}}}, nil).Wait()
	if err != nil || searchHits(sd) != 1 {
		t.Errorf("search subject = %+v err=%v", sd, err)
	}
	sd, _ = c2.Search(&imap.SearchCriteria{Text: []string{"mailbox works"}}, nil).Wait()
	if searchHits(sd) != 1 {
		t.Errorf("search text = %+v", sd)
	}
	sd, _ = c2.Search(&imap.SearchCriteria{Body: []string{"absent-token"}}, nil).Wait()
	if searchHits(sd) != 0 {
		t.Errorf("search miss = %+v", sd)
	}
	if _, err := c2.Search(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Weird"}}}, nil).Wait(); err == nil {
		t.Error("unsupported header search should answer NO")
	}
	// Mutating verbs are live (mutate_test.go covers them); a CREATE here
	// must simply succeed.
	if err := c2.Create("Brain/Knowledge", nil).Wait(); err != nil {
		t.Errorf("create err = %v", err)
	}
}

// A client that sends only the local part (Apple Mail does, when the
// address domain and the server name line up) logs in when the local part
// names exactly one account, and fails plainly when it is ambiguous.
func TestLoginWithBareLocalPart(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", "paris@example.com", "pw", "")
	h.account(t, "acme", "rome@example.com", "pw", "")
	h.account(t, "other", "rome@other.example", "pw", "")

	c := dial(t, h.addr)
	if err := c.Login("paris", "pw").Wait(); err != nil {
		t.Fatalf("bare local part: %v", err)
	}
	lst, err := c.List("", "*", nil).Collect()
	if err != nil || len(lst) != 1 {
		t.Fatalf("list after bare login = %+v err=%v", lst, err)
	}
	c2 := dial(t, h.addr)
	if err := c2.Login("rome", "pw").Wait(); err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("ambiguous local part err = %v", err)
	}
	if err := c2.Login("rome@other.example", "pw").Wait(); err != nil {
		t.Errorf("full address still works: %v", err)
	}
}

func TestAppendNotifiesSelectedSession(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", "paris@example.com", "pw", "")
	c := dial(t, h.addr)
	if err := c.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	sel, _ := c.Select("INBOX", nil).Wait()
	if sel.NumMessages != 0 {
		t.Fatalf("empty INBOX expected, got %d", sel.NumMessages)
	}
	h.appendHello(t, "acme", "paris@example.com", "k1", "one", "first")
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := c.Mailbox().NumMessages; n != 1 {
		t.Fatalf("EXISTS after append = %d, want 1", n)
	}
	// Replacing the same object_key expunges uid 1 and appends uid 2; the
	// client sees EXPUNGE 1 then EXISTS 1 and can fetch the new content.
	res := h.appendHello(t, "acme", "paris@example.com", "k1", "one v2", "second")
	if !res.Replaced || res.UID != 2 {
		t.Fatalf("replace = %+v", res)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := c.Mailbox().NumMessages; n != 1 {
		t.Fatalf("EXISTS after replace = %d, want 1", n)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil || len(msgs) != 1 || msgs[0].UID != 2 || msgs[0].Envelope.Subject != "one v2" {
		t.Errorf("after replace = %+v err=%v", msgs, err)
	}
}

func TestLoginFailuresAndThrottle(t *testing.T) {
	h := newHarness(t, config.Config{IMAPLoginRate: 3})
	h.account(t, "acme", "paris@example.com", "right", "")
	h.account(t, "acme", "off@example.com", "pw", chimap.StatusDisabled)
	h.account(t, "suspended", "gone@example.com", "pw", "")

	c := dial(t, h.addr)
	err := c.Login("paris@example.com", "wrong").Wait()
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("wrong password err = %v", err)
	}
	err = c.Login("nobody@example.com", "x").Wait()
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("unknown user err = %v", err)
	}
	err = c.Login("off@example.com", "pw").Wait()
	if err == nil || !strings.Contains(err.Error(), "Account disabled") {
		t.Fatalf("disabled err = %v", err)
	}
	// 4th attempt from this IP within the minute: throttled before any
	// password work.
	err = c.Login("paris@example.com", "right").Wait()
	if err == nil || !strings.Contains(err.Error(), "Too many login attempts") {
		t.Fatalf("throttle err = %v", err)
	}
	if h.ctrl.cache.hit(loginKey("paris@example.com", "", "right")) {
		t.Error("nothing should be cached")
	}
}

func TestAdmissionAndConnCap(t *testing.T) {
	h := newHarness(t, config.Config{IMAPMaxConnsPerAccount: 1})
	h.account(t, "suspended", "gone@example.com", "pw", "")
	h.account(t, "acme", "paris@example.com", "pw", "")

	c := dial(t, h.addr)
	err := c.Login("gone@example.com", "pw").Wait()
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("suspended tenant err = %v", err)
	}
	if err := c.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	c2 := dial(t, h.addr)
	err = c2.Login("paris@example.com", "pw").Wait()
	if err == nil || !strings.Contains(err.Error(), "Too many connections") {
		t.Fatalf("conn cap err = %v", err)
	}
	// Logging out the first frees the slot.
	if err := c.Logout().Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		c3 := dial(t, h.addr)
		if err := c3.Login("paris@example.com", "pw").Wait(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot never released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBodyStructureCodec(t *testing.T) {
	rec := &chimap.Record{Headers: map[string]string{"From": "a@b.c", "Subject": "s"}, Text: "t", HTML: "<b>h</b>",
		Parts: []chimap.PartRef{{Name: "x.pdf", Type: "application/pdf", Size: 3}}}
	ing, err := BuildRecordMessage(rec, "k", "b.c", time.Unix(1_700_000_000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := DecodeBodyStructure(ing.Message.BodyStructure)
	if err != nil {
		t.Fatal(err)
	}
	if bs.MediaType() != "multipart/mixed" {
		t.Errorf("media = %s", bs.MediaType())
	}
	again, _ := EncodeBodyStructure(bs)
	if !bytes.Equal(again, ing.Message.BodyStructure) {
		t.Errorf("codec not stable:\n%s\n%s", again, ing.Message.BodyStructure)
	}
	var paths []string
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		paths = append(paths, part.MediaType())
		return true
	})
	want := "multipart/mixed,multipart/alternative,text/plain,text/html,text/plain"
	if got := strings.Join(paths, ","); got != want {
		t.Errorf("walk = %s, want %s", got, want)
	}
	if ing.Message.Subject != "s" || ing.Message.FromAddr != "a@b.c" || ing.Message.Size != int64(len(ing.Rendered)) {
		t.Errorf("row = %+v", ing.Message)
	}
}

func searchHits(sd *imap.SearchData) int {
	if sd == nil || sd.All == nil {
		return 0
	}
	switch set := sd.All.(type) {
	case imap.SeqSet:
		n, _ := set.Nums()
		return len(n)
	case imap.UIDSet:
		n, _ := set.Nums()
		return len(n)
	}
	return 0
}

func hasFlag(flags []imap.Flag, f imap.Flag) bool {
	for _, x := range flags {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}
