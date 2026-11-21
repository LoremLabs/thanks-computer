package attached

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// Hub is this node's side of every link it holds: it runs the handshake,
// keeps the claim, and forwards requests down to the machine.
type Hub struct {
	store  *Store
	claims *Claims
	node   string
	now    func() time.Time

	// Complete receives a detached job's end from a machine: the result of
	// the async op whose continuation the machine was handed. It returns
	// only once the result is durably recorded, because its return is what
	// lets the machine delete its copy. nil refuses completions.
	Complete func(ctx context.Context, cred *Credential, opc, token string, result json.RawMessage) error

	// Notify tells another node that a connection it holds was replaced.
	// nil on a single node. Best effort: the replaced connection can do
	// nothing in the meantime, since every request re-reads the claim.
	Notify func(ctx context.Context, node, workspaceID, conn, reason string)

	// Logf, when set, receives one line per link event.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	conns map[string]*hubConn // by workspace id
	seen  map[string]time.Time
}

// NewHub builds the hub for the node named node. That name is what a claim
// records, and what other nodes address a request to.
func NewHub(store *Store, claims *Claims, node string) *Hub {
	return &Hub{store: store, claims: claims, node: node, now: time.Now,
		conns: map[string]*hubConn{}, seen: map[string]time.Time{}}
}

// Node is this hub's name.
func (h *Hub) Node() string { return h.node }

func (h *Hub) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

// hubConn is one authenticated link.
type hubConn struct {
	id     string
	gen    int64
	cred   string
	tenant string
	w      string
	sess   *yamux.Session

	mu    sync.Mutex
	claim Claim // the exact value last written: renewals swap from it

	closeOnce sync.Once
	reason    string
}

// close ends the link, telling the machine why first. A machine told
// `replaced` or `revoked` does not come back.
func (c *hubConn) close(reason string) {
	c.closeOnce.Do(func() {
		c.reason = reason
		if st, err := c.sess.OpenStream(); err == nil {
			// Wait, briefly, for the machine to say it read the reason. A
			// session closed right behind the message can end the machine's
			// accept loop before it has taken the stream, and a machine that
			// never learns it was replaced or revoked would dial again.
			_ = st.SetDeadline(time.Now().Add(2 * time.Second))
			if writeMsg(st, Header{Kind: KindBye, Reason: reason}) == nil {
				var ack Reply
				_ = newMsgReader(st).read(&ack)
			}
			_ = st.Close()
		}
		_ = c.sess.Close()
	})
}

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 25 * time.Second
	return cfg
}

// Serve runs one link from its handshake to its end. rw is the connection
// the machine dialed; host is the name the machine dialed it by, as this
// chassis saw it. It returns when the link is over, for any reason.
func (h *Hub) Serve(ctx context.Context, rw io.ReadWriteCloser, host string) error {
	sess, err := yamux.Server(rw, yamuxConfig())
	if err != nil {
		_ = rw.Close()
		return err
	}
	defer sess.Close()

	c, err := h.handshake(ctx, sess, host)
	if err != nil {
		return err
	}
	h.logf("attach: link up workspace=%s conn=%s gen=%d", c.w, c.id, c.gen)
	defer h.drop(c)

	go func() {
		for {
			st, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go h.handleUp(ctx, c, st)
		}
	}()

	renew := time.NewTicker(h.claims.Lease() / 2)
	defer renew.Stop()
	for {
		select {
		case <-sess.CloseChan():
			return nil
		case <-ctx.Done():
			c.close(ReasonShutdown)
			return nil
		case <-renew.C:
			h.renew(ctx, c)
		}
	}
}

