package attached

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kvtools/valkeyrie"
	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/kv"
	boltdb "github.com/loremlabs/thanks-computer/chassis/kv/boltstore"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

const testHost = "attach.example.test"

var testSpec = workspace.Spec{Tenant: "acme", Stack: "loop", Name: "computer/rain"}

// rig is one chassis node: the runtime DB, the KV, and the attach service.
type rig struct {
	t      *testing.T
	db     *sql.DB
	ws     *workspace.Store
	store  *Store
	claims *Claims
	svc    *Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "rt.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ws := workspace.NewStore(db, nil)
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	claims := NewClaims(kv.New(s, 0, 0))
	return &rig{t: t, db: db, ws: ws, store: store, claims: claims, svc: NewService(store, claims, "node-a")}
}

// enroll makes a code for spec and spends it with a fresh key.
func (r *rig) enroll(spec workspace.Spec) (*Credential, ed25519.PrivateKey) {
	r.t.Helper()
	code, _, err := r.svc.Invite(context.Background(), spec, "Rain's computer", 0)
	if err != nil {
		r.t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cred, _, err := r.store.Enroll(context.Background(), code, Enrollment{PublicKey: pub, OS: "darwin", Root: "~/projects/onepony", Machine: "MacBook"})
	if err != nil {
		r.t.Fatal(err)
	}
	return cred, priv
}

// handlerFunc is a machine that answers requests with a function.
type handlerFunc struct {
	request func(ctx context.Context, h Header) Reply
	stopped chan string
}

func (f handlerFunc) Request(ctx context.Context, h Header) Reply { return f.request(ctx, h) }
func (f handlerFunc) Stop(_ context.Context, job string) error {
	if f.stopped != nil {
		f.stopped <- job
	}
	return nil
}

// echo answers an exec with its own command as stdout.
func echo(ctx context.Context, h Header) Reply {
	var p ExecPayload
	_ = json.Unmarshal(h.Payload, &p)
	b, _ := json.Marshal(ExecReply{Stdout: "ran: " + p.Command})
	return Reply{OK: true, Payload: b}
}

// machine is a connected link and the two ends' outcomes.
type machine struct {
	link    *Link
	welcome Welcome
	linkErr chan error
	hubErr  chan error
	cancel  context.CancelFunc
	client  net.Conn
}

// connect dials the rig with cred's key and waits for the handshake's end,
// one way or the other. ok says whether the link came up.
func (r *rig) connect(cred string, key ed25519.PrivateKey, host string, h Handler) (*machine, bool) {
	r.t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	m := &machine{linkErr: make(chan error, 1), hubErr: make(chan error, 1), cancel: cancel, client: b}
	up := make(chan Welcome, 1)
	m.link = &Link{Credential: cred, Key: key, Host: host, Machine: "MacBook", Root: "~/projects/onepony", OS: "darwin", Version: "test",
		Handler: h, OnWelcome: func(w Welcome) { up <- w }}
	go func() { m.hubErr <- r.svc.Hub.Serve(ctx, a, testHost) }()
	go func() { m.linkErr <- m.link.Run(ctx, b) }()
	r.t.Cleanup(func() {
		cancel()
		_ = a.Close()
		_ = b.Close()
	})
	select {
	case m.welcome = <-up:
		return m, true
	case err := <-m.linkErr:
		m.linkErr <- err
		return m, false
	case <-time.After(15 * time.Second):
		r.t.Fatal("the handshake neither succeeded nor failed")
		return nil, false
	}
}

func (r *rig) mustConnect(cred *Credential, key ed25519.PrivateKey, h Handler) *machine {
	r.t.Helper()
	m, ok := r.connect(cred.ID, key, testHost, h)
	if !ok {
		r.t.Fatalf("the link did not come up: %v", <-m.linkErr)
	}
	return m
}

// exec runs a command on the attached workspace the way the Manager would.
func (r *rig) exec(spec workspace.Spec, command string, timeout time.Duration) (workspace.ExecResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	comp, err := r.svc.Wake(ctx, spec, "")
	if err != nil {
		return workspace.ExecResult{Exit: -1}, err
	}
	return comp.Exec(ctx, workspace.ExecRequest{Command: command}, workspace.Limits{})
}

func code(err error) string {
	var we *workspace.Error
	if errors.As(err, &we) {
		return we.Code
	}
	if err != nil {
		return "!" + err.Error()
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for stop := time.Now().Add(10 * time.Second); time.Now().Before(stop); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestExecReachesTheMachine(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	m := r.mustConnect(cred, key, handlerFunc{request: echo})
	if !m.welcome.OK || m.welcome.Name != testSpec.Name || m.welcome.Label != "Rain's computer" || m.welcome.Gen != 1 {
		t.Fatalf("welcome = %+v", m.welcome)
	}
	res, err := r.exec(testSpec, "git status", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "ran: git status" || res.Exit != 0 {
		t.Fatalf("result = %+v", res)
	}
	st, err := r.svc.Status(context.Background(), testSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enrolled || !st.Attached || st.Machine != "MacBook" || st.Root != "~/projects/onepony" || st.Version != "test" {
		t.Fatalf("status = %+v", st)
	}
}

// The machine goes: the name reads away at once. It comes back with the
// same credential and no new code.
func TestAwayThenBackWithoutANewCode(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	m := r.mustConnect(cred, key, handlerFunc{request: echo})
	m.cancel()
	<-m.hubErr
	if _, err := r.exec(testSpec, "true", time.Second); code(err) != workspace.CodeAway {
		t.Fatalf("exec with the machine gone: %v, want away", err)
	}
	st, _ := r.svc.Status(context.Background(), testSpec)
	if !st.Enrolled || st.Attached {
		t.Fatalf("status with the machine gone = %+v", st)
	}
	m2 := r.mustConnect(cred, key, handlerFunc{request: echo})
	if m2.welcome.Gen != 2 {
		t.Fatalf("second connection's generation = %d", m2.welcome.Gen)
	}
	if res, err := r.exec(testSpec, "again", 5*time.Second); err != nil || string(res.Stdout) != "ran: again" {
		t.Fatalf("exec after reconnect = %+v, %v", res, err)
	}
}

func TestHandshakeRefusals(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)

	refused := func(name, credID string, k ed25519.PrivateKey, host, wantReason string) {
		t.Helper()
		m, ok := r.connect(credID, k, host, handlerFunc{request: echo})
		if ok {
			t.Fatalf("%s: the link came up", name)
		}
		var ref *Refused
		if err := <-m.linkErr; !errors.As(err, &ref) || ref.Reason != wantReason {
			t.Fatalf("%s: err = %v, want refused as %s", name, err, wantReason)
		}
	}
	refused("another key", cred.ID, otherKey, testHost, ReasonRefused)
	// The machine signs the host IT dialed. An answer made for another host
	// (a relay in the middle) does not verify here.
	refused("another host", cred.ID, key, "evil.example.test", ReasonRefused)
	refused("no such credential", "atc-00000000000000000000000000000000", key, testHost, ReasonRevoked)

	// None of that touched the claim or spent a generation's worth of trust:
	// the real machine still connects.
	if st, _ := r.svc.Status(context.Background(), testSpec); st.Attached {
		t.Fatalf("a refused handshake left a claim: %+v", st)
	}
	r.mustConnect(cred, key, handlerFunc{request: echo})
}

// An answer is good for one connection. Signed for connection A's nonce and
// id, it is refused on connection B, which was sent its own.
func TestChallengeAnswerIsBoundToItsConnection(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)

	// dial runs the hello by hand and returns the stream at the point where
	// the challenge has been read.
	dial := func() (*yamux.Stream, Challenge) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		go func() { _ = r.svc.Hub.Serve(context.Background(), a, testHost) }()
		sess, err := yamux.Client(b, yamuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		st, err := sess.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMsg(st, Header{Kind: KindHello, Credential: cred.ID}); err != nil {
			t.Fatal(err)
		}
		var ch Challenge
		if err := newMsgReader(st).read(&ch); err != nil {
			t.Fatal(err)
		}
		return st, ch
	}
	stA, chA := dial()
	stB, chB := dial()
	if chA.Conn == chB.Conn || string(chA.Nonce) == string(chB.Nonce) {
		t.Fatal("two connections were given the same challenge")
	}
	answerA := ed25519.Sign(key, ChallengeBytes(testHost, cred.ID, chA.Conn, chA.Nonce))

	// A's answer on B.
	if err := writeMsg(stB, Answer{Signature: answerA}); err != nil {
		t.Fatal(err)
	}
	var w Welcome
	if err := newMsgReader(stB).read(&w); err != nil || w.OK || w.Reason != ReasonRefused {
		t.Fatalf("A's answer on B: welcome %+v, err %v", w, err)
	}
	// A's answer made under another protocol version, on A.
	old := ChallengeBytes(testHost, cred.ID, chA.Conn, chA.Nonce)
	old = append([]byte{0, 0, 0, 14}, append([]byte("txco-attach-v0"), old[4+len(ProtocolVersion):]...)...)
	if err := writeMsg(stA, Answer{Signature: ed25519.Sign(key, old)}); err != nil {
		t.Fatal(err)
	}
	if err := newMsgReader(stA).read(&w); err != nil || w.OK {
		t.Fatalf("an answer for another protocol version: welcome %+v, err %v", w, err)
	}
}

func TestChallengeBytesAreUnambiguous(t *testing.T) {
	a := ChallengeBytes("host", "cred", "conn", []byte("n"))
	for name, b := range map[string][]byte{
		"parts shifted": ChallengeBytes("hos", "tcred", "conn", []byte("n")),
		"another host":  ChallengeBytes("host2", "cred", "conn", []byte("n")),
		"another conn":  ChallengeBytes("host", "cred", "conn2", []byte("n")),
		"another nonce": ChallengeBytes("host", "cred", "conn", []byte("m")),
	} {
		if string(a) == string(b) {
			t.Fatalf("%s: the same bytes to sign", name)
		}
	}
	if !strings.Contains(string(a), ProtocolVersion) {
		t.Fatal("the protocol version is not signed")
	}
}

// A second connection for the name takes it over. The first is told it was
// replaced, which is final, and never receives another request.
func TestNewerConnectionReplacesOlder(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	var first, second atomic.Int64
	m1 := r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		first.Add(1)
		return echo(ctx, h)
	}})
	if _, err := r.exec(testSpec, "one", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		second.Add(1)
		return echo(ctx, h)
	}})
	err := <-m1.linkErr
	if !Final(err) {
		t.Fatalf("the first link ended with %v, want a final `replaced`", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.exec(testSpec, "two", 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if first.Load() != 1 || second.Load() != 5 {
		t.Fatalf("requests: first %d, second %d; want 1 and 5", first.Load(), second.Load())
	}
}

// A link whose claim was taken by a newer generation — on another node, so
// nobody has told it — still never receives a request: the holder reads the
// claim before forwarding each one.
func TestStaleLinkNeverReceivesARequest(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	var got atomic.Int64
	m := r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		got.Add(1)
		return echo(ctx, h)
	}})
	ctx := context.Background()
	mine, _, _ := r.claims.Read(ctx, testSpec.Tenant, cred.WorkspaceID)

	// Another node installs a newer generation for the same workspace.
	gen, _ := r.claims.NextGen(ctx, testSpec.Tenant, cred.WorkspaceID)
	if _, _, err := r.claims.Install(ctx, testSpec.Tenant, cred.WorkspaceID, Claim{Gen: gen, Node: "node-b", Conn: "conn-elsewhere"}); err != nil {
		t.Fatal(err)
	}
	// A request addressed to the OLD link, as a caller with a stale read of
	// the claim would address it.
	_, err := r.svc.Hub.Send(ctx, cred.WorkspaceID, mine.Conn, mine.Gen, Header{Kind: KindRequest, ID: "req_stale"})
	if code(err) != workspace.CodeAway {
		t.Fatalf("a request to a replaced link: %v, want away", err)
	}
	if got.Load() != 0 {
		t.Fatal("the replaced link received the request")
	}
	if err := <-m.linkErr; !Final(err) {
		t.Fatalf("the stale link ended with %v, want a final `replaced`", err)
	}
	// Its release, on the way out, did not touch the newer claim.
	cur, _, _ := r.claims.Read(ctx, testSpec.Tenant, cred.WorkspaceID)
	if cur.Gen != gen || cur.Released {
		t.Fatalf("the newer claim after the old link left = %+v", cur)
	}
}

