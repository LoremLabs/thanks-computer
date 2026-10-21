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
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/webfeed"
)

// txco://web/feed fetches one public feed (RSS, Atom or JSON Feed) and
// returns its newest items as JSON: titles, addresses, dates and a few
// lines each, never the document (docs/advanced/web-feed.md).
//
//	WITH url, count? (1–50, default 20), since? (RFC 3339 or a date), into? (default _feed)
//
// The fetch is chassis/fetch, as html/extract's: http(s) on 80 or 443,
// every address through the egress guard, redirects checked the same
// way, bounded in bytes and time (the --web-feed-* flags). Any media
// type is accepted and the body is read for what it is, because feeds
// are served under a dozen types and often the wrong one; an HTML page
// is refused with the feeds it advertises. Like EXEC "https://…", any
// rule may call it: the URL is the rule's to choose.
//
// The answer is in-band at `into`, with a nil Go error either way:
//
//	{ok: true, url, final_url, status, format, feed: {title, link, description, updated}, items: [{id, title, url, date, summary, author, source}…], count, total, truncated}
//	{ok: false, url, final_url?, content_type?, error: {code: "txco_feed_<code>", message, status?, feeds?}}
type webFeedDeps struct {
	enabled bool
	fetcher interface {
		Get(context.Context, fetch.Request) (*fetch.Result, error)
	}

	slots     chan struct{} // fetches in flight on this node
	tenantMax int
	rate      *throttle.Throttle

	mu       sync.Mutex
	inflight map[string]int
}

func newWebFeedDeps(conf config.Config, guard egress.Guard) (*webFeedDeps, error) {
	timeout, err := time.ParseDuration(conf.WebFeedTimeout)
	if err != nil {
		return nil, fmt.Errorf("web-feed-timeout: %w", err)
	}
	f, err := fetch.New(fetch.Options{
		Guard:        guard,
		MaxBytes:     int64(conf.WebFeedMaxBytes),
		MaxRedirects: 3,
		Timeout:      timeout,
		// No MediaTypes: feeds arrive as application/rss+xml, atom+xml,
		// xml, text/xml, json, feed+json, and often text/html or
		// text/plain. The reader decides from the bytes.
		Accept: "application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, text/xml;q=0.9, application/json;q=0.8, text/html;q=0.5, */*;q=0.1",
	})
	if err != nil {
		return nil, err
	}
	return &webFeedDeps{
		enabled:   conf.WebFeed,
		fetcher:   f,
		slots:     make(chan struct{}, conf.WebFeedConcurrency),
		tenantMax: conf.WebFeedTenantConcurrency,
		rate:      throttle.New(conf.WebFeedRatePerMin, time.Minute),
		inflight:  map[string]int{},
	}, nil
}

// webFeedName is the one name the handler answers to. Builtins resolve
// by longest prefix, so without this check txco://web/feeds would reach
// it too.
const webFeedName = "txco://web/feed"

// webFeedRun is one call's bookkeeping, for the completion event.
type webFeedRun struct {
	start     time.Time
	status    int
	bytes     int64
	redirects int
	parseMS   int64
	format    string
	items     int
	total     int
	output    int
}

