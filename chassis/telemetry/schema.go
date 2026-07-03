package telemetry

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// metricsPath is where stacks emit metric intents (see package doc).
const metricsPath = "_txc.telemetry.metrics"

// Validation limits. Deliberately constants, not config: they are
// cardinality/abuse guardrails, and a per-tenant knob would just be a
// way to turn the guardrails off.
const (
	maxMetricsPerRequest = 64
	maxAttrsPerMetric    = 16
	maxAttrKeyLen        = 64
	maxAttrValueLen      = 256
	maxUnitLen           = 63
)

// nameRE matches OTel instrument names (which cap at 255 chars and
// must start with a letter), so any name we accept is exportable.
var nameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-/]{0,254}$`)

// attrDenylist drops author attributes whose KEY suggests sensitive
// content. Matched case-insensitively as substrings, so `user_email`
// and `Api_Token` are caught too. Prefer dropping over failing.
var attrDenylist = []string{
	"email", "token", "authorization", "cookie", "password", "secret", "api_key",
}

// Drop reasons (the DropFunc vocabulary produced here).
const (
	dropInvalidName     = "invalid_name"
	dropInvalidKind     = "invalid_kind"
	dropUnsupportedKind = "unsupported_kind"
	dropInvalidValue    = "invalid_value"
	dropOverLimit       = "over_limit"
)

// HasMetrics reports whether the payload carries any metric intents —
// the zero-cost fast path for the overwhelmingly common case of a
// request that emits none.
func HasMetrics(payload []byte) bool {
	return gjson.GetBytes(payload, metricsPath).Exists()
}

// rawMetricCount is the unvalidated intent count, for drop accounting
// on requests whose intents never reach validation. Non-array (or
// empty) still counts 1: something emitted here, and that fact must
// not round to zero in diagnostics.
func rawMetricCount(payload []byte) int64 {
	arr := gjson.GetBytes(payload, metricsPath)
	if n := int64(len(arr.Array())); n > 0 {
		return n
	}
	return 1
}

// ParseAndValidate extracts `_txc.telemetry.metrics` from the final
// request envelope and returns the events that survive validation,
// enriched with the trusted request context (tenant/stack/src/time).
// Invalid entries are dropped (counted via dropped), never fatal.
func ParseAndValidate(payload []byte, tenant, stack, src string, now time.Time, dropped DropFunc) []MetricEvent {
	arr := gjson.GetBytes(payload, metricsPath)
	if !arr.IsArray() {
		return nil
	}

	items := arr.Array()
	if len(items) > maxMetricsPerRequest {
		dropped.Drop(tenant, dropOverLimit, int64(len(items)-maxMetricsPerRequest))
		items = items[:maxMetricsPerRequest]
	}

	events := make([]MetricEvent, 0, len(items))
	for _, item := range items {
		name := item.Get("name").String()
		if !nameRE.MatchString(name) {
			dropped.Drop(tenant, dropInvalidName, 1)
			continue
		}

		kind := item.Get("kind").String()
		switch kind {
		case "counter", "histogram":
			// supported
		case "gauge":
			// Real kind, deliberately deferred (distributed gauge
			// semantics); distinct reason so authors can tell "typo"
			// from "not yet".
			dropped.Drop(tenant, dropUnsupportedKind, 1)
			continue
		default:
			dropped.Drop(tenant, dropInvalidKind, 1)
			continue
		}

		// Strict numeric typing: gjson's Float() coerces "5" and true;
		// a metric value must be an actual JSON number.
		v := item.Get("value")
		if v.Type != gjson.Number {
			dropped.Drop(tenant, dropInvalidValue, 1)
			continue
		}
		value := v.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) || (kind == "counter" && value < 0) {
			dropped.Drop(tenant, dropInvalidValue, 1)
			continue
		}

		unit := truncate(item.Get("unit").String(), maxUnitLen)

		events = append(events, MetricEvent{
			Tenant: tenant,
			Stack:  stack,
			Src:    src,
			Name:   name,
			Kind:   kind,
			Value:  value,
			Unit:   unit,
			Attrs:  sanitizeAttrs(item.Get("attrs")),
			Time:   now,
		})
	}
	return events
}

// sanitizeAttrs applies the attribute policy: scalar values only
// (string/number/bool — nulls, arrays, and objects are dropped, not
// flattened), key and value length caps, denylisted keys removed, and
// at most maxAttrsPerMetric attributes (JSON object order). Silent
// per-attribute drops are deliberate — attributes are decoration, and
// one oversized or sensitive attr shouldn't cost the whole metric.
func sanitizeAttrs(attrs gjson.Result) map[string]any {
	if !attrs.IsObject() {
		return nil
	}
	out := make(map[string]any)
	attrs.ForEach(func(key, value gjson.Result) bool {
		if len(out) >= maxAttrsPerMetric {
			return false
		}
		k := key.String()
		if k == "" || len(k) > maxAttrKeyLen || deniedAttrKey(k) {
			return true
		}
		switch value.Type {
		case gjson.String:
			out[k] = truncate(value.String(), maxAttrValueLen)
		case gjson.Number:
			out[k] = value.Float()
		case gjson.True, gjson.False:
			out[k] = value.Bool()
		default:
			// null / nested object / array — dropped by policy.
		}
		return true
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncate caps s at max bytes without splitting a multi-byte rune —
// OTLP strings ride protobuf, which requires valid UTF-8.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func deniedAttrKey(k string) bool {
	lk := strings.ToLower(k)
	for _, needle := range attrDenylist {
		if strings.Contains(lk, needle) {
			return true
		}
	}
	return false
}
