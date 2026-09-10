package lmtp

import "testing"

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

	t.Run("rspamd clean with auth", func(t *testing.T) {
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