// handshake authenticates the machine on the link's first stream and makes
// it the holder of its credential's workspace.
//
// Nothing the machine sends chooses a workspace. The hello names a
// credential; the workspace comes from that credential's row.
func (h *Hub) handshake(ctx context.Context, sess *yamux.Session, host string) (*hubConn, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	st, err := sess.AcceptStreamWithContext(hctx)
	if err != nil {
		return nil, fmt.Errorf("attached: no hello: %w", err)
	}
	defer st.Close()
	// One deadline over the whole exchange. It is also the life of the
	// nonce: an answer after it is never read.
	_ = st.SetDeadline(h.now().Add(handshakeTimeout))
	in := newMsgReader(st)

	var hello Header
	if err := in.read(&hello); err != nil || hello.Kind != KindHello || hello.Credential == "" {
		return nil, errors.New("attached: malformed hello")
	}
	cred, err := h.store.Credential(hctx, hello.Credential)
	if err != nil {
		return nil, err
	}
	if cred.Revoked() {
		// One answer for "never existed" and "revoked": a caller learns
		// nothing about which ids are real.
		_ = writeMsg(st, Challenge{Refused: ReasonRevoked})
		return nil, errors.New("attached: hello for a credential that is not live")
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	connID, err := randomID("conn_")
	if err != nil {
		return nil, err
	}
	if err := writeMsg(st, Challenge{Nonce: nonce, Conn: connID}); err != nil {
		return nil, err
	}
	var ans Answer
	if err := in.read(&ans); err != nil {
		return nil, fmt.Errorf("attached: no answer to the challenge: %w", err)
	}
	// The nonce is a local of this call: it is compared once, here, and
	// exists nowhere else.
	if !ed25519.Verify(cred.PublicKey, ChallengeBytes(host, cred.ID, connID, nonce), ans.Signature) {
		_ = writeMsg(st, Welcome{Reason: ReasonRefused})
		return nil, errors.New("attached: challenge answered with a bad signature")
	}

	// Only now, with the key proven, is a generation issued.
	gen, err := h.claims.NextGen(hctx, cred.Tenant, cred.WorkspaceID)
	if err != nil {
		return nil, err
	}
	c := &hubConn{id: connID, gen: gen, cred: cred.ID, tenant: cred.Tenant, w: cred.WorkspaceID, sess: sess}
	written, replaced, err := h.claims.Install(hctx, cred.Tenant, cred.WorkspaceID, Claim{
		Gen: gen, Node: h.node, Conn: connID,
		Machine: clip(hello.Machine, 128), Root: clip(hello.Root, 1024),
		OS: clip(hello.OS, 64), Version: clip(hello.Version, 64),
	})
	if errors.Is(err, ErrReplaced) {
		_ = writeMsg(st, Welcome{Reason: ReasonReplaced})
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	c.claim = written

	h.mu.Lock()
	old := h.conns[c.w]
	h.conns[c.w] = c
	h.mu.Unlock()
	if old != nil {
		go old.close(ReasonReplaced)
	}
	if replaced != nil && replaced.Node != h.node && h.Notify != nil {
		go h.Notify(context.WithoutCancel(ctx), replaced.Node, c.w, replaced.Conn, ReasonReplaced)
	}

	if err := writeMsg(st, Welcome{OK: true, Gen: gen, Label: cred.Label, Name: cred.Name}); err != nil {
		h.drop(c)
		return nil, err
	}
	return c, nil
}

// drop forgets a link and releases its claim. The release swaps only from
// this link's own value, so it never touches a newer claim.
func (h *Hub) drop(c *hubConn) {
	h.mu.Lock()
	if h.conns[c.w] == c {
		delete(h.conns, c.w)
	}
	h.mu.Unlock()
	_ = c.sess.Close()
	c.mu.Lock()
	mine := c.claim
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = h.claims.Release(ctx, c.tenant, c.w, mine)
	h.logf("attach: link down workspace=%s conn=%s gen=%d reason=%s", c.w, c.id, c.gen, c.reason)
}

// renew extends the claim's lease, and closes the link if its credential
// was revoked or its claim replaced. Neither is what makes a revoke or a
// replace effective (Send checks both before every request); they only
// close the link sooner.
func (h *Hub) renew(ctx context.Context, c *hubConn) {
	cred, err := h.store.Credential(ctx, c.cred)
	if err == nil && cred.Revoked() {
		c.close(ReasonRevoked)
		return
	}
	c.mu.Lock()
	mine := c.claim
	c.mu.Unlock()
	next, err := h.claims.Renew(ctx, c.tenant, c.w, mine)
	switch {
	case errors.Is(err, ErrReplaced):
		c.close(ReasonReplaced)
	case err != nil:
		// The KV is unwell. The lease still has half its life; readers see
		// the name away if it runs out.
		h.logf("attach: renew workspace=%s conn=%s: %v", c.w, c.id, err)
	default:
		c.mu.Lock()
		c.claim = next
		c.mu.Unlock()
	}
}

// Close ends the link holding workspaceID here, if one does and it is conn
// (any, when conn is ""). It reports whether a link was closed.
func (h *Hub) Close(workspaceID, conn, reason string) bool {
	h.mu.Lock()
	c := h.conns[workspaceID]
	h.mu.Unlock()
	if c == nil || (conn != "" && c.id != conn) {
		return false
	}
	c.close(reason)
	return true
}

// CloseAll ends every link this node holds: the node is going down.
func (h *Hub) CloseAll(reason string) {
	h.mu.Lock()
	conns := make([]*hubConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.close(reason)
	}
}

// firstSight records a request id and reports whether it is new. Ids are
// kept for an hour.
func (h *Hub) firstSight(id string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) > 4096 {
		for k, t := range h.seen {
			if now.Sub(t) > time.Hour {
				delete(h.seen, k)
			}
		}
	}
	if t, ok := h.seen[id]; ok && now.Sub(t) <= time.Hour {
		return false
	}
	h.seen[id] = now
	return true
}

// Send forwards hdr to the machine on the link (workspaceID, conn, gen) and
// returns its reply. It is called on the node that holds the link.
//
// BEFORE anything reaches the machine it checks, in this order:
//
//  1. the credential, in the DB, is not revoked. This is what makes a revoke
//     effective: from the moment it commits, nothing more is forwarded;
//  2. the claim, in the KV, still names this connection and generation. A
//     link that was replaced and not yet told can never take a request;
//  3. the request's id is new.
//
// These are read for every request, never cached: the machine is about to
// be handed a run's environment.
//
// The error says what the caller may conclude. CodeAway: nothing was
// delivered. CodeUnknown: it may have been, and must not be sent again.
func (h *Hub) Send(ctx context.Context, workspaceID, conn string, gen int64, hdr Header) (Reply, error) {
	h.mu.Lock()
	c := h.conns[workspaceID]
	h.mu.Unlock()
	if c == nil || c.id != conn || c.gen != gen {
		return Reply{}, away("the machine is not connected to this node")
	}
	cred, err := h.store.Credential(ctx, c.cred)
	if err != nil {
		return Reply{}, &workspace.Error{Code: "provider", Message: err.Error()}
	}
	if cred.Revoked() {
		c.close(ReasonRevoked)
		return Reply{}, away("the machine's credential was revoked")
	}
	claim, found, err := h.claims.Read(ctx, c.tenant, c.w)
	if err != nil {
		return Reply{}, &workspace.Error{Code: "provider", Message: err.Error()}
	}
	if !found || !claim.Holds(c.id, c.gen) {
		c.close(ReasonReplaced)
		return Reply{}, away("the machine's connection was replaced")
	}
	if !claim.Live(h.now()) {
		return Reply{}, away("the machine's claim has lapsed")
	}
	isRequest := hdr.Kind == KindRequest
	if isRequest && !h.firstSight(hdr.ID) {
		return Reply{}, unknown("a request with this id was already sent; it is not sent again")
	}

	st, err := c.sess.OpenStream()
	if err != nil {
		return Reply{}, away("the machine's connection is closed")
	}
	defer st.Close()
	// The stream follows ctx: when the caller gives up, the read ends.
	stop := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()) })
	defer stop()

	if err := writeMsg(st, hdr); err != nil {
		if isRequest {
			return Reply{}, unknown("the connection failed while the request was being sent")
		}
		return Reply{}, away("the machine's connection failed")
	}
	var rep Reply
	if err := newMsgReader(st).read(&rep); err != nil {
		if isRequest {
			if ctx.Err() != nil {
				// We gave up. Tell the machine, so the command does not run
				// on for nobody; whether it ran is still not known here.
				go h.cancel(c, hdr.ID)
				return Reply{}, unknown("no reply from the machine by the deadline")
			}
			return Reply{}, unknown("the connection was lost before the machine replied")
		}
		return Reply{}, away("the machine did not answer")
	}
	return rep, nil
}

