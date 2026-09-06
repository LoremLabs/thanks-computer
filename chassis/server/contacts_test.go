package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newContactsDeps builds the op deps over a temp SQLite index with a mirror
// DB that owns exactly the given (hostname → tenant) pairs.
func newContactsDeps(t *testing.T, owned map[string]string) contactsDeps {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "contacts.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := chcon.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	mirror, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE tenant_hostnames (hostname TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
		`CREATE TABLE dns_zones (origin TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
	} {
		if _, err := mirror.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for host, slug := range owned {
		if _, err := mirror.Exec(`INSERT OR IGNORE INTO tenants VALUES (?, ?, NULL)`, "t_"+slug, slug); err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.Exec(`INSERT INTO tenant_hostnames VALUES (?, ?, '2026-09-05T00:00:00Z', NULL)`, host, "t_"+slug); err != nil {
			t.Fatal(err)
		}
	}
	fixed := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	return contactsDeps{store: store, snap: func() *sql.DB { return mirror }, dialect: registry.SQLite,
		maxBytes: 1 << 20, prefix: "/carddav", now: func() time.Time { return fixed }}
}

func callCon(t *testing.T, fn func(context.Context, contactsDeps, []byte) (event.Payload, error), d contactsDeps, tenant, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"demo/100/contacts"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestContactsAccountOp(t *testing.T) {
	d := newContactsDeps(t, map[string]string{"pony.example.com": "acme"})

	out := callCon(t, contactsAccount, d, "acme", `{"username":"Paris@Pony.Example.com","password_style":"words"}`)
	if gjson.Get(out, "_contacts.error").Exists() {
		t.Fatalf("create: %s", out)
	}
	pw := gjson.Get(out, "_contacts.password").String()
	if gjson.Get(out, "_contacts.username").String() != "paris@pony.example.com" || !gjson.Get(out, "_contacts.created").Bool() ||
		strings.Count(pw, "-") != 4 || gjson.Get(out, "_contacts.principal").String() != "/carddav/paris@pony.example.com/" {
		t.Errorf("create = %s", out)
	}
	a, ok, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if !ok || a.Tenant != "acme" {
		t.Fatalf("account = %+v ok=%v", a, ok)
	}
	if match, _ := apppass.VerifyPassword(a.PwHash, pw); !match {
		t.Error("generated password does not verify")
	}
	// The shared-credential path: an explicit password (the one IMAP minted).
	out = callCon(t, contactsAccount, d, "acme", `{"username":"paris@pony.example.com","password":"river-galaxy-bamboo-orbit-velvet","into":"_ca"}`)
	if gjson.Get(out, "_ca.error").Exists() || gjson.Get(out, "_ca.password").Exists() || gjson.Get(out, "_ca.created").Bool() {
		t.Errorf("explicit password = %s", out)
	}
	a2, _, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if match, _ := apppass.VerifyPassword(a2.PwHash, "river-galaxy-bamboo-orbit-velvet"); !match {
		t.Error("explicit password not stored")
	}
	out = callCon(t, contactsAccount, d, "acme", `{"username":"paris@pony.example.com","rotate":true}`)
	if !gjson.Get(out, "_contacts.rotated").Bool() || gjson.Get(out, "_contacts.password").String() == "" {
		t.Errorf("rotate = %s", out)
	}
	for meta, code := range map[string]string{
		`{"username":"x@else.example.com"}`:                          "txco_contacts_domain_not_owned",
		`{"username":"nope"}`:                                        "txco_contacts_invalid_arg",
		`{"username":"x@pony.example.com","password":"short"}`:       "txco_contacts_invalid_arg",
		`{"username":"x@pony.example.com","policy":{"put":"maybe"}}`: "txco_contacts_invalid_arg",
	} {
		if got := gjson.Get(callCon(t, contactsAccount, d, "acme", meta), "_contacts.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	if got := gjson.Get(callCon(t, contactsAccount, d, "other", `{"username":"paris@pony.example.com"}`), "_contacts.error.code").String(); got != "txco_contacts_domain_not_owned" {
		t.Errorf("other tenant → %s", got)
	}
	if got := gjson.Get(callCon(t, contactsAccount, d, "", `{"username":"paris@pony.example.com"}`), "_contacts.error.code").String(); got != "txco_contacts_no_tenant" {
		t.Errorf("no tenant → %s", got)
	}
	if got := gjson.Get(callCon(t, contactsAccount, contactsDeps{}, "acme", `{"username":"paris@pony.example.com"}`), "_contacts.error.code").String(); got != "txco_contacts_disabled" {
		t.Errorf("no store → %s", got)
	}
}

const appleVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//Apple Inc.//macOS 15.0//EN\r\nN:Example;Carol;;;\r\nFN:Carol Example\r\n" +
	"item1.EMAIL;type=INTERNET;type=pref:Carol@Example.com\r\nitem1.X-ABLabel:_$!<Other>!$_\r\nNOTE:Call first\\; then email.\r\n" +
	"UID:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345\r\nREV:2026-09-05T10:00:00Z\r\nEND:VCARD\r\n"

func TestContactsAddressbookAndObjectsOps(t *testing.T) {
	d := newContactsDeps(t, map[string]string{"pony.example.com": "acme"})
	callCon(t, contactsAccount, d, "acme", `{"username":"paris@pony.example.com"}`)

	out := callCon(t, contactsAddressbook, d, "acme", `{"username":"paris@pony.example.com","name":"senders","display_name":"Paris senders","description":"who may write","policy":{"put":"stack","delete":"stack"}}`)
	if gjson.Get(out, "_contacts.error").Exists() || !gjson.Get(out, "_contacts.created").Bool() ||
		gjson.Get(out, "_contacts.path").String() != "/carddav/paris@pony.example.com/addressbooks/senders/" ||
		gjson.Get(out, "_contacts.policy.put").String() != "stack" || gjson.Get(out, "_contacts.description").String() != "who may write" {
		t.Fatalf("ensure = %s", out)
	}
	out = callCon(t, contactsAddressbook, d, "acme", `{"username":"paris@pony.example.com","name":"senders"}`)
	if gjson.Get(out, "_contacts.created").Bool() || gjson.Get(out, "_contacts.display_name").String() != "Paris senders" {
		t.Errorf("re-ensure = %s", out)
	}
	for meta, code := range map[string]string{
		`{"username":"paris@pony.example.com","name":"Bad Name"}`:                     "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","name":"x","policy":{"fly":"observe"}}`: "txco_contacts_invalid_arg",
		`{"username":"nobody@pony.example.com","name":"x"}`:                           "txco_contacts_no_account",
	} {
		if got := gjson.Get(callCon(t, contactsAddressbook, d, "acme", meta), "_contacts.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}

	// put from card{}: uid derived from name; fn defaults to the email.
	out = callCon(t, contactsPut, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"bob-example.com.vcf","card":{"emails":[{"value":"Bob@Example.com"}]}}`)
	if gjson.Get(out, "_contacts.error").Exists() || !gjson.Get(out, "_contacts.created").Bool() || gjson.Get(out, "_contacts.noop").Bool() ||
		gjson.Get(out, "_contacts.uid").String() != "bob-example.com.paris@pony.example.com" || gjson.Get(out, "_contacts.name").String() != "bob-example.com.vcf" ||
		gjson.Get(out, "_contacts.path").String() != "/carddav/paris@pony.example.com/addressbooks/senders/bob-example.com.vcf" {
		t.Fatalf("put = %s", out)
	}
	etag := gjson.Get(out, "_contacts.etag").String()
	// Same card an hour later ⇒ noop, same etag (REV ignored).
	d.now = func() time.Time { return time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC) }
	out = callCon(t, contactsPut, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"bob-example.com.vcf","card":{"emails":[{"value":"Bob@Example.com"}]}}`)
	if !gjson.Get(out, "_contacts.noop").Bool() || gjson.Get(out, "_contacts.etag").String() != etag || gjson.Get(out, "_contacts.created").Bool() {
		t.Errorf("noop put = %s", out)
	}
	// Changed card, addressed by uid, no name ⇒ same resource, new etag.
	out = callCon(t, contactsPut, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","uid":"bob-example.com.paris@pony.example.com","card":{"fn":"Bob Example","emails":[{"value":"bob@example.com"}]}}`)
	if gjson.Get(out, "_contacts.noop").Bool() || gjson.Get(out, "_contacts.etag").String() == etag ||
		gjson.Get(out, "_contacts.name").String() != "bob-example.com.vcf" || gjson.Get(out, "_contacts.modseq").Int() != 2 {
		t.Errorf("changed put = %s", out)
	}
	// get: bytes + parsed facts.
	out = callCon(t, contactsGet, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","uid":"bob-example.com.paris@pony.example.com"}`)
	if !strings.Contains(gjson.Get(out, "_contacts.vcard").String(), "FN:Bob Example\r\n") || gjson.Get(out, "_contacts.card.fn").String() != "Bob Example" ||
		gjson.Get(out, "_contacts.card.addresses.0").String() != "bob@example.com" || gjson.Get(out, "_contacts.addresses.0").String() != "bob@example.com" ||
		gjson.Get(out, "_contacts.fn").String() != "Bob Example" || gjson.Get(out, "_contacts.kind").String() != "individual" || gjson.Get(out, "_contacts.version").String() != "3.0" {
		t.Errorf("get = %s", out)
	}
	// put from vcard text, an Apple-shaped card kept as written; uid disagreement refused.
	meta, _ := sjsonSet(`{"username":"paris@pony.example.com","addressbook":"senders"}`, "vcard", appleVCard)
	out = callCon(t, contactsPut, d, "acme", meta)
	if gjson.Get(out, "_contacts.error").Exists() || gjson.Get(out, "_contacts.uid").String() != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345" ||
		gjson.Get(out, "_contacts.name").String() != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345.vcf" {
		t.Errorf("vcard put = %s", out)
	}
	got := callCon(t, contactsGet, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","uid":"8F2C1A34-1111-4C2B-9E5D-ABCDEF012345"}`)
	if gjson.Get(got, "_contacts.vcard").String() != appleVCard || gjson.Get(got, "_contacts.card.note").String() != "Call first; then email." ||
		gjson.Get(got, "_contacts.addresses.0").String() != "carol@example.com" {
		t.Errorf("vcard kept as written? %s", got)
	}
	meta, _ = sjsonSet(meta, "uid", "other")
	if got := gjson.Get(callCon(t, contactsPut, d, "acme", meta), "_contacts.error.code").String(); got != "txco_contacts_invalid_arg" {
		t.Errorf("uid disagreement → %s", got)
	}
	for meta, code := range map[string]string{
		`{"username":"paris@pony.example.com","addressbook":"senders","card":{"fn":"x"}}`:                                              "txco_contacts_invalid_arg", // no uid, no name
		`{"username":"paris@pony.example.com","addressbook":"senders","name":"x.vcf","card":{"emails":[{"value":"nope"}]}}`:            "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"senders","name":"x.vcf"}`:                                                 "txco_contacts_invalid_arg", // neither
		`{"username":"paris@pony.example.com","addressbook":"nope","name":"x.vcf","card":{"fn":"x"}}`:                                  "txco_contacts_no_addressbook",
		`{"username":"paris@pony.example.com","addressbook":"senders","name":"../x","card":{"fn":"x"}}`:                                "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"senders","vcard":"garbage"}`:                                              "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"senders","vcard":"BEGIN:VCARD\r\nVERSION:2.1\r\nUID:a\r\nEND:VCARD\r\n"}`: "txco_contacts_invalid_arg",
	} {
		if got := gjson.Get(callCon(t, contactsPut, d, "acme", meta), "_contacts.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	small := d
	small.maxBytes = 60
	if got := gjson.Get(callCon(t, contactsPut, small, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"big.vcf","card":{"fn":"x"}}`), "_contacts.error.code").String(); got != "txco_contacts_too_large" {
		t.Errorf("too large → %s", got)
	}
	// list: books, then objects (facts incl. addresses, never bytes).
	out = callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_contacts.count").Int() != 1 || gjson.Get(out, "_contacts.addressbooks.0.objects").Int() != 2 || gjson.Get(out, "_contacts.home").String() != "/carddav/paris@pony.example.com/addressbooks/" {
		t.Errorf("list books = %s", out)
	}
	out = callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","limit":1}`)
	if gjson.Get(out, "_contacts.count").Int() != 1 || gjson.Get(out, "_contacts.next").Int() != 2 || gjson.Get(out, "_contacts.items.0.name").String() != "bob-example.com.vcf" ||
		gjson.Get(out, "_contacts.items.0.addresses.0").String() != "bob@example.com" || gjson.Get(out, "_contacts.items.0.vcard").Exists() {
		t.Errorf("list objects page 1 = %s", out)
	}
	out = callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","after":2}`)
	if gjson.Get(out, "_contacts.count").Int() != 1 || gjson.Get(out, "_contacts.next").Int() != 0 || gjson.Get(out, "_contacts.items.0.uid").String() != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345" {
		t.Errorf("list objects page 2 = %s", out)
	}
	// delete by uid, then by name (absent ⇒ deleted false).
	out = callCon(t, contactsDelete, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","uid":"8F2C1A34-1111-4C2B-9E5D-ABCDEF012345"}`)
	if !gjson.Get(out, "_contacts.deleted").Bool() || gjson.Get(out, "_contacts.name").String() != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345.vcf" {
		t.Errorf("delete = %s", out)
	}
	out = callCon(t, contactsDelete, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"8F2C1A34-1111-4C2B-9E5D-ABCDEF012345.vcf"}`)
	if gjson.Get(out, "_contacts.deleted").Bool() {
		t.Errorf("second delete = %s", out)
	}
	if got := gjson.Get(callCon(t, contactsGet, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"8F2C1A34-1111-4C2B-9E5D-ABCDEF012345.vcf"}`), "_contacts.error.code").String(); got != "txco_contacts_no_object" {
		t.Errorf("get deleted → %s", got)
	}
	out = callCon(t, contactsAddressbook, d, "acme", `{"username":"paris@pony.example.com","name":"senders","remove":true}`)
	if !gjson.Get(out, "_contacts.removed").Bool() {
		t.Errorf("remove = %s", out)
	}
	if got := gjson.Get(callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders"}`), "_contacts.error.code").String(); got != "txco_contacts_no_addressbook" {
		t.Errorf("list removed → %s", got)
	}
}

