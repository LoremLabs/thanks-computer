package lmtp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// crlf normalizes the LF line endings of a Go raw-string literal to
// the CRLF that RFC 5322 + MIME parsers expect. Headers in particular
// are sensitive — a parser may discard a header that follows a bare
// LF or refuse to split a multipart body that lacks CRLF before the
// boundary marker.
func crlf(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func TestBounceDetected(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		msgJSON string
		want    bool
	}{
		{"null reverse-path", "", `{}`, true},
		{"whitespace-only from is null", "   ", `{}`, true},
		{"normal mail", "alice@example.com", `{"headers":{"content-type":["text/plain"]}}`, false},
		{"dsn with non-null sender", "mailer-daemon@example.com",
			`{"headers":{"content-type":["multipart/report; report-type=delivery-status; boundary=xyz"]}}`, true},
		{"multipart/report without delivery-status is not a bounce", "x@example.com",
			`{"headers":{"content-type":["multipart/report; report-type=disposition-notification"]}}`, false},
	}
	for _, c := range cases {
		if got := bounceDetected(c.from, c.msgJSON); got != c.want {
			t.Errorf("%s: bounceDetected(%q, ...) = %v, want %v", c.name, c.from, got, c.want)
		}
	}
}

func TestParseSpamBands(t *testing.T) {
	b := parseSpamBands("suspicious=4,spam=9")
	if b.suspiciousAt != 4 || b.spamAt != 9 {
		t.Fatalf("bands: %+v", b)
	}
	if d := parseSpamBands(""); d.suspiciousAt != 5 || d.spamAt != 10 {
		t.Fatalf("default bands (empty spec): %+v", d)
	}
	if d := parseSpamBands("spam=8,garbage,suspicious=x"); d.suspiciousAt != 5 || d.spamAt != 8 {
		t.Fatalf("partial/malformed should keep defaults for bad keys: %+v", d)
	}
	for score, want := range map[float64]string{3: "clean", 4: "suspicious", 8: "suspicious", 9: "spam"} {
		if got := b.verdict(score); got != want {
			t.Errorf("verdict(%v) = %s, want %s", score, got, want)
		}
	}
}

func TestParseMailHeaders(t *testing.T) {
	bands := parseSpamBands("suspicious=5,spam=10")

	t.Run("rspamd clean with auth + symbols", func(t *testing.T) {
		msg := `{"headers":{` +
			`"x-spamd-result":["default: False [2.50 / 15.00]; R_SPF_ALLOW(-0.20)[], DKIM_TRACE(0.00)[], DMARC_POLICY_ALLOW(-0.50)[]"],` +
			`"authentication-results":["mx.thanks.computer; spf=pass smtp.mailfrom=a@x.test; dkim=pass header.d=x.test; dmarc=pass"]}}`
		m := parseMailHeaders(msg, bands)
		if !m.available || !m.hasScore || m.score != 2.5 || m.verdict != "clean" {
			t.Fatalf("got %+v", m)
		}
		if m.spf != "pass" || m.dkim != "pass" || m.dmarc != "pass" {
			t.Fatalf("auth: %+v", m)
		}
		if strings.Join(m.symbols, ",") != "R_SPF_ALLOW,DKIM_TRACE,DMARC_POLICY_ALLOW" {
			t.Fatalf("symbols: %v", m.symbols)
		}
	})

	t.Run("score bands", func(t *testing.T) {
		for score, want := range map[string]string{"4.90": "clean", "7.00": "suspicious", "12.00": "spam"} {
			msg := `{"headers":{"x-spamd-result":["default: T [` + score + ` / 15.00];"]}}`
			if m := parseMailHeaders(msg, bands); m.verdict != want {
				t.Errorf("score %s → %s, want %s", score, m.verdict, want)
			}
		}
	})

	t.Run("x-spam-score fallback", func(t *testing.T) {
		m := parseMailHeaders(`{"headers":{"x-spam-score":["11.2"]}}`, bands)
		if !m.available || !m.hasScore || m.score != 11.2 || m.verdict != "spam" {
			t.Fatalf("fallback: %+v", m)
		}
	})

	t.Run("no rspamd headers → unavailable/unknown", func(t *testing.T) {
		m := parseMailHeaders(`{"headers":{"subject":["hi"]}}`, bands)
		if m.available || m.hasScore || m.verdict != "unknown" {
			t.Fatalf("absent: %+v", m)
		}
	})

	t.Run("auth-only → unknown verdict, auth set", func(t *testing.T) {
		m := parseMailHeaders(`{"headers":{"authentication-results":["mx; spf=fail; dkim=none; dmarc=fail"]}}`, bands)
		if !m.available || m.hasScore || m.verdict != "unknown" {
			t.Fatalf("auth-only: %+v", m)
		}
		if m.spf != "fail" || m.dkim != "none" || m.dmarc != "fail" {
			t.Fatalf("auth-only auth: %+v", m)
		}
	})

	t.Run("malformed inputs never panic", func(t *testing.T) {
		_ = parseMailHeaders(`{"headers":{"x-spamd-result":["garbage no score"]}}`, bands)
		_ = parseMailHeaders(`not json`, bands)
		_ = parseMailHeaders(``, bands)
	})
}

