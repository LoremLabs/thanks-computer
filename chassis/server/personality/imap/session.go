package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"go.uber.org/zap"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

const mailboxDelim = '/'

// session is one client connection's view. It implements
// imapserver.Session (+ SessionNamespace). One goroutine per connection
// drives it, so its fields need no lock; shared state lives in the
// Controller (throttles, cache, hub) and the store.
type session struct {
	c    *Controller
	conn *imapserver.Conn
	ip   string

	acct   *chimap.Account
	domain string
	slot   bool // holds a per-account connection slot

	sel *selected
}

// selected is the SELECTed mailbox: the UID-ordered heads (sequence number
// = index+1 in the server's view) and this session's tracker.
type selected struct {
	mb      chimap.Mailbox
	heads   []chimap.MessageHead
	mt      *imapserver.MailboxTracker
	tracker *imapserver.SessionTracker
}

var _ imapserver.SessionNamespace = (*session)(nil)

func newSession(c *Controller, conn *imapserver.Conn) *session {
	s := &session{c: c, conn: conn}
	if conn != nil && conn.NetConn() != nil {
		if host, _, err := net.SplitHostPort(conn.NetConn().RemoteAddr().String()); err == nil {
			s.ip = host
		} else {
			s.ip = conn.NetConn().RemoteAddr().String()
		}
	}
	return s
}

func (s *session) ctx() context.Context {
	if s.c.ctx != nil {
		return s.c.ctx
	}
	return context.Background()
}

func no(code imap.ResponseCode, text string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: code, Text: text}
}

func cannot(text string) error { return no(imap.ResponseCodeCannot, text) }

func (s *session) Close() error {
	if s.sel != nil {
		s.sel.tracker.Close()
		s.sel = nil
	}
	if s.slot && s.acct != nil {
		s.c.conns.release(s.acct.Username)
		s.slot = false
	}
	return nil
}

// ---- authentication ------------------------------------------------------

// Login verifies the credentials against imap_accounts. Order matters:
// throttles first (no password work for a flood), then the account lookup
// with a dummy verify on a miss (no existence oracle), the verified-login
// cache, argon2id, the tenant admission check, and the per-account
// connection cap.
func (s *session) Login(username, password string) error {
	username = chimap.NormalizeUsername(username)
	note := func(outcome string) { s.c.noteLogin(outcome, username, s.ip, s.isTLS()) }
	if s.c.loginIP != nil && s.ip != "" {
		if ok, _ := s.c.loginIP.Allow(s.ip); !ok {
			note("throttled")
			return no(imap.ResponseCodeLimit, "Too many login attempts")
		}
	}
	if s.c.loginAcct != nil && username != "" {
		if ok, _ := s.c.loginAcct.Allow(username); !ok {
			note("throttled")
			return no(imap.ResponseCodeLimit, "Too many login attempts")
		}
	}
	var acct chimap.Account
	var ok bool
	var err error
	if strings.Contains(username, "@") {
		acct, ok, err = s.c.store.GetAccount(s.ctx(), username)
	} else {
		// Mail clients routinely send just the local part when the
		// address domain and the server name line up. Accept it when it
		// names exactly one account; anything else is a plain failure.
		var n int
		acct, n, err = s.c.store.GetAccountByLocalPart(s.ctx(), username)
		ok = n == 1
		if ok {
			s.c.pu.Logger.Debug("imap login: bare local part resolved", zap.String("given", username), zap.String("user", acct.Username))
			username = acct.Username
		}
	}
	if err != nil {
		s.c.pu.Logger.Warn("imap login lookup failed", zap.String("user", username), zap.String("err", err.Error()))
		note("error")
		return no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	if !ok {
		chimap.VerifyDummy(password)
		note("failed")
		return imapserver.ErrAuthFailed
	}
	key := loginKey(acct.Username, acct.PwHash, password)
	if !s.c.cache.hit(key) {
		match, verr := chimap.VerifyPassword(acct.PwHash, password)
		if verr != nil {
			s.c.pu.Logger.Warn("imap account has an unreadable password hash", zap.String("user", username), zap.String("err", verr.Error()))
			note("error")
			return imapserver.ErrAuthFailed
		}
		if !match {
			note("failed")
			return imapserver.ErrAuthFailed
		}
		s.c.cache.put(key)
	}
	if acct.Status != chimap.StatusActive {
		note("disabled")
		return no(imap.ResponseCodeAuthorizationFailed, "Account disabled")
	}
	if s.c.pu.Admission != nil {
		if d := s.c.pu.Admission.Decide(acct.Tenant); !d.Admit {
			note("denied")
			return no(imap.ResponseCodeUnavailable, "Service unavailable for this account")
		}
	}
	if s.c.conns != nil && !s.c.conns.acquire(acct.Username) {
		note("too_many_conns")
		return no(imap.ResponseCodeLimit, "Too many connections for this account")
	}
	s.slot = true
	s.acct = &acct
	s.domain = domainOf(acct.Username)
	note("ok")
	return nil
}

// isTLS reports whether the connection is (now) TLS — implicit or after
// STARTTLS (go-imap swaps the underlying conn on upgrade).
func (s *session) isTLS() bool {
	if s.conn == nil || s.conn.NetConn() == nil {
		return false
	}
	_, ok := s.conn.NetConn().(*tls.Conn)
	return ok
}

func (s *session) requireAuth() error {
	if s.acct == nil {
		return no(imap.ResponseCodeAuthenticationFailed, "Not authenticated")
	}
	return nil
}

// ---- authenticated state -------------------------------------------------

func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Delim: mailboxDelim}}}, nil
}