func TestContactsSyncOp(t *testing.T) {
	d := newContactsDeps(t, map[string]string{"pony.example.com": "acme"})
	callCon(t, contactsAccount, d, "acme", `{"username":"paris@pony.example.com"}`)
	callCon(t, contactsAddressbook, d, "acme", `{"username":"paris@pony.example.com","name":"senders"}`)
	callCon(t, contactsPut, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","name":"ann-x.io.vcf","card":{"emails":[{"value":"ann@x.io"}]}}`)

	out := callCon(t, contactsSync, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders",
		"put":[{"name":"ann-x.io.vcf","card":{"emails":[{"value":"ann@x.io"}]}},
		       {"name":"bob-x.io.vcf","card":{"emails":[{"value":"bob@x.io"}]}}],
		"delete":["nobody.paris@pony.example.com"]}`)
	if gjson.Get(out, "_contacts.error").Exists() || gjson.Get(out, "_contacts.noop").Int() != 1 || gjson.Get(out, "_contacts.created").Int() != 1 ||
		gjson.Get(out, "_contacts.deleted").Int() != 0 || gjson.Get(out, "_contacts.missing").Int() != 1 || gjson.Get(out, "_contacts.count").Int() != 3 ||
		gjson.Get(out, "_contacts.items.1.uid").String() != "bob-x.io.paris@pony.example.com" {
		t.Fatalf("sync 1 = %s", out)
	}
	out = callCon(t, contactsSync, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders","delete":["ann-x.io.paris@pony.example.com"]}`)
	if gjson.Get(out, "_contacts.deleted").Int() != 1 || gjson.Get(out, "_contacts.count").Int() != 1 || gjson.Get(out, "_contacts.items").String() != "[]" {
		t.Errorf("sync 2 = %s", out)
	}
	live := callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders"}`)
	if gjson.Get(live, "_contacts.count").Int() != 1 || gjson.Get(live, "_contacts.items.0.addresses.0").String() != "bob@x.io" {
		t.Errorf("after sync 2 = %s", live)
	}
	// A bad entry refuses the batch, names the entry, writes nothing.
	out = callCon(t, contactsSync, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders",
		"put":[{"name":"dan-x.io.vcf","card":{"emails":[{"value":"dan@x.io"}]}},{"name":"e.vcf","card":{"emails":[{"value":"not an email"}]}}]}`)
	if gjson.Get(out, "_contacts.error.code").String() != "txco_contacts_invalid_arg" || !strings.HasPrefix(gjson.Get(out, "_contacts.error.message").String(), "put[1]:") {
		t.Errorf("sync bad entry = %s", out)
	}
	live = callCon(t, contactsList, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders"}`)
	if gjson.Get(live, "_contacts.count").Int() != 1 {
		t.Errorf("a refused sync wrote: %s", live)
	}
	// A store-level failure inside the batch names the entry too.
	out = callCon(t, contactsSync, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders",
		"put":[{"name":"other.vcf","card":{"uid":"bob-x.io.paris@pony.example.com","fn":"Bob"}},{"name":"bob-x.io.vcf","card":{"uid":"bob-x.io.paris@pony.example.com","fn":"Robert"}}],
		"delete":[""]}`)
	if gjson.Get(out, "_contacts.error.code").String() != "txco_contacts_invalid_arg" {
		t.Errorf("empty delete uid = %s", out)
	}
	for meta, code := range map[string]string{
		`{"username":"paris@pony.example.com","addressbook":"senders","put":"x"}`:       "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"senders","put":[1]}`:       "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"senders","delete":[1]}`:    "txco_contacts_invalid_arg",
		`{"username":"paris@pony.example.com","addressbook":"nope","put":[]}`:           "txco_contacts_no_addressbook",
		`{"username":"nobody@pony.example.com","addressbook":"senders","delete":["a"]}`: "txco_contacts_no_account",
	} {
		if got := gjson.Get(callCon(t, contactsSync, d, "acme", meta), "_contacts.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	// An empty sync is fine: nothing happens.
	out = callCon(t, contactsSync, d, "acme", `{"username":"paris@pony.example.com","addressbook":"senders"}`)
	if gjson.Get(out, "_contacts.error").Exists() || gjson.Get(out, "_contacts.count").Int() != 0 {
		t.Errorf("empty sync = %s", out)
	}
}

func TestContactsPaths(t *testing.T) {
	if got := addressbookPath("/carddav/", "a@b.c", "x"); got != "/carddav/a@b.c/addressbooks/x/" {
		t.Errorf("addressbookPath = %s", got)
	}
	if got := addressbookPath("", "a@b.c", "x"); got != "/carddav/a@b.c/addressbooks/x/" {
		t.Errorf("empty prefix = %s", got)
	}
	if got := contactsObjectPath("dav", "a@b.c", "x", "y.vcf"); got != "/dav/a@b.c/addressbooks/x/y.vcf" {
		t.Errorf("objectPath = %s", got)
	}
}
