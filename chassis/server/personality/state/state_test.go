package state

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	chstate "github.com/loremlabs/thanks-computer/chassis/state"
)

// The dispatcher is driven one poll at a time against a fake bus: a
// goroutine that takes envelopes off pu.Bus and plays the processor's
// part — accepts, denies, answers, or stays silent.

type harness struct {
	t     *testing.T
	ctx   context.Context
	c     *Controller
	store *chstate.Store
	bus   chan *event.Envelope
	wake  chan struct{}
	clock time.Time
}

func newHarness(t *testing.T, mod func(*config.Config)) *harness {
	t.Helper()
	store, err := chstate.Open("sqlite", chstate.Config{DBPath: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	conf := config.Config{
		Personalities:      "web,state",
		Fqdn:               "node-a",
		StatePeriod:        1,
		StateAcceptTimeout: 1,
		StateStaleAfter:    2,
		StateMaxAttempts:   3,
	}
	if mod != nil {
		mod(&conf)
	}
	bus := make(chan *event.Envelope) // unbuffered: "refused" = nobody reading
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: bus}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wake := make(chan struct{}, 1)
	c := NewController(ctx, pu, store, wake)
	c.subscribed = func(context.Context, string) (bool, error) { return true, nil }
	h := &harness{t: t, ctx: ctx, c: c, store: store, bus: bus, wake: wake,
		clock: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	store.SetClock(func() time.Time { return h.clock })
	return h
}

// seed commits one transition on (machine, id) and returns its event.
func (h *harness) seed(tenant, id, from, to string) chstate.Event {
	h.t.Helper()
	ctx := context.Background()
	cur, err := h.store.Get(ctx, tenant, "m", id)
	if err != nil {
		if cur, err = h.store.Create(ctx, chstate.CreateReq{Tenant: tenant, Machine: "m", ID: id, State: from}); err != nil {
			h.t.Fatal(err)
		}
	}
	_, ev, err := h.store.Transition(ctx, chstate.TransitionReq{Tenant: tenant, Machine: "m", ID: id,
		From: cur.State, To: to, ExpectedVersion: cur.Version, Cause: chstate.Cause{Source: "web", Trace: "rid_cause"}})
	if err != nil {
		h.t.Fatal(err)
	}
	return ev
}

func (h *harness) event(id string) chstate.Event {
	h.t.Helper()
	ev, err := h.store.GetEvent(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ev
}

// reader plays the processor for every envelope on the bus. It records
// what it saw, in order.
type reader struct {
	mu   sync.Mutex
	seen []*event.Envelope
}

func (h *harness) read(handle func(env *event.Envelope)) *reader {
	r := &reader{}
	go func() {
		for {
			select {
			case env := <-h.bus:
				r.mu.Lock()
				r.seen = append(r.seen, env)
				r.mu.Unlock()
				go handle(env)
			case <-h.ctx.Done():
				return
			}
		}
	}()
	return r
}

func (r *reader) envelopes() []*event.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*event.Envelope(nil), r.seen...)
}

func tenantOf(env *event.Envelope) string {
	return gjson.Get(env.Payload.Raw, "_txc.state.tenant").String()
}

func accept(env *event.Envelope) {
	env.Accepted <- event.Acceptance{Tenant: tenantOf(env), Stack: "_state"}
}

func TestAcceptedIsDoneWhileRunStillHolds(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "working", "waiting")
	release := make(chan struct{})
	r := h.read(func(env *event.Envelope) {
		accept(env)
		<-release // the run goes on; the dispatcher must not wait for it
		env.ResCh <- event.Payload{Raw: env.Payload.Raw, Type: event.JSON}
	})
	if !h.c.poll(h.ctx) {
		t.Fatal("poll claimed nothing")
	}
	got := h.event(ev.ID)
	if got.Status != chstate.StatusDone || got.DeliveredRid == "" || got.DeliveredAt.IsZero() {
		t.Fatalf("after acceptance = %+v (the run is still holding)", got)
	}
	envs := r.envelopes()
	if len(envs) != 1 {
		t.Fatalf("envelopes = %d", len(envs))
	}
	env := envs[0]
	raw := env.Payload.Raw
	if env.Src != "state" || gjson.Get(raw, "_txc.src").String() != "state" {
		t.Fatalf("src = %q / %s", env.Src, raw)
	}
	st := gjson.Get(raw, "_txc.state")
	if st.Get("tenant").String() != "acme" || st.Get("event_id").String() != ev.ID || st.Get("machine").String() != "m" ||
		st.Get("id").String() != "t1" || st.Get("version").Int() != 2 || st.Get("from").String() != "working" ||
		st.Get("to").String() != "waiting" || st.Get("attempt").Int() != 1 || st.Get("node").String() != "node-a" ||
		st.Get("fired_at").String() == "" || st.Get("committed_at").String() == "" ||
		st.Get("cause.source").String() != "web" || st.Get("cause.trace").String() != "rid_cause" {
		t.Fatalf("@state = %s", st.Raw)
	}
	rid := gjson.Get(raw, "_txc.rid").String()
	if rid == "" || rid != env.Rid || got.DeliveredRid != rid {
		t.Fatalf("rid = %q env=%q delivered=%q", rid, env.Rid, got.DeliveredRid)
	}
	if ctxRid, _ := env.Ctx.Value(config.CtxKeyRid).(string); ctxRid != rid {
		t.Fatalf("ctx rid = %q, want %q", ctxRid, rid)
	}
	if _, hasDeadline := env.Ctx.Deadline(); !hasDeadline {
		t.Fatal("the run context must carry the run timeout")
	}
	if gjson.Get(raw, "_ts").String() == "" {
		t.Fatal("no _ts")
	}
	close(release)
	// A second poll finds nothing: done is terminal.
	if h.c.poll(h.ctx) {
		t.Fatal("done event re-claimed")
	}
}

