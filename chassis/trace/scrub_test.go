package trace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const (
	secretToken  = "pony_0123456789abcdef0123456789abcdef01234567"
	secretCookie = "SESSIONVALUE7781"
	secretRaw    = "MAILRAWBYTES"
	secretHTML   = "MAILHTMLBYTES"
	secretFile   = "ATTACHEDBYTES"
)

// secretEnvelope has one of everything, in the case each head writes it.
const secretEnvelope = `{"_txc":{"src":"http",` +
	`"web":{"req":{"headers":{"Authorization":["Bearer ` + secretToken + `"],"Cookie":["s=` + secretCookie + `"],"Proxy-Authorization":["Basic ` + secretToken + `"],"X-Api-Key":["` + secretToken + `"],"X-Auth-Token":["` + secretToken + `"],"User-Agent":["curl"]},` +
	`"cookies":{"s":["` + secretCookie + `"]}},` +
	`"res":{"headers":{"set-cookie":["s=` + secretCookie + `"],"content-type":["text/plain"]}}},` +
	`"llm":{"headers":{"authorization":"Bearer ` + secretToken + `"}},` +
	`"lmtp":{"msg":{"raw":"` + secretRaw + `","html":"` + secretHTML + `","attachments":[{"content":"` + secretFile + `"}],"text":"kept"}}}}`

func assertNoSecrets(t *testing.T, b []byte) {
	t.Helper()
	for _, s := range []string{secretToken, secretCookie, secretRaw, secretHTML, secretFile} {
		if strings.Contains(string(b), s) {
			t.Errorf("%q reached the record: %s", s, b)
		}
	}
}

