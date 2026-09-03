package imap

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"go.uber.org/zap"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// The hub is the head-local change fan-out: one snapshot + one
// go-imap MailboxTracker per mailbox that has a SELECTed session on this
// node, and ONE way to move either — sync, which reads the index and diffs
// it against the snapshot. Local commits (an op's append, a session's
// STORE) and remote commits (an op on another node against a shared
// index) go through the same path: a local Change pokes the mailbox's
// worker; a ticker pokes every open mailbox; and every selected-state
// command syncs synchronously before it resolves sequence numbers. No bus
// (§25.7): with a shared index, cross-node changes surface on the next
// command and, in IDLE, within --imap-sync-interval.
//
// Invariants (a violation panics inside go-imap's tracker, on whichever
// goroutine queued the update — which for an op is a processor run, so
// these are load-bearing):
//
//	I1 tracker.numMessages == len(st.heads) whenever st.mu is not held —
//	   only apply, under st.mu, moves either; sessions never Queue*.
//	I2 st.heads is UID-ascending and a new UID only ever appears above the
//	   highest one held (uidnext is stored and monotonic under one
//	   uidvalidity). ResetMailbox is caught by the uidvalidity check; an
//	   index edited by hand is caught by diff (a non-tail UID ⇒ gone).
//	I3 queue order per sync: expunges by DESCENDING former sequence, then
//	   one EXISTS per appended row, then FETCH FLAGS in the new numbering.
//	I4 never QueueExpunge(0), never QueueNumMessages(0), never a count
//	   below the tracker's.
//	I5 the mailbox modseq is read BEFORE the heads and recorded as the
//	   snapshot's modseq: a commit between the two reads makes the next
//	   sync re-diff (a no-op), never miss a change.
//	I6 no store write is ever issued with st.mu held.
//	I7 a tracker is never rewound; a uidvalidity change or a deleted
//	   mailbox marks the state gone and sessions are told to reselect.

var errMailboxGone = errors.New("imap: mailbox reset or deleted")

type updateKind uint8

const (
	upExpunge updateKind = iota
	upExists
	upFlags
)

// update is one tracker mutation. Produced only by diff, consumed only by
// apply.
type update struct {
	kind  updateKind
	seq   uint32 // upExpunge: former seq (pre-change numbering); upFlags: seq in the new numbering
	n     uint32 // upExists: the new count (never 0)
	uid   uint32
	flags []string
}

// mailboxState is this node's snapshot of one SELECTed mailbox.
type mailboxState struct {
	id string

	mu          sync.Mutex // serialises sync; I1 holds whenever it is free
	tracker     *imapserver.MailboxTracker
	mb          chimap.Mailbox
	uidvalidity uint32
	modseq      int64 // the mailbox modseq the snapshot reflects
	heads       []chimap.MessageHead
	gone        bool
	goneCh      chan struct{} // closed once, on reset/delete; Idle selects on it

	sm       sync.Mutex
	suppress map[uint32]*imapserver.SessionTracker // uid → origin of a pending local flag write

	sessions int           // guarded by hub.mu
	kick     chan struct{} // cap 1; closed by hub.close on the last session
}

// hub owns the states and the ticker.
type hub struct {
	store *chimap.Store
	log   *zap.Logger
	ctx   context.Context

	mu    sync.Mutex
	boxes map[string]*mailboxState

	stop context.CancelFunc
	wg   sync.WaitGroup
}

