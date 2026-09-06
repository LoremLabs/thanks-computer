package contacts

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
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The two inlet lanes, the pair the IMAP and calendar heads have:
//
//   - observe: a committed client mutation becomes an envelope dispatched
//     into the account tenant's `_contacts` stack AFTER the reply —
//     fire-and-forget, bounded queue, sampled. A slow or absent stack can
//     never delay a client.
//   - answer: an address book whose policy says `stack` for the verb
//     dispatches BEFORE commit and waits (bounded) for `@contacts.res`;
//     absent or `ok: false` ⇒ refused. The answer may carry a rewrite
//     (`@contacts.res.card` or `.vcard`) the head commits instead of the
//     client's bytes.
//
// The head stamps the trusted tenant slug in `_txc.contacts.tenant` and
// detectTenantBody proposes `_contacts/0`. The `_txc.contacts.*` facts are
// read-only for stacks; `@contacts.res` is the one author-writable path.

const (
	subscriptionStack = "_contacts"

	observeQueueDepth      = 1024
	observeDispatchTimeout = 60 * time.Second
	answerRunTimeout       = 60 * time.Second

	phaseObserve = "observe"
	phaseAnswer  = "answer"

	opPut           = "put"
	opDelete        = "delete"
	opMkaddressbook = "mkaddressbook"
	opRemove        = "remove"
	opProppatch     = "proppatch"
)

// abRef is the identity of an address book on the envelope.
type abRef struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
}

func refOf(ab chcon.Addressbook) abRef {
	return abRef{ID: ab.ID, Name: ab.Name, DisplayName: ab.DisplayName}
}

// objRef is the affected object on the envelope.
type objRef struct {
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	ETag      string `json:"etag,omitempty"`
	PriorETag string `json:"prior_etag,omitempty"`
	Size      int64  `json:"size"`
	Exists    bool   `json:"exists"`
}

// rewrite is a stack's replacement for the client's object.
type rewrite struct {
	card  *chcon.Card
	vcard []byte
}

// mutation is one client mutation as the lanes see it.
type mutation struct {
	tenant      string
	account     string
	op          string // put | delete | mkaddressbook | remove | proppatch
	addressbook abRef
	object      *objRef
	vcard       []byte
	card        *chcon.Card
	prior       *chcon.Card
	props       map[string]string // proppatch / mkaddressbook: the properties set
	clientIP    string
	rewrite     *rewrite // set from an answer, read by the committer
}

// answer is the translated `@contacts.res` of an answer-lane run.
type answer struct {
	ok      bool
	code    string // cannot | limit | unavailable | ""
	msg     string
	rewrite *rewrite
	outcome string // metric label
}

