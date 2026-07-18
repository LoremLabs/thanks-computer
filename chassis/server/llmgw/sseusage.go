package llmgw

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// Token-usage capture: a passive tee over the upstream response. It
// never modifies, delays, or fails the bytes the client receives —
// observability failure here is ALWAYS non-fatal, reported only as a
// structured skip reason for debug logs. Record-only in this phase:
// captured tokens land on the completion envelope; nothing is charged.

// Structured skip reasons (debug-log taxonomy; "" = not skipped).
const (
	skipUnsupportedEncoding = "unsupported_encoding"
	skipCompressedLimit     = "compressed_limit"
	skipDecompressedLimit   = "decompressed_limit"
	skipInvalidJSON         = "invalid_json"
)

const (
	// Non-stream JSON: capture at most this many raw (possibly
	// compressed) bytes; past it, usage capture is omitted.
	nonStreamCaptureMax = 2 << 20 // 2 MiB
	// And never inflate past this when the body was gzipped.
	nonStreamInflateMax = 4 << 20 // 4 MiB
	// SSE: data payloads are retained only for the three interesting
	// event types, which are small; a "data:" accumulation past this is
	// abandoned (frame skipped), and a single line past sseLineMax
	// resyncs at the next newline. Content deltas are discarded at the
	// line stage — memory stays O(1) on unbounded streams.
	sseInterestingDataMax = 64 << 10
	sseLineMax            = 1 << 20
)

// usageResult is what the capture learned. Token fields are
// presence-gated (has*) — absent means "not captured", never zero.
type usageResult struct {
	captured   bool   // at least one interesting event/field parsed
	skipReason string // one of the skip* constants; "" = none

	model      string // response model (message_start / JSON body)
	stopReason string

	inputTokens   int64
	outputTokens  int64
	cacheRead     int64
	cacheCreation int64

	hasInput         bool
	hasOutput        bool
	hasCacheRead     bool
	hasCacheCreation bool
}

// usageCapture implements io.Writer over the upstream response bytes.
// Write NEVER returns an error and never blocks on anything external.
type usageCapture struct {
	sse   bool // SSE framing mode; else bounded non-stream JSON capture
	inert bool // capture pre-declined (unsupported encoding); Write is a no-op

	// SSE framing state — incremental, safe across arbitrary chunk
	// boundaries.
	line         []byte // partial current line
	discard      bool   // dropping an oversized line until its newline
	event        string // current frame's event field ("" = default "message")
	data         []byte // accumulated data lines, interesting events only
	dataOverflow bool

	// Non-stream capture state.
	body      []byte
	truncated bool
	gzipped   bool

	res usageResult
}

// newUsageCapture picks the capture mode from the upstream response
// headers. Returns nil when the response is neither SSE nor JSON —
// nothing to capture, nothing to log. An SSE/JSON response in an
// encoding we don't handle yields an inert capture whose finish()
// reports unsupported_encoding.
func newUsageCapture(contentType, contentEncoding string) *usageCapture {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		if enc != "" && enc != "identity" {
			// Effectively never happens (SSE defeats compression), but
			// correctness over cleverness.
			return &usageCapture{sse: true, inert: true, res: usageResult{skipReason: skipUnsupportedEncoding}}
		}
		return &usageCapture{sse: true}
	case strings.HasPrefix(ct, "application/json"):
		switch enc {
		case "", "identity":
			return &usageCapture{}
		case "gzip":
			return &usageCapture{gzipped: true}
		default:
			return &usageCapture{inert: true, res: usageResult{skipReason: skipUnsupportedEncoding}}
		}
	default:
		return nil
	}
}

// Write consumes a chunk of upstream bytes. Always returns (len(p), nil).
func (u *usageCapture) Write(p []byte) (int, error) {
	n := len(p)
	if u.inert {
		return n, nil
	}
	if !u.sse {
		if !u.truncated {
			if len(u.body)+len(p) > nonStreamCaptureMax {
				u.truncated = true
				u.body = nil
			} else {
				u.body = append(u.body, p...)
			}
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !u.discard {
				if len(u.line)+len(p) > sseLineMax {
					u.discard = true
					u.line = nil
				} else {
					u.line = append(u.line, p...)
				}
			}
			return n, nil
		}
		if u.discard {
			// The oversized line just ended; resync from the next one.
			u.discard = false
		} else {
			line := append(u.line, p[:i]...)
			u.processLine(bytes.TrimSuffix(line, []byte{'\r'}))
		}
		u.line = u.line[:0]
		p = p[i+1:]
	}
	return n, nil
}

func interestingEvent(event string) bool {
	switch event {
	case "message_start", "message_delta", "message_stop":
		return true
	}
	return false
}