func (s *session) mailboxes() ([]chimap.Mailbox, error) {
	return s.c.store.ListMailboxes(s.ctx(), s.acct.Tenant, s.acct.Username)
}

func (s *session) mailbox(name string) (chimap.Mailbox, error) {
	mb, ok, err := s.c.store.GetMailbox(s.ctx(), s.acct.Tenant, s.acct.Username, name)
	if err != nil {
		return chimap.Mailbox{}, no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	if !ok {
		return chimap.Mailbox{}, no(imap.ResponseCodeNonExistent, "No such mailbox")
	}
	return mb, nil
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: mailboxDelim})
	}
	all, err := s.mailboxes()
	if err != nil {
		return no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	names := make(map[string]bool, len(all))
	for _, mb := range all {
		names[mb.Name] = true
	}
	// Implicit parents: an ancestor nobody created LISTs as \Noselect
	// (RFC 3501 §6.3.3) so a client can show the tree.
	var implicit []string
	for _, mb := range all {
		parts := strings.Split(mb.Name, string(mailboxDelim))
		for i := 1; i < len(parts); i++ {
			p := strings.Join(parts[:i], string(mailboxDelim))
			if !names[p] {
				names[p] = false
				implicit = append(implicit, p)
			}
		}
	}
	var out []imap.ListData
	for _, p := range implicit {
		if options.SelectSubscribed || options.SelectSpecialUse {
			continue
		}
		match := false
		for _, pat := range patterns {
			if imapserver.MatchList(p, mailboxDelim, ref, pat) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		out = append(out, imap.ListData{Mailbox: p, Delim: mailboxDelim,
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect, imap.MailboxAttrHasChildren}})
	}
	for _, mb := range all {
		match := false
		for _, p := range patterns {
			if imapserver.MatchList(mb.Name, mailboxDelim, ref, p) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if options.SelectSubscribed && !mb.Subscribed {
			continue
		}
		if options.SelectSpecialUse && len(mb.Attrs) == 0 {
			continue
		}
		d := imap.ListData{Mailbox: mb.Name, Delim: mailboxDelim}
		if hasChild(names, mb.Name) {
			d.Attrs = append(d.Attrs, imap.MailboxAttrHasChildren)
		} else {
			d.Attrs = append(d.Attrs, imap.MailboxAttrHasNoChildren)
		}
		if mb.Subscribed && (options.ReturnSubscribed || options.SelectSubscribed) {
			d.Attrs = append(d.Attrs, imap.MailboxAttrSubscribed)
		}
		if options.ReturnSpecialUse || options.SelectSpecialUse {
			for _, a := range mb.Attrs {
				d.Attrs = append(d.Attrs, imap.MailboxAttr(a))
			}
		}
		if options.ReturnStatus != nil {
			st, err := s.status(mb, options.ReturnStatus)
			if err != nil {
				return err
			}
			d.Status = st
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mailbox < out[j].Mailbox })
	for i := range out {
		if err := w.WriteList(&out[i]); err != nil {
			return err
		}
	}
	return nil
}

func hasChild(names map[string]bool, name string) bool {
	prefix := name + string(mailboxDelim)
	for n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func (s *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return nil, err
	}
	return s.status(mb, options)
}

func (s *session) status(mb chimap.Mailbox, options *imap.StatusOptions) (*imap.StatusData, error) {
	heads, err := s.c.store.ListMessageHeads(s.ctx(), mb.ID)
	if err != nil {
		return nil, no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	// uidnext is read fresh: the cached row may predate appends.
	if cur, ok, err := s.c.store.GetMailboxByID(s.ctx(), mb.ID); err == nil && ok {
		mb = cur
	}
	d := &imap.StatusData{Mailbox: mb.Name}
	if options.NumMessages {
		n := uint32(len(heads))
		d.NumMessages = &n
	}
	if options.UIDNext {
		d.UIDNext = imap.UID(mb.UIDNext)
	}
	if options.UIDValidity {
		d.UIDValidity = mb.UIDValidity
	}
	if options.NumUnseen {
		var n uint32
		for _, h := range heads {
			if !chimap.HasFlag(h.Flags, string(imap.FlagSeen)) {
				n++
			}
		}
		d.NumUnseen = &n
	}
	if options.NumDeleted {
		var n uint32
		for _, h := range heads {
			if chimap.HasFlag(h.Flags, string(imap.FlagDeleted)) {
				n++
			}
		}
		d.NumDeleted = &n
	}
	if options.Size {
		var sz int64
		for _, h := range heads {
			sz += h.Size
		}
		d.Size = &sz
	}
	if options.NumRecent {
		n := uint32(0)
		d.NumRecent = &n
	}
	if options.AppendLimit {
		lim := uint32(0)
		d.AppendLimit = &lim
	}
	return d, nil
}

func (s *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return nil, err
	}
	if s.sel != nil {
		s.sel.tracker.Close()
		s.sel = nil
	}
	heads, err := s.c.store.ListMessageHeads(s.ctx(), mb.ID)
	if err != nil {
		return nil, no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	mt := s.c.hub.tracker(mb.ID, uint32(len(heads)))
	s.sel = &selected{mb: mb, heads: heads, mt: mt, tracker: mt.NewSession()}

	flags := s.sel.flagSet()
	perm := append(append([]imap.Flag{}, flags...), imap.FlagWildcard)
	var firstUnseen uint32
	for i, h := range heads {
		if !chimap.HasFlag(h.Flags, string(imap.FlagSeen)) {
			firstUnseen = uint32(i) + 1
			break
		}
	}
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    perm,
		NumMessages:       uint32(len(heads)),
		FirstUnseenSeqNum: firstUnseen,
		UIDNext:           imap.UID(mb.UIDNext),
		UIDValidity:       mb.UIDValidity,
	}, nil
}

// flagSet is the union of system flags and every keyword in use, sorted.
func (sel *selected) flagSet() []imap.Flag {
	set := map[imap.Flag]struct{}{
		imap.FlagSeen: {}, imap.FlagAnswered: {}, imap.FlagFlagged: {}, imap.FlagDeleted: {}, imap.FlagDraft: {},
	}
	for _, h := range sel.heads {
		for _, f := range h.Flags {
			set[imap.Flag(f)] = struct{}{}
		}
	}
	out := make([]imap.Flag, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *session) Unselect() error {
	if s.sel != nil {
		s.sel.tracker.Close()
		s.sel = nil
	}
	return nil
}

func (s *session) Subscribe(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return err
	}
	return s.c.store.SetSubscribed(s.ctx(), mb.ID, true)
}

func (s *session) Unsubscribe(name string) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	mb, err := s.mailbox(name)
	if err != nil {
		return err
	}
	return s.c.store.SetSubscribed(s.ctx(), mb.ID, false)
}

