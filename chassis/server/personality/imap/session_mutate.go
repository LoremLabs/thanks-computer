package imap

import (
	"errors"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"go.uber.org/zap"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// The mutating verbs. Every one runs the same shape:
//
//	resolve policy → deny? NO [NOPERM]
//	            → stack? ask the _imap stack first (answer lane); NO unless ok
//	commit to the store (the hub fans the change out to sessions)
//	            → observe? tell the _imap stack after (observe lane)
//
// Client-visible responses never wait on the observe lane; the answer
// lane is bounded by --imap-resp-timeout.

var _ imapserver.SessionMove = (*session)(nil)
var _ imapserver.SessionAppendLimit = (*session)(nil)

func noPerm(text string) error { return no(imap.ResponseCodeNoPerm, text) }

// refuse renders an answer-lane refusal as the client's NO.
func refuse(a answer) error {
	switch a.code {
	case "cannot":
		return no(imap.ResponseCodeCannot, a.msg)
	case "limit":
		return no(imap.ResponseCodeLimit, a.msg)
	case "unavailable":
		return no(imap.ResponseCodeUnavailable, a.msg)
	}
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: a.msg}
}

// gate resolves the mode and, for `stack`, asks. Returns the mode to act
// on after the ask (observe/local/stack-ok) or an error to answer.
func (s *session) gate(m mode, mut mutation) (mode, *answer, error) {
	switch m {
	case modeDeny:
		return m, nil, noPerm("Refused by mailbox policy")
	case modeStack:
		a := s.c.lanes.ask(mut)
		if !a.ok {
			return m, &a, refuse(a)
		}
		return m, &a, nil
	}
	return m, nil, nil
}

func (s *session) after(m mode, mut mutation) {
	if m == modeObserve || m == modeStack {
		s.c.lanes.observe(mut)
	}
}

func (s *session) base(op string, mb mboxRef) mutation {
	return mutation{tenant: s.acct.Tenant, account: s.acct.Username, op: op, mailbox: mb, clientIP: s.ip}
}

// AppendLimit caps a client APPEND literal at --imap-append-max-bytes.
func (s *session) AppendLimit() uint32 {
	if s.c.pu != nil && s.c.pu.Conf.IMAPAppendMaxBytes > 0 {
		return uint32(s.c.pu.Conf.IMAPAppendMaxBytes)
	}
	return 32 << 20
}

// ---- tree ------------------------------------------------------------------

// parentOf returns the nearest existing ancestor mailbox (nil for a
// top-level name) so CREATE resolves the `create` verb on it.
func (s *session) parentOf(name string) *chimap.Mailbox {
	for {
		i := strings.LastIndex(name, string(mailboxDelim))
		if i < 0 {
			return nil
		}
		name = name[:i]
		if mb, ok, err := s.c.store.GetMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, name); err == nil && ok {
			return &mb
		}
	}
}

func (s *session) Create(name string, options *imap.CreateOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	name = chimap.NormalizeMailboxName(name)
	if name == "" {
		return no(imap.ResponseCodeCannot, "Empty mailbox name")
	}
	if name == "INBOX" {
		return no(imap.ResponseCodeAlreadyExists, "Mailbox already exists")
	}
	parent := s.parentOf(name)
	var attrs []string
	if options != nil {
		for _, a := range options.SpecialUse {
			attrs = append(attrs, string(a))
		}
	}
	var pref mboxRef
	if parent != nil {
		pref = refOf(*parent)
	}
	mut := s.base("create", pref)
	mut.dest = &mboxRef{Name: name}
	m, _, err := s.gate(policyMode(parent, s.acct, verbCreate), mut)
	if err != nil {
		return err
	}
	mb, err := s.c.store.CreateMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, name, "", attrs, nil)
	if errors.Is(err, chimap.ErrMailboxExists) {
		return no(imap.ResponseCodeAlreadyExists, "Mailbox already exists")
	}
	if err != nil {
		return s.storeErr("create", err)
	}
	mut.dest = ptr(refOf(mb))
	s.after(m, mut)
	return nil
}

func (s *session) Delete(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return err
	}
	if mb.Name == "INBOX" {
		return no(imap.ResponseCodeCannot, "INBOX cannot be deleted")
	}
	mut := s.base("delete", refOf(mb))
	m, _, err := s.gate(policyMode(&mb, s.acct, verbDelete), mut)
	if err != nil {
		return err
	}
	if s.sel != nil && s.sel.mb.ID == mb.ID {
		_ = s.Unselect()
	}
	if _, err := s.c.store.DeleteMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, mb.Name); err != nil {
		return s.storeErr("delete", err)
	}
	s.after(m, mut)
	return nil
}

