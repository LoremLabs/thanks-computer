package mail

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/usage"
)

const testDDL = `
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE tenant_hostnames (id TEXT PRIMARY KEY, hostname TEXT NOT NULL, tenant_id TEXT NOT NULL, stack TEXT NOT NULL, created_at TEXT NOT NULL, created_by TEXT, revoked_at TEXT, verified_at TEXT);
CREATE TABLE mail_campaign_sends (tenant_id TEXT NOT NULL, campaign TEXT NOT NULL, recipient TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'claimed', message_id TEXT NOT NULL DEFAULT '', sent_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, campaign, recipient));
CREATE TABLE dns_zones (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, origin TEXT NOT NULL, mname TEXT, rname TEXT, refresh INTEGER, retry INTEGER, expire INTEGER, minimum INTEGER, default_ttl INTEGER, mode TEXT NOT NULL DEFAULT 'pattern', created_at TEXT NOT NULL, created_by TEXT, updated_at TEXT NOT NULL, revoked_at TEXT);`

type fakeSubmit struct {
	calls []struct {
		from, to string
		msg      []byte
	}
	err error
}

func (f *fakeSubmit) fn(_ context.Context, from, to string, msg []byte) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		from, to string
		msg      []byte
	}{from, to, msg})
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
	mustExec(t, db, `INSERT INTO tenant_hostnames VALUES('h1','acme.com','tnt_acme','web','t','op',NULL,'2026-01-01T00:00:00Z')`)
	mustExec(t, db, `INSERT INTO tenant_hostnames VALUES('h2','unv.acme.com','tnt_acme','web','t','op',NULL,NULL)`)
	mustExec(t, db, `INSERT INTO tenant_hostnames VALUES('h3','gone.acme.com','tnt_acme','web','t','op','2026-02-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	// zoned.example: acme serves DNS for it (active zone) but has NO verified
	// hostname row — DNS delegation alone makes it a valid sender domain.
	mustExec(t, db, `INSERT INTO dns_zones(id,tenant_id,origin,mname,rname,created_at,updated_at) VALUES('dz1','tnt_acme','zoned.example','ns1','h','t','t')`)
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
	if c.from != "noreply@acme.com" || c.to != "matt@example.com" {
		t.Fatalf("submit from/to = %q/%q", c.from, c.to)
	}
	if !strings.Contains(string(c.msg), "Welcome") || !strings.Contains(string(c.msg), "Thanks for signing up") {
		t.Fatalf("message missing subject/body:\n%s", c.msg)
	}
	if len(u.events) != 1 || u.events[0].Src != "sendmail" || !u.events[0].Billable || u.events[0].Tenant != "acme" {
		t.Fatalf("usage events = %+v", u.events)
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
	if len(sub.calls) != 1 || sub.calls[0].to != "b@example.com" {
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

func TestRenderAndCompose(t *testing.T) {
	body, err := renderBody("<p>Hi {{.name}}</p>", map[string]any{"name": "Bob"})
	if err != nil || !strings.Contains(string(body), "Hi Bob") {
		t.Fatalf("renderBody: %q %v", body, err)
	}
	full, err := renderDefault("Subj", body, "")
	if err != nil || !strings.Contains(full, "Hi Bob") || !strings.Contains(full, "Subj") {
		t.Fatalf("renderDefault: %v", err)
	}
	if txt := htmlToText(full); strings.Contains(txt, "<") {
		t.Fatalf("htmlToText left tags: %q", txt)
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
