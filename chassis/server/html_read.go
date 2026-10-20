package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/htmlextract"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// txco://html/read fetches one public HTML page and returns it, or the
// element a selector picks, as Markdown — a part at a time
// (docs/advanced/html-read.md). html/extract's sibling: the same fetch,
// the same document bounds, the same per-node and per-tenant limits (it
// shares htmlExtractDeps), and the page's text instead of values picked
// out of it.
//
//	WITH url, root?, part? (1…, or "last"; default 1), part_chars? (200–20000; default 3000), into? (default _page)
//
// The answer is in-band at `into`, with a nil Go error either way:
//
//	{ok: true, url, final_url, status, content_type, title, root, part, parts, chars, total_chars, text, truncated?}
//	{ok: false, url, content_type?, parts?, error: {code: "txco_html_<code>", message, status?}}
type htmlReadAnswer struct {
	OK          bool       `json:"ok"`
	URL         string     `json:"url"`
	FinalURL    string     `json:"final_url,omitempty"`
	Status      int        `json:"status,omitempty"`
	ContentType string     `json:"content_type,omitempty"`
	Title       string     `json:"title,omitempty"`
	Root        string     `json:"root,omitempty"`
	Part        int        `json:"part,omitempty"`
	Parts       *int       `json:"parts,omitempty"`
	Chars       int        `json:"chars,omitempty"`
	TotalChars  int        `json:"total_chars,omitempty"`
	Text        *string    `json:"text,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"`
	Error       *htmlError `json:"error,omitempty"`
}

// htmlReadRun is one call's bookkeeping, for the completion event.
type htmlReadRun struct {
	start     time.Time
	bytes     int64
	redirects int
	parseMS   int64
	nodes     int
	parts     int
	part      int
	chars     int
}

func htmlRead(ctx context.Context, d *htmlExtractDeps, in []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_page")
	rawURL := gjson.GetBytes(meta, "url").String()
	run := &htmlReadRun{start: time.Now()}
	ans := htmlReadAnswer{URL: rawURL}
	failStatus := func(code, msg string, status int) (event.Payload, error) {
		ans.OK = false
		ans.Error = &htmlError{Code: "txco_html_" + code, Message: msg, Status: status}
		emitHTMLReadEvent(ctx, rawURL, ans, run)
		return htmlReadPayload(into, ans), nil
	}
	fail := func(code, msg string) (event.Payload, error) { return failStatus(code, msg, 0) }

	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return fail("no_tenant", "no tenant in request scope")
	}
	if _, ferr := fetch.CheckURL(rawURL); ferr != nil {
		return fail(ferr.Code, ferr.Message)
	}
	opts, oerr := htmlReadOptions(meta, d.limits)
	if oerr != nil {
		return fail(oerr.Code, oerr.Message)
	}

	if ok, _ := d.rate.Allow(tenant); !ok {
		return fail("rate_limited", "this tenant has started too many page reads and extractions on this node in the last minute")
	}
	if !d.enter(tenant) {
		return fail("busy", "this tenant already has its limit of page reads and extractions in flight on this node")
	}
	defer d.leave(tenant)

	select {
	case d.fetchSlots <- struct{}{}:
	case <-ctx.Done():
		return fail("busy", "every fetch slot on this node stayed busy until the op's deadline")
	}
	stage := gjson.GetBytes(in, "_txc.op").String()
	_ = processor.AddFuel(ctx, processor.FuelCostHTMLExtract, stage)
	res, err := d.fetcher.Get(ctx, fetch.Request{URL: rawURL, UserAgent: htmlReadUserAgent(ctx)})
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
	opts.BaseURL = res.FinalURL

	select {
	case d.parseSlots <- struct{}{}:
	case <-ctx.Done():
		return fail("busy", "every parse slot on this node stayed busy until the op's deadline")
	}
	parseStart := time.Now()
	out, err := htmlextract.Read(ctx, res.Body, res.ContentType, opts, d.limits)
	elapsed := time.Since(parseStart)
	<-d.parseSlots
	run.parseMS = elapsed.Milliseconds() + 1 // every started millisecond
	_ = processor.AddFuel(ctx, run.parseMS*processor.FuelCostHTMLExtractPerMs, stage)
	if out != nil {
		run.nodes, run.parts = out.Nodes, out.Parts
		ans.Title, ans.Root = out.Title, out.Root
	}
	if err != nil {
		var he *htmlextract.Error
		if errors.As(err, &he) {
			if he.Code == htmlextract.CodeNoSuchPart && out != nil {
				parts := out.Parts
				ans.Parts = &parts
			}
			return fail(he.Code, he.Message)
		}
		return fail("internal_error", "the read failed unexpectedly")
	}
	run.part, run.chars = out.Part, out.Chars
	parts, text := out.Parts, out.Text
	ans.OK, ans.Part, ans.Parts, ans.Chars, ans.TotalChars, ans.Text, ans.Truncated = true, out.Part, &parts, out.Chars, out.TotalChars, &text, out.Truncated
	emitHTMLReadEvent(ctx, rawURL, ans, run)
	return htmlReadPayload(into, ans), nil
}

