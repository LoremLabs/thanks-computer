package server

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// contacts.go — the shared plumbing of the txco://contacts/* family, the
// op-writable surface over the contacts store (chassis/contacts) the
// `contacts` personality serves as CardDAV; contacts_ops.go has the
// handlers:
//
//	txco://contacts/account      create or update an account (argon2id); the
//	                             same password block as txco://imap/account
//	                             and txco://calendar/account, so a product can
//	                             hand one credential to all three heads
//	txco://contacts/addressbook  ensure/update/remove an address book and its
//	                             policy
//	txco://contacts/put          materialize a card by UID: from `card{}`
//	                             (the chassis renders vCard 3.0) or `vcard`
//	                             (text, kept as written)
//	txco://contacts/get          one object, bytes + parsed facts
//	txco://contacts/list         the account's address books, or one book's
//	                             objects with their addresses
//	txco://contacts/delete       tombstone one object
//	txco://contacts/sync         a bounded batch of puts and deletes in one
//	                             transaction — how a stack without loops
//	                             reconciles a list of people
//
// Scoping is trusted: tenant from processor.TenantScope(ctx), never a
// mutable _txc.* field. An account belongs to the tenant that created it;
// its username's domain must pass the sendmail ownership rule
// (mail.DomainOwnedByTenant). Ops never dispatch the `_contacts` lanes — a
// stack writing its own address book is not a client mutation.
//
// Output lands under `into` (default `_contacts`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._contacts.error.code != ""` and the run continues.

type contactsDeps struct {
	store *chcon.Store // nil ⇒ txco_contacts_disabled
	// snap returns the mirror DB the domain-ownership rule reads (dbcache
	// snapshot); nil ⇒ every domain is refused.
	snap     func() *sql.DB
	dialect  registry.Dialect
	maxBytes int64
	prefix   string // --contacts-path-prefix, for the paths in results
	now      func() time.Time
}

// syncMaxEntries caps each of txco://contacts/sync's two lists.
const syncMaxEntries = 200

func contactsInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_contacts"
	}
	return into
}

func contactsErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// contactsPrelude is the common head of every handler: tenant, store, meta.
func contactsPrelude(ctx context.Context, d contactsDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = contactsInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, contactsErr(into, "txco_contacts_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", nil, into, contactsErr(into, "txco_contacts_disabled", "no contacts store on this node (contacts personality off and --contacts-store=sqlite, or the shared store failed to open at boot)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

func contactsNow(d contactsDeps) time.Time {
	if d.now == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return d.now().UTC().Truncate(time.Second)
}

func (d contactsDeps) domainOwned(ctx context.Context, tenant, domain string) (bool, error) {
	return imapDeps{snap: d.snap, dialect: d.dialect}.domainOwned(ctx, tenant, domain)
}

// contactsAccountFor resolves the WITH username to the tenant's account.
func contactsAccountFor(ctx context.Context, d contactsDeps, tenant string, meta []byte, into string) (chcon.Account, event.Payload, bool) {
	username := chcon.NormalizeUsername(gjson.GetBytes(meta, "username").String())
	if username == "" {
		return chcon.Account{}, contactsErr(into, "txco_contacts_invalid_arg", "missing `username`"), false
	}
	acct, exists, err := d.store.GetAccount(ctx, username)
	if err != nil {
		return chcon.Account{}, contactsErr(into, "txco_contacts_store", err.Error()), false
	}
	if !exists || acct.Tenant != tenant {
		return chcon.Account{}, contactsErr(into, "txco_contacts_no_account", fmt.Sprintf("no contacts account %q for this tenant", username)), false
	}
	return acct, event.Payload{}, true
}

// addressbookFor resolves the WITH `addressbook` (a name) to a live book of
// the account.
func addressbookFor(ctx context.Context, d contactsDeps, acct chcon.Account, meta []byte, into string) (chcon.Addressbook, event.Payload, bool) {
	name := strings.TrimSpace(gjson.GetBytes(meta, "addressbook").String())
	if name == "" {
		return chcon.Addressbook{}, contactsErr(into, "txco_contacts_invalid_arg", "missing `addressbook` (the address book's name)"), false
	}
	ab, found, err := d.store.GetAddressbook(ctx, acct.Tenant, acct.Username, name)
	if err != nil {
		return chcon.Addressbook{}, contactsErr(into, "txco_contacts_store", err.Error()), false
	}
	if !found {
		return chcon.Addressbook{}, contactsErr(into, "txco_contacts_no_addressbook", fmt.Sprintf("no address book %q for %s", name, acct.Username)), false
	}
	return ab, event.Payload{}, true
}

// Path shapes the head serves (depth under the prefix is what the CardDAV
// library switches on: principal 1, home 2, address book 3, object 4).
func contactsPrefix(prefix string) string {
	p := strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if p == "" {
		p = "/carddav"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func contactsPrincipalPath(prefix, username string) string {
	return contactsPrefix(prefix) + "/" + username + "/"
}

func contactsHomePath(prefix, username string) string {
	return contactsPrincipalPath(prefix, username) + "addressbooks/"
}

func addressbookPath(prefix, username, name string) string {
	return contactsHomePath(prefix, username) + name + "/"
}

func contactsObjectPath(prefix, username, name, resource string) string {
	return addressbookPath(prefix, username, name) + resource
}

func addressbookJSON(b *jsonx.Builder, p, prefix string, ab chcon.Addressbook) {
	b.Set(p+".id", ab.ID)
	b.Set(p+".name", ab.Name)
	b.Set(p+".path", addressbookPath(prefix, ab.Username, ab.Name))
	b.Set(p+".display_name", ab.DisplayName)
	b.Set(p+".description", ab.Description)
	b.Set(p+".sort_order", ab.SortOrder)
	b.SetRaw(p+".policy", rawJSON(ab.Policy, "{}"))
	b.Set(p+".sync_token", ab.SyncToken)
	b.Set(p+".updated_at", ab.UpdatedAt.UTC().Format(time.RFC3339))
}

func contactJSON(b *jsonx.Builder, p, prefix, username, abName string, o chcon.Object) {
	b.Set(p+".name", o.Name)
	b.Set(p+".path", contactsObjectPath(prefix, username, abName, o.Name))
	b.Set(p+".uid", o.UID)
	b.Set(p+".etag", o.ETag)
	b.Set(p+".size", o.Size)
	b.Set(p+".version", o.Version)
	b.Set(p+".fn", o.FN)
	b.Set(p+".kind", o.Kind)
	if len(o.Addresses) == 0 {
		b.SetRaw(p+".addresses", "[]")
	} else {
		for i, a := range o.Addresses {
			b.Set(fmt.Sprintf("%s.addresses.%d", p, i), a)
		}
	}
	b.Set(p+".modseq", o.ModSeq)
	b.Set(p+".updated_at", o.UpdatedAt.UTC().Format(time.RFC3339))
}