// cancel tells the machine a request's caller has gone.
func (h *Hub) cancel(c *hubConn, id string) {
	st, err := c.sess.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(2 * time.Second))
	if writeMsg(st, Header{Kind: KindCancel, ID: id}) == nil {
		var rep Reply
		_ = newMsgReader(st).read(&rep)
	}
}

// handleUp serves a stream the machine opened.
func (h *Hub) handleUp(ctx context.Context, c *hubConn, st *yamux.Stream) {
	defer st.Close()
	_ = st.SetDeadline(h.now().Add(60 * time.Second))
	var hdr Header
	if err := newMsgReader(st).read(&hdr); err != nil {
		return
	}
	fail := func(code, msg string) {
		_ = writeMsg(st, Reply{Error: &WireError{Code: code, Message: msg}})
	}
	switch hdr.Kind {
	case KindComplete:
		// The same authority check as a request going down: a revoked
		// machine's word is not taken either.
		cred, err := h.store.Credential(ctx, c.cred)
		if err != nil {
			fail("provider", "credential lookup failed")
			return
		}
		if cred.Revoked() {
			fail(ReasonRevoked, "the credential was revoked")
			c.close(ReasonRevoked)
			return
		}
		if h.Complete == nil {
			fail("unsupported", "this chassis takes no completions")
			return
		}
		if err := h.Complete(context.WithoutCancel(ctx), cred, hdr.OPC, hdr.Token, hdr.Payload); err != nil {
			var we *workspace.Error
			if errors.As(err, &we) {
				fail(we.Code, we.Message)
			} else {
				fail("provider", err.Error())
			}
			return
		}
		// Only now: the result is durably recorded, so the machine may
		// delete its copy.
		_ = writeMsg(st, Reply{OK: true})
	default:
		fail("bad_request", "unknown stream kind "+hdr.Kind)
	}
}
