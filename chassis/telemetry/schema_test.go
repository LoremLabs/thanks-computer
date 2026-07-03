package telemetry

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

// dropRecorder is a DropFunc that tallies per-reason counts.
type dropRecorder struct{ counts map[string]int64 }

func newDropRecorder() *dropRecorder { return &dropRecorder{counts: map[string]int64{}} }
func (d *dropRecorder) fn() DropFunc {
	return func(_, reason string, n int64) { d.counts[reason] += n }
}

func envelopeWith(metrics string) []byte {
	return []byte(`{"data":1,"_txc":{"tenant":"spoofed","telemetry":{"metrics":` + metrics + `}}}`)
}

func TestParseAndValidateHappyPath(t *testing.T) {
	payload := envelopeWith(`[
		{"name":"book.queued","kind":"counter","value":1,"attrs":{"source":"search"}},
		{"name":"checkout.duration","kind":"histogram","value":812.5,"unit":"ms","attrs":{"plan":"premium","retries":2,"cached":true}}
	]`)
	rec := newDropRecorder()
	events := ParseAndValidate(payload, "driplit", "driplit/www", "http", t0, rec.fn())

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (%+v)", len(events), events)
	}
	c := events[0]
	if c.Name != "book.queued" || c.Kind != "counter" || c.Value != 1 || c.Unit != "" {
		t.Errorf("counter event wrong: %+v", c)
	}
	if c.Tenant != "driplit" || c.Stack != "driplit/www" || c.Src != "http" || !c.Time.Equal(t0) {
		t.Errorf("enrichment wrong (must be the trusted args, not the envelope): %+v", c)
	}
	if c.Attrs["source"] != "search" {
		t.Errorf("attrs lost: %+v", c.Attrs)
	}
	h := events[1]
	if h.Kind != "histogram" || h.Value != 812.5 || h.Unit != "ms" {
		t.Errorf("histogram event wrong: %+v", h)
	}
	if h.Attrs["plan"] != "premium" || h.Attrs["retries"] != float64(2) || h.Attrs["cached"] != true {
		t.Errorf("scalar attr coercion wrong: %+v", h.Attrs)
	}
	if len(rec.counts) != 0 {
		t.Errorf("unexpected drops: %v", rec.counts)
	}
}

func TestParseAndValidateRejections(t *testing.T) {
	cases := []struct {
		name       string
		metric     string
		wantReason string
	}{
		{"missing name", `{"kind":"counter","value":1}`, dropInvalidName},
		{"bad name chars", `{"name":"book queued!","kind":"counter","value":1}`, dropInvalidName},
		{"name starts with digit", `{"name":"1book","kind":"counter","value":1}`, dropInvalidName},
		{"missing kind", `{"name":"a","value":1}`, dropInvalidKind},
		{"unknown kind", `{"name":"a","kind":"meter","value":1}`, dropInvalidKind},
		{"gauge deferred", `{"name":"a","kind":"gauge","value":1}`, dropUnsupportedKind},
		{"missing value", `{"name":"a","kind":"counter"}`, dropInvalidValue},
		{"string value not coerced", `{"name":"a","kind":"counter","value":"5"}`, dropInvalidValue},
		{"bool value not coerced", `{"name":"a","kind":"counter","value":true}`, dropInvalidValue},
		{"null value", `{"name":"a","kind":"counter","value":null}`, dropInvalidValue},
		{"negative counter", `{"name":"a","kind":"counter","value":-1}`, dropInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newDropRecorder()
			events := ParseAndValidate(envelopeWith(`[`+tc.metric+`]`), "t", "s", "http", t0, rec.fn())
			if len(events) != 0 {
				t.Fatalf("invalid metric accepted: %+v", events)
			}
			if rec.counts[tc.wantReason] != 1 {
				t.Errorf("drop reasons = %v, want %s=1", rec.counts, tc.wantReason)
			}
		})
	}

	// Negative histogram values are fine (only counters are monotonic).
	events := ParseAndValidate(envelopeWith(`[{"name":"temp.delta","kind":"histogram","value":-3.5}]`), "t", "s", "http", t0, nil)
	if len(events) != 1 || events[0].Value != -3.5 {
		t.Errorf("negative histogram should pass: %+v", events)
	}
}

