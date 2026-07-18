package llmgw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// verdict is what the gateway reads back from the request-phase stack
// run. reject/upstreamURL/headers come from the author-writable
// `_txc.llm.*` subtrees (txcguard-allowlisted); admission/unavailable are
// chassis-stamped denials; request is the mutable Anthropic JSON the
// upstream will receive.
type verdict struct {
	unavailable bool // _txc.route.unavailable: routing layer itself failed

	admission bool // _txc.admission.denied
	admStatus int
	admReason string
	admRetry  string // seconds, as a string; "" = absent

	reject       bool // _txc.llm.reject
	rejectStatus int
	rejectType   string
	rejectMsg    string

	request     string // raw JSON of the top-level `request` object
	upstreamURL string // _txc.llm.upstream.url, "" = configured default
	headers     map[string]string
	context     gjson.Result // _txc.llm.context: stack-emitted items for the gateway to inject
}

// parseVerdict interprets the final pipeline envelope. Pure — unit-tested
// without a bus.
func parseVerdict(raw string) verdict {
	fields := gjson.GetMany(raw,
		"_txc.route.unavailable",
		"_txc.admission.denied", "_txc.admission.status",
		"_txc.admission.reason", "_txc.admission.retry_after",
		"_txc.llm.reject",
		"_txc.llm.upstream.url", "_txc.llm.headers",
		"request", "_txc.llm.context")
	v := verdict{
		unavailable: fields[0].Bool(),
		admission:   fields[1].Bool(),
		admStatus:   int(fields[2].Int()),
		admReason:   fields[3].String(),
		upstreamURL: fields[6].String(),
		request:     fields[8].Raw,
		context:     fields[9],
	}
	if fields[4].Exists() {
		v.admRetry = strconv.Itoa(int(fields[4].Int()))
	}
	if rej := fields[5]; rej.Exists() {
		v.reject = true
		v.rejectStatus = int(rej.Get("status").Int())
		if v.rejectStatus < 100 || v.rejectStatus > 599 {
			v.rejectStatus = http.StatusForbidden
		}
		v.rejectType = rej.Get("type").String()
		if v.rejectType == "" {
			v.rejectType = errTypePermission
		}
		v.rejectMsg = rej.Get("message").String()
		if v.rejectMsg == "" {
			v.rejectMsg = "request rejected by gateway policy"
		}
	}
	if hdrs := fields[7]; hdrs.IsObject() {
		v.headers = map[string]string{}
		hdrs.ForEach(func(key, value gjson.Result) bool {
			if value.Type == gjson.String {
				v.headers[key.String()] = value.String()
			}
			return true
		})
	}
	return v
}

// requestPayload builds the request-phase envelope. Everything under
// `_txc` is chassis-stamped here — the client's bytes are confined to
// the `request` key, so a hostile client can never forge control state
// (the same trust argument as `_txc.cron.tenant`).
func requestPayload(rid, tenant string, verified bool, host string, body []byte, stream bool, authMode string, hdr http.Header) string {
	pb := jsonx.New()
	pb.Set("_txc.src", srcName)
	pb.Set("_txc.rid", rid)
	pb.Set("_txc.llm.phase", phaseRequest)
	pb.Set("_txc.llm.protocol", protocolName)
	pb.Set("_txc.llm.request_id", rid)
	pb.Set("_txc.llm.tenant", tenant)
	pb.Set("_txc.llm.hostname_verified", verified)
	pb.Set("_txc.llm.host", host)
	pb.Set("_txc.llm.stream", stream)
	pb.Set("_txc.llm.auth_mode", authMode)
	if ua := hdr.Get("User-Agent"); ua != "" {
		pb.Set("_txc.llm.req.user_agent", ua)
	}
	if av := hdr.Get("anthropic-version"); av != "" {
		pb.Set("_txc.llm.req.anthropic_version", av)
	}
	if ab := hdr.Get("anthropic-beta"); ab != "" {
		pb.Set("_txc.llm.req.anthropic_beta", ab)
	}
	pb.Set("_ts", time.Now().UTC().Format(time.RFC3339))
	pb.SetRaw("request", string(body))
	return pb.String()
}

