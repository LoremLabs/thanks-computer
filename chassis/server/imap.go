package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/mail"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	imapp "github.com/loremlabs/thanks-computer/chassis/server/personality/imap"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// imap.go holds the handler bodies for txco://imap/account and
// txco://imap/append — the op-writable surface over the IMAP mailbox
// store (chassis/imap) the `imap` personality serves; imap_ops.go has the
// rest of the family (mailbox, remove, flags, list, messages, get).
//
//   txco://imap/account  create or update an account (argon2id), ensuring
//                        its INBOX; password="" generates one and returns
//                        it exactly once.
//   txco://imap/append   materialize a message RECORD into a mailbox: the
//                        canonical retained form (headers subset + normalized
//                        text/html + part references), stored in the CAS by
//                        its own sha; the head renders RFC 5322 on FETCH.
//                        `from` (envelope path, base64 RFC 5322) / `from_sha`
//                        (an owned CAS object) store the exact bytes instead.
//
// Scoping is trusted: tenant from processor.TenantScope(ctx), never a
// mutable _txc.* field. An account belongs to the tenant that created it;
// its username's domain must pass the same ownership rule txco://sendmail
// applies to From: (mail.DomainOwnedByTenant).
//
// Output lands under `into` (default `_imap`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._imap.error.code != ""` and the run continues.

type imapDeps struct {
	store *chimap.Store // nil ⇒ txco_imap_disabled
	fcas  filecas.Store // nil ⇒ append answers txco_imap_disabled
	ix    blob.Index    // may be nil (no tenant sha rows recorded)
	// snap returns the mirror DB the domain-ownership rule reads (dbcache
	// snapshot); nil ⇒ every domain is refused.
	snap     func() *sql.DB
	dialect  registry.Dialect
	maxBytes int64
	now      func() time.Time
}

func imapInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_imap"
	}
	return into
}

func imapErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// imapPrelude is the common head of both handlers: tenant, store, meta.
func imapPrelude(ctx context.Context, d imapDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = imapInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, imapErr(into, "txco_imap_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", nil, into, imapErr(into, "txco_imap_disabled", "no IMAP store on this node (imap personality not active)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

var imapLocalPartRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._+-]{0,62}[a-z0-9])?$`)

// parseUsername validates `<local>@<domain>` and returns the canonical
// pair. The local part is the pony/persona; the domain must be owned by
// the tenant (checked by the caller).
func parseUsername(u string) (username, domain string, err error) {
	username = chimap.NormalizeUsername(u)
	at := strings.LastIndex(username, "@")
	if at <= 0 || at == len(username)-1 || strings.Count(username, "@") != 1 {
		return "", "", fmt.Errorf("username must be <local>@<domain>: %q", u)
	}
	local := username[:at]
	if !imapLocalPartRE.MatchString(local) {
		return "", "", fmt.Errorf("username local part %q: letters, digits, . _ + - only, 1-64 chars, no leading/trailing dot", local)
	}
	canon, ok := tenants.CanonicalizeHost(username[at+1:])
	if !ok || canon == "" || !strings.Contains(canon, ".") && canon != "localhost" {
		return "", "", fmt.Errorf("username domain %q is not a hostname", username[at+1:])
	}
	return local + "@" + canon, canon, nil
}

func (d imapDeps) domainOwned(ctx context.Context, tenant, domain string) (bool, error) {
	if d.snap == nil {
		return false, nil
	}
	return mail.DomainOwnedByTenant(ctx, d.snap(), d.dialect, tenant, domain)
}

// imapAccount creates or updates an IMAP account for the pinned tenant.
// Result at `into`: {username, created, password?} — password only when
// generated.
func imapAccount(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	username, domain, err := parseUsername(gjson.GetBytes(meta, "username").String())
	if err != nil {
		return imapErr(into, "txco_imap_invalid_arg", err.Error()), nil
	}
	owned, err := d.domainOwned(ctx, tenant, domain)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	if !owned {
		return imapErr(into, "txco_imap_domain_not_owned",
			fmt.Sprintf("domain %q is not a verified hostname or delegated zone of this tenant", domain)), nil
	}

	// password: absent ⇒ unchanged on update / generated on create;
	// explicit "" ⇒ generated (and returned once); otherwise stored.
	var generated, pwHash string
	pw := gjson.GetBytes(meta, "password")
	switch {
	case pw.Exists() && pw.String() != "":
		if len(pw.String()) < 8 {
			return imapErr(into, "txco_imap_invalid_arg", "password must be at least 8 characters"), nil
		}
		pwHash, err = chimap.HashPassword(pw.String())
	case pw.Exists():
		generated = chimap.GeneratePassword()
		pwHash, err = chimap.HashPassword(generated)
	default:
		if _, exists, gerr := d.store.GetAccount(ctx, username); gerr == nil && !exists {
			generated = chimap.GeneratePassword()
			pwHash, err = chimap.HashPassword(generated)
		} else if gerr != nil {
			err = gerr
		}
	}
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	status := gjson.GetBytes(meta, "status").String()
	var policy json.RawMessage
	if p := gjson.GetBytes(meta, "policy"); p.Exists() && p.IsObject() {
		policy = json.RawMessage(p.Raw)
	}
	created, err := d.store.UpsertAccount(ctx, tenant, username, pwHash, status, policy)
	if err != nil {
		code := "txco_imap_store"
		if err == chimap.ErrUsernameTaken {
			code = "txco_imap_username_taken"
		}
		return imapErr(into, code, err.Error()), nil
	}
	raw, _ := sjson.Set(`{}`, into+".username", username)
	raw, _ = sjson.Set(raw, into+".created", created)
	if generated != "" {
		raw, _ = sjson.Set(raw, into+".password", generated)
	}
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// imapRecordFromMeta builds the Record from the `message{}` WITH param.
func imapRecordFromMeta(msg gjson.Result) (*chimap.Record, error) {
	if !msg.Exists() || !msg.IsObject() {
		return nil, fmt.Errorf("`message` must be an object {from,to,cc,subject,date,headers{},text,html,attachments[]}")
	}
	rec := &chimap.Record{Headers: map[string]string{}}
	if h := msg.Get("headers"); h.IsObject() {
		h.ForEach(func(k, v gjson.Result) bool {
			if v.Type == gjson.String {
				rec.Headers[k.String()] = v.String()
			} else if v.Exists() && v.Type != gjson.Null {
				rec.Headers[k.String()] = v.Raw
			}
			return true
		})
	}
	for param, header := range map[string]string{
		"from": "From", "to": "To", "cc": "Cc", "reply_to": "Reply-To", "subject": "Subject", "date": "Date",
		"message_id": "Message-ID", "in_reply_to": "In-Reply-To", "references": "References",
	} {
		if v := msg.Get(param); v.Exists() && v.String() != "" {
			rec.Headers[header] = v.String()
		}
	}
	rec.Text = msg.Get("text").String()
	rec.HTML = msg.Get("html").String()
	for i, a := range msg.Get("attachments").Array() {
		sha := a.Get("sha256").String()
		if sha == "" {
			sha = a.Get("from_sha").String()
		}
		if sha != "" && !blob.ValidSha256(sha) {
			return nil, fmt.Errorf("attachments[%d].sha256 must be 64 lowercase hex chars", i)
		}
		rec.Parts = append(rec.Parts, chimap.PartRef{
			N: i + 1, Name: a.Get("name").String(), Type: a.Get("content_type").String(),
			Size: a.Get("size").Int(), SHA256: sha,
		})
	}
	if rec.Text == "" && rec.HTML == "" && len(rec.Parts) == 0 {
		return nil, fmt.Errorf("`message` needs text, html or attachments")
	}
	return rec, nil
}

// imapAppend materializes a record into one of the account's mailboxes.
// Result at `into`: {uid, uidvalidity, sha256, noop, replaced}.
func imapAppend(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	if d.fcas == nil {
		return imapErr(into, "txco_imap_disabled", "no content store on this node (filecas not configured)"), nil
	}
	verbatim := gjson.GetBytes(meta, "from").Exists() || gjson.GetBytes(meta, "from_sha").Exists()
	if verbatim && gjson.GetBytes(meta, "message").Exists() {
		return imapErr(into, "txco_imap_invalid_arg", "give `message{...}` (a record) or `from`/`from_sha` (verbatim bytes), not both"), nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	username := acct.Username
	objectKey := gjson.GetBytes(meta, "object_key").String()
	if objectKey == "" {
		return imapErr(into, "txco_imap_invalid_arg", "missing `object_key` (the stack's stable identity for this message)"), nil
	}
	if len(objectKey) > 512 {
		return imapErr(into, "txco_imap_invalid_arg", "`object_key` exceeds 512 bytes"), nil
	}
	// Mailbox: a name (default INBOX) or `role:<role>`.
	var mb chimap.Mailbox
	var found bool
	var err error
	target := gjson.GetBytes(meta, "mailbox").String()
	if target == "" {
		target = "INBOX"
	}
	if role, isRole := strings.CutPrefix(target, "role:"); isRole {
		mb, found, err = d.store.GetMailboxByRole(ctx, tenant, username, role)
	} else {
		mb, found, err = d.store.GetMailbox(ctx, tenant, username, target)
	}
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	if !found {
		return imapErr(into, "txco_imap_no_mailbox", fmt.Sprintf("no mailbox %q for %s", target, username)), nil
	}
	flags, _ := blobStrings(meta, "flags")
	internal := imapNow(d)
	if v := gjson.GetBytes(meta, "internaldate").String(); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			return imapErr(into, "txco_imap_invalid_arg", "`internaldate` must be RFC3339: "+perr.Error()), nil
		}
		internal = t
	}
	if verbatim {
		return imapAppendVerbatim(ctx, d, tenant, meta, in, into, acct, mb, objectKey, flags, internal)
	}
	rec, err := imapRecordFromMeta(gjson.GetBytes(meta, "message"))
	if err != nil {
		return imapErr(into, "txco_imap_invalid_arg", err.Error()), nil
	}
	ing, err := imapp.BuildRecordMessage(rec, objectKey, domainOfUsername(username), internal, flags)
	if err != nil {
		return imapErr(into, "txco_imap_invalid_arg", err.Error()), nil
	}
	if d.maxBytes > 0 && int64(len(ing.Rendered)) > d.maxBytes {
		return imapErr(into, "txco_imap_too_large",
			fmt.Sprintf("rendered message is %d bytes, over imap-append-max-bytes %d", len(ing.Rendered), d.maxBytes)), nil
	}
	blobChargeBytes(ctx, int64(len(ing.Rendered)), in)

	// Write order: CAS → sha row → index row (every crash point self-heals
	// on retry, same as blob/put).
	present, err := d.fcas.Exists(ctx, ing.SHA256)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	if !present {
		if err := filecas.PutReader(ctx, d.fcas, ing.SHA256, bytes.NewReader(ing.Canonical), int64(len(ing.Canonical))); err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
	}
	if d.ix != nil {
		if _, err := d.ix.PutShaIfAbsent(ctx, tenant, blob.ShaRow{
			SHA256: ing.SHA256, Size: int64(len(ing.Canonical)),
			ContentType: "application/vnd.txco.imap-record+json", FirstSeen: imapNow(d),
		}); err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
	}
	res, err := d.store.AppendMessage(ctx, mb.ID, ing.Message)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	raw, _ := sjson.Set(`{}`, into+".uid", res.UID)
	raw, _ = sjson.Set(raw, into+".uidvalidity", res.UIDValidity)
	raw, _ = sjson.Set(raw, into+".sha256", ing.SHA256)
	raw, _ = sjson.Set(raw, into+".size", len(ing.Rendered))
	raw, _ = sjson.Set(raw, into+".noop", res.Noop)
	raw, _ = sjson.Set(raw, into+".replaced", res.Replaced)
	raw, _ = sjson.Set(raw, into+".mailbox", mb.Name)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func imapNow(d imapDeps) time.Time {
	if d.now == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return d.now().UTC().Truncate(time.Second)
}

func domainOfUsername(username string) string {
	if at := strings.LastIndex(username, "@"); at >= 0 {
		return username[at+1:]
	}
	return ""
}
