package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/htmlextract"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// txco://html/extract fetches one public HTML page and returns the values
// CSS selectors pick out of it — never the page itself
// (docs/advanced/html-extract.md).
//
//	WITH url, selectors = &object(<field>, &object("selector", …, "text", true | "attr", …,
//	     "all"?, "limit"?), …), into? (default _html)
//
// The fetch is chassis/fetch: http(s) on 80 or 443, every address the
// host resolves to checked by the egress guard before it is dialed, every
// redirect checked again, bounded in bytes and time. The parse is
// chassis/htmlextract, bounded in tokens, nodes and CPU. Like EXEC
// "https://…", any rule may call it: the URL is the rule's to choose.
//
// The answer is in-band at `into`, with a nil Go error either way:
//
//	{ok: true, url, final_url, status, content_type, data: {<field>: value…}, truncated?}
//	{ok: false, url, content_type?, error: {code: "txco_html_<code>", message, status?}}
type htmlExtractDeps struct {
	fetcher interface {
		Get(context.Context, fetch.Request) (*fetch.Result, error)
	}
	limits htmlextract.Limits

	fetchSlots chan struct{} // fetches in flight on this node
	parseSlots chan struct{} // parses running on this node
	tenantMax  int
	rate       *throttle.Throttle

	mu       sync.Mutex
	inflight map[string]int
}

func newHTMLExtractDeps(conf config.Config, guard egress.Guard) (*htmlExtractDeps, error) {
	timeout, err := time.ParseDuration(conf.HTMLExtractTimeout)
	if err != nil {
		return nil, fmt.Errorf("html-extract-timeout: %w", err)
	}
	f, err := fetch.New(fetch.Options{
		Guard:        guard,
		MaxBytes:     int64(conf.HTMLExtractMaxBytes),
		MaxRedirects: 3,
		Timeout:      timeout,
		MediaTypes:   []string{"text/html"},
		Accept:       "text/html",
	})
	if err != nil {
		return nil, err
	}
	return &htmlExtractDeps{
		fetcher:    f,
		limits:     htmlextract.DefaultLimits(),
		fetchSlots: make(chan struct{}, conf.HTMLExtractConcurrency),
		parseSlots: make(chan struct{}, conf.HTMLExtractParseConcurrency),
		tenantMax:  conf.HTMLExtractTenantConcurrency,
		rate:       throttle.New(conf.HTMLExtractRatePerMin, time.Minute),
		inflight:   map[string]int{},
	}, nil
}

