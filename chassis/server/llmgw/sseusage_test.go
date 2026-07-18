package llmgw

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

const canonicalStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}}` + "\n" +
	"\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n" +
	"\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello there"}}` + "\n" +
	"\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":5}}` + "\n" +
	"\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n" +
	"\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n" +
	"\n"

// feed writes the stream in fixed-size chunks (chunk=0 ⇒ one shot).
func feed(t *testing.T, u *usageCapture, stream string, chunk int) {
	t.Helper()
	if chunk <= 0 {
		chunk = len(stream)
	}
	for i := 0; i < len(stream); i += chunk {
		end := i + chunk
		if end > len(stream) {
			end = len(stream)
		}
		n, err := u.Write([]byte(stream[i:end]))
		if err != nil || n != end-i {
			t.Fatalf("Write returned (%d,%v), want (%d,nil)", n, err, end-i)
		}
	}
}

func assertCanonical(t *testing.T, res usageResult) {
	t.Helper()
	if !res.captured {
		t.Fatalf("nothing captured: %+v", res)
	}
	if res.model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q", res.model)
	}
	if !res.hasInput || res.inputTokens != 100 {
		t.Errorf("input = (%v,%d)", res.hasInput, res.inputTokens)
	}
	if !res.hasOutput || res.outputTokens != 42 {
		t.Errorf("output = (%v,%d), want final delta 42", res.hasOutput, res.outputTokens)
	}
	if !res.hasCacheRead || res.cacheRead != 50 || !res.hasCacheCreation || res.cacheCreation != 10 {
		t.Errorf("cache = (%v,%d)/(%v,%d)", res.hasCacheRead, res.cacheRead, res.hasCacheCreation, res.cacheCreation)
	}
	if res.stopReason != "end_turn" {
		t.Errorf("stop_reason = %q", res.stopReason)
	}
	if res.skipReason != "" {
		t.Errorf("skipReason = %q", res.skipReason)
	}
}

// TestSSECanonicalAtEveryChunkSize: correctness must not depend on where
// the network happened to split the bytes — including 1-byte feeds that
// split "data:" prefixes, JSON, and CRLF pairs.
func TestSSECanonicalAtEveryChunkSize(t *testing.T) {
	for _, chunk := range []int{0, 1, 2, 3, 7, 32, 1024} {
		u := newUsageCapture("text/event-stream", "")
		feed(t, u, canonicalStream, chunk)
		assertCanonical(t, u.finish())
	}
	// CRLF variant, split mid-pair by the 1-byte feed.
	crlf := strings.ReplaceAll(canonicalStream, "\n", "\r\n")
	u := newUsageCapture("text/event-stream; charset=utf-8", "")
	feed(t, u, crlf, 1)
	assertCanonical(t, u.finish())
}

// TestSSEAbsentUsage: an error stream (or one cut before message_delta)
// yields absent fields — never zeros.
func TestSSEAbsentUsage(t *testing.T) {
	u := newUsageCapture("text/event-stream", "")
	feed(t, u, "event: error\ndata: {\"type\":\"error\"}\n\n", 1)
	res := u.finish()
	if res.captured || res.hasInput || res.hasOutput {
		t.Errorf("captured from an error stream: %+v", res)
	}

	// Disconnect after message_start: input present, output absent.
	u = newUsageCapture("text/event-stream", "")
	head := strings.SplitAfter(canonicalStream, "\n\n")[0]
	feed(t, u, head, 3)
	res = u.finish()
	if !res.hasInput || res.inputTokens != 100 {
		t.Errorf("input lost on partial stream: %+v", res)
	}
	if res.hasOutput {
		t.Errorf("output invented on partial stream: %+v", res)
	}
}

// TestSSEGarbageTolerated: non-JSON data frames, comments, id/retry
// lines, and unknown events are skipped without derailing the parse.
func TestSSEGarbageTolerated(t *testing.T) {
	stream := ": keepalive comment\n" +
		"id: 7\n" +
		"retry: 100\n" +
		"event: message_start\n" +
		"data: {not json at all\n" +
		"\n" +
		canonicalStream
	u := newUsageCapture("text/event-stream", "")
	feed(t, u, stream, 5)
	assertCanonical(t, u.finish())
}

// TestSSEOversizedLineResync: a pathological giant line (over
// sseLineMax) is discarded and parsing resyncs at the next newline.
func TestSSEOversizedLineResync(t *testing.T) {
	giant := "data: " + strings.Repeat("x", sseLineMax+100) + "\n\n"
	u := newUsageCapture("text/event-stream", "")
	feed(t, u, giant+canonicalStream, 8192)
	assertCanonical(t, u.finish())
}

// TestSSEContentNotRetained: bulk content deltas never accumulate —
// memory stays bounded no matter how long the stream runs.
func TestSSEContentNotRetained(t *testing.T) {
	u := newUsageCapture("text/event-stream", "")
	block := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"` + strings.Repeat("y", 1000) + `"}}` + "\n\n"
	for i := 0; i < 1000; i++ {
		feed(t, u, block, 0)
	}
	if len(u.data) != 0 {
		t.Errorf("content delta retained %d bytes", len(u.data))
	}
	feed(t, u, canonicalStream, 0)
	assertCanonical(t, u.finish())
}

