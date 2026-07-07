package mail

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jhillyerd/enmime/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// One DKIM keypair for the whole test package (RSA-2048 keygen is slow).
var testDKIMPriv, testDKIMPub = func() (string, string) {
	priv, pub, err := tenants.GenerateDKIM()
	if err != nil {
		panic(err)
	}
	return priv, pub
}()

const testDDL = `
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE tenant_hostnames (id TEXT PRIMARY KEY, hostname TEXT NOT NULL, tenant_id TEXT NOT NULL, stack TEXT NOT NULL, created_at TEXT NOT NULL, created_by TEXT, revoked_at TEXT, verified_at TEXT, dkim_selector TEXT NOT NULL DEFAULT '', dkim_private_pem TEXT NOT NULL DEFAULT '', dkim_public_b64 TEXT NOT NULL DEFAULT '');
CREATE TABLE mail_campaign_sends (tenant_id TEXT NOT NULL, campaign TEXT NOT NULL, recipient TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'claimed', message_id TEXT NOT NULL DEFAULT '', sent_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, campaign, recipient));
CREATE TABLE dns_zones (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, origin TEXT NOT NULL, mname TEXT, rname TEXT, refresh INTEGER, retry INTEGER, expire INTEGER, minimum INTEGER, default_ttl INTEGER, mode TEXT NOT NULL DEFAULT 'pattern', created_at TEXT NOT NULL, created_by TEXT, updated_at TEXT NOT NULL, revoked_at TEXT, verified_at TEXT, dkim_selector TEXT NOT NULL DEFAULT '', dkim_private_pem TEXT NOT NULL DEFAULT '', dkim_public_b64 TEXT NOT NULL DEFAULT '');`

type fakeSubmit struct {
	calls []struct {
		from  string
		rcpts []string
		msg   []byte
	}
	err error
}

func (f *fakeSubmit) fn(_ context.Context, from string, rcpts []string, msg []byte) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		from  string
		rcpts []string
		msg   []byte
	}{from, rcpts, msg})
	return nil
}

type fakeUsage struct{ events []usage.UsageEvent }