// ---- selected state ------------------------------------------------------

// refresh reloads the UID view from the store. Appends from an op only
// ever add at the tail, and removals reach the tracker as EXPUNGE first,
// so reloading at every command entry keeps the server view and the
// tracker's client view in step.
func (s *session) refresh() error {
	heads, err := s.c.store.ListMessageHeads(s.ctx(), s.sel.mb.ID)
	if err != nil {
		return no(imap.ResponseCodeUnavailable, "Temporary failure")
	}
	s.sel.heads = heads
	return nil
}

func (s *session) requireSelected() error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.sel == nil {
		return no(imap.ResponseCodeClientBug, "No mailbox selected")
	}
	return s.refresh()
}

// resolve maps a client number set onto server sequence numbers (1-based
// indexes into heads). '*' becomes the highest number in the mailbox.
func (s *session) resolve(numSet imap.NumSet) []uint32 {
	var out []uint32
	n := uint32(len(s.sel.heads))
	switch set := numSet.(type) {
	case imap.SeqSet:
		set = staticSeqSet(set, n)
		for i := range s.sel.heads {
			seq := uint32(i) + 1
			client := s.sel.tracker.EncodeSeqNum(seq)
			if client != 0 && set.Contains(client) {
				out = append(out, seq)
			}
		}
	case imap.UIDSet:
		var max imap.UID
		if n > 0 {
			max = imap.UID(s.sel.heads[n-1].UID)
		}
		set = staticUIDSet(set, max)
		for i, h := range s.sel.heads {
			if set.Contains(imap.UID(h.UID)) {
				out = append(out, uint32(i)+1)
			}
		}
	}
	return out
}

