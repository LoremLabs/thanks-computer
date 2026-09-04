package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// calendar.go — the shared plumbing of the txco://calendar/* family, the
// op-writable surface over the calendar store (chassis/calendar) the
// `calendar` personality serves as CalDAV + ICS feeds; calendar_ops.go has
// the handlers:
//
//	txco://calendar/account   create or update an account (argon2id); the
//	                          same password block as txco://imap/account, so
//	                          a product can hand one credential to both heads
//	txco://calendar/calendar  ensure/update/remove a calendar; its policy
//	                          and its ICS feed token (shown once)
//	txco://calendar/put       materialize an object by UID: from `event{}`
//	                          (the chassis renders) or `ical` (text)
//	txco://calendar/get       one object, bytes + parsed facts
//	txco://calendar/list      the account's calendars, or one calendar's objects
//	txco://calendar/delete    tombstone one object
//
// Scoping is trusted: tenant from processor.TenantScope(ctx), never a
// mutable _txc.* field. An account belongs to the tenant that created it;
// its username's domain must pass the sendmail ownership rule
// (mail.DomainOwnedByTenant). Ops never dispatch the `_calendar` lanes — a
// stack writing its own calendar is not a client mutation.
//
// Output lands under `into` (default `_calendar`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._calendar.error.code != ""` and the run continues.

type calendarDeps struct {
	store *chcal.Store // nil ⇒ txco_calendar_disabled
	// snap returns the mirror DB the domain-ownership rule reads (dbcache
	// snapshot); nil ⇒ every domain is refused.
	snap     func() *sql.DB
	dialect  registry.Dialect
	maxBytes int64
	prefix   string // --calendar-path-prefix, for the paths in results
	now      func() time.Time
}

func calendarInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_calendar"
	}
	return into
}

func calendarErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// calendarPrelude is the common head of every handler: tenant, store, meta.
func calendarPrelude(ctx context.Context, d calendarDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = calendarInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, calendarErr(into, "txco_calendar_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", nil, into, calendarErr(into, "txco_calendar_disabled", "no calendar store on this node (calendar personality off and --calendar-store=sqlite, or the shared store failed to open at boot)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

func calendarNow(d calendarDeps) time.Time {
	if d.now == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return d.now().UTC().Truncate(time.Second)
}

func (d calendarDeps) domainOwned(ctx context.Context, tenant, domain string) (bool, error) {
	return imapDeps{snap: d.snap, dialect: d.dialect}.domainOwned(ctx, tenant, domain)
}

// calendarAccountFor resolves the WITH username to the tenant's account.
func calendarAccountFor(ctx context.Context, d calendarDeps, tenant string, meta []byte, into string) (chcal.Account, event.Payload, bool) {
	username := chcal.NormalizeUsername(gjson.GetBytes(meta, "username").String())
	if username == "" {
		return chcal.Account{}, calendarErr(into, "txco_calendar_invalid_arg", "missing `username`"), false
	}
	acct, exists, err := d.store.GetAccount(ctx, username)
	if err != nil {
		return chcal.Account{}, calendarErr(into, "txco_calendar_store", err.Error()), false
	}
	if !exists || acct.Tenant != tenant {
		return chcal.Account{}, calendarErr(into, "txco_calendar_no_account", fmt.Sprintf("no calendar account %q for this tenant", username)), false
	}
	return acct, event.Payload{}, true
}

// calendarFor resolves the WITH `calendar` (a name) to a live calendar of
// the account.
func calendarFor(ctx context.Context, d calendarDeps, acct chcal.Account, meta []byte, into string) (chcal.Calendar, event.Payload, bool) {
	name := strings.TrimSpace(gjson.GetBytes(meta, "calendar").String())
	if name == "" {
		return chcal.Calendar{}, calendarErr(into, "txco_calendar_invalid_arg", "missing `calendar` (the calendar's name)"), false
	}
	cal, found, err := d.store.GetCalendar(ctx, acct.Tenant, acct.Username, name)
	if err != nil {
		return chcal.Calendar{}, calendarErr(into, "txco_calendar_store", err.Error()), false
	}
	if !found {
		return chcal.Calendar{}, calendarErr(into, "txco_calendar_no_calendar", fmt.Sprintf("no calendar %q for %s", name, acct.Username)), false
	}
	return cal, event.Payload{}, true
}

// Path shapes the head serves (depth under the prefix is what the CalDAV
// library switches on: principal 1, home 2, calendar 3, object 4).
func calendarPrefix(prefix string) string {
	p := strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if p == "" {
		p = "/dav"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func principalPath(prefix, username string) string {
	return calendarPrefix(prefix) + "/" + username + "/"
}

func homePath(prefix, username string) string {
	return principalPath(prefix, username) + "calendars/"
}

func calendarPath(prefix, username, name string) string {
	return homePath(prefix, username) + name + "/"
}

func objectPath(prefix, username, name, resource string) string {
	return calendarPath(prefix, username, name) + resource
}

func feedPath(prefix, token string) string {
	return calendarPrefix(prefix) + "/feed/" + token + ".ics"
}

// newFeedToken mints an opaque feed token (24 random bytes, URL-safe) and
// its sha256 hex; only the hash is stored.
func newFeedToken() (token, hash string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// FeedTokenHash is the lookup key for a presented feed token.
func FeedTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func calendarJSON(b *jsonx.Builder, p, prefix string, c chcal.Calendar) {
	b.Set(p+".id", c.ID)
	b.Set(p+".name", c.Name)
	b.Set(p+".path", calendarPath(prefix, c.Username, c.Name))
	b.Set(p+".display_name", c.DisplayName)
	b.Set(p+".description", c.Description)
	b.Set(p+".color", c.Color)
	b.Set(p+".timezone", c.Timezone)
	b.SetRaw(p+".policy", rawJSON(c.Policy, "{}"))
	b.Set(p+".feed", c.FeedTokenHash != "")
	b.Set(p+".sync_token", c.SyncToken)
	b.Set(p+".updated_at", c.UpdatedAt.UTC().Format(time.RFC3339))
}

func objectJSON(b *jsonx.Builder, p, prefix, username, calName string, o chcal.Object) {
	b.Set(p+".name", o.Name)
	b.Set(p+".path", objectPath(prefix, username, calName, o.Name))
	b.Set(p+".uid", o.UID)
	b.Set(p+".etag", o.ETag)
	b.Set(p+".component", o.Component)
	b.Set(p+".size", o.Size)
	b.Set(p+".summary", o.Summary)
	b.Set(p+".start_utc", o.DTStartUTC)
	b.Set(p+".end_utc", o.DTEndUTC)
	b.Set(p+".recurs", o.Recurs)
	b.Set(p+".sequence", o.Sequence)
	b.Set(p+".modseq", o.ModSeq)
	b.Set(p+".updated_at", o.UpdatedAt.UTC().Format(time.RFC3339))
}
