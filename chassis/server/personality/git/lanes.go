package git

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The two inlet lanes, the pair the contacts, calendar and IMAP heads have:
//
//   - answer: a push to a repository whose policy says `stack` dispatches
//     into the tenant's `_git` stack BEFORE the refs move, after the
//     objects are stored and the response headers are out, and waits
//     (bounded) for `@git.res`. Absent, late or `ok: false` ⇒ the push is
//     refused, every ref, with the stack's message in the report the
//     developer reads (`! [remote rejected] main -> main (<msg>)`).
//   - observe: an accepted push becomes an envelope dispatched into the
//     same stack AFTER the reply — fire-and-forget, bounded queue, sampled.
//     A slow or absent stack can never delay a client. The application
//     acts on a push here (OnePony activates a configuration).
//
// The head stamps the trusted tenant slug in `_txc.git.tenant` and
// detectTenantBody proposes `_git/0`. The `_txc.git.*` facts are read-only
// for stacks; `@git.res` is the one author-writable path. What a push
// MEANS is the stack's: the head knows repositories, refs and files.

const (
	subscriptionStack = "_git"

	observeQueueDepth      = 1024
	observeDispatchTimeout = 60 * time.Second
	answerRunTimeout       = 60 * time.Second

	phaseObserve = "observe"
	phaseAnswer  = "answer"

	// maxCommitsOnEnvelope bounds `_txc.git.commits`: a push of a long
	// history lists its newest commits and a count.
	maxCommitsOnEnvelope = 100
)

