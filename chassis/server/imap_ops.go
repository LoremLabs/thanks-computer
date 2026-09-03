package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhillyerd/enmime/v2"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/event"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	imapp "github.com/loremlabs/thanks-computer/chassis/server/personality/imap"
)

// imap_ops.go — the rest of the txco://imap/* surface (§25.5):
//
//	txco://imap/mailbox   create / rename / delete / reset a mailbox; set role, attrs, policy
//	txco://imap/remove    expunge one message by object_key or uid
//	txco://imap/flags     add / remove flags on one message
//	txco://imap/list      the account's mailboxes with counts
//	txco://imap/messages  a UID-windowed listing of one mailbox
//	txco://imap/get       one message: headers, bodies, parts, optionally raw
//
// All tenant-pinned from ctx like account/append; the account must belong
// to the tenant. Output at `into` (default `_imap`), errors as
// `<into>.error.{code,message}` with the run continuing.

// imapAccountFor resolves the WITH username to the tenant's account.
func imapAccountFor(ctx context.Context, d imapDeps, tenant string, meta []byte, into string) (chimap.Account, event.Payload, bool) {
	username := chimap.NormalizeUsername(gjson.GetBytes(meta, "username").String())
	if username == "" {
		return chimap.Account{}, imapErr(into, "txco_imap_invalid_arg", "missing `username`"), false
	}
	acct, exists, err := d.store.GetAccount(ctx, username)
	if err != nil {
		return chimap.Account{}, imapErr(into, "txco_imap_store", err.Error()), false
	}
	if !exists || acct.Tenant != tenant {
		return chimap.Account{}, imapErr(into, "txco_imap_no_account", fmt.Sprintf("no account %q for this tenant", username)), false
	}
	return acct, event.Payload{}, true
}

// imapMailboxFor resolves `mailbox` (a name, default INBOX, or role:<r>)
// or `id` to a live mailbox of the account.
func imapMailboxFor(ctx context.Context, d imapDeps, acct chimap.Account, meta []byte, into string) (chimap.Mailbox, event.Payload, bool) {
	var mb chimap.Mailbox
	var found bool
	var err error
	if id := gjson.GetBytes(meta, "id").String(); id != "" {
		mb, found, err = d.store.GetMailboxByID(ctx, id)
		if found && (mb.Tenant != acct.Tenant || mb.Username != acct.Username) {
			found = false
		}
	} else {
		target := gjson.GetBytes(meta, "mailbox").String()
		if target == "" {
			target = "INBOX"
		}
		if role, isRole := strings.CutPrefix(target, "role:"); isRole {
			mb, found, err = d.store.GetMailboxByRole(ctx, acct.Tenant, acct.Username, role)
		} else {
			mb, found, err = d.store.GetMailbox(ctx, acct.Tenant, acct.Username, target)
		}
	}
	if err != nil {
		return chimap.Mailbox{}, imapErr(into, "txco_imap_store", err.Error()), false
	}
	if !found {
		return chimap.Mailbox{}, imapErr(into, "txco_imap_no_mailbox", "no such mailbox for "+acct.Username), false
	}
	return mb, event.Payload{}, true
}

func mailboxJSON(b *jsonx.Builder, prefix string, mb chimap.Mailbox) {
	b.Set(prefix+".id", mb.ID)
	b.Set(prefix+".name", mb.Name)
	b.Set(prefix+".role", mb.Role)
	attrs := mb.Attrs
	if attrs == nil {
		attrs = []string{}
	}
	b.Set(prefix+".attrs", attrs)
	b.SetRaw(prefix+".policy", rawJSON(mb.Policy, "{}"))
	b.Set(prefix+".uidvalidity", mb.UIDValidity)
	b.Set(prefix+".uidnext", mb.UIDNext)
	b.Set(prefix+".subscribed", mb.Subscribed)
}

func rawJSON(r json.RawMessage, def string) string {
	if len(r) == 0 || !json.Valid(r) {
		return def
	}
	return string(r)
}