func (u *fakeUsage) WriteEvent(e usage.UsageEvent) { u.events = append(u.events, e) }
func (u *fakeUsage) Name() string                  { return "fake" }
func (u *fakeUsage) Close(context.Context) error   { return nil }

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one shared in-memory DB across the pool
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(testDDL); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO tenants VALUES('tnt_acme','acme','acme','t',NULL)`)
	// acme.com: verified. unv.acme.com: unverified. gone.acme.com: revoked.
	hn := `INSERT INTO tenant_hostnames(id,hostname,tenant_id,stack,created_at,created_by,revoked_at,verified_at) VALUES`
	mustExec(t, db, hn+`('h1','acme.com','tnt_acme','web','t','op',NULL,'2026-01-01T00:00:00Z')`)
	mustExec(t, db, hn+`('h2','unv.acme.com','tnt_acme','web','t','op',NULL,NULL)`)
	mustExec(t, db, hn+`('h3','gone.acme.com','tnt_acme','web','t','op','2026-02-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	// zoned.example: acme serves DNS for it (active, VERIFIED zone) but has NO
	// verified hostname row — the verified zone delegation alone makes it a valid
	// sender domain. Carries a DKIM key so the signing path is exercised.
	if _, err := db.Exec(`INSERT INTO dns_zones(id,tenant_id,origin,mname,rname,created_at,updated_at,verified_at,dkim_selector,dkim_private_pem,dkim_public_b64)
		VALUES('dz1','tnt_acme','zoned.example','ns1','h','t','t','t','txco',?,?)`, testDKIMPriv, testDKIMPub); err != nil {
		t.Fatalf("seed zone: %v", err)
	}
	// A chassis-minted structured host with its OWN per-host DKIM key — signs
	// d=<host> (reputation isolation), independent of any dns_zones key.
	if _, err := db.Exec(`INSERT INTO tenant_hostnames(id,hostname,tenant_id,stack,created_at,created_by,verified_at,dkim_selector,dkim_private_pem,dkim_public_b64)
		VALUES('h4','web-abc.struct.example','tnt_acme','web','t','system:structured-host','t','txco',?,?)`, testDKIMPriv, testDKIMPub); err != nil {
		t.Fatalf("seed structured host: %v", err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func newTestMailer(t *testing.T, db *sql.DB, sub *fakeSubmit, u *fakeUsage) *Mailer {
	t.Helper()
	return &Mailer{
		db:            db,
		usage:         u,
		maxRecipients: 50,
		now:           func() time.Time { return time.Unix(0, 0).UTC() },
		submit:        sub.fn,
		relayOK:       true,
	}
}

// env builds an `in` envelope with a `_sendmail` block from key→value.
func env(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	in := "{}"
	for k, v := range fields {
		var err error
		in, err = sjson.Set(in, "_sendmail."+k, v)
		if err != nil {
			t.Fatalf("sjson set %s: %v", k, err)
		}
	}
	return []byte(in)
}

func TestSendCommonCase(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	u := &fakeUsage{}
	m := newTestMailer(t, db, sub, u)

	in := env(t, map[string]any{
		"to":      "matt@example.com",
		"subject": "Welcome",
		"body":    "<p>Thanks for signing up.</p>",
		"from":    "noreply@acme.com",
	})
	p, err := m.Send(context.Background(), "acme", in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := gjson.Get(p.Raw, "_sendmail.result.sent").Int(); got != 1 {
		t.Fatalf("sent=%d want 1; raw=%s", got, p.Raw)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("submit calls=%d want 1", len(sub.calls))
	}
	c := sub.calls[0]
	if c.from != "noreply@acme.com" || len(c.rcpts) != 1 || c.rcpts[0] != "matt@example.com" {
		t.Fatalf("submit from/rcpts = %q/%v", c.from, c.rcpts)
	}
	if !strings.Contains(string(c.msg), "Welcome") || !strings.Contains(string(c.msg), "Thanks for signing up") {
		t.Fatalf("message missing subject/body:\n%s", c.msg)
	}
	if len(u.events) != 1 || u.events[0].Src != "sendmail" || !u.events[0].Billable || u.events[0].Tenant != "acme" {
		t.Fatalf("usage events = %+v", u.events)
	}
}

func TestSendCcBcc(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	in := env(t, map[string]any{
		"to": "matt@example.com", "cc": "audit@example.org", "bcc": "hidden@example.net",
		"subject": "Hi", "body": "b", "from": "noreply@acme.com",
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(sub.calls))
	}
	c := sub.calls[0]
	// Envelope recipients = To + Cc + Bcc.
	want := map[string]bool{"matt@example.com": true, "audit@example.org": true, "hidden@example.net": true}
	if len(c.rcpts) != 3 {
		t.Fatalf("rcpts=%v want 3 (to+cc+bcc)", c.rcpts)
	}
	for _, r := range c.rcpts {
		if !want[r] {
			t.Fatalf("unexpected rcpt %q in %v", r, c.rcpts)
		}
	}
	msg := string(c.msg)
	if !strings.Contains(msg, "audit@example.org") {
		t.Fatalf("Cc must be a visible header:\n%s", msg)
	}
	if strings.Contains(msg, "hidden@example.net") {
		t.Fatalf("Bcc must NOT appear in headers (envelope-only):\n%s", msg)
	}
}

func TestSendReplyToAndHeaders(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	in := env(t, map[string]any{
		"to": "matt@example.com", "subject": "Hi", "body": "b", "from": "noreply@acme.com",
		"reply_to": "support@acme.com",
		"headers": map[string]any{
			"X-Campaign":       "spring",
			"List-Unsubscribe": "<mailto:unsub@acme.com>",
			"In-Reply-To":      "<thread-root@example.com>", // threading: not protected, passes through
			"References":       "<thread-root@example.com>",
			"From":             "evil@bad.com", // protected → ignored
			"Subject":          "hijacked",     // protected → ignored
		},
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatal(err)
	}
	msg := string(sub.calls[0].msg)
	if !strings.Contains(msg, "Reply-To: support@acme.com") {
		t.Fatalf("Reply-To missing:\n%s", msg)
	}
	if !strings.Contains(msg, "X-Campaign: spring") || !strings.Contains(msg, "List-Unsubscribe: <mailto:unsub@acme.com>") {
		t.Fatalf("custom headers missing:\n%s", msg)
	}
	// Threading headers (In-Reply-To / References, RFC 5322 §3.6.4) are not
	// protected and must survive to the wire so a client can group a conversation.
	if !strings.Contains(msg, "In-Reply-To: <thread-root@example.com>") ||
		!strings.Contains(msg, "References: <thread-root@example.com>") {
		t.Fatalf("In-Reply-To/References (threading) missing:\n%s", msg)
	}
	// Protected headers can't be overridden; the real From/Subject survive.
	if strings.Contains(msg, "evil@bad.com") || strings.Contains(msg, "hijacked") {
		t.Fatalf("a protected header was overridden:\n%s", msg)
	}
	if !strings.Contains(msg, "noreply@acme.com") || !strings.Contains(msg, "Subject: Hi") {
		t.Fatalf("real From/Subject not intact:\n%s", msg)
	}
}

func TestSendEnvelopeFrom(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	base := map[string]any{"to": "x@example.com", "subject": "Hi", "body": "b", "from": "noreply@acme.com"}

	// Default → envelope MAIL FROM = the header From.
	if _, err := m.Send(context.Background(), "acme", env(t, base)); err != nil {
		t.Fatal(err)
	}
	if sub.calls[0].from != "noreply@acme.com" {
		t.Fatalf("default envelope should be From; got %q", sub.calls[0].from)
	}

	// envelope_from "<>" → null reverse-path (RFC 3834 auto-reply posture).
	withNull := map[string]any{"to": "x@example.com", "subject": "Hi", "body": "b",
		"from": "noreply@acme.com", "envelope_from": "<>"}
	if _, err := m.Send(context.Background(), "acme", env(t, withNull)); err != nil {
		t.Fatal(err)
	}
	if sub.calls[1].from != "" {
		t.Fatalf("envelope_from=<> should null the reverse-path; got %q", sub.calls[1].from)
	}
}

func TestSendRateLimited(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	m.rl = newRateLimiter(parseRateRules("1/1h")) // cap = 1 send/hour/tenant
	fixed := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fixed }

	// First send → allowed.
	p1, err := m.Send(context.Background(), "acme",
		env(t, map[string]any{"to": "a@example.com", "subject": "Hi", "body": "b", "from": "noreply@acme.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(p1.Raw, "_sendmail.result.sent").Int() != 1 {
		t.Fatalf("first send should send=1: %s", p1.Raw)
	}

	// Second send (different recipient → not campaign dedup) → throttled.
	p2, _ := m.Send(context.Background(), "acme",
		env(t, map[string]any{"to": "b@example.com", "subject": "Hi", "body": "b", "from": "noreply@acme.com"}))
	if gjson.Get(p2.Raw, "_sendmail.result.skipped").Int() != 1 ||
		gjson.Get(p2.Raw, "_sendmail.result.recipients.0.reason").String() != "rate_limited" {
		t.Fatalf("second send should be rate_limited: %s", p2.Raw)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("throttled send must not submit; calls=%d", len(sub.calls))
	}
}

func TestSendExplicitTextOverride(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Hi", "from": "noreply@acme.com",
		"body": "<p>HTML body &amp; stuff</p>",
		"text": "Plain {{.name}} & co <not escaped>",
		"vars": map[string]any{"name": "Bob"},
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatal(err)
	}
	msg := string(sub.calls[0].msg)
	// Explicit text is the plaintext part: var-rendered ({{.name}}→Bob) and
	// NOT HTML-escaped (text/template, so `&`/`<`/`>` stay literal).
	if !strings.Contains(msg, "Plain Bob & co <not escaped>") {
		t.Fatalf("explicit _sendmail.text override (var-rendered, unescaped) missing:\n%s", msg)
	}
}

func TestSendRequiresFields(t *testing.T) {
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})
	for _, miss := range []string{"to", "subject", "body", "from"} {
		fields := map[string]any{"to": "a@example.com", "subject": "s", "body": "b", "from": "noreply@acme.com"}
		delete(fields, miss)
		if _, err := m.Send(context.Background(), "acme", env(t, fields)); err == nil {
			t.Errorf("missing %q: want error", miss)
		}
	}
}

func TestSendFromNotVerified(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	for _, from := range []string{"x@unv.acme.com", "x@gone.acme.com", "x@evil.com"} {
		p, err := m.Send(context.Background(), "acme", env(t, map[string]any{
			"to": "a@example.com", "subject": "s", "body": "b", "from": from,
		}))
		if err == nil {
			t.Errorf("from %q: want error", from)
		}
		if r := gjson.Get(p.Raw, "_sendmail.result.reason").String(); r != "from_not_verified" {
			t.Errorf("from %q reason=%q want from_not_verified", from, r)
		}
	}
	if len(sub.calls) != 0 {
		t.Fatalf("unverified from must not submit; calls=%d", len(sub.calls))
	}
}

func TestSendCampaignDedup(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	u := &fakeUsage{}
	m := newTestMailer(t, db, sub, u)
	fields := map[string]any{
		"to": "matt@example.com", "subject": "Hi", "body": "b",
		"from": "noreply@acme.com", "campaign": "welcome",
	}
	// First send: claimed + sent.
	p1, _ := m.Send(context.Background(), "acme", env(t, fields))
	if gjson.Get(p1.Raw, "_sendmail.result.sent").Int() != 1 {
		t.Fatalf("first send sent != 1: %s", p1.Raw)
	}
	// Second send same (tenant,campaign,recipient): skipped, no second submit.
	p2, _ := m.Send(context.Background(), "acme", env(t, fields))
	if gjson.Get(p2.Raw, "_sendmail.result.skipped").Int() != 1 {
		t.Fatalf("second send skipped != 1: %s", p2.Raw)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("campaign must send once; calls=%d", len(sub.calls))
	}
	if len(u.events) != 1 {
		t.Fatalf("skip must not bill; usage events=%d", len(u.events))
	}
}

func TestSendCampaignReleaseOnFailure(t *testing.T) {
	db := newTestDB(t)
	failing := &fakeSubmit{err: context.DeadlineExceeded}
	m := newTestMailer(t, db, failing, &fakeUsage{})
	fields := map[string]any{
		"to": "matt@example.com", "subject": "Hi", "body": "b",
		"from": "noreply@acme.com", "campaign": "welcome",
	}
	p1, _ := m.Send(context.Background(), "acme", env(t, fields))
	if gjson.Get(p1.Raw, "_sendmail.result.failed").Int() != 1 {
		t.Fatalf("failed send should report failed=1: %s", p1.Raw)
	}
	// The claim must have been released → a retry with a working relay sends.
	working := &fakeSubmit{}
	m.submit = working.fn
	p2, _ := m.Send(context.Background(), "acme", env(t, fields))
	if gjson.Get(p2.Raw, "_sendmail.result.sent").Int() != 1 {
		t.Fatalf("retry after release should send=1 (claim was not released?): %s", p2.Raw)
	}
}

func TestSendArrayFanout(t *testing.T) {
	db := newTestDB(t)
	// Pre-seed a@example.com as already-sent for campaign "welcome".
	mustExec(t, db, `INSERT INTO mail_campaign_sends VALUES('acme','welcome','a@example.com','sent','<x>','t')`)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	in := "{}"
	in, _ = sjson.Set(in, "_sendmail.subject", "Hi {{.name}}")
	in, _ = sjson.Set(in, "_sendmail.body", "<p>Hello {{.name}}</p>")
	in, _ = sjson.Set(in, "_sendmail.from", "noreply@acme.com")
	in, _ = sjson.Set(in, "_sendmail.campaign", "welcome")
	in, _ = sjson.Set(in, "_sendmail.vars", map[string]any{"name": "Friend"})
	in, _ = sjson.SetRaw(in, "_sendmail.to", `["a@example.com",{"email":"b@example.com","vars":{"name":"Bob"}}]`)

	p, err := m.Send(context.Background(), "acme", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(p.Raw, "_sendmail.result.sent").Int() != 1 || gjson.Get(p.Raw, "_sendmail.result.skipped").Int() != 1 {
		t.Fatalf("want sent=1 skipped=1; raw=%s", p.Raw)
	}
	if len(sub.calls) != 1 || len(sub.calls[0].rcpts) == 0 || sub.calls[0].rcpts[0] != "b@example.com" {
		t.Fatalf("only b should send; calls=%+v", sub.calls)
	}
	// Per-recipient vars override: Bob, not Friend.
	if !strings.Contains(string(sub.calls[0].msg), "Bob") {
		t.Fatalf("per-recipient var not applied:\n%s", sub.calls[0].msg)
	}
}

func TestSendTooManyRecipients(t *testing.T) {
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})
	m.maxRecipients = 2
	in := "{}"
	in, _ = sjson.Set(in, "_sendmail.subject", "s")
	in, _ = sjson.Set(in, "_sendmail.body", "b")
	in, _ = sjson.Set(in, "_sendmail.from", "noreply@acme.com")
	in, _ = sjson.SetRaw(in, "_sendmail.to", `["a@x.com","b@x.com","c@x.com"]`)
	p, err := m.Send(context.Background(), "acme", []byte(in))
	if err == nil || gjson.Get(p.Raw, "_sendmail.result.reason").String() != "too_many_recipients" {
		t.Fatalf("want too_many_recipients error; err=%v raw=%s", err, p.Raw)
	}
}

func TestSendNoRelay(t *testing.T) {
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})
	m.relayOK = false
	if _, err := m.Send(context.Background(), "acme", env(t, map[string]any{
		"to": "a@x.com", "subject": "s", "body": "b", "from": "noreply@acme.com",
	})); err == nil {
		t.Fatal("no relay configured: want error")
	}
}

