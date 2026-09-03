package imap

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The two inlet lanes (§25.8), the same pair the DNS head has:
//
//   - observe: a committed client mutation becomes an envelope dispatched
//     into the account tenant's `_imap` stack AFTER the protocol reply —
//     fire-and-forget, bounded queue, sampled. A slow or absent stack can
//     never delay a client.
//   - answer: a mailbox whose policy says `stack` for the verb dispatches
//     BEFORE commit and waits (bounded) for `@imap.res`; absent or
//     `ok: false` ⇒ NO. A continuation suspend counts as ok (processor
//     synthesizes it, mirroring the LMTP 250).
//
// Routing follows the dns idiom: the head stamps the trusted tenant slug
// in `_txc.imap.tenant` and detectTenantBody proposes `_imap/0`. The
// `_txc.imap.*` facts are read-only for stacks; `@imap.res` is the one
// author-writable path.

const (
	subscriptionStack = "_imap"

	observeQueueDepth      = 1024
	observeDispatchTimeout = 60 * time.Second
	answerRunTimeout       = 60 * time.Second

	phaseObserve = "observe"
	phaseAnswer  = "answer"
)

// mboxRef is the identity of a mailbox on the envelope.
type mboxRef struct {
	ID   string
	Name string
	Role string
}

func refOf(mb chimap.Mailbox) mboxRef { return mboxRef{ID: mb.ID, Name: mb.Name, Role: mb.Role} }

// objectRef is one affected message on the envelope.
type objectRef struct {
	UID       uint32   `json:"uid"`
	ObjectKey string   `json:"object_key,omitempty"`
	SHA256    string   `json:"sha256"`
	Flags     []string `json:"flags"`
}

// msgFacts is the parse of an appended message (append only; no bytes —
// the CAS holds them, `parts[].sha256` addresses them).
type msgFacts struct {
	ID      string            `json:"id,omitempty"`
	Date    string            `json:"date,omitempty"`
	Subject string            `json:"subject,omitempty"`
	From    []addr            `json:"from,omitempty"`
	To      []addr            `json:"to,omitempty"`
	Cc      []addr            `json:"cc,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Text    string            `json:"text,omitempty"`
	HTML    string            `json:"html,omitempty"`
	SHA256  string            `json:"sha256"`
	Size    int64             `json:"size"`
	Parts   []partFacts       `json:"parts"`
}

type addr struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr"`
}