// htmlReadOptions reads root, part and part_chars off WITH. A part is a
// whole number from 1 or the word "last"; part_chars a whole number in
// the reader's range. Anything else is invalid_request naming the field.
func htmlReadOptions(meta []byte, lim htmlextract.Limits) (htmlextract.ReadOptions, *htmlextract.Error) {
	opts := htmlextract.ReadOptions{Part: 1, PartChars: htmlextract.DefaultPartChars}
	if r := gjson.GetBytes(meta, "root"); r.Exists() && r.Type != gjson.Null {
		if r.Type != gjson.String {
			return opts, &htmlextract.Error{Code: htmlextract.CodeInvalidRequest, Message: "`root` must be a CSS selector"}
		}
		opts.Root = r.String()
	}
	if p := gjson.GetBytes(meta, "part"); p.Exists() && p.Type != gjson.Null {
		switch {
		case p.Type == gjson.String && strings.EqualFold(strings.TrimSpace(p.String()), "last"):
			opts.Last = true
		case p.Type == gjson.String && strings.TrimSpace(p.String()) != "":
			n, ok := wholeNumber(strings.TrimSpace(p.String()))
			if !ok || n < 1 {
				return opts, &htmlextract.Error{Code: htmlextract.CodeInvalidRequest, Message: "`part` must be a whole number from 1, or \"last\""}
			}
			opts.Part = n
		case p.Type == gjson.Number && float64(int64(p.Float())) == p.Float() && p.Int() >= 1:
			opts.Part = int(p.Int())
		default:
			return opts, &htmlextract.Error{Code: htmlextract.CodeInvalidRequest, Message: "`part` must be a whole number from 1, or \"last\""}
		}
	}
	if c := gjson.GetBytes(meta, "part_chars"); c.Exists() && c.Type != gjson.Null {
		n := -1
		switch {
		case c.Type == gjson.Number && float64(int64(c.Float())) == c.Float():
			n = int(c.Int())
		case c.Type == gjson.String:
			if v, ok := wholeNumber(strings.TrimSpace(c.String())); ok {
				n = v
			}
		}
		if n < htmlextract.MinPartChars || n > htmlextract.MaxPartChars {
			return opts, &htmlextract.Error{Code: htmlextract.CodeInvalidRequest,
				Message: "`part_chars` must be a whole number from " + strconv.Itoa(htmlextract.MinPartChars) + " to " + strconv.Itoa(htmlextract.MaxPartChars)}
		}
		opts.PartChars = n
	}
	return opts, nil
}

func wholeNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func htmlReadPayload(into string, ans htmlReadAnswer) event.Payload {
	b, _ := json.Marshal(ans)
	raw, _ := sjson.SetRaw(`{}`, into, string(b))
	return event.Payload{Raw: raw, Type: event.JSON}
}

func htmlReadUserAgent(ctx context.Context) string {
	return strings.Replace(htmlUserAgent(ctx), "txco-extract/", "txco-read/", 1)
}

// emitHTMLReadEvent records one call on the trace timeline: the host and
// a short hash of the URL (never the path or the query), the status,
// sizes and times, which part of how many — never the text.
func emitHTMLReadEvent(ctx context.Context, rawURL string, ans htmlReadAnswer, run *htmlReadRun) {
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
	if ans.Root != "" {
		fields["root"] = ans.Root
	}
	if ans.OK {
		fields["parts"] = run.parts
		fields["part"] = run.part
		fields["output_chars"] = run.chars
		if ans.Truncated {
			fields["truncated"] = true
		}
	} else if ans.Error != nil {
		fields["error_code"] = ans.Error.Code
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "html.read.completion", Fields: fields})
}