func TestSendDKIMSigns(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	// from a DNS-served zone (zoned.example) that carries a DKIM key.
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Hi", "body": "<p>hi</p>", "from": "noreply@zoned.example",
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(sub.calls))
	}
	msg := string(sub.calls[0].msg)
	if !strings.Contains(msg, "DKIM-Signature:") {
		t.Fatalf("message not DKIM-signed:\n%s", msg)
	}
	if !strings.Contains(msg, "d=zoned.example") || !strings.Contains(msg, "s=txco") {
		t.Fatalf("DKIM-Signature d=/s= wrong:\n%s", msg)
	}
}

func TestSendDKIMSignsPerHostStructuredHost(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	// from a structured host with its own per-host key → d=<host>, not a zone.
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Hi", "body": "<p>hi</p>", "from": "ooo@web-abc.struct.example",
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatal(err)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(sub.calls))
	}
	msg := string(sub.calls[0].msg)
	if !strings.Contains(msg, "DKIM-Signature:") || !strings.Contains(msg, "d=web-abc.struct.example") {
		t.Fatalf("per-host signing wrong (want d=<host>):\n%s", msg)
	}
}

func TestSendUnsignedWhenNoZoneKey(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})
	// acme.com is a verified hostname but NOT a DNS zone → no key → unsigned,
	// but the send still succeeds (SPF covers it).
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Hi", "body": "b", "from": "noreply@acme.com",
	})
	p, err := m.Send(context.Background(), "acme", in)
	if err != nil || gjson.Get(p.Raw, "_sendmail.result.sent").Int() != 1 {
		t.Fatalf("send: %v %s", err, p.Raw)
	}
	if strings.Contains(string(sub.calls[0].msg), "DKIM-Signature:") {
		t.Fatal("should be unsigned (no zone key for acme.com)")
	}
}