// TestNonStreamJSON: plain and gzipped bodies parse within limits.
func TestNonStreamJSON(t *testing.T) {
	body := `{"id":"msg_1","model":"claude-haiku-4-5-20251001","stop_reason":"end_turn",` +
		`"usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}`

	u := newUsageCapture("application/json", "")
	feed(t, u, body, 7)
	assertCanonical(t, u.finish())

	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	u = newUsageCapture("application/json", "gzip")
	feed(t, u, zbuf.String(), 3)
	assertCanonical(t, u.finish())
}

// TestNonStreamSkipReasons: every failure is swallowed into a
// structured reason — never an error, never zeros.
func TestNonStreamSkipReasons(t *testing.T) {
	// unsupported_encoding: an encoding we don't inflate.
	u := newUsageCapture("application/json", "br")
	feed(t, u, "anything", 0)
	if res := u.finish(); res.skipReason != skipUnsupportedEncoding || res.captured {
		t.Errorf("br: %+v", res)
	}
	// SSE + encoding is also declined up front.
	u = newUsageCapture("text/event-stream", "gzip")
	feed(t, u, canonicalStream, 0)
	if res := u.finish(); res.skipReason != skipUnsupportedEncoding || res.captured {
		t.Errorf("sse+gzip: %+v", res)
	}

	// compressed_limit: raw capture past 2 MiB.
	u = newUsageCapture("application/json", "")
	feed(t, u, strings.Repeat("x", nonStreamCaptureMax+1), 1<<20)
	if res := u.finish(); res.skipReason != skipCompressedLimit {
		t.Errorf("compressed_limit: %+v", res)
	}

	// decompressed_limit: small gzip that inflates past 4 MiB.
	var zbuf bytes.Buffer
	zw := gzip.NewWriter(&zbuf)
	_, _ = zw.Write([]byte(`{"usage":{"input_tokens":1},"pad":"`))
	pad := bytes.Repeat([]byte("z"), nonStreamInflateMax)
	_, _ = zw.Write(pad)
	_, _ = zw.Write([]byte(`"}`))
	_ = zw.Close()
	if zbuf.Len() > nonStreamCaptureMax {
		t.Fatalf("test gzip too large to capture: %d", zbuf.Len())
	}
	u = newUsageCapture("application/json", "gzip")
	feed(t, u, zbuf.String(), 1<<20)
	if res := u.finish(); res.skipReason != skipDecompressedLimit {
		t.Errorf("decompressed_limit: %+v", res)
	}

	// invalid_json: an unparseable body.
	u = newUsageCapture("application/json", "")
	feed(t, u, "<!doctype html><body>oops</body>", 0)
	if res := u.finish(); res.skipReason != skipInvalidJSON {
		t.Errorf("invalid_json: %+v", res)
	}

	// corrupt gzip maps into invalid_json (couldn't yield valid JSON).
	u = newUsageCapture("application/json", "gzip")
	feed(t, u, "definitely not gzip", 0)
	if res := u.finish(); res.skipReason != skipInvalidJSON {
		t.Errorf("corrupt gzip: %+v", res)
	}
}

// TestUsageCaptureModeSelection: content types outside SSE/JSON get no
// capture at all (nil — nothing to observe, nothing to log).
func TestUsageCaptureModeSelection(t *testing.T) {
	if newUsageCapture("text/plain", "") != nil {
		t.Errorf("text/plain should not capture")
	}
	if newUsageCapture("", "") != nil {
		t.Errorf("empty content type should not capture")
	}
	if newUsageCapture("application/json; charset=utf-8", "identity") == nil {
		t.Errorf("json+identity should capture")
	}
}