func (s *session) Rename(name, newName string, _ *imap.RenameOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return err
	}
	newName = chimap.NormalizeMailboxName(newName)
	if mb.Name == "INBOX" || newName == "INBOX" {
		return no(imap.ResponseCodeCannot, "INBOX cannot be renamed")
	}
	mut := s.base("rename", refOf(mb))
	mut.dest = &mboxRef{ID: mb.ID, Name: newName, Role: mb.Role}
	m, _, err := s.gate(policyMode(&mb, s.acct, verbRename), mut)
	if err != nil {
		return err
	}
	if _, err := s.c.store.RenameMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, mb.Name, newName); err != nil {
		switch {
		case errors.Is(err, chimap.ErrMailboxExists):
			return no(imap.ResponseCodeAlreadyExists, "Mailbox already exists")
		case errors.Is(err, chimap.ErrMailboxNotFound):
			return no(imap.ResponseCodeNonExistent, "No such mailbox")
		}
		return s.storeErr("rename", err)
	}
	s.after(m, mut)
	return nil
}

// ---- messages ----------------------------------------------------------------

// Append stores the client's literal verbatim (§25.1: the user expects
// back exactly what they put in), parts and all, then the row.
func (s *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mb, ok, err := s.c.store.GetMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, name)
	if err != nil {
		return nil, no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	if !ok {
		_, _ = io.Copy(io.Discard, r)
		return nil, no(imap.ResponseCodeTryCreate, "No such mailbox")
	}
	m := policyMode(&mb, s.acct, verbAppend)
	if m == modeDeny {
		_, _ = io.Copy(io.Discard, r)
		return nil, noPerm("Refused by mailbox policy")
	}
	limit := int64(s.AppendLimit())
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, no(imap.ResponseCodeUnavailable, "Read failed")
	}
	if int64(len(raw)) > limit {
		return nil, no(imap.ResponseCodeTooBig, "Message exceeds the append limit")
	}
	var flags []string
	var when time.Time
	if options != nil {
		flags = fromFlags(options.Flags)
		when = options.Time
	}
	v, err := ParseVerbatim(raw, "", when, flags)
	if err != nil {
		return nil, no(imap.ResponseCodeParse, "Message could not be parsed")
	}
	// Bytes go to the CAS BEFORE any dispatch: the envelope carries
	// references, never the message.
	if err := StoreVerbatim(s.ctx(), s.c.fcas, s.c.ix, s.acct.Tenant, raw, v, time.Now()); err != nil {
		return nil, s.storeErr("append", err)
	}
	mut := s.base("append", refOf(mb))
	mut.msg = &v.Facts
	m, a, err := s.gate(m, mut)
	if err != nil {
		return nil, err
	}
	if a != nil {
		if a.objectKey != "" {
			v.Message.ObjectKey = a.objectKey
		}
		if len(a.flags) > 0 {
			v.Message.Flags = chimap.NormalizeFlags(append(v.Message.Flags, a.flags...))
		}
	}
	res, err := s.c.store.AppendMessage(chimap.WithOrigin(s.ctx(), s.selTracker()), mb.ID, v.Message)
	if err != nil {
		return nil, s.storeErr("append", err)
	}
	mut.uid = res.UID
	mut.objects = []objectRef{{UID: res.UID, ObjectKey: v.Message.ObjectKey, SHA256: v.SHA256, Flags: v.Message.Flags}}
	s.after(m, mut)
	return &imap.AppendData{UID: imap.UID(res.UID), UIDValidity: res.UIDValidity}, nil
}

func (s *session) selTracker() *imapserver.SessionTracker {
	if s.sel == nil {
		return nil
	}
	return s.sel.tracker
}

// objectsFor renders the affected rows for the envelope.
func (s *session) objectsFor(seqs []uint32) []objectRef {
	out := make([]objectRef, 0, len(seqs))
	for _, seq := range seqs {
		h := s.sel.heads[seq-1]
		m, ok, err := s.c.store.GetMessage(s.ctx(), s.sel.mb.ID, h.UID)
		if err != nil || !ok {
			continue
		}
		out = append(out, objectRef{UID: m.UID, ObjectKey: m.ObjectKey, SHA256: m.SHA256, Flags: m.Flags})
	}
	return out
}

func (s *session) Copy(numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	if err := s.requireSelected(); err != nil {
		return nil, err
	}
	dest, ok, err := s.c.store.GetMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, destName)
	if err != nil {
		return nil, no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	if !ok {
		return nil, no(imap.ResponseCodeTryCreate, "No such mailbox")
	}
	seqs := s.resolve(numSet)
	mut := s.base("copy", refOf(s.sel.mb))
	mut.dest = ptr(refOf(dest))
	mut.objects = s.objectsFor(seqs)
	m, _, err := s.gate(policyMode(&dest, s.acct, verbMoveIn), mut)
	if err != nil {
		return nil, err
	}
	data := &imap.CopyData{UIDValidity: dest.UIDValidity}
	for _, seq := range seqs {
		uid := s.sel.heads[seq-1].UID
		res, err := s.c.store.CopyMessage(s.ctx(), s.sel.mb.ID, uid, dest.ID)
		if err != nil {
			return nil, s.storeErr("copy", err)
		}
		data.SourceUIDs.AddNum(imap.UID(uid))
		data.DestUIDs.AddNum(imap.UID(res.UID))
	}
	s.after(m, mut)
	return data, nil
}