func TestRenderAndCompose(t *testing.T) {
	body, err := renderBody("<p>Hi {{.name}}</p>", map[string]any{"name": "Bob"})
	if err != nil || !strings.Contains(string(body), "Hi Bob") {
		t.Fatalf("renderBody: %q %v", body, err)
	}
	full, err := renderDefault("Subj", body, "", nil)
	if err != nil || !strings.Contains(full, "Hi Bob") || !strings.Contains(full, "Subj") {
		t.Fatalf("renderDefault: %v", err)
	}
	if txt := htmlToText(full); strings.Contains(txt, "<") {
		t.Fatalf("htmlToText left tags: %q", txt)
	}
}

func TestRenderShellCustom(t *testing.T) {
	body, err := renderBody("<p>Hi {{.name}}</p>", map[string]any{"name": "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := parseShell(`<!doctype html><title>{{.Subject}}</title><main>{{.Body}}</main>` +
		`<a href="{{.Vars.nexturl}}">next</a><i>DRIPCHROME</i>`)
	if err != nil {
		t.Fatalf("parseShell: %v", err)
	}
	out, err := renderShell(tmpl, "Subj", body, "",
		map[string]any{"nexturl": "https://www.example.com/next?t=abc-123"})
	if err != nil {
		t.Fatalf("renderShell: %v", err)
	}
	// The custom shell wraps the (var-rendered) body + subject, interpolates a
	// per-send var ({{.Vars.nexturl}}) into the button href, and is NOT the default.
	for _, want := range []string{"Subj", "Hi Bob", "DRIPCHROME", `href="https://www.example.com/next?t=abc-123"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("custom shell output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "thanks.computer") {
		t.Fatalf("custom shell must not include the default footer:\n%s", out)
	}
	// parseShell rejects a malformed template.
	if _, err := parseShell(`{{.Body`); err == nil {
		t.Fatal("malformed shell must fail to parse")
	}
}

// customShellTmpl is a minimal valid shell: a distinctive body wrapper + a
// chrome word (DRIPCHROME) that lives ONLY in the shell, never the body.
const customShellTmpl = `<!doctype html><html><head><title>{{.Subject}}</title></head>` +
	`<body><div id="drip-shell">{{.Body}}</div><footer>DRIPCHROME</footer></body></html>`

func TestSendCustomShell(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Welcome", "from": "noreply@acme.com",
		"body":           "<p>BODYWORD here.</p>",
		"templates.html": customShellTmpl,
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("submit calls=%d want 1", len(sub.calls))
	}
	msg, err := enmime.ReadEnvelope(bytes.NewReader(sub.calls[0].msg))
	if err != nil {
		t.Fatalf("parse mime: %v", err)
	}
	// HTML part: the custom shell wraps the body; the default footer is absent.
	for _, want := range []string{"drip-shell", "BODYWORD", "DRIPCHROME"} {
		if !strings.Contains(msg.HTML, want) {
			t.Fatalf("html part missing %q:\n%s", want, msg.HTML)
		}
	}
	if strings.Contains(msg.HTML, "thanks.computer") {
		t.Fatalf("custom shell must replace the default footer:\n%s", msg.HTML)
	}
	// Plaintext derives from the BODY, not the shell-wrapped doc: the body word is
	// present, the shell chrome is not.
	if !strings.Contains(msg.Text, "BODYWORD") {
		t.Fatalf("plaintext should carry the body:\n%s", msg.Text)
	}
	if strings.Contains(msg.Text, "DRIPCHROME") {
		t.Fatalf("shell chrome leaked into the plaintext part:\n%s", msg.Text)
	}
}

func TestSendDefaultShellFallback(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	// No templates.html → the bundled default shell (its footer marks it).
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Welcome", "from": "noreply@acme.com",
		"body": "<p>BODYWORD here.</p>",
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msg, err := enmime.ReadEnvelope(bytes.NewReader(sub.calls[0].msg))
	if err != nil {
		t.Fatalf("parse mime: %v", err)
	}
	if !strings.Contains(msg.HTML, "thanks.computer") {
		t.Fatalf("absent templates.html must fall back to the default shell:\n%s", msg.HTML)
	}
	if strings.Contains(msg.HTML, "drip-shell") {
		t.Fatalf("default shell must not contain custom markers:\n%s", msg.HTML)
	}
}

func TestSendInvalidShell(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Welcome", "from": "noreply@acme.com",
		"body":           "<p>hi</p>",
		"templates.html": "<html>{{.Body", // unterminated action
	})
	p, err := m.Send(context.Background(), "acme", in)
	if err == nil {
		t.Fatal("malformed templates.html must error")
	}
	if r := gjson.Get(p.Raw, "_sendmail.result.reason").String(); r != "invalid_template" {
		t.Fatalf("reason=%q want invalid_template; raw=%s", r, p.Raw)
	}
	if len(sub.calls) != 0 {
		t.Fatalf("a broken shell must not submit; calls=%d", len(sub.calls))
	}
}

func TestSendQuotedPrintableLongLines(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	// One unbreakable >1,000-char pure-ASCII token: a giant style attribute.
	// enmime's default picks 7bit for ASCII regardless of line length, the MTA
	// then folds at ~998 chars, and a fold landing inside an attribute
	// corrupts it (and breaks the DKIM body hash). composeMIME forces
	// quoted-printable on the body parts so no line ever reaches the fold
	// threshold and the content decodes byte-exact.
	style := strings.Repeat("margin-left:0;", 100) + "height:auto"
	body := `<p>before</p><img src="https://example.com/x.png" style="` + style + `" alt="pic" /><p>after</p>`
	in := env(t, map[string]any{
		"to": "x@example.com", "subject": "Long", "from": "noreply@acme.com",
		"body": body,
	})
	if _, err := m.Send(context.Background(), "acme", in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("submit calls=%d want 1", len(sub.calls))
	}
	raw := string(sub.calls[0].msg)
	if n := strings.Count(raw, "Content-Transfer-Encoding: quoted-printable"); n != 2 {
		t.Fatalf("quoted-printable parts=%d want 2 (text + html):\n%s", n, raw)
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("%d-byte line would be folded by the MTA:\n%.120s...", len(line), line)
		}
	}
	msg, err := enmime.ReadEnvelope(bytes.NewReader(sub.calls[0].msg))
	if err != nil {
		t.Fatalf("parse mime: %v", err)
	}
	if !strings.Contains(msg.HTML, `style="`+style+`"`) {
		t.Fatalf("giant attribute did not survive the round-trip:\n%.300s...", msg.HTML)
	}
}

func TestHTMLToTextFidelity(t *testing.T) {
	out := htmlToText(`<p>A &amp; B &#8212; <a href="https://x.test">link</a></p><ul><li>one</li><li>two</li></ul>`)
	if strings.Contains(out, "<") {
		t.Fatalf("tags left: %q", out)
	}
	// Entities decoded, link URL preserved, list items present.
	for _, want := range []string{"A & B", "—", "https://x.test", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in derived text:\n%s", want, out)
		}
	}
}

func TestHTMLToTextEmpty(t *testing.T) {
	if got := htmlToText(""); got != "" {
		t.Fatalf("empty html should yield empty text, got %q", got)
	}
}

func TestDomainOfAndVerify(t *testing.T) {
	if domainOf("a@B.com") != "b.com" || domainOf("nope") != "" {
		t.Fatal("domainOf")
	}
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})
	cases := []struct {
		slug, domain string
		want         bool
	}{
		{"acme", "acme.com", true},
		{"acme", "unv.acme.com", false},  // unverified
		{"acme", "gone.acme.com", false}, // revoked
		{"acme", "evil.com", false},
		{"other", "acme.com", false},        // wrong tenant
		{"acme", "zoned.example", true},     // DNS-zone apex, no verified hostname row
		{"acme", "sub.zoned.example", true}, // subdomain of the served zone
		{"other", "zoned.example", false},   // zone belongs to acme, not "other"
	}
	for _, c := range cases {
		ok, err := m.fromDomainVerified(context.Background(), c.slug, c.domain)
		if err != nil || ok != c.want {
			t.Errorf("verify(%s,%s)=%v,%v want %v", c.slug, c.domain, ok, err, c.want)
		}
	}
}