func staticSeqSet(set imap.SeqSet, max uint32) imap.SeqSet {
	out := make(imap.SeqSet, len(set))
	copy(out, set)
	for i := range out {
		staticRange(&out[i].Start, &out[i].Stop, max)
	}
	return out
}

func staticUIDSet(set imap.UIDSet, max imap.UID) imap.UIDSet {
	out := make(imap.UIDSet, len(set))
	copy(out, set)
	for i := range out {
		staticRange((*uint32)(&out[i].Start), (*uint32)(&out[i].Stop), uint32(max))
	}
	return out
}

func staticRange(start, stop *uint32, max uint32) {
	dyn := false
	if *start == 0 {
		*start = max
		dyn = true
	}
	if *stop == 0 {
		*stop = max
		dyn = true
	}
	if dyn && *start > *stop {
		*start, *stop = *stop, *start
	}
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if err := s.requireSelected(); err != nil {
		return err
	}
	markSeen := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}
	needBytes := len(options.BodySection) > 0 || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	for _, seq := range s.resolve(numSet) {
		head := s.sel.heads[seq-1]
		m, ok, err := s.c.store.GetMessage(s.ctx(), s.sel.mb.ID, head.UID)
		if err != nil {
			return no(imap.ResponseCodeUnavailable, "Temporary failure")
		}
		if !ok {
			continue // expunged under us; the tracker delivers the EXPUNGE
		}
		if markSeen && !chimap.HasFlag(m.Flags, string(imap.FlagSeen)) {
			ctx := chimap.WithOrigin(s.ctx(), s.sel.tracker)
			if fl, err := s.c.store.SetFlags(ctx, m.MailboxID, m.UID, append(m.Flags, string(imap.FlagSeen))); err == nil {
				m.Flags = fl
				s.sel.heads[seq-1].Flags = fl
			}
		}
		var raw []byte
		if needBytes {
			raw, err = renderRow(s.ctx(), s.c.fcas, m, s.domain)
			if err != nil {
				s.c.pu.Logger.Warn("imap render failed", zap.String("mailbox", m.MailboxID), zap.Uint32("uid", m.UID), zap.String("err", err.Error()))
				return no(imap.ResponseCodeUnavailable, "Message content unavailable")
			}
		}
		rw := w.CreateMessage(s.sel.tracker.EncodeSeqNum(seq))
		if err := writeMessage(rw, m, raw, options); err != nil {
			return err
		}
	}
	return nil
}