// A revoke is in force at the very next request, with no notice delivered
// and the link still open.
func TestRevokeRefusesTheNextRequest(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	var got atomic.Int64
	m := r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		got.Add(1)
		return echo(ctx, h)
	}})
	ctx := context.Background()
	mine, _, _ := r.claims.Read(ctx, testSpec.Tenant, cred.WorkspaceID)

	// Only the DB write: no claim cleared, no link closed, no notice. This
	// is a revoke whose every courtesy was lost.
	if id, err := r.store.Revoke(ctx, cred.WorkspaceID); err != nil || id != cred.ID {
		t.Fatalf("revoke = %q, %v", id, err)
	}
	_, err := r.svc.Hub.Send(ctx, cred.WorkspaceID, mine.Conn, mine.Gen, Header{Kind: KindRequest, ID: "req_after_revoke"})
	if code(err) != workspace.CodeAway {
		t.Fatalf("a request after a revoke: %v, want away", err)
	}
	if got.Load() != 0 {
		t.Fatal("a revoked machine received a request")
	}
	if err := <-m.linkErr; !Final(err) {
		t.Fatalf("the revoked link ended with %v, want a final `revoked`", err)
	}
	// The key no longer opens a link at all.
	if _, ok := r.connect(cred.ID, key, testHost, handlerFunc{request: echo}); ok {
		t.Fatal("a revoked credential connected")
	}
	// The name is still attached: away, not the provider's.
	st, _ := r.svc.Status(ctx, testSpec)
	if !st.Enrolled || st.Attached {
		t.Fatalf("status after a revoke = %+v", st)
	}
}