// htmlAnswer is the op's result object.
type htmlAnswer struct {
	OK          bool            `json:"ok"`
	URL         string          `json:"url"`
	FinalURL    string          `json:"final_url,omitempty"`
	Status      int             `json:"status,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
	Error       *htmlError      `json:"error,omitempty"`
}

type htmlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// htmlRun is one call's bookkeeping, for the completion event.
type htmlRun struct {
	start     time.Time
	selectors int
	bytes     int64
	redirects int
	parseMS   int64
	nodes     int
}

func htmlExtract(ctx context.Context, d *htmlExtractDeps, in []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_html")
	rawURL := gjson.GetBytes(meta, "url").String()
	run := &htmlRun{start: time.Now()}
	ans := htmlAnswer{URL: rawURL}
	failStatus := func(code, msg string, status int) (event.Payload, error) {
		ans.OK = false
		ans.Error = &htmlError{Code: "txco_html_" + code, Message: msg, Status: status}
		emitHTMLCompletionEvent(ctx, rawURL, ans, run)
		return htmlPayload(into, ans), nil
	}
	fail := func(code, msg string) (event.Payload, error) { return failStatus(code, msg, 0) }

	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return fail("no_tenant", "no tenant in request scope")
	}
	if _, ferr := fetch.CheckURL(rawURL); ferr != nil {
		return fail(ferr.Code, ferr.Message)
	}
	specs, err := htmlextract.ParseSpecs(gjson.GetBytes(meta, "selectors").Raw, d.limits)
	if err != nil {
		var he *htmlextract.Error
		if errors.As(err, &he) {
			return fail(he.Code, he.Message)
		}
		return fail(htmlextract.CodeInvalidSelectors, err.Error())
	}
	run.selectors = len(specs)

	if ok, _ := d.rate.Allow(tenant); !ok {
		return fail("rate_limited", "this tenant has started too many extractions on this node in the last minute")
	}
	if !d.enter(tenant) {
		return fail("busy", fmt.Sprintf("this tenant already has %d extractions in flight on this node", d.tenantMax))
	}
	defer d.leave(tenant)

	select {
	case d.fetchSlots <- struct{}{}:
	case <-ctx.Done():
		return fail("busy", "every fetch slot on this node stayed busy until the op's deadline")
	}
	stage := gjson.GetBytes(in, "_txc.op").String()
	_ = processor.AddFuel(ctx, processor.FuelCostHTMLExtract, stage)
	res, err := d.fetcher.Get(ctx, fetch.Request{URL: rawURL, UserAgent: htmlUserAgent(ctx)})
	<-d.fetchSlots
	if err != nil {
		var fe *fetch.Error
		if !errors.As(err, &fe) {
			return fail("internal_error", "the fetch failed unexpectedly")
		}
		run.bytes = fe.Bytes
		chargePerMiB(ctx, fe.Bytes, processor.FuelCostHTMLPerMiB, in)
		ans.ContentType = fe.ContentType
		if fe.Code == fetch.CodeHTTPError {
			return failStatus(fe.Code, fe.Message, fe.Status)
		}
		return fail(fe.Code, fe.Message)
	}
	run.bytes, run.redirects = res.Bytes, res.Redirects
	chargePerMiB(ctx, res.Bytes, processor.FuelCostHTMLPerMiB, in)
	ans.FinalURL, ans.Status, ans.ContentType = res.FinalURL, res.Status, res.ContentType

	select {
	case d.parseSlots <- struct{}{}:
	case <-ctx.Done():
		return fail("busy", "every parse slot on this node stayed busy until the op's deadline")
	}
	parseStart := time.Now()
	out, err := htmlextract.Extract(ctx, res.Body, res.ContentType, specs, d.limits)
	elapsed := time.Since(parseStart)
	<-d.parseSlots
	run.parseMS = elapsed.Milliseconds() + 1 // every started millisecond
	_ = processor.AddFuel(ctx, run.parseMS*processor.FuelCostHTMLExtractPerMs, stage)
	if err != nil {
		var he *htmlextract.Error
		if errors.As(err, &he) {
			return fail(he.Code, he.Message)
		}
		return fail("internal_error", "the extraction failed unexpectedly")
	}
	run.nodes = out.Nodes
	ans.OK, ans.Data, ans.Truncated = true, out.Data, out.Truncated
	emitHTMLCompletionEvent(ctx, rawURL, ans, run)
	return htmlPayload(into, ans), nil
}

func (d *htmlExtractDeps) enter(tenant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] >= d.tenantMax {
		return false
	}
	d.inflight[tenant]++
	return true
}

func (d *htmlExtractDeps) leave(tenant string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] <= 1 {
		delete(d.inflight, tenant)
		return
	}
	d.inflight[tenant]--
}

func htmlPayload(into string, ans htmlAnswer) event.Payload {
	b, _ := json.Marshal(ans)
	raw, _ := sjson.SetRaw(`{}`, into, string(b))
	return event.Payload{Raw: raw, Type: event.JSON}
}

func htmlUserAgent(ctx context.Context) string {
	v, _ := ctx.Value(config.CtxKeyVersion).(string)
	if v == "" {
		v = "dev"
	}
	return "txco-extract/" + v + " (+https://www.thanks.computer)"
}

// emitHTMLCompletionEvent records one call on the trace timeline. Timeline
// events are not redacted, so it carries the host and a short hash of the
// URL — never the path or the query, which can hold anything.
func emitHTMLCompletionEvent(ctx context.Context, rawURL string, ans htmlAnswer, run *htmlRun) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	sum := sha256.Sum256([]byte(rawURL))
	fields := map[string]any{
		"url_sha256":  hex.EncodeToString(sum[:8]),
		"duration_ms": time.Since(run.start).Milliseconds(),
		"selectors":   run.selectors,
		"bytes":       run.bytes,
	}
	host := func(s string) string {
		if u, err := url.Parse(s); err == nil {
			return u.Hostname()
		}
		return ""
	}
	if h := host(rawURL); h != "" {
		fields["host"] = h
	}
	if ans.FinalURL != "" {
		if h := host(ans.FinalURL); h != fields["host"] {
			fields["final_host"] = h
		}
	}
	if ans.Status != 0 {
		fields["status"] = ans.Status
	}
	if run.redirects > 0 {
		fields["redirects"] = run.redirects
	}
	if run.parseMS > 0 {
		fields["parse_ms"] = run.parseMS
		fields["nodes"] = run.nodes
	}
	if ans.OK {
		fields["output_bytes"] = len(ans.Data)
		if ans.Truncated {
			fields["truncated"] = true
		}
	} else if ans.Error != nil {
		fields["error_code"] = ans.Error.Code
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "html.completion", Fields: fields})
}
