package imap

import (
	"net"
	"reflect"
	"sort"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The parity test runs one client scenario against go-imap's reference
// in-memory backend and against this head, and compares what a client
// can observe: mailbox names, EXISTS counts, UIDs allocated, flags,
// sizes, envelope subjects, COPY/MOVE results, EXPUNGE outcomes, SEARCH
// hits. Where the two differ on purpose (this head's implicit \Noselect
// parents, its extra CHILDREN attrs) the comparison is normalised.

type observed struct {
	Mailboxes    []string
	ExistsAfter  uint32
	UIDs         []uint32
	Flags        map[uint32][]string
	Sizes        map[uint32]int64
	Subjects     map[uint32]string
	CopyDest     []uint32
	MoveDest     []uint32
	ExistsMoved  uint32
	ExpungedLeft uint32
	SearchHits   int
	ArchiveCount uint32
}

func startMemServer(t *testing.T) string {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("paris@example.com", "pw")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapNamespace: {}, imap.CapUIDPlus: {}, imap.CapMove: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func scenario(t *testing.T, addr string) observed {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Login("paris@example.com", "pw").Wait())
	must(c.Create("Archive", nil).Wait())
	must(c.Create("Projects/Alpha", nil).Wait())
	var o observed
	lst, err := c.List("", "*", nil).Collect()
	must(err)
	for _, l := range lst {
		for _, a := range l.Attrs {
			if a == imap.MailboxAttrNoSelect {
				goto skip
			}
		}
		o.Mailboxes = append(o.Mailboxes, l.Mailbox)
	skip:
	}
	sort.Strings(o.Mailboxes)

	msgs := []string{
		"From: a@example.com\r\nSubject: first\r\nDate: Thu, 03 Sep 2026 10:00:00 +0000\r\n\r\nbody one\r\n",
		"From: b@example.com\r\nSubject: second needle\r\nDate: Thu, 03 Sep 2026 11:00:00 +0000\r\n\r\nbody two\r\n",
		"From: c@example.com\r\nSubject: third\r\nDate: Thu, 03 Sep 2026 12:00:00 +0000\r\n\r\nbody three\r\n",
	}
	for i, m := range msgs {
		var flags []imap.Flag
		if i == 1 {
			flags = []imap.Flag{imap.FlagFlagged}
		}
		ac := c.Append("INBOX", int64(len(m)), &imap.AppendOptions{Flags: flags})
		_, _ = ac.Write([]byte(m))
		must(ac.Close())
		_, err := ac.Wait()
		must(err)
	}
	sel, err := c.Select("INBOX", nil).Wait()
	must(err)
	o.ExistsAfter = sel.NumMessages
	fetched, err := c.Fetch(imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, Envelope: true}).Collect()
	must(err)
	o.Flags, o.Sizes, o.Subjects = map[uint32][]string{}, map[uint32]int64{}, map[uint32]string{}
	for _, m := range fetched {
		o.UIDs = append(o.UIDs, uint32(m.UID))
		fl := make([]string, 0, len(m.Flags))
		for _, f := range m.Flags {
			fl = append(fl, string(f))
		}
		sort.Strings(fl)
		o.Flags[uint32(m.UID)] = fl
		o.Sizes[uint32(m.UID)] = m.RFC822Size
		o.Subjects[uint32(m.UID)] = m.Envelope.Subject
	}
	sd, err := c.Search(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "needle"}}}, nil).Wait()
	must(err)
	o.SearchHits = searchHits(sd)

	cd, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
	must(err)
	o.CopyDest = uidNums(cd.DestUIDs)
	md, err := c.Move(imap.UIDSetNum(2), "Archive").Wait()
	must(err)
	if set, ok := md.DestUIDs.(imap.UIDSet); ok {
		o.MoveDest = uidNums(set)
	}
	// Server truth via STATUS rather than the client's EXISTS counter: the
	// reference backend queues the MOVE's expunges for the moving session
	// too and re-sends them on the next NOOP (a double EXPUNGE), which
	// this head deliberately does not do.
	st0, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
	must(err)
	o.ExistsMoved = *st0.NumMessages

	_, err = c.Store(imap.UIDSetNum(3), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil).Collect()
	must(err)
	_, err = c.Expunge().Collect()
	must(err)
	st1, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
	must(err)
	o.ExpungedLeft = *st1.NumMessages
	st, err := c.Status("Archive", &imap.StatusOptions{NumMessages: true}).Wait()
	must(err)
	o.ArchiveCount = *st.NumMessages
	must(c.Logout().Wait())
	return o
}

func uidNums(set imap.UIDSet) []uint32 {
	n, _ := set.Nums()
	out := make([]uint32, 0, len(n))
	for _, u := range n {
		out = append(out, uint32(u))
	}
	return out
}

func TestParityWithIMAPMemServer(t *testing.T) {
	ref := scenario(t, startMemServer(t))

	h := newHarness(t, config.Config{})
	h.account(t, "acme", "paris@example.com", "pw", "")
	got := scenario(t, h.addr)

	if !reflect.DeepEqual(ref, got) {
		t.Errorf("parity broke:\n reference: %+v\n this head: %+v", ref, got)
	}
	// And the scenario actually exercised something.
	if got.ExistsAfter != 3 || got.ExistsMoved != 2 || got.ExpungedLeft != 1 || got.ArchiveCount != 2 || got.SearchHits != 1 {
		t.Errorf("scenario sanity: %+v", got)
	}
}