// answer is the translated `@git.res` of an answer-lane run.
type answer struct {
	ok      bool
	code    string // cannot | limit | unavailable | ""
	msg     string
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

	// subscribed reports whether a tenant has an active `_git` stack.
	// Defaults to a mirror-snapshot query; tests inject.
	subscribed func(tenant string) bool

	seq     atomic.Uint64
	dropped atomic.Uint64
	queue   chan *Push

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
	deadline, err := time.ParseDuration(pu.Conf.GitRespTimeout)
	if err != nil || deadline <= 0 {
		deadline = 10 * time.Second
	}
	inflight := pu.Conf.GitObserveMaxInflight
	if inflight <= 0 {
		inflight = 1
	}
	l := &lanes{
		pu:       pu,
		ctx:      ctx,
		node:     node,
		sample:   uint64(pu.Conf.GitObserveSample),
		inflight: inflight,
		deadline: deadline,
		queue:    make(chan *Push, observeQueueDepth),
	}
	l.subscribed = l.snapshotSubscribed
	if pu.Mc != nil && pu.Mc.Meter != nil {
		l.observeOut, _ = pu.Mc.Meter.Int64Counter("chassis.git.observe",
			metric.WithDescription("git observe-lane dispatches into _git stacks, by outcome"),
			metric.WithUnit("1"))
		l.answerOut, _ = pu.Mc.Meter.Int64Counter("chassis.git.answer",
			metric.WithDescription("git answer-lane dispatches into _git stacks, by outcome"),
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
				case p := <-l.queue:
					l.dispatchObserve(ctx, p)
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
// active `_git` stack. Pushes are human-paced, so one indexed query per
// push is fine; a nil mirror means nobody is subscribed.
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
	if err != nil && !errors.Is(err, sql.ErrNoRows) && l.pu.Logger != nil {
		l.pu.Logger.Warn("git: subscription lookup failed", zap.String("tenant", tenant), zap.String("err", err.Error()))
	}
	return err == nil
}

// observe is the post-reply entry point: sampling, subscription, then a
// non-blocking enqueue.
func (l *lanes) observe(p *Push) {
	if l == nil || l.sample == 0 || p == nil {
		return
	}
	if !l.subscribed(p.Tenant) {
		return
	}
	if l.sample > 1 && l.seq.Add(1)%l.sample != 0 {
		return
	}
	select {
	case l.queue <- p:
	default:
		l.dropped.Add(1)
		l.record(l.observeOut, "dropped")
	}
}

func (l *lanes) dispatchObserve(ctx context.Context, p *Push) {
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(p, phaseObserve, rid, l.node, time.Now())

	dctx, cancel := context.WithTimeout(ctx, observeDispatchTimeout)
	defer cancel()
	dctx = context.WithValue(dctx, config.CtxKeyRid, rid)
	dctx = authn.WithAuthenticated(dctx, p.Who)

	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(dctx, payload, resCh, "git")
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

// ask is the answer lane: dispatch before the refs move and wait up to the
// deadline for `@git.res`. Default-deny on every failure mode.
func (l *lanes) ask(p *Push) answer {
	if l == nil {
		return answer{outcome: "no_lanes", code: "unavailable", msg: "no stack to ask"}
	}
	if !l.subscribed(p.Tenant) {
		l.record(l.answerOut, "unsubscribed")
		return answer{outcome: "unsubscribed", code: "unavailable", msg: "no _git stack to decide this push"}
	}
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(p, phaseAnswer, rid, l.node, time.Now())

	rctx, cancel := context.WithTimeout(l.ctx, answerRunTimeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	rctx = authn.WithAuthenticated(rctx, p.Who)
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(rctx, payload, resCh, "git")

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
		return answer{outcome: "deadline", code: "unavailable", msg: "the stack did not answer in time"}
	}
}

// translateAnswer reads `_txc.git.res` off a response. Default-deny: no
// res, or ok not true, is a refusal with the stack's message (if any).
func translateAnswer(raw string) answer {
	if _, reason, denied := admission.Denied(raw); denied {
		return answer{outcome: "denied", code: "unavailable", msg: "service unavailable: " + reason}
	}
	res := gjson.Get(raw, "_txc.git.res")
	if !res.Exists() {
		return answer{outcome: "absent", msg: "declined by the repository's stack"}
	}
	a := answer{ok: res.Get("ok").Type == gjson.True, msg: res.Get("msg").String()}
	switch c := res.Get("code").String(); c {
	case "cannot", "limit", "unavailable":
		a.code = c
	}
	if a.ok {
		a.outcome = "ok"
	} else {
		a.outcome = "refused"
		if a.msg == "" {
			a.msg = "declined by the repository's stack"
		}
	}
	return a
}

func (l *lanes) record(c metric.Int64Counter, outcome string) {
	if l == nil || c == nil {
		return
	}
	c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.git.outcome", outcome)))
}

// envelope shapes.
type envCommand struct {
	Ref     string      `json:"ref"`
	Old     string      `json:"old"`
	New     string      `json:"new"`
	Op      string      `json:"op"` // create | update | delete
	Changes []envChange `json:"changes"`
}

type envChange struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Mode string `json:"mode"`
	Op   string `json:"op"`
}

type envCommit struct {
	OID     string   `json:"oid"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Email   string   `json:"email"`
	When    string   `json:"when"`
	Message string   `json:"message"`
}

type envPusher struct {
	Principal  string `json:"principal"`
	Credential string `json:"credential"`
}

// buildEnvelope renders a push as the inlet envelope. Pure, so the shape
// is unit-testable. Every `_txc.git.*` fact is head-stamped. The first
// command's ref/old/new are repeated at the top level for the one-ref push
// (the common case) so a rule reads `@git.ref` without indexing.
func buildEnvelope(p *Push, phase, rid, node string, now time.Time) string {
	b := jsonx.NewObject()
	b.Set("_txc.src", "git")
	b.Set("_txc.rid", rid)
	b.Set("_ts", now.UTC().Format(time.RFC3339))
	b.Set("_txc.git.tenant", p.Tenant)
	b.Set("_txc.git.repo", p.Repo)
	b.Set("_txc.git.phase", phase)
	b.Set("_txc.git.node", node)
	if raw, err := json.Marshal(envPusher{Principal: p.Who.Principal.ID, Credential: p.Who.Credential}); err == nil {
		b.SetRaw("_txc.git.pusher", string(raw))
	}
	cmds := make([]envCommand, 0, len(p.Commands))
	for _, cmd := range p.Commands {
		ec := envCommand{Ref: cmd.Name.String(), Old: cmd.Old.String(), New: cmd.New.String(), Op: string(cmd.Action()), Changes: []envChange{}}
		for _, ch := range p.Changes[cmd.Name] {
			ec.Changes = append(ec.Changes, envChange{Path: ch.Path, OID: ch.Hash.String(), Size: ch.Size, Mode: ch.Mode.String(), Op: ch.Op})
		}
		cmds = append(cmds, ec)
	}
	if raw, err := json.Marshal(cmds); err == nil {
		b.SetRaw("_txc.git.commands", string(raw))
	}
	if len(cmds) > 0 {
		b.Set("_txc.git.ref", cmds[0].Ref)
		b.Set("_txc.git.old", cmds[0].Old)
		b.Set("_txc.git.new", cmds[0].New)
		b.Set("_txc.git.op", cmds[0].Op)
		if raw, err := json.Marshal(cmds[0].Changes); err == nil {
			b.SetRaw("_txc.git.changes", string(raw))
		}
	}
	if raw, err := json.Marshal(p.commits); err == nil {
		b.SetRaw("_txc.git.commits", string(raw))
	}
	b.Set("_txc.git.commits_total", p.commitsTotal)
	if p.ClientIP != "" {
		b.Set("_txc.client.ip", p.ClientIP)
	}
	return b.String()
}

// describeCommits lists the push's new commits (the commit objects among
// what the walk found reachable from the moved refs), newest first as the
// walk yields them, capped for the envelope.
func describeCommits(p *Push) {
	var out []envCommit
	total := 0
	for _, h := range dedupe(p.reachable) {
		o, err := p.staging.EncodedObject(plumbing.CommitObject, h)
		if err != nil {
			continue
		}
		total++
		if len(out) >= maxCommitsOnEnvelope {
			continue
		}
		c, err := object.DecodeCommit(p.staging, o)
		if err != nil {
			continue
		}
		parents := make([]string, 0, len(c.ParentHashes))
		for _, ph := range c.ParentHashes {
			parents = append(parents, ph.String())
		}
		out = append(out, envCommit{OID: c.Hash.String(), Parents: parents, Author: c.Author.Name, Email: c.Author.Email,
			When: c.Author.When.UTC().Format(time.RFC3339), Message: c.Message})
	}
	if out == nil {
		out = []envCommit{}
	}
	p.commits = out
	p.commitsTotal = total
}