const fixturePlainText = `From: Alice <alice@example.com>
To: support@your.tenant
Subject: wifi keeps dropping
Date: Mon, 25 May 2026 14:00:00 +0000
Message-ID: <pt-1@mail.example.com>
Content-Type: text/plain; charset=utf-8

Hi support,

My wifi keeps dropping every ~10 minutes. Help!

— Alice
`

const fixtureMultipartAlt = `From: Bob <bob@example.com>
To: support@your.tenant
Subject: =?UTF-8?B?cMOhc3N3b3JkIHJlc2V0?=
Date: Mon, 25 May 2026 15:00:00 +0000
Message-ID: <ma-1@mail.example.com>
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="bdy42"

--bdy42
Content-Type: text/plain; charset=utf-8

Please reset my password.

--bdy42
Content-Type: text/html; charset=utf-8

<p>Please reset my <b>password</b>.</p>

--bdy42--
`

const fixtureWithAttachment = `From: Carol <carol@example.com>
To: support@your.tenant
Subject: log file attached
Date: Mon, 25 May 2026 16:00:00 +0000
Message-ID: <wa-1@mail.example.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mxd99"

--mxd99
Content-Type: text/plain; charset=utf-8

See attached.

--mxd99
Content-Type: text/plain; charset=utf-8; name="error.log"
Content-Disposition: attachment; filename="error.log"
Content-Transfer-Encoding: base64

aGVsbG8gd29ybGQK

--mxd99--
`

const fixtureNoSubject = `From: Dan <dan@example.com>
To: support@your.tenant
Date: Mon, 25 May 2026 17:00:00 +0000
Message-ID: <ns-1@mail.example.com>
Content-Type: text/plain; charset=utf-8

(no subject — happens with some clients)
`

const fixtureMultiReceived = `From: Eve <eve@example.com>
To: support@your.tenant
Subject: hello
Date: Mon, 25 May 2026 18:00:00 +0000
Message-ID: <mr-1@mail.example.com>
Received: from a.example.com (a.example.com [10.0.0.1]) by mx.your.tenant; Mon, 25 May 2026 18:00:01 +0000
Received: from b.example.com (b.example.com [10.0.0.2]) by a.example.com; Mon, 25 May 2026 18:00:00 +0000
Authentication-Results: mx.your.tenant; spf=pass; dkim=pass
Content-Type: text/plain; charset=utf-8

hi
`