func writeMessage(w *imapserver.FetchResponseWriter, m chimap.Message, raw []byte, options *imap.FetchOptions) error {
	w.WriteUID(imap.UID(m.UID))
	if options.Flags {
		w.WriteFlags(toFlags(m.Flags))
	}
	if options.InternalDate {
		w.WriteInternalDate(m.InternalDate)
	}
	if options.RFC822Size {
		w.WriteRFC822Size(m.Size)
	}
	if options.Envelope {
		var env imap.Envelope
		if err := json.Unmarshal(m.Envelope, &env); err == nil {
			w.WriteEnvelope(&env)
		} else {
			w.WriteEnvelope(&imap.Envelope{})
		}
	}
	if options.BodyStructure != nil {
		bs, err := DecodeBodyStructure(m.BodyStructure)
		if err != nil && raw != nil {
			bs = imapserver.ExtractBodyStructure(bytes.NewReader(raw))
		}
		if bs != nil {
			w.WriteBodyStructure(bs)
		}
	}
	for _, sec := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(raw), sec)
		wc := w.WriteBodySection(sec, int64(len(buf)))
		_, werr := wc.Write(buf)
		cerr := wc.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	for _, sec := range options.BinarySection {
		buf := imapserver.ExtractBinarySection(bytes.NewReader(raw), sec)
		wc := w.WriteBinarySection(sec, int64(len(buf)))
		_, werr := wc.Write(buf)
		cerr := wc.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	for _, sec := range options.BinarySectionSize {
		w.WriteBinarySectionSize(sec, imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), sec))
	}
	return w.Close()
}

// Store applies a flag change. `flags` policy is local by default (no
// event); a mailbox opted into observe/stack hears keyword changes.
func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if err := s.requireSelected(); err != nil {
		return err
	}
	ctx := chimap.WithOrigin(s.ctx(), s.sel.tracker)
	seqs := s.resolve(numSet)
	next := map[uint32][]string{}
	for _, seq := range seqs {
		head := s.sel.heads[seq-1]
		var n []string
		switch flags.Op {
		case imap.StoreFlagsSet:
			n = fromFlags(flags.Flags)
		case imap.StoreFlagsAdd:
			n = append(append([]string{}, head.Flags...), fromFlags(flags.Flags)...)
		case imap.StoreFlagsDel:
			for _, f := range head.Flags {
				if !chimap.HasFlag(fromFlags(flags.Flags), f) {
					n = append(n, f)
				}
			}
		}
		next[head.UID] = n
	}
	m, mut, err := s.flagsGate(seqs, next)
	if err != nil {
		return err
	}
	for _, seq := range seqs {
		head := s.sel.heads[seq-1]
		stored, err := s.c.store.SetFlags(ctx, s.sel.mb.ID, head.UID, next[head.UID])
		if err != nil {
			return no(imap.ResponseCodeUnavailable, "Temporary failure")
		}
		s.sel.heads[seq-1].Flags = stored
	}
	s.after(m, mut)
	if flags.Silent {
		return nil
	}
	for _, seq := range seqs {
		head := s.sel.heads[seq-1]
		rw := w.CreateMessage(s.sel.tracker.EncodeSeqNum(seq))
		rw.WriteUID(imap.UID(head.UID))
		rw.WriteFlags(toFlags(head.Flags))
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.sel == nil {
		return nil
	}
	if err := s.refresh(); err != nil {
		return err
	}
	return s.sel.tracker.Poll(w, allowExpunge)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.sel == nil {
		<-stop
		return nil
	}
	return s.sel.tracker.Idle(w, stop)
}

// ---- search ----------------------------------------------------------------

// Search is column-backed: sequence/UID sets, internal and sent dates,
// flags, size, and the addressed headers (Subject/From/To/Cc/Bcc/
// Message-ID/In-Reply-To) come from the cached ENVELOPE; BODY/TEXT match
// the stored text excerpt. Any other HEADER field answers NO [CANNOT]
// rather than a silent empty result.
func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if err := s.requireSelected(); err != nil {
		return nil, err
	}
	if err := searchSupported(criteria); err != nil {
		return nil, err
	}
	var (
		data   imap.SearchData
		seqSet imap.SeqSet
		uidSet imap.UIDSet
	)
	for i, h := range s.sel.heads {
		seq := uint32(i) + 1
		client := s.sel.tracker.EncodeSeqNum(seq)
		m, ok, err := s.c.store.GetMessage(s.ctx(), s.sel.mb.ID, h.UID)
		if err != nil {
			return nil, no(imap.ResponseCodeUnavailable, "Temporary failure")
		}
		if !ok || !s.match(client, &m, criteria) {
			continue
		}
		uidSet.AddNum(imap.UID(m.UID))
		var num uint32
		switch kind {
		case imapserver.NumKindSeq:
			if client == 0 {
				continue
			}
			seqSet.AddNum(client)
			num = client
		case imapserver.NumKindUID:
			num = m.UID
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if num > data.Max {
			data.Max = num
		}
		data.Count++
	}
	if kind == imapserver.NumKindSeq {
		data.All = seqSet
	} else {
		data.All = uidSet
	}
	return &data, nil
}

