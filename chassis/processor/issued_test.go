package processor

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// TestScrubIssued — an issued password must leave no readable trace: not as
// itself, and not inside a base64 response body, at whatever byte offset
// the rendering put it.
func TestScrubIssued(t *testing.T) {
	const pw = "k7m2-river-galaxy-bamboo-orbit-velvet"
	ctx := WithIssuedSecrets(context.Background())
	NoteIssuedSecret(ctx, pw)
	NoteIssuedSecret(ctx, "short") // below the floor: never scrubbed

	plain := []byte(`{"_cred":{"password":"` + pw + `"},"note":"short"}`)
	got := ScrubIssued(ctx, plain)
	if strings.Contains(string(got), pw) || gjson.GetBytes(got, "_cred.password").String() != "[REDACTED]" ||
		gjson.GetBytes(got, "note").String() != "short" {
		t.Errorf("plain = %s", got)
	}
	if !strings.Contains(string(plain), pw) {
		t.Error("the input was modified")
	}

	// A response body renders the password at some offset; base64 is
	// grouped in threes, so try every offset.
	for pad := 0; pad < 6; pad++ {
		body := `{"username":"paris","password":"` + strings.Repeat("x", pad) + pw + `","ok":true}`
		env := []byte(`{"_txc":{"web":{"res":{"body":"` + base64.StdEncoding.EncodeToString([]byte(body)) + `"}}}}`)
		out := ScrubIssued(ctx, env)
		dec, err := base64.StdEncoding.DecodeString(gjson.GetBytes(out, "_txc.web.res.body").String())
		if err != nil {
			t.Fatalf("pad %d: the scrubbed body no longer decodes: %v", pad, err)
		}
		if strings.Contains(string(dec), "river-galaxy-bamboo") {
			t.Errorf("pad %d: the password survives in the body: %q", pad, dec)
		}
		if !strings.HasPrefix(string(dec), `{"username":"paris"`) || !strings.HasSuffix(string(dec), `","ok":true}`) {
			t.Errorf("pad %d: the scrub reached past the password: %q", pad, dec)
		}
	}

	// Outside a request (no list), nothing happens.
	if got := ScrubIssued(context.Background(), plain); string(got) != string(plain) {
		t.Errorf("no list: %s", got)
	}
}

// TestScrubbingTracer — the wrapper scrubs every step, whoever emits it, and
// the final payload.
func TestScrubbingTracer(t *testing.T) {
	const pw = "txc_k7m2_p3vw8nq2r5t7vx9z2b4d6f8h9k3m5n"
	ctx := WithIssuedSecrets(context.Background())
	rec := &scrubRecorder{}
	tr := ScrubbingTracer(ctx, rec)
	NoteIssuedSecret(ctx, pw)
	tr.Step(trace.StepInfo{Input: []byte(`{"a":"` + pw + `"}`), Output: []byte(`{"b":"` + pw + `"}`)})
	tr.End("ok", "", []byte(`{"c":"`+pw+`"}`))
	for _, b := range append(append([][]byte{}, rec.steps...), rec.final) {
		if strings.Contains(string(b), pw) {
			t.Errorf("recorded %s", b)
		}
	}
}

type scrubRecorder struct {
	steps [][]byte
	final []byte
}

func (r *scrubRecorder) Step(info trace.StepInfo)  { r.steps = append(r.steps, info.Input, info.Output) }
func (r *scrubRecorder) Event(trace.TimelineEvent) {}
func (r *scrubRecorder) End(_, _ string, b []byte) { r.final = b }
