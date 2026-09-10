// Package imapsource is the IMAP source kind: it connects OUT to a remote
// IMAP mailbox a tenant delegated (Gmail, Fastmail, Dovecot, an Exchange-
// compatible server), reads messages past a durable UID cursor, and — once a
// message's run has succeeded — marks it read or moves it aside. It registers
// itself as source kind "imap"; the binary blank-imports it.
//
// Three things are load-bearing and deliberate:
//
//   - The dial is hand-built (net.Dialer → optional TLS → imapclient.New over
//     the conn) rather than the imapclient.Dial* helpers, for two reasons: it
//     routes through the chassis egress Guard (net.Dialer.Control) so a source
//     can't be pointed at loopback/private space, and it holds the net.Conn so
//     a whole-session deadline bounds a stalled server (go-imap's Wait() takes
//     no context).
//
//   - MOVE is gated on the server actually advertising it (Caps().Has(MOVE)).
//     go-imap's Move() otherwise silently falls back to COPY + \Deleted +
//     EXPUNGE — which can lose mail if the expunge half fails — so when MOVE is
//     absent we degrade to marking \Seen and log it, never emulate the move.
//
//   - The credential (mailbox password) arrives via OpenParams.Cred, is used
//     once at LOGIN, and is never stored on the conn. The caller zeroes it the
//     instant Open returns.
package imapsource

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/source"
)

const (
	dialTimeout = 20 * time.Second
	// sessionDeadline bounds the WHOLE imap session (login + one poll/ack
	// pass). go-imap commands ignore context, so this net.Conn deadline is the
	// only thing between a stalled server and a hung poller goroutine. It is
	// refreshed at the start of Poll and each Ack so every phase gets the full
	// budget.
	sessionDeadline = 90 * time.Second
	// fetchCap is a hard ceiling on messages pulled in one Poll regardless of
	// the poller's requested limit, so a first sync of a huge mailbox drains
	// over several passes rather than one enormous fetch.
	fetchCap = 500
)

func init() { source.RegisterKind(imapKind{}) }

type imapKind struct{}

func (imapKind) Name() string { return "imap" }

// imapConfig is the declared config of one IMAP source (the pack line minus
// the poller-only fields). The `secret` name and `on_processed` live in the
// same JSON but are read by the poller, not here.
type imapConfig struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	TLS     string `json:"tls"` // "implicit" (default) | "starttls" | "none"
	User    string `json:"user"`
	Mailbox string `json:"mailbox"` // default "INBOX"
}

func (imapKind) Open(ctx context.Context, p source.OpenParams) (source.Conn, error) {
	var cfg imapConfig
	if err := json.Unmarshal(p.Config, &cfg); err != nil {
		return nil, fmt.Errorf("imapsource: bad config: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("imapsource: config missing host")
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("imapsource: config missing user")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	mode := cfg.TLS
	if mode == "" {
		mode = "implicit"
	}
	port := cfg.Port
	if port == 0 {
		if mode == "implicit" {
			port = 993
		} else {
			port = 143
		}
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// Hand-built dial: egress-guarded, deadline-bearing.
	d := &net.Dialer{Timeout: dialTimeout}
	if p.Guard != nil {
		d.Control = egress.DialControl(p.Guard)
	}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("imapsource: dial %s: %w", addr, err)
	}
	// Whole-session deadline until we're connected and logged in.
	_ = raw.SetDeadline(time.Now().Add(sessionDeadline))

	opts := &imapclient.Options{}
	var c *imapclient.Client
	switch mode {
	case "implicit":
		tconn := tls.Client(raw, &tls.Config{ServerName: cfg.Host})
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("imapsource: tls handshake %s: %w", addr, err)
		}
		c = imapclient.New(tconn, opts)
	case "starttls":
		// NewStartTLS drains the greeting and issues STARTTLS itself (the
		// public path — the client's StartTLS method is unexported in beta.8).
		o := *opts
		o.TLSConfig = &tls.Config{ServerName: cfg.Host}
		cc, terr := imapclient.NewStartTLS(raw, &o)
		if terr != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("imapsource: starttls %s: %w", addr, terr)
		}
		c = cc
	case "none":
		c = imapclient.New(raw, opts)
	default:
		_ = raw.Close()
		return nil, fmt.Errorf("imapsource: unknown tls mode %q (want implicit|starttls|none)", mode)
	}
	// implicit/none still need the greeting drained before LOGIN; starttls
	// already waited above.
	if mode != "starttls" {
		if err := c.WaitGreeting(); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imapsource: greeting %s: %w", addr, err)
		}
	}
	if err := c.Login(cfg.User, string(p.Cred)).Wait(); err != nil {
		_ = c.Logout().Wait()
		_ = c.Close()
		return nil, fmt.Errorf("imapsource: login %s@%s: %w", cfg.User, addr, err)
	}
	capMove := c.Caps().Has(imap.CapMove)
	if !capMove {
		logger.Warn("imapsource: server does not advertise MOVE; a move action will degrade to marking \\Seen",
			zap.String("host", cfg.Host))
	}
	return &imapConn{
		c:       c,
		raw:     raw,
		cfg:     cfg,
		capMove: capMove,
		logger:  logger,
		created: map[string]bool{},
	}, nil
}