// processLine handles one complete SSE line (newline stripped).
func (u *usageCapture) processLine(line []byte) {
	switch {
	case len(line) == 0:
		u.dispatch()
	case bytes.HasPrefix(line, []byte("event:")):
		u.event = string(bytes.TrimSpace(line[len("event:"):]))
	case bytes.HasPrefix(line, []byte("data:")):
		if !interestingEvent(u.event) {
			return // content deltas etc. — never retained
		}
		d := line[len("data:"):]
		if len(d) > 0 && d[0] == ' ' {
			d = d[1:]
		}
		if u.dataOverflow || len(u.data)+len(d)+1 > sseInterestingDataMax {
			u.dataOverflow = true
			return
		}
		if len(u.data) > 0 {
			u.data = append(u.data, '\n')
		}
		u.data = append(u.data, d...)
	}
	// Comments (":…"), id:, retry: — ignored.
}

// dispatch fires at a frame boundary (blank line).
func (u *usageCapture) dispatch() {
	event, data, overflow := u.event, u.data, u.dataOverflow
	u.event = ""
	u.data = u.data[:0]
	u.dataOverflow = false
	if len(data) == 0 || overflow || !interestingEvent(event) {
		return
	}
	if !gjson.ValidBytes(data) {
		return // malformed frame: skip silently, keep parsing
	}
	switch event {
	case "message_start":
		if m := gjson.GetBytes(data, "message.model"); m.Type == gjson.String && m.String() != "" {
			u.res.model = m.String()
			u.res.captured = true
		}
		if t := gjson.GetBytes(data, "message.usage.input_tokens"); t.Exists() {
			u.res.inputTokens, u.res.hasInput = t.Int(), true
			u.res.captured = true
		}
		if t := gjson.GetBytes(data, "message.usage.cache_read_input_tokens"); t.Exists() {
			u.res.cacheRead, u.res.hasCacheRead = t.Int(), true
		}
		if t := gjson.GetBytes(data, "message.usage.cache_creation_input_tokens"); t.Exists() {
			u.res.cacheCreation, u.res.hasCacheCreation = t.Int(), true
		}
	case "message_delta":
		// output_tokens is cumulative: every delta overwrites, so the
		// FINAL delta wins — this is why a prefix buffer can't work.
		if t := gjson.GetBytes(data, "usage.output_tokens"); t.Exists() {
			u.res.outputTokens, u.res.hasOutput = t.Int(), true
			u.res.captured = true
		}
		if s := gjson.GetBytes(data, "delta.stop_reason"); s.Type == gjson.String && s.String() != "" {
			u.res.stopReason = s.String()
		}
	case "message_stop":
		u.res.captured = true
	}
}

// finish settles the result. For non-stream captures this is where the
// (bounded) decompress + single parse happens. All errors are swallowed
// into a skip reason; the client response was long since sent.
func (u *usageCapture) finish() usageResult {
	if u.inert || u.sse {
		return u.res
	}
	if u.truncated {
		u.res.skipReason = skipCompressedLimit
		return u.res
	}
	if len(u.body) == 0 {
		return u.res
	}
	body := u.body
	if u.gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			u.res.skipReason = skipInvalidJSON
			return u.res
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, nonStreamInflateMax+1))
		_ = zr.Close()
		if err != nil {
			u.res.skipReason = skipInvalidJSON
			return u.res
		}
		if len(inflated) > nonStreamInflateMax {
			u.res.skipReason = skipDecompressedLimit
			return u.res
		}
		body = inflated
	}
	if !gjson.ValidBytes(body) {
		u.res.skipReason = skipInvalidJSON
		return u.res
	}
	if m := gjson.GetBytes(body, "model"); m.Type == gjson.String && m.String() != "" {
		u.res.model = m.String()
		u.res.captured = true
	}
	if t := gjson.GetBytes(body, "usage.input_tokens"); t.Exists() {
		u.res.inputTokens, u.res.hasInput = t.Int(), true
		u.res.captured = true
	}
	if t := gjson.GetBytes(body, "usage.output_tokens"); t.Exists() {
		u.res.outputTokens, u.res.hasOutput = t.Int(), true
		u.res.captured = true
	}
	if t := gjson.GetBytes(body, "usage.cache_read_input_tokens"); t.Exists() {
		u.res.cacheRead, u.res.hasCacheRead = t.Int(), true
	}
	if t := gjson.GetBytes(body, "usage.cache_creation_input_tokens"); t.Exists() {
		u.res.cacheCreation, u.res.hasCacheCreation = t.Int(), true
	}
	if s := gjson.GetBytes(body, "stop_reason"); s.Type == gjson.String && s.String() != "" {
		u.res.stopReason = s.String()
	}
	return u.res
}
