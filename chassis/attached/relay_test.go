package attached

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// memRelay joins nodes in one process: a request to a node's name reaches
// that node's handler, and a name nobody answers to is "no responders".
type memRelay struct {
	mu    sync.Mutex
	nodes map[string]LocalHandler
	drop  bool // lose the answer on its way back
}

func (m *memRelay) Forward(ctx context.Context, node string, env Envelope) (Reply, error) {
	m.mu.Lock()
	h, drop := m.nodes[node], m.drop
	m.mu.Unlock()
	if h == nil {
		return Reply{}, away("nothing answers for that node")
	}
	// Across the wire and back, as a bus would carry it.
	b, _ := json.Marshal(env)
	var got Envelope
	_ = json.Unmarshal(b, &got)
	ans := RelayAnswerOf(h.Forward(context.Background(), got))
	if drop {
		<-ctx.Done()
		return Reply{}, unknown("no answer from the machine's node in time")
	}
	b, _ = json.Marshal(ans)
	var back RelayAnswer
	_ = json.Unmarshal(b, &back)
	return back.Result()
}

func (m *memRelay) Notify(_ context.Context, node string, n Notice) error {
	m.mu.Lock()
	h := m.nodes[node]
	m.mu.Unlock()
	if h != nil {
		h.Notify(n)
	}
	return nil
}

func (m *memRelay) Shutdown(context.Context) error { return nil }

// twoNodes is one runtime DB and one KV, as a fleet shares them, with a
// service on each of two nodes. The machine dials node A.
func twoNodes(t *testing.T) (a *rig, b *Service, relay *memRelay) {
	t.Helper()
	a = newRig(t)
	b = NewService(a.store, a.claims, "node-b")
	relay = &memRelay{nodes: map[string]LocalHandler{}}
	relay.nodes["node-a"] = a.svc.Local()
	relay.nodes["node-b"] = b.Local()
	a.svc.SetRelay(relay)
	b.SetRelay(relay)
	return a, b, relay
}

func execOn(svc *Service, command string, timeout time.Duration) (workspace.ExecResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	comp, err := svc.Wake(ctx, testSpec, "")
	if err != nil {
		return workspace.ExecResult{Exit: -1}, err
	}
	return comp.Exec(ctx, workspace.ExecRequest{Command: command}, workspace.Limits{})
}

// The op runs on a node that does not hold the link.
func TestExecFromAnotherNode(t *testing.T) {
	a, b, _ := twoNodes(t)
	cred, key := a.enroll(testSpec)
	a.mustConnect(cred, key, handlerFunc{request: echo})

	res, err := execOn(b, "from node b", 5*time.Second)
	if err != nil || string(res.Stdout) != "ran: from node b" {
		t.Fatalf("exec = %+v, %v", res, err)
	}
	st, err := b.Status(context.Background(), testSpec)
	if err != nil || !st.Attached || st.Machine != "MacBook" {
		t.Fatalf("status from the other node = %+v, %v", st, err)
	}
	// A stop travels the same way.
	if err := b.Stop(context.Background(), testSpec, "job_x"); err != nil {
		t.Fatalf("stop from the other node: %v", err)
	}
}

// The node the claim names is gone: nothing was delivered, so `away`.
func TestHolderNodeGoneIsAway(t *testing.T) {
	a, b, relay := twoNodes(t)
	cred, key := a.enroll(testSpec)
	a.mustConnect(cred, key, handlerFunc{request: echo})
	relay.mu.Lock()
	delete(relay.nodes, "node-a") // the Machine died; its claim has not lapsed yet
	relay.mu.Unlock()
	if _, err := execOn(b, "x", 2*time.Second); code(err) != workspace.CodeAway {
		t.Fatalf("exec to a node that is gone: %v, want away", err)
	}
}

// The holder took the request and its answer never came back: the command
// may have run. `unknown`, and it is not sent again.
func TestAnswerLostBetweenNodesIsUnknown(t *testing.T) {
	a, b, relay := twoNodes(t)
	cred, key := a.enroll(testSpec)
	var runs atomic.Int64
	a.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		runs.Add(1)
		return echo(ctx, h)
	}})
	relay.mu.Lock()
	relay.drop = true
	relay.mu.Unlock()
	if _, err := execOn(b, "x", 600*time.Millisecond); code(err) != workspace.CodeUnknown {
		t.Fatalf("an answer lost between nodes: %v, want unknown", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("the request ran %d times", runs.Load())
	}
}

// The holding node checks the same three things for a request that came
// over the relay as for its own: a stale link, named by a sender with an
// old read of the claim, gets nothing.
func TestRelayedRequestToAStaleLinkIsRefused(t *testing.T) {
	a, b, _ := twoNodes(t)
	cred, key := a.enroll(testSpec)
	var got atomic.Int64
	a.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		got.Add(1)
		return echo(ctx, h)
	}})
	ctx := context.Background()
	old, _, _ := a.claims.Read(ctx, testSpec.Tenant, cred.WorkspaceID)
	// Revoked in the DB only: no notice, the link still up on node A.
	if _, err := a.store.Revoke(ctx, cred.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	_, err := b.Forward(ctx, "node-a", cred.WorkspaceID, old.Conn, old.Gen, Header{Kind: KindRequest, ID: "req_r"})
	if code(err) != workspace.CodeAway || got.Load() != 0 {
		t.Fatalf("a relayed request after a revoke: %v, delivered %d", err, got.Load())
	}
}