// lanes owns the observe queue/workers and the answer dispatch.
type lanes struct {
	pu       *processor.Unit
	ctx      context.Context
	node     string
	sample   uint64
	inflight int
	deadline time.Duration

	// subscribed reports whether a tenant has an active `_contacts` stack.
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
	deadline, err := time.ParseDuration(pu.Conf.ContactsRespTimeout)
	if err != nil || deadline <= 0 {
		deadline = 30 * time.Second
	}
	inflight := pu.Conf.ContactsObserveMaxInflight
	if inflight <= 0 {
		inflight = 1
	}
	l := &lanes{
		pu:       pu,
		ctx:      ctx,
		node:     node,
		sample:   uint64(pu.Conf.ContactsObserveSample),
		inflight: inflight,
		deadline: deadline,
		queue:    make(chan mutation, observeQueueDepth),
	}
	l.subscribed = l.snapshotSubscribed
	if pu.Mc != nil && pu.Mc.Meter != nil {
		l.observeOut, _ = pu.Mc.Meter.Int64Counter("chassis.contacts.observe",
			metric.WithDescription("contacts observe-lane dispatches into _contacts stacks, by outcome"),
			metric.WithUnit("1"))
		l.answerOut, _ = pu.Mc.Meter.Int64Counter("chassis.contacts.answer",
			metric.WithDescription("contacts answer-lane dispatches into _contacts stacks, by outcome"),
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
// active `_contacts` stack. Mutations are human-paced, so one indexed
// query per mutation is fine; a nil mirror means nobody is subscribed.
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
		l.pu.Logger.Warn("contacts: subscription lookup failed", zap.String("tenant", tenant), zap.String("err", err.Error()))
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
	envelope := event.PackageJSON(dctx, payload, resCh, "contacts")
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
// deadline for `@contacts.res`. Default-deny on every failure mode.
func (l *lanes) ask(m mutation) answer {
	if l == nil {
		return answer{outcome: "no_lanes", code: "unavailable", msg: "no stack to ask"}
	}
	if !l.subscribed(m.tenant) {
		l.record(l.answerOut, "unsubscribed")
		return answer{outcome: "unsubscribed", code: "unavailable", msg: "no _contacts stack to decide"}
	}
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(m, phaseAnswer, rid, l.node, time.Now())

	rctx, cancel := context.WithTimeout(l.ctx, answerRunTimeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(rctx, payload, resCh, "contacts")

	deadline := time.NewTimer(l.deadline)
	defer deadline.Stop()
	select {
	case l.pu.Bus <- envelope:
	case <-deadline.C:
		cancel()
		l.record(l.answerOut, "bus_timeout")
		return answer{outcome: "bus_timeout", code: "unavailable", msg: "stack unavailable"}
	}
	select {
	case res := <-resCh:
		cancel()
		a := translateAnswer(res.Raw)
		l.record(l.answerOut, a.outcome)
		return a
	case <-deadline.C:
		go func() {
			defer cancel()
			select {
			case <-resCh:
			case <-rctx.Done():
			}
		}()
		l.record(l.answerOut, "deadline")
		return answer{outcome: "deadline", code: "unavailable", msg: "stack did not answer in time"}
	}
}

// translateAnswer reads `_txc.contacts.res` off a response. Default-deny:
// no res, or ok not true, is a refusal with the stack's message (if any).
func translateAnswer(raw string) answer {
	if _, reason, denied := admission.Denied(raw); denied {
		return answer{outcome: "denied", code: "unavailable", msg: "service unavailable: " + reason}
	}
	res := gjson.Get(raw, "_txc.contacts.res")
	if !res.Exists() {
		return answer{outcome: "absent", msg: "refused by the address book's stack"}
	}
	a := answer{ok: res.Get("ok").Type == gjson.True, msg: res.Get("msg").String()}
	switch c := res.Get("code").String(); c {
	case "cannot", "limit", "unavailable":
		a.code = c
	}
	if a.ok {
		a.outcome = "ok"
		if cd := res.Get("card"); cd.IsObject() {
			if c, err := chcon.CardFromJSON([]byte(cd.Raw)); err == nil {
				a.rewrite = &rewrite{card: &c}
			}
		} else if vc := res.Get("vcard").String(); strings.TrimSpace(vc) != "" {
			a.rewrite = &rewrite{vcard: []byte(vc)}
		}
	} else {
		a.outcome = "refused"
		if a.msg == "" {
			a.msg = "refused by the address book's stack"
		}
	}
	return a
}

func (l *lanes) record(c metric.Int64Counter, outcome string) {
	if l == nil || c == nil {
		return
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.contacts.outcome", outcome)))
}

// buildEnvelope renders a mutation as the inlet envelope. Pure, so the
// shape is unit-testable. Every `_txc.contacts.*` fact is head-stamped.
func buildEnvelope(m mutation, phase, rid, node string, now time.Time) string {
	b := jsonx.NewObject()
	b.Set("_txc.src", "contacts")
	b.Set("_txc.rid", rid)
	b.Set("_ts", now.UTC().Format(time.RFC3339))
	b.Set("_txc.contacts.tenant", m.tenant)
	b.Set("_txc.contacts.account", m.account)
	b.Set("_txc.contacts.phase", phase)
	b.Set("_txc.contacts.op", m.op)
	b.Set("_txc.contacts.node", node)
	if raw, err := json.Marshal(m.addressbook); err == nil {
		b.SetRaw("_txc.contacts.addressbook", string(raw))
	}
	if m.object != nil {
		if raw, err := json.Marshal(m.object); err == nil {
			b.SetRaw("_txc.contacts.object", string(raw))
		}
	}
	if len(m.vcard) > 0 {
		b.Set("_txc.contacts.vcard", string(m.vcard))
	}
	if m.card != nil {
		if raw, err := json.Marshal(m.card); err == nil {
			b.SetRaw("_txc.contacts.card", string(raw))
		}
	}
	if m.prior != nil {
		if raw, err := json.Marshal(m.prior); err == nil {
			b.SetRaw("_txc.contacts.prior.card", string(raw))
		}
	}
	if len(m.props) > 0 {
		if raw, err := json.Marshal(m.props); err == nil {
			b.SetRaw("_txc.contacts.props", string(raw))
		}
	}
	if m.clientIP != "" {
		b.Set("_txc.client.ip", m.clientIP)
	}
	return b.String()
}