type partFacts struct {
	N      int    `json:"n"`
	Name   string `json:"name,omitempty"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// mutation is one client mutation as the lanes see it.
type mutation struct {
	tenant   string
	account  string
	op       string // append | move | copy | expunge | flags | create | delete | rename
	mailbox  mboxRef
	dest     *mboxRef
	uid      uint32
	objects  []objectRef
	msg      *msgFacts
	clientIP string
}

// answer is the translated `@imap.res` of an answer-lane run.
type answer struct {
	ok        bool
	code      string // cannot | limit | unavailable | ""
	msg       string
	flags     []string
	objectKey string
	outcome   string // metric label
}

// lanes owns the observe queue/workers and the answer dispatch.
type lanes struct {
	pu       *processor.Unit
	ctx      context.Context
	node     string
	sample   uint64
	inflight int
	deadline time.Duration

	// subscribed reports whether a tenant has an active `_imap` stack.
	// Defaults to a mirror-snapshot query; tests inject.
	subscribed func(tenant string) bool

	seq     atomic.Uint64
	dropped atomic.Uint64
	queue   chan mutation

	observeOut metric.Int64Counter
	answerOut  metric.Int64Counter

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newLanes(ctx context.Context, pu *processor.Unit) *lanes {
	if pu == nil {
		return nil
	}
	node, _ := os.Hostname()
	deadline, err := time.ParseDuration(pu.Conf.IMAPRespTimeout)
	if err != nil || deadline <= 0 {
		deadline = 30 * time.Second
	}
	inflight := pu.Conf.IMAPObserveMaxInflight
	if inflight <= 0 {
		inflight = 1
	}
	l := &lanes{
		pu:       pu,
		ctx:      ctx,
		node:     node,
		sample:   uint64(pu.Conf.IMAPObserveSample),
		inflight: inflight,
		deadline: deadline,
		queue:    make(chan mutation, observeQueueDepth),
	}
	l.subscribed = l.snapshotSubscribed
	if pu.Mc != nil && pu.Mc.Meter != nil {
		l.observeOut, _ = pu.Mc.Meter.Int64Counter("chassis.imap.observe",
			metric.WithDescription("IMAP observe-lane dispatches into _imap stacks, by outcome"),
			metric.WithUnit("1"))
		l.answerOut, _ = pu.Mc.Meter.Int64Counter("chassis.imap.answer",
			metric.WithDescription("IMAP answer-lane dispatches into _imap stacks, by outcome"),
			metric.WithUnit("1"))
	}
	return l
}

func (l *lanes) start() {
	if l == nil {
		return
	}
	ctx, cancel := context.WithCancel(l.ctx)
	l.cancel = cancel
	for i := 0; i < l.inflight; i++ {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-l.queue:
					l.dispatchObserve(ctx, m)
				}
			}
		}()
	}
}

func (l *lanes) stop() {
	if l == nil {
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
}

// snapshotSubscribed asks the dbcache mirror whether the tenant has an
// active `_imap` stack. Mutations are human-paced, so one indexed query
// per mutation is fine; a nil mirror (tests, embedders) means nobody is
// subscribed.
func (l *lanes) snapshotSubscribed(tenant string) bool {
	if l.pu.Dbc == nil {
		return false
	}
	db := l.pu.Dbc.Snapshot()
	if db == nil {
		return false
	}
	var one int
	err := db.QueryRow(`SELECT 1 FROM stacks s
	                       JOIN tenants t ON t.tenant_id = s.tenant_id
	                      WHERE t.slug = ? AND t.revoked_at IS NULL
	                        AND s.name = ? AND s.active_version IS NOT NULL
	                      LIMIT 1`, tenant, subscriptionStack).Scan(&one)
	if err != nil && err != sql.ErrNoRows && l.pu.Logger != nil {
		l.pu.Logger.Warn("imap: subscription lookup failed", zap.String("tenant", tenant), zap.String("err", err.Error()))
	}
	return err == nil
}

// observe is the post-commit entry point: sampling, subscription, then a
// non-blocking enqueue.
func (l *lanes) observe(m mutation) {
	if l == nil || l.sample == 0 {
		return
	}
	if !l.subscribed(m.tenant) {
		return
	}
	if l.sample > 1 && l.seq.Add(1)%l.sample != 0 {
		return
	}
	select {
	case l.queue <- m:
	default:
		l.dropped.Add(1)
		l.record(l.observeOut, "dropped")
	}
}

func (l *lanes) dispatchObserve(ctx context.Context, m mutation) {
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(m, phaseObserve, rid, l.node, time.Now())

	dctx, cancel := context.WithTimeout(ctx, observeDispatchTimeout)
	defer cancel()
	dctx = context.WithValue(dctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(dctx, payload, resCh, "imap")
	select {
	case l.pu.Bus <- envelope:
	case <-dctx.Done():
		l.record(l.observeOut, "bus_timeout")
		return
	}
	select {
	case res := <-resCh:
		if _, _, denied := admission.Denied(res.Raw); denied {
			l.record(l.observeOut, "denied")
			return
		}
		l.record(l.observeOut, "ok")
	case <-dctx.Done():
		l.record(l.observeOut, "timeout")
	}
}

// ask is the answer lane: dispatch before commit and wait up to the
// deadline for `@imap.res`. Default-deny on every failure mode; the
// outcome label says which.
func (l *lanes) ask(m mutation) answer {
	if l == nil {
		return answer{outcome: "no_lanes", code: "unavailable", msg: "No stack to ask"}
	}
	if !l.subscribed(m.tenant) {
		// Policy says ask the stack, and there is none: deny, loudly
		// enough for the operator (a mailbox policy of `stack` without an
		// `_imap` stack is a misconfiguration, not a client error).
		l.record(l.answerOut, "unsubscribed")
		return answer{outcome: "unsubscribed", code: "unavailable", msg: "No _imap stack to decide"}
	}
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(m, phaseAnswer, rid, l.node, time.Now())

	rctx, cancel := context.WithTimeout(l.ctx, answerRunTimeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(rctx, payload, resCh, "imap")

	deadline := time.NewTimer(l.deadline)
	defer deadline.Stop()
	select {
	case l.pu.Bus <- envelope:
	case <-deadline.C:
		cancel()
		l.record(l.answerOut, "bus_timeout")
		return answer{outcome: "bus_timeout", code: "unavailable", msg: "Stack unavailable"}
	}
	select {
	case res := <-resCh:
		cancel()
		a := translateAnswer(res.Raw)
		l.record(l.answerOut, a.outcome)
		return a
	case <-deadline.C:
		// The run keeps its own context; a late answer is discarded.
		go func() {
			defer cancel()
			select {
			case <-resCh:
			case <-rctx.Done():
			}
		}()
		l.record(l.answerOut, "deadline")
		return answer{outcome: "deadline", code: "unavailable", msg: "Stack did not answer in time"}
	}
}

// translateAnswer reads `_txc.imap.res` off a response. Default-deny: no
// res, or ok not true, is a NO with the stack's message (if any).
func translateAnswer(raw string) answer {
	if _, reason, denied := admission.Denied(raw); denied {
		return answer{outcome: "denied", code: "unavailable", msg: "Service unavailable: " + reason}
	}
	res := gjson.Get(raw, "_txc.imap.res")
	if !res.Exists() {
		return answer{outcome: "absent", msg: "Refused by the mailbox's stack"}
	}
	a := answer{
		ok:        res.Get("ok").Type == gjson.True,
		msg:       res.Get("msg").String(),
		objectKey: res.Get("object_key").String(),
	}
	switch c := res.Get("code").String(); c {
	case "cannot", "limit", "unavailable":
		a.code = c
	}
	for _, f := range res.Get("flags").Array() {
		if s := strings.TrimSpace(f.String()); s != "" {
			a.flags = append(a.flags, s)
		}
	}
	if a.ok {
		a.outcome = "ok"
	} else {
		a.outcome = "refused"
		if a.msg == "" {
			a.msg = "Refused by the mailbox's stack"
		}
	}
	return a
}

func (l *lanes) record(c metric.Int64Counter, outcome string) {
	if l == nil || c == nil {
		return
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.imap.outcome", outcome)))
}

// buildEnvelope renders a mutation as the inlet envelope. Pure, so the
// shape is unit-testable. Every `_txc.imap.*` fact is head-stamped.
func buildEnvelope(m mutation, phase, rid, node string, now time.Time) string {
	b := jsonx.NewObject()
	b.Set("_txc.src", "imap")
	b.Set("_txc.rid", rid)
	b.Set("_ts", now.UTC().Format(time.RFC3339))
	b.Set("_txc.imap.tenant", m.tenant)
	b.Set("_txc.imap.account", m.account)
	b.Set("_txc.imap.phase", phase)
	b.Set("_txc.imap.op", m.op)
	b.Set("_txc.imap.node", node)
	setRef := func(path string, r mboxRef) {
		if r.ID != "" {
			b.Set(path+".id", r.ID)
		}
		b.Set(path+".name", r.Name)
		b.Set(path+".role", r.Role)
	}
	setRef("_txc.imap.mailbox", m.mailbox)
	if m.dest != nil {
		setRef("_txc.imap.dest", *m.dest)
	}
	if m.uid > 0 {
		b.Set("_txc.imap.uid", m.uid)
	}
	if m.objects != nil {
		objs := make([]objectRef, len(m.objects))
		copy(objs, m.objects)
		for i := range objs {
			if objs[i].Flags == nil {
				objs[i].Flags = []string{}
			}
		}
		raw, _ := json.Marshal(objs)
		b.SetRaw("_txc.imap.objects", string(raw))
	}
	if m.msg != nil {
		raw, _ := json.Marshal(m.msg)
		b.SetRaw("_txc.imap.msg", string(raw))
	}
	if m.clientIP != "" {
		b.Set("_txc.client.ip", m.clientIP)
	}
	return b.String()
}
