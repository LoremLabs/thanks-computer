package dns

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The observe tap is the DNS head's first inlet lane: an answered query
// becomes a compact envelope dispatched into the zone tenant's `_dns`
// stack AFTER the wire reply has been written — fire-and-forget,
// scheduled-inlet style. The query path never waits on it, never
// blocks on it, and never darkens because of it: a full queue drops the
// observation (counted), a slow stack just runs late.
//
// What is tapped: exactly one QUERY-opcode question that resolved to a
// served zone whose tenant has an active `_dns` stack (zone.observe,
// decided at snapshot build). What is never tapped: names outside any
// served zone (no tenant to route to — and that is where scanner noise
// lives), queries the response-rate-limiter dropped (a flood must not
// become opstack load), and RFC2136 UPDATEs.
//
// Routing follows the cron/room/scheduled idiom: the head stamps the
// trusted tenant slug in `_txc.dns.tenant` and server.go's
// detectTenantBody proposes `_dns/0`; boot/100 promotes it through the
// same sanctioned _sys→tenant pin as every other inlet. The `_txc.dns.*`
// facts are read-only for stacks (default-closed txcguard); a stack that
// wants to ANSWER queries is the stack-answered lane, not this one.

const (
	// observeStack is the per-tenant stack the tap dispatches into; its
	// existence is the subscription (like `_cron`, `_scheduled`).
	observeStack = "_dns"

	// observeQueueDepth bounds the in-memory hand-off between the query
	// path and the tap workers. A burst beyond depth + workers drops
	// (counted) — the alternative, blocking the handler, is a self-DoS.
	observeQueueDepth = 1024

	// observeDispatchTimeout caps one tapped run (bus hand-off + the
	// stack's response). Same order as the scheduled inlet; a stack that
	// parks a continuation past this simply finishes detached.
	observeDispatchTimeout = 60 * time.Second
)

// observation is the post-reply record of one answered query, captured on
// the query path from values already in hand. No presentation-format
// work happens there — RR.String() runs in the worker.
type observation struct {
	q         dns.Question
	reply     *dns.Msg // the message that went on the wire (post-truncation)
	clientIP  string
	transport string // "udp" | "tcp"
	ednsSize  uint16 // 0 when the query carried no OPT
	zone      *zone
}

// observeTap owns the bounded queue and the worker pool that turns
// observations into `_dns` stack runs.
type observeTap struct {
	pu       *processor.Unit
	node     string
	sample   uint64 // 1 = every query; N = one in N (never 0 here)
	inflight int

	seq     atomic.Uint64 // sampling sequence
	dropped atomic.Uint64 // queue-full drops (also counted in outcomes)
	queue   chan observation

	outcomes metric.Int64Counter // chassis.dns.observe, by outcome

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newObserveTap builds the tap from config, or returns nil when the tap is
// off (--dns-observe-sample=0). queueDepth <= 0 selects the default.
func newObserveTap(pu *processor.Unit, queueDepth int) *observeTap {
	if pu == nil || pu.Conf.DNSObserveSample <= 0 {
		return nil
	}
	if queueDepth <= 0 {
		queueDepth = observeQueueDepth
	}
	inflight := pu.Conf.DNSObserveMaxInflight
	if inflight <= 0 {
		inflight = 1
	}
	node, _ := os.Hostname()
	t := &observeTap{
		pu:       pu,
		node:     node,
		sample:   uint64(pu.Conf.DNSObserveSample),
		inflight: inflight,
		queue:    make(chan observation, queueDepth),
	}
	if pu.Mc != nil && pu.Mc.Meter != nil {
		t.outcomes, _ = pu.Mc.Meter.Int64Counter("chassis.dns.observe",
			metric.WithDescription("DNS observe-tap dispatches into _dns stacks, by outcome"),
			metric.WithUnit("1"))
	}
	return t
}

// start launches the worker pool. Workers stop when stop() is called (or
// the parent ctx ends); the queue is never closed, so a late offer from a
// draining handler can never panic — it just drops.
func (t *observeTap) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	for i := 0; i < t.inflight; i++ {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ob := <-t.queue:
					t.dispatch(ctx, ob)
				}
			}
		}()
	}
}

// stop cancels the workers (abandoning queued observations — they are
// analytics, not deliveries) and waits for in-flight dispatches to
// unwind. Safe to call when start was never called.
func (t *observeTap) stop() {
	if t.cancel != nil {
		t.cancel()
	}
	t.wg.Wait()
}

// offer is the query-path entry point: apply sampling, then a non-blocking
// enqueue. Called only after the wire reply was written.
func (t *observeTap) offer(ob observation) {
	if t.sample > 1 && t.seq.Add(1)%t.sample != 0 {
		return
	}
	select {
	case t.queue <- ob:
	default:
		t.dropped.Add(1)
		t.record("dropped")
	}
}

// Dropped returns the number of observations discarded because the queue
// was full (cumulative since start).
func (t *observeTap) Dropped() uint64 { return t.dropped.Load() }