func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
	if err := s.requireSelected(); err != nil {
		return err
	}
	dest, ok, err := s.c.store.GetMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, destName)
	if err != nil {
		return no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	if !ok {
		return no(imap.ResponseCodeTryCreate, "No such mailbox")
	}
	if dest.ID == s.sel.mb.ID {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Source and destination mailboxes are identical"}
	}
	seqs := s.resolve(numSet)
	mut := s.base("move", refOf(s.sel.mb))
	mut.dest = ptr(refOf(dest))
	mut.objects = s.objectsFor(seqs)
	m, _, err := s.gate(strictest(policyMode(&s.sel.mb, s.acct, verbMoveOut), policyMode(&dest, s.acct, verbMoveIn)), mut)
	if err != nil {
		return err
	}
	uids := make([]uint32, 0, len(seqs))
	for _, seq := range seqs {
		uids = append(uids, s.sel.heads[seq-1].UID)
	}
	moved, _, err := s.c.store.MoveMessages(chimap.WithOrigin(s.ctx(), s.sel.tracker), s.sel.mb.ID, uids, dest.ID)
	if err != nil {
		return s.storeErr("move", err)
	}
	data := &imap.CopyData{UIDValidity: dest.UIDValidity}
	for _, uid := range uids {
		if d, ok := moved[uid]; ok {
			data.SourceUIDs.AddNum(imap.UID(uid))
			data.DestUIDs.AddNum(imap.UID(d))
		}
	}
	// The committed expunges reach this session through the hub like any
	// other change: go-imap polls after the handler (before the tagged
	// OK), so the `* n EXPUNGE` lines follow COPYUID in RFC 6851 order.
	if err := w.WriteCopyData(data); err != nil {
		return err
	}
	s.after(m, mut)
	return nil
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if err := s.requireSelected(); err != nil {
		return err
	}
	var only []uint32
	var seqs []uint32
	if uids != nil {
		for i, h := range s.sel.heads {
			if uids.Contains(imap.UID(h.UID)) {
				only = append(only, h.UID)
				seqs = append(seqs, uint32(i)+1)
			}
		}
		if len(only) == 0 {
			return nil
		}
	} else {
		for i := range s.sel.heads {
			seqs = append(seqs, uint32(i)+1)
		}
	}
	// Only \Deleted rows are candidates; tell the stack about those.
	var doomed []uint32
	for _, seq := range seqs {
		if chimap.HasFlag(s.sel.heads[seq-1].Flags, string(imap.FlagDeleted)) {
			doomed = append(doomed, seq)
		}
	}
	if len(doomed) == 0 {
		return nil
	}
	mut := s.base("expunge", refOf(s.sel.mb))
	mut.objects = s.objectsFor(doomed)
	m, _, err := s.gate(policyMode(&s.sel.mb, s.acct, verbDelete), mut)
	if err != nil {
		return err
	}
	// EXPUNGE responses are written by go-imap's post-command poll from
	// the hub's queue (descending sequence), never here.
	if _, err := s.c.store.Expunge(chimap.WithOrigin(s.ctx(), s.sel.tracker), s.sel.mb.ID, only); err != nil {
		return s.storeErr("expunge", err)
	}
	s.after(m, mut)
	return nil
}

// flagsGate is Store's policy hook: `flags` is local by default; a
// mailbox that opts into observe/stack hears keyword changes too.
func (s *session) flagsGate(seqs []uint32, next map[uint32][]string) (mode, mutation, error) {
	mut := s.base("flags", refOf(s.sel.mb))
	for _, seq := range seqs {
		h := s.sel.heads[seq-1]
		m, ok, err := s.c.store.GetMessage(s.ctx(), s.sel.mb.ID, h.UID)
		if err != nil || !ok {
			continue
		}
		mut.objects = append(mut.objects, objectRef{UID: m.UID, ObjectKey: m.ObjectKey, SHA256: m.SHA256, Flags: chimap.NormalizeFlags(next[h.UID])})
	}
	m, _, err := s.gate(policyMode(&s.sel.mb, s.acct, verbFlags), mut)
	return m, mut, err
}

func (s *session) storeErr(op string, err error) error {
	if s.c.pu != nil && s.c.pu.Logger != nil {
		s.c.pu.Logger.Warn("imap "+op+" failed", zap.String("account", s.acct.Username), zap.String("err", err.Error()))
	}
	return no(imap.ResponseCodeUnavailable, "Temporary failure")
}

func ptr[T any](v T) *T { return &v }