func TestServiceRevokeClosesTheLink(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	m := r.mustConnect(cred, key, handlerFunc{request: echo})
	if err := r.svc.Revoke(context.Background(), testSpec); err != nil {
		t.Fatal(err)
	}
	if err := <-m.linkErr; !Final(err) {
		t.Fatalf("link ended with %v", err)
	}
	if _, err := r.exec(testSpec, "true", time.Second); code(err) != workspace.CodeAway {
		t.Fatalf("exec after revoke: %v", err)
	}
}

// The reply never comes. The caller hears `unknown`, the machine is told to
// stop, and nothing is sent a second time.
func TestLostReplyIsUnknownAndNotRunAgain(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	var runs atomic.Int64
	cancelled := make(chan struct{})
	r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		runs.Add(1)
		<-ctx.Done() // runs until cancelled, far past the caller's patience
		close(cancelled)
		return Reply{OK: true}
	}})
	// The machine is given a deadline a little short of the caller's; this
	// handler ignores neither, it just never answers in time. To get a true
	// "no reply", call Send directly with a deadline the header does not carry.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	mine, _, _ := r.claims.Read(context.Background(), testSpec.Tenant, cred.WorkspaceID)
	_, err := r.svc.Hub.Send(ctx, cred.WorkspaceID, mine.Conn, mine.Gen, Header{Kind: KindRequest, ID: "req_slow"})
	if code(err) != workspace.CodeUnknown {
		t.Fatalf("no reply by the deadline: %v, want unknown", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the machine was never told the caller gave up")
	}
	// The same id again, as a confused retry would send it.
	_, err = r.svc.Hub.Send(context.Background(), cred.WorkspaceID, mine.Conn, mine.Gen, Header{Kind: KindRequest, ID: "req_slow"})
	if code(err) != workspace.CodeUnknown {
		t.Fatalf("a repeated request id: %v, want unknown", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("the request ran %d times", runs.Load())
	}
}

// The connection dies under a running request: unknown, never away.
func TestConnectionLostMidRequestIsUnknown(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	started := make(chan struct{})
	m := r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		close(started)
		<-ctx.Done()
		return Reply{OK: true}
	}})
	errc := make(chan error, 1)
	go func() {
		_, err := r.exec(testSpec, "long", 10*time.Second)
		errc <- err
	}()
	<-started
	_ = m.client.Close()
	if err := <-errc; code(err) != workspace.CodeUnknown {
		t.Fatalf("connection lost mid-request: %v, want unknown", err)
	}
}