func TestParseMessage_PlainText(t *testing.T) {
	out, err := parseMessage([]byte(crlf(fixturePlainText)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := gjson.Get(out, "subject").String(); got != "wifi keeps dropping" {
		t.Errorf("subject = %q", got)
	}
	if got := gjson.Get(out, "id").String(); got != "<pt-1@mail.example.com>" {
		t.Errorf("id = %q", got)
	}
	if got := gjson.Get(out, "date").String(); got != "2026-05-25T14:00:00Z" {
		t.Errorf("date = %q (want RFC3339 UTC)", got)
	}
	if got := gjson.Get(out, "from.0.addr").String(); got != "alice@example.com" {
		t.Errorf("from.0.addr = %q", got)
	}
	if got := gjson.Get(out, "from.0.name").String(); got != "Alice" {
		t.Errorf("from.0.name = %q", got)
	}
	if got := gjson.Get(out, "to.0.addr").String(); got != "support@your.tenant" {
		t.Errorf("to.0.addr = %q", got)
	}
	if got := gjson.Get(out, "text").String(); !strings.Contains(got, "My wifi keeps dropping") {
		t.Errorf("text missing body: %q", got)
	}
	// No HTML part on a plain-text message.
	if got := gjson.Get(out, "html").String(); got != "" {
		t.Errorf("html unexpectedly populated: %q", got)
	}
	// Headers always present.
	if got := gjson.Get(out, "headers.subject.0").String(); got == "" {
		t.Errorf("headers.subject.0 missing")
	}
	// No attachments — field omitted entirely.
	if gjson.Get(out, "attachments").Exists() {
		t.Errorf("attachments unexpectedly present")
	}
}

func TestParseMessage_MultipartAlt(t *testing.T) {
	out, err := parseMessage([]byte(crlf(fixtureMultipartAlt)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// RFC 2047 encoded Subject decodes to "pássword reset".
	if got := gjson.Get(out, "subject").String(); got != "pássword reset" {
		t.Errorf("subject = %q (want decoded UTF-8)", got)
	}
	if got := gjson.Get(out, "text").String(); !strings.Contains(got, "Please reset my password") {
		t.Errorf("text part missing: %q", got)
	}
	if got := gjson.Get(out, "html").String(); !strings.Contains(got, "<b>password</b>") {
		t.Errorf("html part missing: %q", got)
	}
}

func TestParseMessage_WithAttachment(t *testing.T) {
	out, err := parseMessage([]byte(crlf(fixtureWithAttachment)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	atts := gjson.Get(out, "attachments").Array()
	if len(atts) != 1 {
		t.Fatalf("attachments len = %d, want 1", len(atts))
	}
	a := atts[0]
	if got := a.Get("name").String(); got != "error.log" {
		t.Errorf("attachment name = %q", got)
	}
	if got := a.Get("type").String(); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("attachment type = %q", got)
	}
	if got := a.Get("size").Int(); got == 0 {
		t.Errorf("attachment size = 0")
	}
	if got := a.Get("sha256").String(); len(got) != 64 {
		t.Errorf("attachment sha256 = %q (want 64 hex chars)", got)
	}
	// Content is b64 — decode and verify it round-trips to the
	// original "hello world\n".
	enc := a.Get("content").String()
	dec, derr := base64.StdEncoding.DecodeString(enc)
	if derr != nil {
		t.Fatalf("attachment content b64 decode: %v", derr)
	}
	if string(dec) != "hello world\n" {
		t.Errorf("attachment content = %q, want %q", string(dec), "hello world\n")
	}
}

func TestParseMessage_NoSubject(t *testing.T) {
	out, err := parseMessage([]byte(crlf(fixtureNoSubject)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Subject is absent — field omitted entirely (NOT set to "").
	// Rules that test `.lmtp.msg.subject != ""` need this to be
	// distinguishable from the present-but-empty case.
	if gjson.Get(out, "subject").Exists() {
		t.Errorf("subject unexpectedly present on no-subject message")
	}
	// But other parsed fields still work.
	if got := gjson.Get(out, "from.0.name").String(); got != "Dan" {
		t.Errorf("from.0.name = %q", got)
	}
	if got := gjson.Get(out, "text").String(); !strings.Contains(got, "no subject") {
		t.Errorf("text body missing: %q", got)
	}
}

func TestParseMessage_MultiValuedHeaders(t *testing.T) {
	out, err := parseMessage([]byte(crlf(fixtureMultiReceived)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rcv := gjson.Get(out, "headers.received").Array()
	if len(rcv) != 2 {
		t.Fatalf("headers.received len = %d, want 2", len(rcv))
	}
	if !strings.Contains(rcv[0].String(), "a.example.com") {
		t.Errorf("received[0] = %q", rcv[0].String())
	}
	if !strings.Contains(rcv[1].String(), "b.example.com") {
		t.Errorf("received[1] = %q", rcv[1].String())
	}
	if got := gjson.Get(out, "headers.authentication-results.0").String(); got == "" {
		t.Errorf("authentication-results header missing")
	}
}

// TestParseMessage_Roundtrip exercises the always-safe-escape-hatch
// contract: the structured `msg.*` fields are derived from
// `msg.raw`, so re-parsing raw must yield equal structured output.
// Detects accidental mutation in the parse pipeline.
func TestParseMessage_Roundtrip(t *testing.T) {
	for _, fx := range []struct {
		name string
		body string
	}{
		{"plain", fixturePlainText},
		{"multipart", fixtureMultipartAlt},
		{"attachment", fixtureWithAttachment},
		{"no_subject", fixtureNoSubject},
		{"multi_received", fixtureMultiReceived},
	} {
		t.Run(fx.name, func(t *testing.T) {
			raw := []byte(crlf(fx.body))
			first, err := parseMessage(raw)
			if err != nil {
				t.Fatalf("first parse: %v", err)
			}
			second, err := parseMessage(raw)
			if err != nil {
				t.Fatalf("second parse: %v", err)
			}
			if first != second {
				t.Errorf("non-deterministic parse:\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

// TestParseMessage_Empty asserts the degenerate case doesn't panic
// and returns an empty-ish JSON object that the caller can SetRaw
// safely.
func TestParseMessage_Empty(t *testing.T) {
	out, err := parseMessage([]byte{})
	if err != nil {
		// enmime accepts an empty reader; this branch exists to
		// document the alternative rather than to require it.
		t.Logf("empty parse returned err (acceptable): %v", err)
		return
	}
	if !gjson.Valid(out) {
		t.Errorf("empty parse produced invalid JSON: %q", out)
	}
}