// imapMailbox creates, updates, renames, deletes or resets a mailbox.
// Result: {id, name, role, attrs, policy, uidvalidity, created, deleted?}.
func imapMailbox(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	name := chimap.NormalizeMailboxName(gjson.GetBytes(meta, "name").String())
	id := gjson.GetBytes(meta, "id").String()
	if name == "" && id == "" {
		return imapErr(into, "txco_imap_invalid_arg", "need `name` (full path) or `id`"), nil
	}
	var policy json.RawMessage
	if p := gjson.GetBytes(meta, "policy"); p.Exists() {
		if !p.IsObject() {
			return imapErr(into, "txco_imap_invalid_arg", "`policy` must be an object of verb → deny|local|observe|stack"), nil
		}
		var bad string
		p.ForEach(func(k, v gjson.Result) bool {
			switch v.String() {
			case "deny", "local", "observe", "stack":
				return true
			}
			bad = k.String() + "=" + v.String()
			return false
		})
		if bad != "" {
			return imapErr(into, "txco_imap_invalid_arg", "policy "+bad+": mode must be deny|local|observe|stack"), nil
		}
		policy = json.RawMessage(p.Raw)
	}
	attrs, attrsGiven := blobStrings(meta, "attrs")
	var role *string
	if r := gjson.GetBytes(meta, "role"); r.Exists() {
		v := r.String()
		role = &v
	}

	// Resolve the existing mailbox, if any.
	var mb chimap.Mailbox
	var found bool
	var err error
	if id != "" {
		mb, found, err = d.store.GetMailboxByID(ctx, id)
		if found && (mb.Tenant != tenant || mb.Username != acct.Username) {
			found = false
		}
	} else {
		mb, found, err = d.store.GetMailbox(ctx, tenant, acct.Username, name)
	}
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}

	out := jsonx.NewObject()
	created := false
	switch {
	case gjson.GetBytes(meta, "delete").Bool():
		if !found {
			return imapErr(into, "txco_imap_no_mailbox", "no such mailbox"), nil
		}
		if _, err := d.store.DeleteMailbox(ctx, tenant, acct.Username, mb.Name); err != nil {
			code := "txco_imap_store"
			if err == chimap.ErrINBOX {
				code = "txco_imap_invalid_arg"
			}
			return imapErr(into, code, err.Error()), nil
		}
		mailboxJSON(out, into, mb)
		out.Set(into+".deleted", true)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	case !found:
		var a []string
		if attrsGiven {
			a = attrs
		}
		r := ""
		if role != nil {
			r = *role
		}
		if name == "" {
			return imapErr(into, "txco_imap_no_mailbox", "no mailbox with that id (pass `name` to create one)"), nil
		}
		mb, err = d.store.CreateMailbox(ctx, tenant, acct.Username, name, r, a, policy)
		if err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
		created = true
	default:
		var a []string
		if attrsGiven {
			a = attrs
			if a == nil {
				a = []string{}
			}
		}
		if role != nil || attrsGiven || len(policy) > 0 {
			mb, err = d.store.UpdateMailbox(ctx, mb.ID, role, a, policy)
			if err != nil {
				return imapErr(into, "txco_imap_store", err.Error()), nil
			}
		}
	}
	if to := chimap.NormalizeMailboxName(gjson.GetBytes(meta, "rename_to").String()); to != "" && to != mb.Name {
		mb, err = d.store.RenameMailbox(ctx, tenant, acct.Username, mb.Name, to)
		if err != nil {
			code := "txco_imap_store"
			switch err {
			case chimap.ErrMailboxExists, chimap.ErrINBOX:
				code = "txco_imap_invalid_arg"
			}
			return imapErr(into, code, err.Error()), nil
		}
	}
	if gjson.GetBytes(meta, "reset").Bool() {
		mb, err = d.store.ResetMailbox(ctx, mb.ID)
		if err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
	}
	mailboxJSON(out, into, mb)
	out.Set(into+".created", created)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// imapMessageFor resolves `uid` or `object_key` within a mailbox.
func imapMessageFor(ctx context.Context, d imapDeps, mb chimap.Mailbox, meta []byte, into string) (chimap.Message, event.Payload, bool) {
	var m chimap.Message
	var found bool
	var err error
	switch {
	case gjson.GetBytes(meta, "uid").Exists():
		m, found, err = d.store.GetMessage(ctx, mb.ID, uint32(gjson.GetBytes(meta, "uid").Uint()))
	case gjson.GetBytes(meta, "object_key").String() != "":
		m, found, err = d.store.GetMessageByKey(ctx, mb.ID, gjson.GetBytes(meta, "object_key").String())
	default:
		return chimap.Message{}, imapErr(into, "txco_imap_invalid_arg", "need `uid` or `object_key`"), false
	}
	if err != nil {
		return chimap.Message{}, imapErr(into, "txco_imap_store", err.Error()), false
	}
	if !found {
		return chimap.Message{}, imapErr(into, "txco_imap_no_message", "no such message"), false
	}
	return m, event.Payload{}, true
}