// A machine reconnects to another node. The node it left is told, and its
// old link is closed as replaced; requests follow the claim to the new node.
func TestReconnectToAnotherNode(t *testing.T) {
	a, b, _ := twoNodes(t)
	cred, key := a.enroll(testSpec)
	first := a.mustConnect(cred, key, handlerFunc{request: echo})

	// The same machine, dialing again and landing on node B.
	onB := &rig{t: t, db: a.db, ws: a.ws, store: a.store, claims: a.claims, svc: b}
	var served atomic.Int64
	onB.mustConnect(cred, key, handlerFunc{request: func(ctx context.Context, h Header) Reply {
		served.Add(1)
		return echo(ctx, h)
	}})
	if err := <-first.linkErr; !Final(err) {
		t.Fatalf("the link left on node A ended with %v, want a final `replaced`", err)
	}
	// From either node, the request reaches the link on B.
	for _, from := range []*Service{a.svc, b} {
		if res, err := execOn(from, "after the move", 5*time.Second); err != nil || string(res.Stdout) != "ran: after the move" {
			t.Fatalf("exec = %+v, %v", res, err)
		}
	}
	if served.Load() != 2 {
		t.Fatalf("the link on node B served %d requests", served.Load())
	}
}

// A revoke on one node closes the link on another.
func TestRevokeFromAnotherNodeClosesTheLink(t *testing.T) {
	a, b, _ := twoNodes(t)
	cred, key := a.enroll(testSpec)
	m := a.mustConnect(cred, key, handlerFunc{request: echo})
	if err := b.Revoke(context.Background(), testSpec); err != nil {
		t.Fatal(err)
	}
	if err := <-m.linkErr; !Final(err) {
		t.Fatalf("link ended with %v", err)
	}
}

// A notice with a reason no chassis gives closes nothing.
func TestNoticeWithAMadeUpReasonIsIgnored(t *testing.T) {
	a, _, _ := twoNodes(t)
	cred, key := a.enroll(testSpec)
	a.mustConnect(cred, key, handlerFunc{request: echo})
	claim, _, _ := a.claims.Read(context.Background(), testSpec.Tenant, cred.WorkspaceID)
	a.svc.Local().Notify(Notice{WorkspaceID: cred.WorkspaceID, Conn: claim.Conn, Reason: "shutdown"})
	if res, err := a.exec(testSpec, "still here", 5*time.Second); err != nil || string(res.Stdout) != "ran: still here" {
		t.Fatalf("exec after a made-up notice = %+v, %v", res, err)
	}
}

func TestRelayAnswerFit(t *testing.T) {
	big := func(out, errOut int) RelayAnswer {
		b, _ := json.Marshal(ExecReply{Exit: 3, Stdout: strings.Repeat("\x01", out), Stderr: strings.Repeat("e", errOut)})
		return RelayAnswer{Reply: Reply{OK: true, Payload: b}}
	}
	size := func(a RelayAnswer) int { b, _ := json.Marshal(a); return len(b) }

	small := big(10, 10)
	if got := small.Fit(1 << 20); size(got) != size(small) {
		t.Fatal("an answer that fits was changed")
	}
	// Control bytes escape to six bytes each: the size is not the length.
	fit := big(400_000, 50_000).Fit(1 << 20)
	if size(fit) > 1<<20 {
		t.Fatalf("still %d bytes", size(fit))
	}
	var er ExecReply
	if err := json.Unmarshal(fit.Reply.Payload, &er); err != nil {
		t.Fatal(err)
	}
	if er.Exit != 3 || !er.StdoutTruncated || len(er.Stdout) == 0 || er.StderrTruncated || len(er.Stderr) != 50_000 {
		t.Fatalf("exit %d, stdout %d (cut %v), stderr %d (cut %v)", er.Exit, len(er.Stdout), er.StdoutTruncated, len(er.Stderr), er.StderrTruncated)
	}
	// An error passes through untouched, and survives the wire.
	e := RelayAnswerOf(Reply{}, unknown("lost"))
	if _, err := e.Fit(10).Result(); code(err) != workspace.CodeUnknown {
		t.Fatalf("an error answer: %v", err)
	}
	// Something that cannot be cut says so, rather than being sent broken.
	odd := RelayAnswer{Reply: Reply{OK: true, Payload: json.RawMessage(`"` + strings.Repeat("x", 5000) + `"`)}}
	if _, err := odd.Fit(1000).Result(); code(err) != "provider" {
		t.Fatalf("an uncuttable answer: %v", err)
	}
}

func TestListIdleForOneProvider(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour).UTC()
	for _, row := range []workspace.Row{
		{Tenant: "acme", Stack: "loop", Name: "pony/a", Provider: "sprites", ProviderRef: "s1", LastUsedAt: old, CreatedAt: old},
		{Tenant: "acme", Stack: "loop", Name: "computer/a", Provider: workspace.ProviderAttached, ProviderRef: "atc-x", LastUsedAt: old, CreatedAt: old},
		{Tenant: "acme", Stack: "loop", Name: "pony/b", Provider: "local", ProviderRef: "/x", LastUsedAt: old, CreatedAt: old},
	} {
		if err := r.ws.Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := r.ws.ListIdleFor(ctx, "sprites", time.Now().Add(-time.Hour), 10)
	if err != nil || len(rows) != 1 || rows[0].Name != "pony/a" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}
