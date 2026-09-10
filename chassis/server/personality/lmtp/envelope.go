package lmtp

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// The RFC 5322 → JSON message parse (parseMessage and its helpers) moved to
// chassis/mail.ParseMessage so the remote-source watcher and this inlet
// produce byte-identical message JSON. What stays here is LMTP-specific:
// bounce detection and the Rspamd/Authentication-Results normalization that
// feeds `_txc.mail.*`.

// bounceDetected reports whether an inbound message is a bounce / auto-generated
// DSN, so a stack can halt before replying and avoid mail loops. Surfaced as
// `_txc.lmtp.is_bounce` (read it as `@lmtp.is_bounce`). Two signals:
//   - a null reverse-path (empty SMTP MAIL FROM) — the RFC 5321 marker every
//     compliant sender stamps on bounces and auto-responses; the primary, most
//     reliable check (and what the OOO guard's `@lmtp.mail.from != ""` keys on).
//   - a formal delivery-status report (Content-Type: multipart/report;
//     report-type=delivery-status) — catches a DSN sent with a non-null sender.
func bounceDetected(mailFrom, msgJSON string) bool {
	if strings.TrimSpace(mailFrom) == "" {
		return true
	}
	ct := strings.ToLower(gjson.Get(msgJSON, "headers.content-type").String())
	return strings.Contains(ct, "multipart/report") && strings.Contains(ct, "delivery-status")
}

// spamBands maps an Rspamd score to a verdict band.
type spamBands struct {
	suspiciousAt float64
	spamAt       float64
}

// parseSpamBands parses "suspicious=5,spam=10" into bands; missing/malformed
// keys fall back to the documented defaults (5 / 10).
func parseSpamBands(spec string) spamBands {
	b := spamBands{suspiciousAt: 5, spamAt: 10}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "suspicious":
			b.suspiciousAt = f
		case "spam":
			b.spamAt = f
		}
	}
	return b
}

func (b spamBands) verdict(score float64) string {
	switch {
	case score >= b.spamAt:
		return "spam"
	case score >= b.suspiciousAt:
		return "suspicious"
	default:
		return "clean"
	}
}

// mailMeta is the normalized inbound spam/auth picture parsed from an upstream
// Rspamd milter's annotation headers. Surfaced under `_txc.mail.*` (read as
// `@mail.*` in txcl) so tenant _mail stacks can make their own policy
// decisions; the chassis only supplies the facts.
type mailMeta struct {
	available bool    // any Rspamd header present (false ⇒ Rspamd down/skipped — mail still flowed)
	score     float64 // raw Rspamd score
	hasScore  bool    // a numeric score was found
	verdict   string  // clean | suspicious | spam | unknown
	spf       string  // pass | fail | softfail | neutral | none | temperror | permerror | ""
	dkim      string
	dmarc     string
}

var (
	// X-Spamd-Result: "default: False [2.50 / 15.00]; SYM1(0.0)[..], SYM2(..)..."
	spamdScoreRe = regexp.MustCompile(`\[\s*(-?\d+(?:\.\d+)?)\s*/`)
	// Authentication-Results: "... ; spf=pass ...; dkim=pass ...; dmarc=pass ..."
	arSPFRe   = regexp.MustCompile(`(?i)\bspf=([a-z]+)`)
	arDKIMRe  = regexp.MustCompile(`(?i)\bdkim=([a-z]+)`)
	arDMARCRe = regexp.MustCompile(`(?i)\bdmarc=([a-z]+)`)
)

// firstHeader returns the first value of a (lowercased) header from the parsed
// message JSON. headersJSON stores each header as a JSON array, so we index
// [0]; a scalar is handled defensively.
func firstHeader(msgJSON, key string) string {
	r := gjson.Get(msgJSON, "headers."+key)
	if r.IsArray() {
		if arr := r.Array(); len(arr) > 0 {
			return strings.TrimSpace(arr[0].String())
		}
		return ""
	}
	return strings.TrimSpace(r.String())
}

func authToken(ar string, re *regexp.Regexp) string {
	if m := re.FindStringSubmatch(ar); len(m) == 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

// parseMailHeaders normalizes Rspamd's annotation headers into mailMeta:
//   - score + symbols from X-Spamd-Result (falling back to X-Spam-Score /
//     X-Rspamd-Score for a bare score),
//   - spf/dkim/dmarc from Authentication-Results (RFC 8601 method=result),
//   - available = any of those headers present; when none are (Rspamd was
//     unavailable and the milter accepted the mail anyway) the verdict is
//     "unknown".
//
// bands maps the score to clean/suspicious/spam. Mirrors bounceDetected: reads
// the array-valued, lowercased header keys from msgJSON; never panics on
// missing/malformed input.
func parseMailHeaders(msgJSON string, bands spamBands) mailMeta {
	m := mailMeta{verdict: "unknown"}

	if res := firstHeader(msgJSON, "x-spamd-result"); res != "" {
		m.available = true
		if sm := spamdScoreRe.FindStringSubmatch(res); len(sm) == 2 {
			if f, err := strconv.ParseFloat(sm[1], 64); err == nil {
				m.score, m.hasScore = f, true
			}
		}
	}
	if !m.hasScore {
		for _, h := range []string{"x-spam-score", "x-rspamd-score"} {
			if v := firstHeader(msgJSON, h); v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					m.score, m.hasScore, m.available = f, true, true
					break
				}
			}
		}
	}

	if ar := firstHeader(msgJSON, "authentication-results"); ar != "" {
		m.available = true
		m.spf = authToken(ar, arSPFRe)
		m.dkim = authToken(ar, arDKIMRe)
		m.dmarc = authToken(ar, arDMARCRe)
	}

	if m.available && m.hasScore {
		m.verdict = bands.verdict(m.score)
	}
	return m
}
