package imap

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// fakeStack drains the bus like the processor would: records every
// envelope and answers with `respond(envelope)` (or `{}`).
type fakeStack struct {
	mu      sync.Mutex
	seen    []string
	respond func(raw string) string
	delay   time.Duration
}

func (f *fakeStack) serve(bus <-chan *event.Envelope) {
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			raw := env.Payload.Raw
			f.mu.Lock()
			f.seen = append(f.seen, raw)
			delay := f.delay
			f.mu.Unlock()
			out := "{}"
			if f.respond != nil {
				out = f.respond(raw)
			}
			// One goroutine per envelope, like the processor: a slow run
			// must not queue the next dispatch behind it.
			go func(env *event.Envelope, out string) {
				if delay > 0 {
					time.Sleep(delay)
				}
				env.ResCh <- event.Payload{Raw: out, Type: event.JSON}
			}(env, out)
		}
	}()
}

func (f *fakeStack) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := append([]string{}, f.seen...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("saw %d envelopes, want %d: %v", len(got), n, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// withStack wires a bus + fake stack into a harness and subscribes the
// tenant "acme".
func withStack(t *testing.T, h *harness, fs *fakeStack) {
	t.Helper()
	bus := make(chan *event.Envelope, 16)
	h.ctrl.pu.Bus = bus
	h.ctrl.lanes.subscribed = func(tenant string) bool { return tenant == "acme" }
	h.ctrl.lanes.deadline = 500 * time.Millisecond
	fs.serve(bus)
}

func login(t *testing.T, h *harness, user, pw string) *imapclient.Client {
	t.Helper()
	c := dial(t, h.addr)
	if err := c.Login(user, pw).Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

const rawMsg = "From: Owner <owner@example.com>\r\nTo: paris@example.com\r\nSubject: dragged in\r\nMessage-ID: <abc@example.com>\r\nDate: Thu, 03 Sep 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease read the attachment.\r\n--b1\r\nContent-Type: application/pdf; name=\"guide.pdf\"\r\nContent-Disposition: attachment; filename=\"guide.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--b1--\r\n"

func TestTreeVerbsAndList(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", "paris@example.com", "pw", "")
	c := login(t, h, "paris@example.com", "pw")

	if err := c.Create("Brain/Knowledge", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive}}).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Create("Brain/Knowledge", nil).Wait(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("dup create err = %v", err)
	}
	lst, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]imap.MailboxAttr{}
	for _, l := range lst {
		names[l.Mailbox] = l.Attrs
	}
	if _, ok := names["Brain/Knowledge"]; !ok || len(names) != 3 {
		t.Fatalf("list = %v", names)
	}
	if !hasAttr(names["Brain"], imap.MailboxAttrNoSelect) || !hasAttr(names["Brain"], imap.MailboxAttrHasChildren) {
		t.Errorf("implicit parent attrs = %v", names["Brain"])
	}
	if !hasAttr(names["Brain/Knowledge"], imap.MailboxAttrArchive) || !hasAttr(names["Brain/Knowledge"], imap.MailboxAttrHasNoChildren) {
		t.Errorf("leaf attrs = %v", names["Brain/Knowledge"])
	}
	if err := c.Rename("Brain/Knowledge", "Docs", nil).Wait(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := c.Rename("INBOX", "Old", nil).Wait(); err == nil {
		t.Error("renaming INBOX must fail")
	}
	if err := c.Delete("INBOX").Wait(); err == nil {
		t.Error("deleting INBOX must fail")
	}
	if err := c.Delete("Docs").Wait(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	lst, _ = c.List("", "*", nil).Collect()
	if len(lst) != 1 || lst[0].Mailbox != "INBOX" {
		t.Errorf("after delete = %+v", lst)
	}
	// Selecting a mailbox then deleting it unselects cleanly.
	_ = c.Create("Tmp", nil).Wait()
	if _, err := c.Select("Tmp", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("Tmp").Wait(); err != nil {
		t.Fatal(err)
	}
	if c.Mailbox() != nil {
		// go-imap keeps its own view; the server unselected. A NOOP must
		// not error.
		if err := c.Noop().Wait(); err != nil {
			t.Errorf("noop after delete: %v", err)
		}
	}
}

func TestClientAppendCopyMoveExpunge(t *testing.T) {
	h := newHarness(t, config.Config{})
	ix := blob.NewKVIndex(nil)
	_ = ix
	h.account(t, "acme", "paris@example.com", "pw", "")
	c := login(t, h, "paris@example.com", "pw")
	_ = c.Create("Archive", nil).Wait()

	ac := c.Append("INBOX", int64(len(rawMsg)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagFlagged}, Time: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)})
	if _, err := ac.Write([]byte(rawMsg)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	ad, err := ac.Wait()
	if err != nil || ad.UID != 1 {
		t.Fatalf("append = %+v err=%v", ad, err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{
		Flags: true, InternalDate: true, RFC822Size: true, Envelope: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	m := msgs[0]
	if got := m.FindBodySection(&imap.FetchItemBodySection{Peek: true}); !bytes.Equal(got, []byte(rawMsg)) {
		t.Errorf("verbatim bytes differ:\n%q", got)
	}
	if m.Envelope.Subject != "dragged in" || m.Envelope.MessageID != "abc@example.com" || m.RFC822Size != int64(len(rawMsg)) {
		t.Errorf("envelope = %+v size=%d", m.Envelope, m.RFC822Size)
	}
	if m.BodyStructure.MediaType() != "multipart/mixed" || !hasFlag(m.Flags, imap.FlagFlagged) || !m.InternalDate.Equal(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("structure=%s flags=%v date=%v", m.BodyStructure.MediaType(), m.Flags, m.InternalDate)
	}
	// The row is verbatim, its parts are in the CAS with the tenant's rows.
	mb, _, _ := h.store.GetMailbox(context.Background(), "acme", "paris@example.com", "INBOX")
	row, _, _ := h.store.GetMessage(context.Background(), mb.ID, 1)
	if row.Kind != chimap.KindVerbatim || !strings.Contains(string(row.Parts), `"guide.pdf"`) {
		t.Errorf("row = %+v", row)
	}
	var parts []partFacts
	_ = json.Unmarshal(row.Parts, &parts)
	if len(parts) != 1 {
		t.Fatalf("parts = %+v", parts)
	}
	if ok, _ := h.fcas.Exists(context.Background(), parts[0].SHA256); !ok {
		t.Error("attachment bytes not in the CAS")
	}
	if ok, _ := h.fcas.Exists(context.Background(), row.SHA256); !ok {
		t.Error("message bytes not in the CAS")
	}

	// A second message, then COPY 1 → Archive, MOVE 2 → Archive, EXPUNGE.
	ac = c.Append("INBOX", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	cd, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
	if err != nil || len(cd.DestUIDs) == 0 {
		t.Fatalf("copy = %+v err=%v", cd, err)
	}
	md, err := c.Move(imap.UIDSetNum(2), "Archive").Wait()
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if set, ok := md.DestUIDs.(imap.UIDSet); !ok || len(set) != 1 || set[0].Start != 2 {
		t.Errorf("move dest uids = %v", md.DestUIDs)
	}
	if c.Mailbox().NumMessages != 1 {
		t.Errorf("EXISTS after move = %d", c.Mailbox().NumMessages)
	}
	st, _ := c.Status("Archive", &imap.StatusOptions{NumMessages: true}).Wait()
	if *st.NumMessages != 2 {
		t.Errorf("archive count = %d", *st.NumMessages)
	}
	// \Deleted + EXPUNGE removes the last INBOX message.
	if _, err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if c.Mailbox().NumMessages != 0 {
		t.Errorf("EXISTS after expunge = %d", c.Mailbox().NumMessages)
	}
	if _, err := c.Move(imap.UIDSetNum(9), "Nope").Wait(); err == nil || !strings.Contains(err.Error(), "No such mailbox") {
		t.Errorf("move to missing = %v", err)
	}
}

func TestPolicyDenyAndLanes(t *testing.T) {
	h := newHarness(t, config.Config{})
	fs := &fakeStack{respond: func(raw string) string {
		// Answer lane: allow appends whose subject says so; refuse others
		// with a code; observe envelopes get {}.
		if gjson.Get(raw, "_txc.imap.phase").String() != "answer" {
			return "{}"
		}
		if strings.Contains(gjson.Get(raw, "_txc.imap.msg.subject").String(), "allow") {
			return `{"_txc":{"imap":{"res":{"ok":true,"flags":["$Ingested"],"object_key":"docs/1"}}}}`
		}
		return `{"_txc":{"imap":{"res":{"ok":false,"code":"cannot","msg":"not here"}}}}`
	}}
	withStack(t, h, fs)
	h.account(t, "acme", "paris@example.com", "pw", "")
	ctx := context.Background()
	if _, err := h.store.CreateMailbox(ctx, "acme", "paris@example.com", "Locked", "locked", nil, json.RawMessage(`{"append":"deny","create":"deny"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateMailbox(ctx, "acme", "paris@example.com", "Knowledge", "knowledge", nil, json.RawMessage(`{"append":"stack"}`)); err != nil {
		t.Fatal(err)
	}
	c := login(t, h, "paris@example.com", "pw")

	// deny: refused at the protocol layer, nothing dispatched.
	ac := c.Append("Locked", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	if _, err := ac.Wait(); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Errorf("deny append err = %v", err)
	}
	if err := c.Create("Locked/Sub", nil).Wait(); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Errorf("deny create err = %v", err)
	}
	if len(fs.seen) != 0 {
		t.Fatalf("deny must not dispatch: %v", fs.seen)
	}

	// stack: refused by the stack with its code + message.
	ac = c.Append("Knowledge", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	if _, err := ac.Wait(); err == nil || !strings.Contains(err.Error(), "not here") {
		t.Errorf("stack refuse err = %v", err)
	}
	seen := fs.wait(t, 1)
	env := seen[0]
	if gjson.Get(env, "_txc.src").String() != "imap" || gjson.Get(env, "_txc.imap.phase").String() != "answer" ||
		gjson.Get(env, "_txc.imap.op").String() != "append" || gjson.Get(env, "_txc.imap.mailbox.role").String() != "knowledge" ||
		gjson.Get(env, "_txc.imap.tenant").String() != "acme" || gjson.Get(env, "_txc.imap.msg.parts.0.name").String() != "guide.pdf" ||
		gjson.Get(env, "_txc.imap.msg.raw").Exists() || gjson.Get(env, "_txc.client.ip").String() == "" {
		t.Errorf("answer envelope = %s", env)
	}
	// stack: allowed, with the stack's flags + object_key on the row, then
	// observed after commit.
	allowed := strings.Replace(rawMsg, "Subject: dragged in", "Subject: allow this", 1)
	ac = c.Append("Knowledge", int64(len(allowed)), nil)
	_, _ = ac.Write([]byte(allowed))
	_ = ac.Close()
	ad, err := ac.Wait()
	if err != nil || ad.UID != 1 {
		t.Fatalf("stack allow = %+v err=%v", ad, err)
	}
	mb, _, _ := h.store.GetMailbox(ctx, "acme", "paris@example.com", "Knowledge")
	row, _, _ := h.store.GetMessage(ctx, mb.ID, 1)
	if row.ObjectKey != "docs/1" || !chimap.HasFlag(row.Flags, "$Ingested") {
		t.Errorf("row after stack ok = %+v", row)
	}
	seen = fs.wait(t, 3)
	if gjson.Get(seen[2], "_txc.imap.phase").String() != "observe" || gjson.Get(seen[2], "_txc.imap.uid").Int() != 1 ||
		gjson.Get(seen[2], "_txc.imap.objects.0.object_key").String() != "docs/1" {
		t.Errorf("observe envelope = %s", seen[2])
	}

	// observe (default): a CREATE and a RENAME are observed, with dest.
	if err := c.Create("Notes", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("Notes", "Journal", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	seen = fs.wait(t, 5)
	if gjson.Get(seen[3], "_txc.imap.op").String() != "create" || gjson.Get(seen[3], "_txc.imap.dest.name").String() != "Notes" ||
		gjson.Get(seen[4], "_txc.imap.op").String() != "rename" || gjson.Get(seen[4], "_txc.imap.dest.name").String() != "Journal" ||
		gjson.Get(seen[4], "_txc.imap.mailbox.name").String() != "Notes" {
		t.Errorf("observe create/rename = %s | %s", seen[3], seen[4])
	}
	// flags are local by default: no envelope for STORE on INBOX.
	h.appendHello(t, "acme", "paris@example.com", "k", "s", "t")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fs.wait(t, 5)); n != 5 {
		t.Errorf("STORE dispatched under local policy: %d envelopes", n)
	}
}

func TestAnswerLaneDeadlineAndAbsent(t *testing.T) {
	h := newHarness(t, config.Config{})
	fs := &fakeStack{delay: 2 * time.Second}
	withStack(t, h, fs)
	h.account(t, "acme", "paris@example.com", "pw", "")
	if _, err := h.store.CreateMailbox(context.Background(), "acme", "paris@example.com", "Slow", "", nil, json.RawMessage(`{"append":"stack"}`)); err != nil {
		t.Fatal(err)
	}
	c := login(t, h, "paris@example.com", "pw")
	ac := c.Append("Slow", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	start := time.Now()
	_, err := ac.Wait()
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Errorf("deadline err = %v", err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("client waited %v, deadline is 500ms", time.Since(start))
	}
	// A stack that answers without @imap.res is a refusal (default-deny).
	fs.mu.Lock()
	fs.delay = 0
	fs.mu.Unlock()
	ac = c.Append("Slow", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	if _, err := ac.Wait(); err == nil || !strings.Contains(err.Error(), "Refused by the mailbox") {
		t.Errorf("absent res err = %v", err)
	}
	// Unsubscribed tenant with a stack policy: NO [UNAVAILABLE].
	h.ctrl.lanes.subscribed = func(string) bool { return false }
	ac = c.Append("Slow", int64(len(rawMsg)), nil)
	_, _ = ac.Write([]byte(rawMsg))
	_ = ac.Close()
	if _, err := ac.Wait(); err == nil || !strings.Contains(err.Error(), "No _imap stack") {
		t.Errorf("unsubscribed err = %v", err)
	}
}

func TestTranslateAnswerAndEnvelope(t *testing.T) {
	a := translateAnswer(`{"_txc":{"imap":{"res":{"ok":true,"flags":[" $A ",""],"object_key":"k"}}}}`)
	if !a.ok || a.outcome != "ok" || len(a.flags) != 1 || a.flags[0] != "$A" || a.objectKey != "k" {
		t.Errorf("ok = %+v", a)
	}
	a = translateAnswer(`{"_txc":{"imap":{"res":{"ok":"true"}}}}`)
	if a.ok {
		t.Error("string \"true\" must not count as ok")
	}
	a = translateAnswer(`{"_txc":{"admission":{"denied":true,"reason":"suspended"}}}`)
	if a.ok || a.code != "unavailable" || a.outcome != "denied" {
		t.Errorf("denied = %+v", a)
	}
	a = translateAnswer(`{"_txc":{"imap":{"res":{"code":"limit"}}}}`)
	if a.ok || a.code != "limit" || a.outcome != "refused" || a.msg == "" {
		t.Errorf("refused = %+v", a)
	}
	env := buildEnvelope(mutation{tenant: "t", account: "a@b", op: "move", mailbox: mboxRef{ID: "m1", Name: "INBOX"},
		dest: &mboxRef{ID: "m2", Name: "Archive", Role: "archive"}, objects: []objectRef{{UID: 3, SHA256: "s"}}, clientIP: "10.0.0.1"},
		"observe", "rid1", "node", time.Unix(0, 0))
	for path, want := range map[string]string{
		"_txc.src": "imap", "_txc.imap.tenant": "t", "_txc.imap.account": "a@b", "_txc.imap.phase": "observe",
		"_txc.imap.op": "move", "_txc.imap.mailbox.id": "m1", "_txc.imap.dest.role": "archive", "_txc.client.ip": "10.0.0.1",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q (%s)", path, got, want, env)
		}
	}
	if gjson.Get(env, "_txc.imap.objects.0.uid").Int() != 3 || gjson.Get(env, "_txc.imap.objects.0.flags").Raw != "[]" {
		t.Errorf("objects = %s", gjson.Get(env, "_txc.imap.objects").Raw)
	}
}

func TestPolicyResolution(t *testing.T) {
	acct := &chimap.Account{Policy: json.RawMessage(`{"append":"stack","flags":"observe"}`)}
	mb := &chimap.Mailbox{Policy: json.RawMessage(`{"append":"local","bogus":"nope"}`)}
	if policyMode(mb, acct, verbAppend) != modeLocal || policyMode(nil, acct, verbAppend) != modeStack ||
		policyMode(mb, acct, verbFlags) != modeObserve || policyMode(nil, nil, verbFlags) != modeLocal ||
		policyMode(nil, nil, verbDelete) != modeObserve {
		t.Error("policy resolution order wrong")
	}
	if strictest(modeObserve, modeStack) != modeStack || strictest(modeDeny, modeStack) != modeDeny || strictest(modeLocal, modeObserve) != modeObserve {
		t.Error("strictest wrong")
	}
}

func TestTrustedProxy(t *testing.T) {
	h := newHarness(t, config.Config{IMAPProxyProtocol: []string{"10.0.0.0/8", "192.168.1.5", "bogus"}})
	for addr, want := range map[string]bool{"10.1.2.3:44": true, "192.168.1.5:1": true, "192.168.1.6:1": false, "127.0.0.1:9": false} {
		tcp, _ := net.ResolveTCPAddr("tcp", addr)
		if got := h.ctrl.trustedProxy(tcp); got != want {
			t.Errorf("trustedProxy(%s) = %v, want %v", addr, got, want)
		}
	}
}

func hasAttr(attrs []imap.MailboxAttr, a imap.MailboxAttr) bool {
	for _, x := range attrs {
		if x == a {
			return true
		}
	}
	return false
}