func webFeed(ctx context.Context, d *webFeedDeps, opName string, in []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, webfeed.DefaultInto)
	run := &webFeedRun{start: time.Now()}
	tenant := processor.TenantScope(ctx)
	rawURL := gjson.GetBytes(meta, "url").String()
	failure := webfeed.Failure{URL: rawURL}
	fail := func(e *webfeed.Error) (event.Payload, error) {
		e.Code = "txco_feed_" + e.Code
		if e.Status != 0 {
			run.status = e.Status
		}
		failure.Error = e
		emitWebFeedEvent(ctx, rawURL, failure.FinalURL, run, false, e.Code)
		return webFeedPayload(into, failure)
	}
	failf := func(code, format string, args ...any) (event.Payload, error) {
		return fail(webfeed.Errorf(code, format, args...))
	}

	if opName != webFeedName {
		return failf(webfeed.CodeInvalidRequest, "there is no operation %q; did you mean %s?", opName, webFeedName)
	}
	if tenant == "" {
		return failf(webfeed.CodeNoTenant, "no tenant in request scope")
	}
	if !d.enabled {
		return failf(webfeed.CodeDisabled, "feed reading is turned off on this chassis")
	}
	req, perr := webfeed.Parse(meta)
	if perr != nil {
		return fail(perr)
	}
	if _, ferr := fetch.CheckURL(req.URL); ferr != nil {
		return failf(ferr.Code, "%s", ferr.Message)
	}

	if ok, _ := d.rate.Allow(tenant); !ok {
		return failf(webfeed.CodeRateLimited, "this tenant has started too many feed reads on this node in the last minute")
	}
	if !d.enter(tenant) {
		return failf(webfeed.CodeBusy, "this tenant already has %d feed reads in flight on this node", d.tenantMax)
	}
	defer d.leave(tenant)

	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return failf(webfeed.CodeBusy, "every fetch slot on this node stayed busy until the op's deadline")
	}
	stage := gjson.GetBytes(in, "_txc.op").String()
	_ = processor.AddFuel(ctx, processor.FuelCostWebFeed, stage)
	res, err := d.fetcher.Get(ctx, fetch.Request{URL: req.URL, UserAgent: webFeedUserAgent(ctx)})
	<-d.slots
	if err != nil {
		var fe *fetch.Error
		if !errors.As(err, &fe) {
			return failf(webfeed.CodeInternal, "the fetch failed unexpectedly")
		}
		run.bytes = fe.Bytes
		chargePerMiB(ctx, fe.Bytes, processor.FuelCostHTMLPerMiB, in)
		failure.ContentType = fe.ContentType
		return fail(&webfeed.Error{Code: fe.Code, Message: fe.Message, Status: fe.Status})
	}
	run.bytes, run.redirects, run.status = res.Bytes, res.Redirects, res.Status
	chargePerMiB(ctx, res.Bytes, processor.FuelCostHTMLPerMiB, in)
	failure.FinalURL, failure.ContentType = res.FinalURL, res.ContentType

	parseStart := time.Now()
	doc, rerr := webfeed.Read(res.Body, res.ContentType, res.FinalURL)
	run.parseMS = time.Since(parseStart).Milliseconds() + 1 // every started millisecond
	_ = processor.AddFuel(ctx, run.parseMS*processor.FuelCostHTMLExtractPerMs, stage)
	if rerr != nil {
		return fail(rerr)
	}
	items, truncated := webfeed.Select(doc.Items, req)
	truncated = truncated || doc.Truncated
	run.format, run.items, run.total = doc.Format, len(items), doc.Total
	ans := webfeed.Answer{
		OK: true, URL: rawURL, FinalURL: res.FinalURL, Status: res.Status,
		Format: doc.Format, Feed: doc.Feed, Items: items, Count: len(items), Total: doc.Total, Truncated: truncated,
	}
	pl, _ := webFeedPayload(into, ans)
	run.output = len(pl.Raw)
	emitWebFeedEvent(ctx, rawURL, res.FinalURL, run, truncated, "")
	return pl, nil
}

func (d *webFeedDeps) enter(tenant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] >= d.tenantMax {
		return false
	}
	d.inflight[tenant]++
	return true
}

func (d *webFeedDeps) leave(tenant string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] <= 1 {
		delete(d.inflight, tenant)
		return
	}
	d.inflight[tenant]--
}

func webFeedPayload(into string, v any) (event.Payload, error) {
	b, _ := json.Marshal(v)
	raw, _ := sjson.SetRaw(`{}`, into, string(b))
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func webFeedUserAgent(ctx context.Context) string {
	v, _ := ctx.Value(config.CtxKeyVersion).(string)
	if v == "" {
		v = "dev"
	}
	return "txco-feed/" + v + " (+https://www.thanks.computer)"
}

// emitWebFeedEvent records one call on the trace timeline. Timeline
// events are not redacted, so it carries the host and a short hash of
// the URL, never the path or the query, and never an item.
func emitWebFeedEvent(ctx context.Context, rawURL, finalURL string, run *webFeedRun, truncated bool, errCode string) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	sum := sha256.Sum256([]byte(rawURL))
	fields := map[string]any{
		"url_sha256":  hex.EncodeToString(sum[:8]),
		"duration_ms": time.Since(run.start).Milliseconds(),
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
	if finalURL != "" {
		if h := host(finalURL); h != fields["host"] {
			fields["final_host"] = h
		}
	}
	if run.status != 0 {
		fields["status"] = run.status
	}
	if run.redirects > 0 {
		fields["redirects"] = run.redirects
	}
	if run.parseMS > 0 {
		fields["parse_ms"] = run.parseMS
	}
	if errCode != "" {
		fields["error_code"] = errCode
	} else {
		fields["format"] = run.format
		fields["items"] = run.items
		fields["total"] = run.total
		fields["output_bytes"] = run.output
		if truncated {
			fields["truncated"] = true
		}
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "web.feed.completion", Fields: fields})
}