func newHub(ctx context.Context, store *chimap.Store, log *zap.Logger) *hub {
	if log == nil {
		log = zap.NewNop()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &hub{store: store, log: log, ctx: ctx, boxes: make(map[string]*mailboxState)}
}

// open registers a session on the mailbox. The first SELECT on this node
// builds the snapshot and a tracker whose count equals len(heads); later
// SELECTs sync first, so the returned view always equals the tracker's
// count. Callers pass the returned state + tracker to close exactly once.
func (h *hub) open(ctx context.Context, mailboxID string) (*mailboxState, *imapserver.SessionTracker, chimap.Mailbox, []chimap.MessageHead, error) {
	h.mu.Lock()
	st := h.boxes[mailboxID]
	fresh := st == nil
	if fresh {
		st = &mailboxState{
			id:       mailboxID,
			goneCh:   make(chan struct{}),
			suppress: make(map[uint32]*imapserver.SessionTracker),
			kick:     make(chan struct{}, 1),
		}
		h.boxes[mailboxID] = st
	}
	st.sessions++
	h.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if fresh {
		if err := h.initLocked(ctx, st); err != nil {
			h.markGoneLocked(st)
			h.close(st, nil)
			return nil, nil, chimap.Mailbox{}, nil, err
		}
		h.wg.Add(1)
		go st.loop(h)
	} else {
		if _, _, err := h.syncLocked(ctx, st); err != nil {
			h.close(st, nil)
			return nil, nil, chimap.Mailbox{}, nil, err
		}
	}
	tr := st.tracker.NewSession()
	return st, tr, st.mb, copyHeads(st.heads), nil
}

// initLocked takes the first snapshot (I5: mailbox before heads).
func (h *hub) initLocked(ctx context.Context, st *mailboxState) error {
	mb, ok, err := h.store.GetMailboxByID(ctx, st.id)
	if err != nil {
		return err
	}
	if !ok {
		return errMailboxGone
	}
	heads, err := h.store.ListMessageHeads(ctx, st.id)
	if err != nil {
		return err
	}
	st.mb, st.uidvalidity, st.modseq, st.heads = mb, mb.UIDValidity, mb.ModSeq, heads
	st.tracker = imapserver.NewMailboxTracker(uint32(len(heads)))
	return nil
}

// close unregisters a session; the last one drops the state and stops
// its worker. tr may be nil (an open that failed).
func (h *hub) close(st *mailboxState, tr *imapserver.SessionTracker) {
	if tr != nil {
		tr.Close()
	}
	h.mu.Lock()
	st.sessions--
	if st.sessions <= 0 {
		if h.boxes[st.id] == st {
			delete(h.boxes, st.id)
		}
		if st.kick != nil {
			close(st.kick) // no poke can follow: pokes only reach states in the map, under h.mu
			st.kick = nil
		}
	}
	h.mu.Unlock()
}

// sync brings the snapshot + tracker up to the index and returns a copy of
// the heads (the session's own view).
func (h *hub) sync(ctx context.Context, st *mailboxState) (chimap.Mailbox, []chimap.MessageHead, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	mb, heads, err := h.syncLocked(ctx, st)
	return mb, copyHeads(heads), err
}

func (h *hub) syncLocked(ctx context.Context, st *mailboxState) (chimap.Mailbox, []chimap.MessageHead, error) {
	if st.gone {
		return st.mb, st.heads, errMailboxGone
	}
	if st.tracker == nil {
		return st.mb, st.heads, errors.New("imap: mailbox state not initialised")
	}
	mb, ok, err := h.store.GetMailboxByID(ctx, st.id) // modseq BEFORE heads (I5)
	if err != nil {
		h.log.Warn("imap: mailbox sync failed; keeping the last snapshot", zap.String("mailbox", st.id), zap.String("err", err.Error()))
		return st.mb, st.heads, err
	}
	if !ok || mb.UIDValidity != st.uidvalidity {
		h.markGoneLocked(st)
		return st.mb, st.heads, errMailboxGone
	}
	if mb.ModSeq == st.modseq {
		st.mb = mb
		return mb, st.heads, nil
	}
	heads, err := h.store.ListMessageHeads(ctx, st.id)
	if err != nil {
		return st.mb, st.heads, err
	}
	ups, ok := diff(st.heads, heads)
	if !ok {
		h.log.Warn("imap: index changed under the snapshot in a non-monotonic way; sessions must reselect", zap.String("mailbox", st.id))
		h.markGoneLocked(st)
		return st.mb, st.heads, errMailboxGone
	}
	st.apply(ups)
	st.heads, st.modseq, st.mb = heads, mb.ModSeq, mb
	return mb, heads, nil
}

// markGoneLocked flips the state (st.mu held) and drops it from the map so
// a new SELECT builds a fresh one.
func (h *hub) markGoneLocked(st *mailboxState) {
	if !st.gone {
		st.gone = true
		close(st.goneCh)
	}
	h.mu.Lock()
	if h.boxes[st.id] == st {
		delete(h.boxes, st.id)
	}
	h.mu.Unlock()
}

// onChange is the store listener: record the writer's origin for a flag
// change (so it is not echoed to that session) and poke the worker.
// Runs on the writer's goroutine; never blocks.
func (h *hub) onChange(c chimap.Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.boxes[c.MailboxID]
	if st == nil {
		return
	}
	if c.Kind == chimap.ChangeFlags {
		if src, ok := c.Origin.(*imapserver.SessionTracker); ok && src != nil {
			st.sm.Lock()
			st.suppress[c.UID] = src
			st.sm.Unlock()
		}
	}
	st.poke()
}

// poke wakes the worker; a pending kick coalesces bursts. Callers hold
// hub.mu (the state is in the map, so kick is open).
func (st *mailboxState) poke() {
	if st.kick == nil {
		return
	}
	select {
	case st.kick <- struct{}{}:
	default:
	}
}

// loop is the per-mailbox worker: sync on every kick until closed.
func (st *mailboxState) loop(h *hub) {
	defer h.wg.Done()
	for range st.kick {
		st.mu.Lock()
		_, _, _ = h.syncLocked(h.ctx, st)
		st.mu.Unlock()
	}
}

// start runs the ticker that pokes every open mailbox each interval — the
// only path by which a change made on ANOTHER node reaches an IDLE
// session. every <= 0 disables it.
func (h *hub) start(every time.Duration) {
	if every <= 0 {
		return
	}
	if every < time.Second {
		every = time.Second
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.stop = cancel
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.mu.Lock()
				for _, st := range h.boxes {
					st.poke()
				}
				h.mu.Unlock()
			}
		}
	}()
}