type imapConn struct {
	c       *imapclient.Client
	raw     net.Conn
	cfg     imapConfig
	capMove bool
	logger  *zap.Logger
	created map[string]bool // move destinations we've already ensured exist
}

// imapCursor is the persisted, opaque-to-the-poller cursor. UIDValidity ties
// the high-water mark to one incarnation of the mailbox: when the server
// renumbers (UIDVALIDITY changes) every old UID is meaningless and the mark
// resets to 0.
type imapCursor struct {
	UIDValidity uint32 `json:"uidvalidity"`
	UIDHWM      uint32 `json:"uid_hwm"`
}

func (cn *imapConn) touch() { _ = cn.raw.SetDeadline(time.Now().Add(sessionDeadline)) }

func (cn *imapConn) Poll(ctx context.Context, cur source.Cursor, limit int) ([]source.Item, source.Cursor, error) {
	cn.touch()
	var ic imapCursor
	if len(cur) > 0 {
		_ = json.Unmarshal(cur, &ic) // a corrupt cursor reads as {0,0} → full resync, which is safe
	}

	sd, err := cn.c.Select(cn.cfg.Mailbox, &imap.SelectOptions{}).Wait()
	if err != nil {
		return nil, nil, fmt.Errorf("imapsource: select %q: %w", cn.cfg.Mailbox, err)
	}
	if sd.UIDValidity != ic.UIDValidity {
		if ic.UIDValidity != 0 {
			cn.logger.Warn("imapsource: UIDVALIDITY changed; resetting cursor",
				zap.Uint32("was", ic.UIDValidity), zap.Uint32("now", sd.UIDValidity),
				zap.String("mailbox", cn.cfg.Mailbox))
		}
		ic.UIDValidity = sd.UIDValidity
		ic.UIDHWM = 0
	}

	// The closed range (hwm+1 .. uidNext-1): every currently-existing UID we
	// have not yet read. No wildcard, so no ambiguity about how the server
	// renders `*`.
	if sd.UIDNext < 2 { // empty mailbox or nothing above the mark
		return nil, drainedCursor(ic, 0), nil
	}
	lo := imap.UID(ic.UIDHWM + 1)
	hi := sd.UIDNext - 1
	if hi < lo {
		return nil, drainedCursor(ic, uint32(hi)), nil
	}

	crit := &imap.SearchCriteria{UID: []imap.UIDSet{{{Start: lo, Stop: hi}}}}
	found, err := cn.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, nil, fmt.Errorf("imapsource: uid search: %w", err)
	}
	uids := found.AllUIDs()
	if len(uids) == 0 {
		return nil, drainedCursor(ic, uint32(hi)), nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	cap := limit
	if cap <= 0 || cap > fetchCap {
		cap = fetchCap
	}
	if len(uids) > cap {
		uids = uids[:cap]
	}

	sec := &imap.FetchItemBodySection{Peek: true} // BODY.PEEK[] — never sets \Seen
	fopts := &imap.FetchOptions{
		UID:         true,
		Flags:       true,
		BodySection: []*imap.FetchItemBodySection{sec},
	}
	msgs, err := cn.c.Fetch(imap.UIDSetNum(uids...), fopts).Collect()
	if err != nil {
		return nil, nil, fmt.Errorf("imapsource: fetch: %w", err)
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })

	items := make([]source.Item, 0, len(msgs))
	for _, m := range msgs {
		body := m.FindBodySection(sec)
		if body == nil {
			// A message vanished between SEARCH and FETCH (expunged): skip it,
			// don't fail the batch.
			continue
		}
		uid := uint32(m.UID)
		meta, _ := json.Marshal(map[string]any{"uid": uid, "flags": m.Flags})
		itemCur, _ := json.Marshal(imapCursor{UIDValidity: ic.UIDValidity, UIDHWM: uid})
		raw := make([]byte, len(body)) // Collect reuses buffers; copy out
		copy(raw, body)
		items = append(items, source.Item{
			Key:    fmt.Sprintf("%d:%d", ic.UIDValidity, uid),
			Raw:    raw,
			Meta:   meta,
			Cursor: source.Cursor(itemCur),
		})
	}
	// Per-item cursors carry the advance on success; a nil drained means "the
	// last successful item's cursor is the whole story". (The empty/reset
	// cases returned their own drained above.)
	return items, nil, nil
}