// A command that runs out of time is reported by the machine as a timeout,
// in time for the caller to hear it: `unknown` is kept for a reply that
// never comes.
func TestMachineReportsItsOwnTimeout(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	r.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		<-ctx.Done() // the request's own deadline
		b, _ := json.Marshal(ExecReply{Exit: -1, Error: &WireError{Code: "timeout", Message: "the command ran out of time"}})
		return Reply{OK: true, Payload: b}
	}})
	_, err := r.exec(testSpec, "sleep 60", 2*time.Second)
	if !errors.Is(err, workspace.ErrTimeout) {
		t.Fatalf("err = %v, want the workspace timeout", err)
	}
}

func TestMachineRefusesARepeatedID(t *testing.T) {
	var runs atomic.Int64
	l := &Link{Handler: handlerFunc{request: func(ctx context.Context, h Header) Reply {
		runs.Add(1)
		return Reply{OK: true}
	}}, running: map[string]context.CancelFunc{}, seen: map[string]time.Time{}}
	if rep := l.request(context.Background(), Header{ID: "req_1"}); !rep.OK {
		t.Fatalf("first = %+v", rep)
	}
	if rep := l.request(context.Background(), Header{ID: "req_1"}); rep.OK || rep.Error.Code != "unknown" {
		t.Fatalf("repeat = %+v", rep)
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d times", runs.Load())
	}
}

