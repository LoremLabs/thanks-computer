package imap

import (
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// hub is the head-local change fan-out: one imapserver.MailboxTracker per
// mailbox that has been SELECTed on this node. The store's post-commit
// Change (an op's append, a session's STORE) is turned into the queued
// EXISTS / EXPUNGE / FETCH updates that Poll and IDLE drain. Ops and the
// head share a process, so no bus is involved (§25.7).
type hub struct {
	mu       sync.Mutex
	trackers map[string]*imapserver.MailboxTracker
}

func newHub() *hub { return &hub{trackers: make(map[string]*imapserver.MailboxTracker)} }

// tracker returns the mailbox's tracker, creating it with numMessages when
// this is the first session to select the mailbox on this node.
func (h *hub) tracker(mailboxID string, numMessages uint32) *imapserver.MailboxTracker {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.trackers[mailboxID]; ok {
		return t
	}
	t := imapserver.NewMailboxTracker(numMessages)
	h.trackers[mailboxID] = t
	return t
}

func (h *hub) get(mailboxID string) *imapserver.MailboxTracker {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trackers[mailboxID]
}

// onChange is the store listener. A mailbox nobody has selected has no
// tracker and the change is simply dropped — the next SELECT reads the
// store.
func (h *hub) onChange(c chimap.Change) {
	t := h.get(c.MailboxID)
	if t == nil {
		return
	}
	switch c.Kind {
	case chimap.ChangeExpunge:
		if c.Seq > 0 {
			t.QueueExpunge(c.Seq)
		}
	case chimap.ChangeAppend:
		t.QueueNumMessages(c.Total)
	case chimap.ChangeFlags:
		src, _ := c.Origin.(*imapserver.SessionTracker)
		t.QueueMessageFlags(c.Seq, imap.UID(c.UID), toFlags(c.Flags), src)
	}
}

func toFlags(in []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(in))
	for _, f := range in {
		out = append(out, imap.Flag(f))
	}
	return out
}

func fromFlags(in []imap.Flag) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, string(f))
	}
	return out
}