func (cn *imapConn) Ack(ctx context.Context, it source.Item, act source.Action) error {
	if act.Op == "none" {
		return nil
	}
	cn.touch()
	uid, err := uidFromMeta(it.Meta)
	if err != nil {
		return err
	}
	numSet := imap.UIDSetNum(imap.UID(uid))

	op := act.Op
	if op == "move" && !cn.capMove {
		// Documented degrade: never emulate MOVE with COPY + \Deleted.
		cn.logger.Warn("imapsource: MOVE unsupported by server; marking \\Seen instead",
			zap.String("dest", act.Dest), zap.Uint32("uid", uid))
		op = "seen"
	}

	switch op {
	case "seen":
		sf := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
		if _, err := cn.c.Store(numSet, sf, nil).Collect(); err != nil {
			return fmt.Errorf("imapsource: mark seen uid %d: %w", uid, err)
		}
		return nil
	case "move":
		if !cn.created[act.Dest] {
			// Create-if-missing (idempotent: an "already exists" is fine).
			if err := cn.c.Create(act.Dest, nil).Wait(); err != nil {
				cn.logger.Debug("imapsource: create dest (may already exist)",
					zap.String("dest", act.Dest), zap.String("err", err.Error()))
			}
			cn.created[act.Dest] = true
		}
		if _, err := cn.c.Move(numSet, act.Dest).Wait(); err != nil {
			return fmt.Errorf("imapsource: move uid %d → %q: %w", uid, act.Dest, err)
		}
		return nil
	default:
		return fmt.Errorf("imapsource: unknown ack op %q", op)
	}
}

func (cn *imapConn) Close() error {
	_ = cn.c.Logout().Wait()
	return cn.c.Close()
}

// drainedCursor marshals a cursor to persist when a poll produced no items —
// records a UIDVALIDITY reset and advances the mark to `atLeast` (the top of
// the searched range, since we now know nothing below it is unread) so an
// empty or renumbered mailbox is not rescanned every cycle.
func drainedCursor(ic imapCursor, atLeast uint32) source.Cursor {
	if atLeast > ic.UIDHWM {
		ic.UIDHWM = atLeast
	}
	b, _ := json.Marshal(ic)
	return source.Cursor(b)
}

func uidFromMeta(meta json.RawMessage) (uint32, error) {
	var m struct {
		UID uint32 `json:"uid"`
	}
	if err := json.Unmarshal(meta, &m); err != nil || m.UID == 0 {
		return 0, fmt.Errorf("imapsource: item meta has no uid")
	}
	return m.UID, nil
}