func (h *hub) stopTicker() {
	if h.stop != nil {
		h.stop()
	}
}

func (st *mailboxState) takeSuppress(uid uint32) *imapserver.SessionTracker {
	st.sm.Lock()
	defer st.sm.Unlock()
	src := st.suppress[uid]
	delete(st.suppress, uid)
	return src
}

// apply queues the updates on the tracker (st.mu held). The only caller of
// Queue* in the package.
func (st *mailboxState) apply(ups []update) {
	for _, u := range ups {
		switch u.kind {
		case upExpunge:
			st.tracker.QueueExpunge(u.seq)
		case upExists:
			st.tracker.QueueNumMessages(u.n)
		case upFlags:
			st.tracker.QueueMessageFlags(u.seq, imap.UID(u.uid), toFlags(u.flags), st.takeSuppress(u.uid))
		}
	}
}

// diff computes the tracker updates that take the client view from old
// to new (both UID-ascending). ok is false when a new UID sits below a
// surviving old one (I2 broken — not a tail append), which the caller
// treats as a reset.
func diff(old, new []chimap.MessageHead) ([]update, bool) {
	var expunged []uint32 // former seqs, ascending
	var flags []update
	i, j := 0, 0
	for i < len(old) || j < len(new) {
		switch {
		case j == len(new) || (i < len(old) && old[i].UID < new[j].UID):
			expunged = append(expunged, uint32(i)+1)
			i++
		case i == len(old) || new[j].UID < old[i].UID:
			if i < len(old) {
				return nil, false
			}
			j++ // tail append, counted below
		default:
			if new[j].ModSeq != old[i].ModSeq {
				flags = append(flags, update{kind: upFlags, seq: uint32(j) + 1, uid: new[j].UID, flags: new[j].Flags})
			}
			i++
			j++
		}
	}
	ups := make([]update, 0, len(expunged)+len(new)+len(flags))
	for k := len(expunged) - 1; k >= 0; k-- {
		ups = append(ups, update{kind: upExpunge, seq: expunged[k]})
	}
	for n := len(old) - len(expunged) + 1; n <= len(new); n++ {
		ups = append(ups, update{kind: upExists, n: uint32(n)})
	}
	ups = append(ups, flags...)
	return ups, true
}

func copyHeads(in []chimap.MessageHead) []chimap.MessageHead {
	out := make([]chimap.MessageHead, len(in))
	copy(out, in)
	return out
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