func TestStopReachesTheMachine(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	stopped := make(chan string, 1)
	r.mustConnect(cred, key, handlerFunc{request: echo, stopped: stopped})
	if err := r.svc.Stop(context.Background(), testSpec, "job_x"); err != nil {
		t.Fatal(err)
	}
	if job := <-stopped; job != "job_x" {
		t.Fatalf("stopped %q", job)
	}
}

// A job's end goes up the link and is acknowledged only once the chassis
// has recorded it. A revoked machine's word is not taken.
func TestCompletionIsAcknowledgedAfterItIsRecorded(t *testing.T) {
	r := newRig(t)
	cred, key := r.enroll(testSpec)
	var mu sync.Mutex
	var recorded []string
	fail := true
	r.svc.Hub.Complete = func(_ context.Context, c *Credential, opc, token string, result json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if c.ID != cred.ID {
			t.Errorf("completion attributed to %s", c.ID)
		}
		if fail {
			fail = false
			return errors.New("the store is down")
		}
		recorded = append(recorded, opc+"/"+token+"/"+string(result))
		return nil
	}
	m := r.mustConnect(cred, key, handlerFunc{request: echo})
	done := Header{OPC: "opc1", Token: "tok1", Job: "job_1", Payload: json.RawMessage(`{"exit":0}`)}
	if err := m.link.Complete(context.Background(), done); err == nil {
		t.Fatal("a completion the chassis failed to record was acknowledged")
	}
	if err := m.link.Complete(context.Background(), done); err != nil {
		t.Fatalf("the completion, sent again: %v", err)
	}
	mu.Lock()
	if len(recorded) != 1 || recorded[0] != `opc1/tok1/{"exit":0}` {
		t.Fatalf("recorded = %v", recorded)
	}
	mu.Unlock()

	if _, err := r.store.Revoke(context.Background(), cred.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := m.link.Complete(context.Background(), done); code(err) != ReasonRevoked {
		t.Fatalf("a revoked machine's completion: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("a revoked machine's completion was recorded: %v", recorded)
	}
}

func TestOversizeOutputIsCutAgainByTheChassis(t *testing.T) {
	big := strings.Repeat("x", 5000)
	b, _ := json.Marshal(ExecReply{Stdout: big, Stderr: "e"})
	res, err := resultOf(Reply{OK: true, Payload: b}, workspace.Limits{MaxOutputBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 1000 || !res.StdoutTruncated || res.StderrTruncated || string(res.Stderr) != "e" {
		t.Fatalf("stdout %d bytes truncated=%v, stderr %q truncated=%v", len(res.Stdout), res.StdoutTruncated, res.Stderr, res.StderrTruncated)
	}
}