// runStack sends one envelope through the bus and waits for the final
// payload — the same round-trip every inlet does, so admission, tracing,
// and usage apply uniformly. The _llm stack is expected to answer with a
// plain (buffered) result; if a rule streams anyway (StreamHead), we
// drain the chunks so the processor's bare sends unblock, then report an
// error — the gateway owns the transport, stacks must not.
func (g *Gateway) runStack(ctx context.Context, payload string) (string, error) {
	resCh := make(chan event.Payload)
	envelope := event.PackageJSON(ctx, payload, resCh, srcName)
	select {
	case g.pu.Bus <- envelope:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-g.ctx.Done():
		return "", errShutdown
	}
	streaming := false
	for {
		select {
		case res := <-resCh:
			switch res.Type {
			case event.StreamHead, event.StreamChunk:
				streaming = true
				continue // drain; the processor blocks on each send
			case event.StreamEnd:
				return "", errStackStreamed
			case event.ErrorStr:
				return "", errors.New("pipeline error: " + res.Raw)
			default:
				if streaming {
					return "", errStackStreamed
				}
				return res.Raw, nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-g.ctx.Done():
			return "", errShutdown
		}
	}
}

var (
	errShutdown      = errors.New("llmgw: chassis shutting down")
	errStackStreamed = errors.New("llmgw: _llm stack streamed a response; the gateway owns the transport")
)

// completion carries what the gateway knows after the upstream exchange
// finished (or failed after the stack ran).
type completion struct {
	rid      string
	tenant   string
	verified bool
	host     string

	status     int   // upstream status sent to the client (0 = dial failed)
	durationMS int64 // whole request, first byte in → last byte out
	bytesIn    int64 // client request body size
	bytesOut   int64 // bytes written to the client

	// Model provenance is explicit — three fields, no "smart" single one:
	// what the client asked for, what the stack actually forwarded, and
	// what the upstream said it served (capture-dependent, may be "").
	requestedModel        string
	effectiveRequestModel string
	responseModel         string

	stream             bool
	upstream           string // base URL actually used
	clientDisconnected bool
	errStr             string

	usage         usageResult         // token usage captured from the response (record-only)
	contextResult []contextResultItem // gateway ground truth of context injection
}

// fireCompletion records the exchange after the client is done: one
// usage event (transfer metering — the pipeline's own usage lines meter
// stack fuel) and one fire-and-forget envelope into the tenant's _llm/0
// with phase="completed" so policy can react in txcl. No client waits.
//
// The envelope's resCh is buffered and drained to a terminal message —
// the processor does bare sends on resCh, so an unread channel would
// leak its goroutine. Rooted on the gateway's ctx (not the dead request
// ctx) with the request's rid restored so trace/usage lines correlate.
func (g *Gateway) fireCompletion(c completion) {
	if g.pu.Usage != nil {
		status := "ok"
		if c.errStr != "" || c.status == 0 || c.status >= 400 {
			status = "error"
		}
		g.pu.Usage.WriteEvent(usage.UsageEvent{
			RID:        c.rid,
			Tenant:     c.tenant,
			Src:        srcName,
			Stack:      stackName,
			DurationMS: c.durationMS,
			Status:     status,
			BytesIn:    int(c.bytesIn),
			BytesOut:   int(c.bytesOut),
			Billable:   true,
		})
	}

	// The completion run gets its OWN rid: the trace store keys a run's
	// top-level in.json/out.json by rid, so reusing the request's rid
	// made the completion run overwrite the request-phase trace.
	// Correlation stays on _txc.llm.request_id (the original rid), which
	// is also the rid on the transfer usage event above.
	crid := hxid.NewTimeSort().String()
	pb := jsonx.New()
	pb.Set("_txc.src", srcName)
	pb.Set("_txc.rid", crid)
	pb.Set("_txc.llm.phase", phaseCompleted)
	pb.Set("_txc.llm.protocol", protocolName)
	pb.Set("_txc.llm.request_id", c.rid)
	pb.Set("_txc.llm.tenant", c.tenant)
	pb.Set("_txc.llm.hostname_verified", c.verified)
	pb.Set("_txc.llm.host", c.host)
	pb.Set("_txc.llm.completion.status", c.status)
	pb.Set("_txc.llm.completion.duration_ms", c.durationMS)
	pb.Set("_txc.llm.completion.bytes_in", c.bytesIn)
	pb.Set("_txc.llm.completion.bytes_out", c.bytesOut)
	pb.Set("_txc.llm.completion.requested_model", c.requestedModel)
	pb.Set("_txc.llm.completion.effective_request_model", c.effectiveRequestModel)
	if c.responseModel != "" {
		pb.Set("_txc.llm.completion.response_model", c.responseModel)
	}
	pb.Set("_txc.llm.completion.stream", c.stream)
	pb.Set("_txc.llm.completion.upstream", c.upstream)
	pb.Set("_txc.llm.completion.client_disconnected", c.clientDisconnected)
	if c.errStr != "" {
		pb.Set("_txc.llm.completion.error", c.errStr)
	}
	// Token usage is presence-gated: absent means "not captured", never
	// zero — mis-recording 0 tokens would be worse than recording none.
	if c.usage.hasInput {
		pb.Set("_txc.llm.completion.usage.input_tokens", c.usage.inputTokens)
	}
	if c.usage.hasOutput {
		pb.Set("_txc.llm.completion.usage.output_tokens", c.usage.outputTokens)
	}
	if c.usage.hasCacheRead {
		pb.Set("_txc.llm.completion.usage.cache_read_input_tokens", c.usage.cacheRead)
	}
	if c.usage.hasCacheCreation {
		pb.Set("_txc.llm.completion.usage.cache_creation_input_tokens", c.usage.cacheCreation)
	}
	if c.usage.stopReason != "" {
		pb.Set("_txc.llm.completion.stop_reason", c.usage.stopReason)
	}
	if len(c.contextResult) > 0 {
		if b, err := json.Marshal(c.contextResult); err == nil {
			pb.SetRaw("_txc.llm.context_result", string(b))
		}
	}
	pb.Set("_ts", time.Now().UTC().Format(time.RFC3339))

	ctx, cancel := context.WithTimeout(g.ctx, g.maxWait)
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyRid, crid)

	resCh := make(chan event.Payload, 8)
	envelope := event.PackageJSON(ctx, pb.String(), resCh, srcName)
	select {
	case g.pu.Bus <- envelope:
	case <-ctx.Done():
		g.log.Warn("llmgw completion envelope dropped (bus unavailable)",
			zap.String("rid", c.rid), zap.String("tenant", c.tenant))
		return
	}
	for {
		select {
		case p := <-resCh:
			switch p.Type {
			case event.StreamHead, event.StreamChunk:
				continue // drain a misbehaving streaming rule
			default:
				return // JSON / ErrorStr / StreamEnd: terminal
			}
		case <-ctx.Done():
			return
		}
	}
}
