package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// Some ops issue a secret exactly once: txco://credential/create answers
// with a password that exists nowhere else. It has to reach the rule that
// shows it to a person — the envelope, the HTTP response — and must never
// reach a trace. A path-based redact hint cannot promise that: a stack is
// free to render the password into a response body, which the envelope
// carries base64-encoded. So the op notes the value (NoteIssuedSecret) and
// every trace record of the request is scrubbed of it BY VALUE: as written,
// and as it reads inside base64 text.

// issuedMinLen is the shortest value scrubbed. Everything the chassis
// issues is far longer; a short value would shred unrelated bytes.
const issuedMinLen = 16

type issuedSecrets struct {
	mu   sync.Mutex
	vals []string
}

var ctxKeyIssued = ctxKeyType{name: "issued-secrets"}

// WithIssuedSecrets installs the request's list of issued secrets, unless
// one is there already. Run installs it; a caller that records the
// request's final payload (runWithTrace) installs it first, so it sees what
// the run noted.
func WithIssuedSecrets(ctx context.Context) context.Context {
	if _, ok := ctx.Value(ctxKeyIssued).(*issuedSecrets); ok {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyIssued, &issuedSecrets{})
}

// NoteIssuedSecret records a secret an op just issued, so ScrubIssued keeps
// it out of this request's trace. A no-op outside a request.
func NoteIssuedSecret(ctx context.Context, v string) {
	is, ok := ctx.Value(ctxKeyIssued).(*issuedSecrets)
	if !ok || len(v) < issuedMinLen {
		return
	}
	is.mu.Lock()
	is.vals = append(is.vals, v)
	is.mu.Unlock()
}

// issuedRedacted replaces a secret written as itself.
var issuedRedacted = []byte("[REDACTED]")

// ScrubIssued returns b with every secret this request issued removed —
// the value itself replaced by [REDACTED], and its base64 forms by `A`s of
// the same length (so the text still decodes, to zero bytes where the
// secret was). b is not modified; the result is a copy when anything was
// found.
func ScrubIssued(ctx context.Context, b []byte) []byte {
	is, ok := ctx.Value(ctxKeyIssued).(*issuedSecrets)
	if !ok || len(b) == 0 {
		return b
	}
	is.mu.Lock()
	vals := append([]string(nil), is.vals...)
	is.mu.Unlock()
	for _, v := range vals {
		b = bytes.ReplaceAll(b, []byte(v), issuedRedacted)
		for _, form := range base64Forms(v) {
			b = bytes.ReplaceAll(b, form, bytes.Repeat([]byte("A"), len(form)))
		}
	}
	return b
}

// base64Forms are the substrings the standard base64 encoding of ANY text
// containing v must contain — one for each of the three byte offsets v can
// start at within a 3-byte group. Only the characters fixed by v's bytes
// alone are kept: the group v starts in (when it shares it with earlier
// bytes) and the group it ends in (when it shares it with later ones) are
// dropped.
func base64Forms(v string) [][]byte {
	var out [][]byte
	for k := 0; k < 3; k++ {
		enc := base64.StdEncoding.EncodeToString(append(make([]byte, k), v...))
		start, end := 0, len(enc)
		if k > 0 {
			start = 4
		}
		if (k+len(v))%3 != 0 {
			end -= 4
		}
		if end-start >= 8 {
			out = append(out, []byte(enc[start:end]))
		}
	}
	return out
}

// ScrubbingTracer wraps a request's tracer so every step it records, from
// any emitter, and the final payload are scrubbed of the secrets the
// request issued (ScrubIssued). Install the list on ctx first
// (WithIssuedSecrets): the wrapper reads the one the run writes to.
func ScrubbingTracer(ctx context.Context, t trace.RequestTracer) trace.RequestTracer {
	if t == nil {
		return t
	}
	return &scrubbingTracer{inner: t, ctx: ctx}
}

type scrubbingTracer struct {
	inner trace.RequestTracer
	ctx   context.Context
}

func (s *scrubbingTracer) Step(info trace.StepInfo) {
	info.Input, info.Output = ScrubIssued(s.ctx, info.Input), ScrubIssued(s.ctx, info.Output)
	s.inner.Step(info)
}

func (s *scrubbingTracer) Event(ev trace.TimelineEvent) { s.inner.Event(ev) }

func (s *scrubbingTracer) End(status, reason string, finalPayload []byte) {
	s.inner.End(status, reason, ScrubIssued(s.ctx, finalPayload))
}