// imapRemove expunges one message. Result: {removed, uid}.
func imapRemove(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	mb, ep, ok := imapMailboxFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	m, ep, ok := imapMessageFor(ctx, d, mb, meta, into)
	if !ok {
		if gjson.Get(ep.Raw, into+".error.code").String() == "txco_imap_no_message" {
			raw, _ := sjson.Set(`{}`, into+".removed", false)
			return event.Payload{Raw: raw, Type: event.JSON}, nil
		}
		return ep, nil
	}
	removed, err := d.store.RemoveMessage(ctx, mb.ID, m.UID)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	raw, _ := sjson.Set(`{}`, into+".removed", removed)
	raw, _ = sjson.Set(raw, into+".uid", m.UID)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// imapFlags adds / removes flags on one message. Result: {uid, flags[]}.
func imapFlags(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	mb, ep, ok := imapMailboxFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	m, ep, ok := imapMessageFor(ctx, d, mb, meta, into)
	if !ok {
		return ep, nil
	}
	add, _ := blobStrings(meta, "add")
	remove, _ := blobStrings(meta, "remove")
	next := make([]string, 0, len(m.Flags)+len(add))
	for _, f := range m.Flags {
		if !chimap.HasFlag(remove, f) {
			next = append(next, f)
		}
	}
	next = append(next, add...)
	flags, err := d.store.SetFlags(ctx, mb.ID, m.UID, next)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	raw, _ := sjson.Set(`{}`, into+".uid", m.UID)
	raw, _ = sjson.Set(raw, into+".flags", flags)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// imapList lists the account's mailboxes (optionally under `prefix`) with
// message / unseen counts. Result: {mailboxes:[…], count}.
func imapList(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	all, err := d.store.ListMailboxes(ctx, tenant, acct.Username)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	prefix := gjson.GetBytes(meta, "prefix").String()
	out := jsonx.NewObject()
	n := 0
	for _, mb := range all {
		if prefix != "" && !strings.HasPrefix(mb.Name, prefix) {
			continue
		}
		total, unseen, err := d.store.CountByMailbox(ctx, mb.ID)
		if err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
		p := fmt.Sprintf("%s.mailboxes.%d", into, n)
		mailboxJSON(out, p, mb)
		out.Set(p+".messages", total)
		out.Set(p+".unseen", unseen)
		n++
	}
	if n == 0 {
		out.SetRaw(into+".mailboxes", "[]")
	}
	out.Set(into+".count", n)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

func messageJSON(b *jsonx.Builder, p string, m chimap.Message) {
	b.Set(p+".uid", m.UID)
	b.Set(p+".object_key", m.ObjectKey)
	b.Set(p+".kind", m.Kind)
	b.Set(p+".sha256", m.SHA256)
	b.Set(p+".size", m.Size)
	b.Set(p+".internaldate", m.InternalDate.UTC().Format(time.RFC3339))
	flags := m.Flags
	if flags == nil {
		flags = []string{}
	}
	b.Set(p+".flags", flags)
	b.Set(p+".subject", m.Subject)
	b.Set(p+".from", m.FromAddr)
	b.SetRaw(p+".parts", rawJSON(m.Parts, "[]"))
}

// imapMessages is the UID-windowed listing. WITH after (uid cursor),
// limit (default 100, max 1000), flags[] (any-of). Result: {items[], next,
// count}.
func imapMessages(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	mb, ep, ok := imapMailboxFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	flags, _ := blobStrings(meta, "flags")
	items, next, err := d.store.ListMessages(ctx, mb.ID, uint32(gjson.GetBytes(meta, "after").Uint()), limit, flags)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	for i, m := range items {
		messageJSON(out, fmt.Sprintf("%s.items.%d", into, i), m)
	}
	if len(items) == 0 {
		out.SetRaw(into+".items", "[]")
	}
	out.Set(into+".count", len(items))
	out.Set(into+".next", next)
	out.Set(into+".mailbox", mb.Name)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// imapGet returns one message: the row facts plus headers/text/html
// (from the record, or parsed from verbatim bytes) and, with raw=true, the
// RFC 5322 bytes base64 (capped by imap-append-max-bytes).
func imapGet(ctx context.Context, d imapDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := imapPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := imapAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	mb, ep, ok := imapMailboxFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	m, ep, ok := imapMessageFor(ctx, d, mb, meta, into)
	if !ok {
		return ep, nil
	}
	out := jsonx.NewObject()
	messageJSON(out, into, m)
	out.Set(into+".mailbox", mb.Name)
	if d.fcas == nil {
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	obj, err := d.fcas.Get(ctx, m.SHA256)
	if err != nil {
		return imapErr(into, "txco_imap_store", "object: "+err.Error()), nil
	}
	wantRaw := gjson.GetBytes(meta, "raw").Bool()
	switch m.Kind {
	case chimap.KindRecord:
		rec, err := chimap.ParseRecord(obj)
		if err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
		out.Set(into+".headers", rec.Headers)
		out.Set(into+".text", rec.Text)
		out.Set(into+".html", rec.HTML)
		if wantRaw {
			rendered, err := chimap.Render(rec, m.SHA256, chimap.RenderOptions{ObjectKey: m.ObjectKey, Domain: domainOfUsername(acct.Username), InternalDate: m.InternalDate})
			if err != nil {
				return imapErr(into, "txco_imap_store", err.Error()), nil
			}
			obj = rendered
		}
	case chimap.KindVerbatim:
		if me, perr := enmime.ReadEnvelope(bytes.NewReader(obj)); perr == nil {
			h := map[string]string{}
			for _, k := range me.GetHeaderKeys() {
				h[strings.ToLower(k)] = me.GetHeader(k)
			}
			out.Set(into+".headers", h)
			out.Set(into+".text", me.Text)
			out.Set(into+".html", me.HTML)
		}
	}
	if wantRaw {
		if d.maxBytes > 0 && int64(len(obj)) > d.maxBytes {
			return imapErr(into, "txco_imap_too_large", fmt.Sprintf("raw message is %d bytes, over imap-append-max-bytes %d", len(obj), d.maxBytes)), nil
		}
		blobChargeBytes(ctx, int64(len(obj)), in)
		out.Set(into+".raw", base64.StdEncoding.EncodeToString(obj))
		out.Set(into+".encoding", "base64")
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// imapAppendVerbatim is the `from` / `from_sha` path of txco://imap/append:
// exact RFC 5322 bytes, from the envelope (base64) or an object the tenant
// already owns in the CAS.
func imapAppendVerbatim(ctx context.Context, d imapDeps, tenant string, meta, in []byte, into string, acct chimap.Account, mb chimap.Mailbox, objectKey string, flags []string, internal time.Time) (event.Payload, error) {
	var raw []byte
	switch {
	case gjson.GetBytes(meta, "from_sha").Exists():
		sha := strings.ToLower(strings.TrimSpace(gjson.GetBytes(meta, "from_sha").String()))
		if !blob.ValidSha256(sha) {
			return imapErr(into, "txco_imap_invalid_arg", "`from_sha` must be 64 lowercase hex chars"), nil
		}
		if d.ix == nil {
			return imapErr(into, "txco_imap_disabled", "no blob index on this node"), nil
		}
		// Ownership, never the CAS: one tenant must not learn what another
		// holds.
		if _, owned, err := d.ix.GetSha(ctx, tenant, sha); err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		} else if !owned {
			return imapErr(into, "txco_imap_no_message", "no object with that sha256 for this tenant"), nil
		}
		b, err := d.fcas.Get(ctx, sha)
		if err != nil {
			return imapErr(into, "txco_imap_store", err.Error()), nil
		}
		raw = b
	default:
		from := normReadFilePath(gjson.GetBytes(meta, "from").String())
		src := gjson.GetBytes(in, from)
		if !src.Exists() || src.Type != gjson.String {
			return imapErr(into, "txco_imap_invalid_arg", fmt.Sprintf("`from` path %q is absent or not a base64 string", from)), nil
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(src.String()))
		if err != nil {
			return imapErr(into, "txco_imap_invalid_arg", "`from`: base64 decode: "+err.Error()), nil
		}
		raw = b
	}
	if d.maxBytes > 0 && int64(len(raw)) > d.maxBytes {
		return imapErr(into, "txco_imap_too_large", fmt.Sprintf("message is %d bytes, over imap-append-max-bytes %d", len(raw), d.maxBytes)), nil
	}
	v, err := imapp.ParseVerbatim(raw, objectKey, internal, flags)
	if err != nil {
		return imapErr(into, "txco_imap_invalid_arg", err.Error()), nil
	}
	blobChargeBytes(ctx, int64(len(raw)), in)
	if err := imapp.StoreVerbatim(ctx, d.fcas, d.ix, tenant, raw, v, imapNow(d)); err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	res, err := d.store.AppendMessage(ctx, mb.ID, v.Message)
	if err != nil {
		return imapErr(into, "txco_imap_store", err.Error()), nil
	}
	out, _ := sjson.Set(`{}`, into+".uid", res.UID)
	out, _ = sjson.Set(out, into+".uidvalidity", res.UIDValidity)
	out, _ = sjson.Set(out, into+".sha256", v.SHA256)
	out, _ = sjson.Set(out, into+".size", len(raw))
	out, _ = sjson.Set(out, into+".kind", chimap.KindVerbatim)
	out, _ = sjson.Set(out, into+".noop", res.Noop)
	out, _ = sjson.Set(out, into+".replaced", res.Replaced)
	out, _ = sjson.Set(out, into+".mailbox", mb.Name)
	return event.Payload{Raw: out, Type: event.JSON}, nil
}