// dispatch runs one observation through the bus and drains the response.
// Mirrors scheduled.fire minus the durable-store bookkeeping: there is no
// row to mark, so every outcome is a counter + (debug) log line.
func (t *observeTap) dispatch(ctx context.Context, ob observation) {
	rid := hxid.NewTimeSort().String()
	payload := buildObserveEnvelope(ob, rid, t.node, time.Now())

	dctx, cancel := context.WithTimeout(ctx, observeDispatchTimeout)
	defer cancel()
	dctx = context.WithValue(dctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(dctx, payload, resCh, "dns")

	select {
	case t.pu.Bus <- envelope:
	case <-dctx.Done():
		// Never reached the bus (shutdown / bus stopped).
		t.record("bus_timeout")
		return
	}

	select {
	case res := <-resCh:
		if status, reason, ok := admission.Denied(res.Raw); ok {
			// A suspended/rate-limited tenant's tap is simply not run;
			// per-query Warn would be log spam under a flood, so this is
			// debug + counter (the tenant's usage line already shows it).
			t.record("denied")
			if t.pu.Logger.Core().Enabled(zap.DebugLevel) {
				t.pu.Logger.Debug("dns observe denied by admission",
					zap.String("rid", rid), zap.String("tenant", ob.zone.tenantSlug),
					zap.Int("status", status), zap.String("reason", reason))
			}
			return
		}
		t.record("ok")
	case <-dctx.Done():
		// Dispatched but the response didn't drain in time. Nothing to
		// retry (fire-and-forget); the run may still be finishing detached.
		t.record("timeout")
	}
}

func (t *observeTap) record(outcome string) {
	if t.outcomes == nil {
		return
	}
	t.outcomes.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("txco.dns.outcome", outcome),
	))
}

// buildObserveEnvelope renders one observation as the inlet envelope.
// Pure (no I/O) so the shape is unit-testable byte-for-byte.
//
// Facts ride under `_txc.dns.*` (read in txcl as `@dns.*`); types, class
// and rcode are their presentation mnemonics ("A", "IN", "NXDOMAIN"), and
// records are zone-file presentation strings — one universal encoding for
// every RR type rather than a per-type JSON schema. `_txc.dns.phase` is
// "observe" so a stack can tell a post-reply tap from a (future) answer
// request on one path, the way `@llm.phase` splits the AI gateway's two
// dispatches.
func buildObserveEnvelope(ob observation, rid, node string, now time.Time) string {
	b := envelopeBase(ob, rid, node, now, "observe")
	b.Set("_txc.dns.reply.rcode", rcodeName(ob.reply.Rcode))
	b.Set("_txc.dns.reply.authoritative", ob.reply.Authoritative)
	b.Set("_txc.dns.reply.truncated", ob.reply.Truncated)
	b.Set("_txc.dns.reply.answer", rrStrings(ob.reply.Answer))
	b.Set("_txc.dns.reply.authority", rrStrings(ob.reply.Ns))
	return b.String()
}

// envelopeBase renders the facts every `_dns` envelope carries — identity,
// route hint, question, client, zone — for the given phase ("observe" for
// the post-reply tap, "answer" for a stack-answered dispatch). Each lane
// adds its own section (`reply` / `proposed`) on top.
func envelopeBase(ob observation, rid, node string, now time.Time, phase string) *jsonx.Builder {
	b := jsonx.NewObject()
	b.Set("_txc.src", "dns")
	b.Set("_txc.rid", rid)
	b.Set("_ts", now.UTC().Format(time.RFC3339))

	// Trusted route hint: the served zone's tenant SLUG from the snapshot
	// (the routable name — the boot re-tenant rejects a tenant_id), never
	// client input. detectTenantBody turns it into the `_dns/0` proposal.
	b.Set("_txc.dns.tenant", ob.zone.tenantSlug)
	b.Set("_txc.dns.phase", phase)
	b.Set("_txc.dns.node", node)

	b.Set("_txc.dns.q.name", strings.ToLower(dns.Fqdn(ob.q.Name)))
	b.Set("_txc.dns.q.type", dns.Type(ob.q.Qtype).String())
	b.Set("_txc.dns.q.class", dns.Class(ob.q.Qclass).String())

	b.Set("_txc.dns.client.ip", ob.clientIP)
	b.Set("_txc.dns.client.transport", ob.transport)
	if ob.ednsSize > 0 {
		b.Set("_txc.dns.client.edns_udpsize", int(ob.ednsSize))
	}
	// Chassis-wide client identity, same key the web and LMTP heads stamp.
	b.Set("_txc.client.ip", ob.clientIP)

	b.Set("_txc.dns.zone.origin", ob.zone.origin)
	b.Set("_txc.dns.zone.mode", ob.zone.mode)
	return b
}

// rrStrings renders RRs in zone-file presentation format (what
// `dns.NewRR` parses back). Always a non-nil slice so the envelope carries
// `[]`, not null, for an empty section.
func rrStrings(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rr.String())
	}
	return out
}

// rcodeName is the presentation mnemonic for an rcode, with a stable
// fallback for values miekg/dns has no name for.
func rcodeName(rcode int) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}
	return fmt.Sprintf("RCODE%d", rcode)
}