var searchableHeaders = map[string]bool{
	"subject": true, "from": true, "to": true, "cc": true, "bcc": true, "message-id": true, "in-reply-to": true,
}

func searchSupported(c *imap.SearchCriteria) error {
	for _, h := range c.Header {
		if !searchableHeaders[strings.ToLower(h.Key)] {
			return cannot("SEARCH HEADER " + h.Key + " is not supported")
		}
	}
	if c.ModSeq != nil {
		return cannot("CONDSTORE is not supported")
	}
	for i := range c.Not {
		if err := searchSupported(&c.Not[i]); err != nil {
			return err
		}
	}
	for i := range c.Or {
		for j := range c.Or[i] {
			if err := searchSupported(&c.Or[i][j]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session) match(clientSeq uint32, m *chimap.Message, c *imap.SearchCriteria) bool {
	n := uint32(len(s.sel.heads))
	for _, set := range c.SeqNum {
		set = staticSeqSet(set, n)
		if clientSeq == 0 || !set.Contains(clientSeq) {
			return false
		}
	}
	for _, set := range c.UID {
		var max imap.UID
		if n > 0 {
			max = imap.UID(s.sel.heads[n-1].UID)
		}
		if !staticUIDSet(set, max).Contains(imap.UID(m.UID)) {
			return false
		}
	}
	if !matchDate(m.InternalDate, c.Since, c.Before) {
		return false
	}
	for _, f := range c.Flag {
		if !chimap.HasFlag(m.Flags, string(f)) {
			return false
		}
	}
	for _, f := range c.NotFlag {
		if chimap.HasFlag(m.Flags, string(f)) {
			return false
		}
	}
	if c.Larger != 0 && m.Size <= c.Larger {
		return false
	}
	if c.Smaller != 0 && m.Size >= c.Smaller {
		return false
	}
	var env imap.Envelope
	needEnv := len(c.Header) > 0 || !c.SentSince.IsZero() || !c.SentBefore.IsZero()
	if needEnv {
		_ = json.Unmarshal(m.Envelope, &env)
	}
	if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		if env.Date.IsZero() || !matchDate(env.Date, c.SentSince, c.SentBefore) {
			return false
		}
	}
	for _, h := range c.Header {
		if !matchHeader(&env, h.Key, h.Value) {
			return false
		}
	}
	for _, t := range c.Text {
		if !containsFold(m.TextExcerpt, t) && !containsFold(m.Subject, t) && !containsFold(m.FromAddr, t) {
			return false
		}
	}
	for _, b := range c.Body {
		if !containsFold(m.TextExcerpt, b) {
			return false
		}
	}
	for i := range c.Not {
		if s.match(clientSeq, m, &c.Not[i]) {
			return false
		}
	}
	for i := range c.Or {
		if !s.match(clientSeq, m, &c.Or[i][0]) && !s.match(clientSeq, m, &c.Or[i][1]) {
			return false
		}
	}
	return true
}

func matchHeader(env *imap.Envelope, key, value string) bool {
	var vals []string
	addrs := func(l []imap.Address) {
		for _, a := range l {
			vals = append(vals, a.Name, a.Addr())
		}
	}
	switch strings.ToLower(key) {
	case "subject":
		vals = []string{env.Subject}
	case "from":
		addrs(env.From)
	case "to":
		addrs(env.To)
	case "cc":
		addrs(env.Cc)
	case "bcc":
		addrs(env.Bcc)
	case "message-id":
		vals = []string{env.MessageID}
	case "in-reply-to":
		vals = env.InReplyTo
	}
	if value == "" {
		for _, v := range vals {
			if v != "" {
				return true
			}
		}
		return false
	}
	for _, v := range vals {
		if containsFold(v, value) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return sub == "" || strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// matchDate compares on the calendar day in UTC (RFC 3501: zone-unaware).
func matchDate(t, since, before time.Time) bool {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}