func TestScrub(t *testing.T) {
	in := []byte(secretEnvelope)
	out := Scrub(in)
	assertNoSecrets(t, out)
	if !gjson.ValidBytes(out) {
		t.Fatalf("not JSON: %s", out)
	}
	// What is safe stays, and a masked field still shows it was there.
	for path, want := range map[string]string{
		"_txc.web.req.headers.User-Agent.0":   "curl",
		"_txc.web.res.headers.content-type.0": "text/plain",
		"_txc.web.req.headers.Authorization":  "[REDACTED]",
		"_txc.web.res.headers.set-cookie":     "[REDACTED]",
		"_txc.web.req.cookies":                "[REDACTED]",
		"_txc.lmtp.msg.text":                  "kept",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if string(in) != secretEnvelope {
		t.Fatal("Scrub changed its input: the runtime envelope shares these bytes")
	}
}

func TestScrubMatchesHeadersInAnyCase(t *testing.T) {
	for _, name := range []string{"authorization", "AUTHORIZATION", "AuThOrIzAtIoN", "COOKIE", "Set-Cookie", "SET-COOKIE", "x-api-key"} {
		b := []byte(`{"_txc":{"web":{"req":{"headers":{"` + name + `":["` + secretToken + `"]}}}}}`)
		assertNoSecrets(t, Scrub(b))
	}
}

func TestScrubLeavesOtherShapesAlone(t *testing.T) {
	for _, s := range []string{``, `not json`, `{"a":1}`, `{"_txc":{"web":{"req":{"headers":"odd"}}}}`} {
		if got := string(Scrub([]byte(s))); got != s {
			t.Errorf("Scrub(%q) = %q", s, got)
		}
	}
}

// The sequence that leaked: a tenant's hint exists, but the request has not
// reached its stack. Every record of the boot pipeline is written first.
func TestBootRecordsAreScrubbedBeforeAnyStackHint(t *testing.T) {
	lookup := func(tenant, stack string) Hints {
		if tenant == "acme" && stack == "ranch" {
			return Hints{Redact: []string{"_txc.web.req.headers.Authorization"}}
		}
		return Hints{}
	}
	inner := newCaptureSink()
	tr := NewRedactingSink(inner, lookup).Begin(RequestInfo{RID: "r", Tenant: "_sys", Stack: "boot", Payload: []byte(secretEnvelope)})
	for _, name := range []string{"detect", "static", "route"} {
		tr.Step(StepInfo{Tenant: "_sys", Stack: "boot", Name: name, Input: []byte(secretEnvelope), Output: []byte(secretEnvelope)})
	}
	tr.Step(StepInfo{Tenant: "acme", Stack: "ranch", Name: "token", Input: []byte(secretEnvelope)})
	tr.End("ok", "", []byte(secretEnvelope))

	got := inner.get("r")
	assertNoSecrets(t, got.info.Payload)
	assertNoSecrets(t, got.final)
	for _, s := range got.steps {
		assertNoSecrets(t, s.Input)
		assertNoSecrets(t, s.Output)
	}
}

func runAsync(t *testing.T, cap int, lookup HintLookup, stack string, body []byte) *captureReq {
	t.Helper()
	inner := newCaptureSink()
	sink := NewAsyncSink(NewRedactingSink(inner, lookup), AsyncOpts{BodyCapBytes: cap, BufferSize: 16})
	tr := sink.Begin(RequestInfo{RID: "r", Tenant: "_sys", Stack: "boot", Payload: body})
	tr.Step(StepInfo{Tenant: "t", Stack: stack, Name: "s", Input: body, Output: body})
	tr.End("ok", "", body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return inner.get("r")
}

// A cut can land inside any value, and JSON that has lost its end cannot be
// searched. Every cut position must still leave nothing behind.
func TestAsyncCapCannotDefeatTheScrub(t *testing.T) {
	body := []byte(secretEnvelope)
	for cap := 1; cap < len(body)+8; cap += 7 {
		got := runAsync(t, cap, nil, "s", body)
		assertNoSecrets(t, got.info.Payload)
		assertNoSecrets(t, got.final)
		for _, s := range got.steps {
			assertNoSecrets(t, s.Input)
			assertNoSecrets(t, s.Output)
		}
		if t.Failed() {
			t.Fatalf("cap %d", cap)
		}
	}
}

// A large mail is the case that broke: the cut lands inside raw.
func TestAsyncLargeMailKeepsItsSmallFacts(t *testing.T) {
	raw := strings.Repeat(secretRaw, 20000)
	body := []byte(`{"_txc":{"src":"lmtp","lmtp":{"mail":{"from":"a@example.com"},"msg":{"raw":"` + raw + `","text":"kept"}}}}`)
	got := runAsync(t, 4096, nil, "s", body)
	assertNoSecrets(t, got.info.Payload)
	if s := gjson.GetBytes(got.info.Payload, "_txc.lmtp.mail.from").String(); s != "a@example.com" {
		t.Fatalf("the rest of the record went with it: %s", got.info.Payload)
	}
	if got.info.PayloadBytes != len(body) {
		t.Fatalf("PayloadBytes = %d, want the original %d", got.info.PayloadBytes, len(body))
	}
}

// A stack's own hint cannot be applied to a record cut short, so the record
// goes rather than what the hint hides.
func TestCutRecordUnderAStackHintIsDropped(t *testing.T) {
	const private = "STACKPRIVATEVALUE"
	lookup := func(_, stack string) Hints {
		if stack == "ranch" {
			return Hints{Redact: []string{"_tkp.secret"}}
		}
		return Hints{}
	}
	body := []byte(`{"pad":"` + strings.Repeat("x", 500) + `","_tkp":{"secret":"` + private + `"}}`)
	// The cut lands inside the secret: "pad" + 30 bytes of the object after it.
	cap := strings.Index(string(body), private) + 6
	got := runAsync(t, cap, lookup, "ranch", body)
	if len(got.steps) != 1 {
		t.Fatalf("steps = %d", len(got.steps))
	}
	if s := string(got.steps[0].Input); strings.Contains(s, private[:6]) || !strings.Contains(s, "dropped") {
		t.Fatalf("input = %s", s)
	}
	// No hint in play: a cut record is kept, as it always was.
	got = runAsync(t, cap, lookup, "other", body)
	if s := string(got.steps[0].Input); !strings.HasPrefix(s, `{"pad":"xxx`) {
		t.Fatalf("input = %.40s", s)
	}
}

func assertKeepsSecrets(t *testing.T, b []byte) {
	t.Helper()
	for _, s := range []string{secretToken, secretCookie, secretRaw, secretHTML, secretFile} {
		if !strings.Contains(string(b), s) {
			t.Errorf("%q is missing from the record: %s", s, b)
		}
	}
}

// --trace-no-scrub is for reading the credentials a request really carried, so
// every record has to keep them: this is the behavior from before Scrub.
func TestHintOnlySinkKeepsWhatScrubRemoves(t *testing.T) {
	inner := newCaptureSink()
	sink := NewHintOnlySink(inner, nil)
	body := []byte(secretEnvelope)
	tr := sink.Begin(RequestInfo{RID: "r", Tenant: "_sys", Stack: "boot", Payload: body})
	tr.Step(StepInfo{Tenant: "t", Stack: "s", Name: "s", Input: body, Output: body})
	tr.End("ok", "", body)

	got := inner.get("r")
	assertKeepsSecrets(t, got.info.Payload)
	assertKeepsSecrets(t, got.steps[0].Input)
	assertKeepsSecrets(t, got.steps[0].Output)
	assertKeepsSecrets(t, got.final)
}

// The flag turns off the chassis's scrub, never a stack's own decision.
func TestHintOnlySinkStillAppliesStackHints(t *testing.T) {
	inner := newCaptureSink()
	sink := NewHintOnlySink(inner, func(_, stack string) Hints {
		if stack == "s" {
			return Hints{Redact: []string{"_txc.web.req.headers.Authorization"}}
		}
		return Hints{}
	})
	body := []byte(secretEnvelope)
	tr := sink.Begin(RequestInfo{RID: "r", Tenant: "_sys", Stack: "boot", Payload: body})
	tr.Step(StepInfo{Tenant: "t", Stack: "s", Name: "s", Input: body})
	tr.End("ok", "", nil)

	in := inner.get("r").steps[0].Input
	if got := gjson.GetBytes(in, "_txc.web.req.headers.Authorization").String(); got != "[REDACTED]" {
		t.Errorf("stack hint not applied: %q", got)
	}
	if !strings.Contains(string(in), secretCookie) {
		t.Errorf("the flag should keep what no hint names: %s", in)
	}
}

// runAsyncNoScrub is the wiring server.go does for --trace-no-scrub.
func runAsyncNoScrub(t *testing.T, cap int, lookup HintLookup, body []byte) *captureReq {
	t.Helper()
	inner := newCaptureSink()
	sink := NewAsyncSink(NewHintOnlySink(inner, lookup), AsyncOpts{BodyCapBytes: cap, BufferSize: 16, NoScrub: true})
	tr := sink.Begin(RequestInfo{RID: "r", Tenant: "_sys", Stack: "boot", Payload: body})
	tr.Step(StepInfo{Tenant: "t", Stack: "s", Name: "s", Input: body, Output: body})
	tr.End("ok", "", body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return inner.get("r")
}

// Over the cap, the async layer is what would scrub, so it has to obey too.
func TestAsyncCapHonorsNoScrub(t *testing.T) {
	body := []byte(secretEnvelope)
	cap := len(body) - 1 // over the cap, with every secret still ahead of the cut
	got := runAsyncNoScrub(t, cap, nil, body)
	assertKeepsSecrets(t, got.info.Payload)
	assertKeepsSecrets(t, got.steps[0].Input)
	assertKeepsSecrets(t, got.final)
}

// A hint that cannot be applied to a cut record still loses the record: the
// flag has no say over what a stack asked to hide.
func TestNoScrubStillDropsACutRecordUnderAHint(t *testing.T) {
	lookup := func(_, stack string) Hints {
		if stack == "s" {
			return Hints{Omit: []string{"_txc.web.req.headers.Authorization"}}
		}
		return Hints{}
	}
	got := runAsyncNoScrub(t, 64, lookup, []byte(secretEnvelope))
	if in := got.steps[0].Input; string(in) != string(droppedRecord) {
		t.Fatalf("cut record kept under a hint: %s", in)
	}
}