func TestDeniedBeforeAcceptanceRetriesUncounted(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	h.read(func(env *event.Envelope) {
		env.ResCh <- event.Payload{Raw: admission.MarkDenied(env.Payload.Raw,
			admission.Decision{Status: 402, Reason: "payment_required"}, tenantOf(env)), Type: event.JSON}
	})
	h.c.poll(h.ctx)
	got := h.event(ev.ID)
	if got.Status != chstate.StatusPending || got.Attempts != 0 || got.ClaimedBy != "" {
		t.Fatalf("after 402 = %+v", got)
	}
	if want := h.clock.Add(deniedBackoff); !got.NextAttemptAt.Equal(want) {
		t.Fatalf("next = %v, want %v", got.NextAttemptAt, want)
	}
	if got.LastError != "admission denied: payment_required" {
		t.Fatalf("last_error = %q", got.LastError)
	}
	// 429 waits retry_after (or the floor), in a fresh harness so the 402
	// reader above cannot take the envelope.
	h3 := newHarness(t, nil)
	ev3 := h3.seed("acme", "t3", "a", "b")
	h3.read(func(env *event.Envelope) {
		raw := admission.MarkDenied(env.Payload.Raw, admission.Decision{Status: 429, Reason: "rate_limited"}, tenantOf(env))
		raw, _ = sjson.Set(raw, "_txc.admission.retry_after", 7)
		env.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	h3.c.poll(h3.ctx)
	got = h3.event(ev3.ID)
	if got.Status != chstate.StatusPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(h3.clock.Add(7*time.Second)) {
		t.Fatalf("after 429 = %+v", got)
	}
}

func TestResponseWithoutAcceptanceSkips(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	h.read(func(env *event.Envelope) {
		env.ResCh <- event.Payload{Raw: env.Payload.Raw, Type: event.JSON} // never left _sys
	})
	h.c.poll(h.ctx)
	got := h.event(ev.ID)
	if got.Status != chstate.StatusSkipped || got.LastError == "" {
		t.Fatalf("after unaccepted response = %+v", got)
	}
}

func TestNoSignalWithinAcceptTimeoutRetriesCounted(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	h.read(func(env *event.Envelope) {}) // takes it and says nothing
	start := time.Now()
	h.c.poll(h.ctx)
	if time.Since(start) < time.Second {
		t.Fatal("returned before the accept timeout")
	}
	got := h.event(ev.ID)
	if got.Status != chstate.StatusPending || got.Attempts != 1 || got.LastError != "accept timeout" {
		t.Fatalf("after timeout = %+v", got)
	}
	if want := h.clock.Add(retryBackoff(1)); !got.NextAttemptAt.Equal(want) {
		t.Fatalf("next = %v, want %v", got.NextAttemptAt, want)
	}
}

func TestBusRefusedRetriesUncounted(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	// No reader at all.
	h.c.poll(h.ctx)
	got := h.event(ev.ID)
	if got.Status != chstate.StatusPending || got.Attempts != 0 || got.LastError != "bus refused handoff" {
		t.Fatalf("after refused handoff = %+v", got)
	}
}

func TestReclaimRepresentsSameEventAsAttempt2(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	// A node that claimed and died.
	if won, _ := h.store.ClaimDue(context.Background(), "node-dead", 10); len(won) != 1 {
		t.Fatal("seed claim")
	}
	r := h.read(accept)
	if h.c.poll(h.ctx) {
		t.Fatal("a fresh claim must not be reclaimed")
	}
	h.clock = h.clock.Add(3 * time.Second) // past --state-stale-after (2)
	if !h.c.poll(h.ctx) {
		t.Fatal("stale claim not reclaimed and presented")
	}
	got := h.event(ev.ID)
	if got.Status != chstate.StatusDone || got.Attempts != 1 {
		t.Fatalf("after reclaim+accept = %+v", got)
	}
	envs := r.envelopes()
	if len(envs) != 1 || gjson.Get(envs[0].Payload.Raw, "_txc.state.event_id").String() != ev.ID ||
		gjson.Get(envs[0].Payload.Raw, "_txc.state.attempt").Int() != 2 {
		t.Fatalf("re-presentation = %d envelopes, %s", len(envs), envs[0].Payload.Raw)
	}
}

func TestNoStateStackSkips(t *testing.T) {
	h := newHarness(t, nil)
	ev := h.seed("acme", "t1", "a", "b")
	h.c.subscribed = func(context.Context, string) (bool, error) { return false, nil }
	r := h.read(accept)
	h.c.poll(h.ctx)
	got := h.event(ev.ID)
	if got.Status != chstate.StatusSkipped || got.LastError != "no active _state stack" {
		t.Fatalf("no stack = %+v", got)
	}
	if len(r.envelopes()) != 0 {
		t.Fatal("presented without a subscriber")
	}
	// Not replayed when a stack appears.
	h.c.subscribed = func(context.Context, string) (bool, error) { return true, nil }
	if h.c.poll(h.ctx) {
		t.Fatal("skipped event replayed")
	}
}

func TestPerRecordOrderAcrossPolls(t *testing.T) {
	h := newHarness(t, nil)
	v2 := h.seed("acme", "t1", "a", "b")
	v3 := h.seed("acme", "t1", "b", "c")
	other := h.seed("acme", "t2", "a", "b")
	r := h.read(accept)
	if !h.c.poll(h.ctx) {
		t.Fatal("first poll claimed nothing")
	}
	ids := func() []string {
		var out []string
		for _, e := range r.envelopes() {
			out = append(out, gjson.Get(e.Payload.Raw, "_txc.state.event_id").String())
		}
		return out
	}
	first := ids()
	if len(first) != 2 || !(first[0] == v2.ID && first[1] == other.ID || first[0] == other.ID && first[1] == v2.ID) {
		t.Fatalf("first pass = %v, want v2 and t2 only", first)
	}
	if h.event(v3.ID).Status != chstate.StatusPending {
		t.Fatal("v3 moved before v2 was accepted")
	}
	if !h.c.poll(h.ctx) {
		t.Fatal("second poll claimed nothing")
	}
	all := ids()
	if len(all) != 3 || all[2] != v3.ID || h.event(v3.ID).Status != chstate.StatusDone {
		t.Fatalf("second pass = %v", all)
	}
}

func TestAttemptCapMarksDead(t *testing.T) {
	h := newHarness(t, nil) // max attempts 3
	ev := h.seed("acme", "t1", "a", "b")
	h.read(func(env *event.Envelope) {}) // silent every time
	for i := 1; i <= 3; i++ {
		h.clock = h.clock.Add(2 * time.Hour) // past any backoff
		if !h.c.poll(h.ctx) {
			t.Fatalf("pass %d claimed nothing", i)
		}
	}
	got := h.event(ev.ID)
	if got.Status != chstate.StatusDead || got.Attempts != 3 {
		t.Fatalf("after 3 timeouts = %+v", got)
	}
	h.clock = h.clock.Add(2 * time.Hour)
	if h.c.poll(h.ctx) {
		t.Fatal("dead event presented")
	}
}

func TestRestartPresentsEventsCommittedBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := chstate.Open("sqlite", chstate.Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec, err := first.Create(ctx, chstate.CreateReq{Tenant: "acme", Machine: "m", ID: "t1", State: "a"})
	if err != nil {
		t.Fatal(err)
	}
	_, ev, err := first.Transition(ctx, chstate.TransitionReq{Tenant: "acme", Machine: "m", ID: "t1", From: "a", To: "b", ExpectedVersion: rec.Version})
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close() // the "crash": nothing presented it

	second, err := chstate.Open("sqlite", chstate.Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	bus := make(chan *event.Envelope)
	pu := &processor.Unit{Conf: config.Config{Personalities: "state", StateAcceptTimeout: 1}, Logger: zap.NewNop(), Bus: bus}
	c := NewController(ctx, pu, second, nil)
	c.subscribed = func(context.Context, string) (bool, error) { return true, nil }
	go func() { accept(<-bus) }()
	if !c.poll(ctx) {
		t.Fatal("restart presented nothing")
	}
	got, _ := second.GetEvent(ctx, ev.ID)
	if got.Status != chstate.StatusDone {
		t.Fatalf("after restart = %+v", got)
	}
}

func TestWakeNudgesTheLoop(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.StatePeriod = 60 }) // the tick will not come in time
	h.read(accept)
	h.c.Start()
	defer h.c.Stop()
	ev := h.seed("acme", "t1", "a", "b")
	h.wake <- struct{}{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.event(ev.ID).Status == chstate.StatusDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not presented on the nudge: %+v", h.event(ev.ID))
}

func TestInertWithoutPersonalityOrStore(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web"}, Logger: zap.NewNop()}
	c := NewController(context.Background(), pu, nil, nil)
	c.Start()
	c.Stop()
	store, _ := chstate.Open("sqlite", chstate.Config{DBPath: filepath.Join(t.TempDir(), "s.db")})
	defer store.Close()
	c = NewController(context.Background(), pu, store, nil)
	if c.enabled() {
		t.Fatal("enabled without the personality")
	}
	pu.Conf.Personalities = "state"
	c = NewController(context.Background(), pu, nil, nil)
	if c.enabled() {
		t.Fatal("enabled without a store")
	}
}

func TestRetryBackoff(t *testing.T) {
	for i, want := range []time.Duration{retryBase, 2 * retryBase, 4 * retryBase} {
		if got := retryBackoff(i + 1); got != want {
			t.Errorf("attempt %d = %v, want %v", i+1, got, want)
		}
	}
	if got := retryBackoff(40); got != retryCap {
		t.Errorf("capped = %v, want %v", got, retryCap)
	}
}