func TestParseAndValidateBadShapes(t *testing.T) {
	for _, metrics := range []string{`"not-an-array"`, `{}`, `null`, `42`} {
		if events := ParseAndValidate(envelopeWith(metrics), "t", "s", "http", t0, nil); events != nil {
			t.Errorf("metrics=%s: got %+v, want nil", metrics, events)
		}
	}
	if events := ParseAndValidate([]byte(`{"no":"telemetry"}`), "t", "s", "http", t0, nil); events != nil {
		t.Errorf("absent path: got %+v, want nil", events)
	}
	// One bad entry doesn't sink its valid siblings.
	rec := newDropRecorder()
	events := ParseAndValidate(envelopeWith(`[{"name":"ok.one","kind":"counter","value":1},"garbage",{"name":"ok.two","kind":"counter","value":2}]`), "t", "s", "http", t0, rec.fn())
	if len(events) != 2 {
		t.Errorf("valid siblings lost: %+v (drops %v)", events, rec.counts)
	}
}

func TestParseAndValidateOverLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < maxMetricsPerRequest+10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":"m%d","kind":"counter","value":1}`, i)
	}
	sb.WriteString("]")

	rec := newDropRecorder()
	events := ParseAndValidate(envelopeWith(sb.String()), "t", "s", "http", t0, rec.fn())
	if len(events) != maxMetricsPerRequest {
		t.Errorf("got %d events, want %d", len(events), maxMetricsPerRequest)
	}
	if rec.counts[dropOverLimit] != 10 {
		t.Errorf("over_limit = %d, want 10", rec.counts[dropOverLimit])
	}
}

func TestAttrPolicy(t *testing.T) {
	// Denylist is a case-insensitive substring match on keys.
	payload := envelopeWith(`[{"name":"a","kind":"counter","value":1,"attrs":{
		"email":"x@y.z","user_email":"x@y.z","Api_Token":"tok","password_hint":"p",
		"plan":"premium","nested":{"a":1},"list":[1],"nothing":null}}]`)
	events := ParseAndValidate(payload, "t", "s", "http", t0, nil)
	if len(events) != 1 {
		t.Fatalf("event dropped: attr policy must not sink the metric")
	}
	attrs := events[0].Attrs
	if len(attrs) != 1 || attrs["plan"] != "premium" {
		t.Errorf("attr policy wrong, got %+v want only plan", attrs)
	}

	// Caps: long values truncate (rune-safe), long/empty keys drop, count caps at max.
	long := strings.Repeat("é", 300) // 2 bytes per rune
	bigAttrs := map[string]any{"long": long, strings.Repeat("k", maxAttrKeyLen+1): "x", "": "x"}
	for i := 0; i < maxAttrsPerMetric+5; i++ {
		bigAttrs[fmt.Sprintf("pad%02d", i)] = i
	}
	raw, _ := json.Marshal([]map[string]any{{"name": "a", "kind": "counter", "value": 1, "attrs": bigAttrs}})
	events = ParseAndValidate(envelopeWith(string(raw)), "t", "s", "http", t0, nil)
	if len(events) != 1 {
		t.Fatalf("event dropped under attr caps")
	}
	attrs = events[0].Attrs
	if len(attrs) > maxAttrsPerMetric {
		t.Errorf("attr count %d exceeds cap %d", len(attrs), maxAttrsPerMetric)
	}
	if got, ok := attrs["long"].(string); ok {
		if len(got) > maxAttrValueLen || !strings.HasPrefix(long, got) || len(got) < maxAttrValueLen-3 {
			t.Errorf("value truncation wrong: len=%d", len(got))
		}
		for _, r := range got {
			if r == '�' {
				t.Errorf("truncation split a rune")
			}
		}
	}
	for k := range attrs {
		if k == "" || len(k) > maxAttrKeyLen {
			t.Errorf("bad key survived: %q", k)
		}
	}
}
