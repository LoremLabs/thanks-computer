package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// contactsAccount creates or updates a contacts account for the pinned
// tenant. Result at `into`: {username, created, principal, password?,
// rotated?} — password only when generated (create, explicit "", or
// `rotate`); rotated only when an existing account's password was
// regenerated. Pass the password the IMAP account got (`password =
// ._imapacct.password`) and the heads share one credential.
func contactsAccount(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	username, domain, err := parseUsername(gjson.GetBytes(meta, "username").String())
	if err != nil {
		return contactsErr(into, "txco_contacts_invalid_arg", err.Error()), nil
	}
	owned, err := d.domainOwned(ctx, tenant, domain)
	if err != nil {
		return contactsErr(into, "txco_contacts_store", err.Error()), nil
	}
	if !owned {
		return contactsErr(into, "txco_contacts_domain_not_owned",
			fmt.Sprintf("domain %q is not a verified hostname or delegated zone of this tenant", domain)), nil
	}
	pwr, pcode, pmsg := resolveAccountPassword(meta, func() (bool, error) {
		_, exists, gerr := d.store.GetAccount(ctx, username)
		return exists, gerr
	})
	if pcode != "" {
		return contactsErr(into, "txco_contacts_"+pcode, pmsg), nil
	}
	status := gjson.GetBytes(meta, "status").String()
	var policy json.RawMessage
	if p := gjson.GetBytes(meta, "policy"); p.Exists() {
		if !p.IsObject() {
			return contactsErr(into, "txco_contacts_invalid_arg", "`policy` must be an object of verb → deny|local|observe|stack"), nil
		}
		if err := chcon.ValidatePolicy(json.RawMessage(p.Raw)); err != nil {
			return contactsErr(into, "txco_contacts_invalid_arg", err.Error()), nil
		}
		policy = json.RawMessage(p.Raw)
	}
	created, err := d.store.UpsertAccount(ctx, tenant, username, pwr.hash, status, policy)
	if err != nil {
		code := "txco_contacts_store"
		if errors.Is(err, chcon.ErrUsernameTaken) {
			code = "txco_contacts_username_taken"
		}
		return contactsErr(into, code, err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".username", username)
	out.Set(into+".created", created)
	out.Set(into+".principal", contactsPrincipalPath(d.prefix, username))
	if pwr.generated != "" {
		out.Set(into+".password", pwr.generated)
	}
	if pwr.rotated {
		out.Set(into+".rotated", true)
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// contactsAddressbook ensures (creates or updates) or removes one address
// book. Result: {id, name, path, display_name, description, sort_order,
// policy, sync_token, created} or {name, removed}.
func contactsAddressbook(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !chcon.ValidAddressbookName(name) {
		return contactsErr(into, "txco_contacts_invalid_arg", "`name` must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)"), nil
	}
	if gjson.GetBytes(meta, "remove").Bool() {
		ab, found, err := d.store.GetAddressbook(ctx, tenant, acct.Username, name)
		if err != nil {
			return contactsErr(into, "txco_contacts_store", err.Error()), nil
		}
		removed := false
		if found {
			if removed, err = d.store.RemoveAddressbook(ctx, ab.ID); err != nil {
				return contactsErr(into, "txco_contacts_store", err.Error()), nil
			}
		}
		out := jsonx.NewObject()
		out.Set(into+".name", name)
		out.Set(into+".removed", removed)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	ab := chcon.Addressbook{Tenant: tenant, Username: acct.Username, Name: name,
		DisplayName: gjson.GetBytes(meta, "display_name").String(),
		Description: gjson.GetBytes(meta, "description").String(),
		SortOrder:   int(gjson.GetBytes(meta, "sort_order").Int()),
	}
	if p := gjson.GetBytes(meta, "policy"); p.Exists() {
		if !p.IsObject() {
			return contactsErr(into, "txco_contacts_invalid_arg", "`policy` must be an object of verb → deny|local|observe|stack"), nil
		}
		if err := chcon.ValidatePolicy(json.RawMessage(p.Raw)); err != nil {
			return contactsErr(into, "txco_contacts_invalid_arg", err.Error()), nil
		}
		ab.Policy = json.RawMessage(p.Raw)
	}
	got, created, err := d.store.EnsureAddressbook(ctx, ab)
	if err != nil {
		return contactsErr(into, "txco_contacts_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	addressbookJSON(out, into, d.prefix, got)
	out.Set(into+".created", created)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// prepareContact turns one put entry ({card{} | vcard, uid?, name?}) into a
// storable object: from `card{}` the chassis renders vCard 3.0 (uid derived
// from name when absent); from `vcard` the text is kept as written and the
// UID is the bytes' own. code "" on success, else the error code suffix.
func prepareContact(d contactsDeps, username string, entry gjson.Result, now time.Time) (chcon.Object, string, string) {
	vcW := entry.Get("vcard")
	cardW := entry.Get("card")
	if vcW.Exists() == cardW.Exists() {
		return chcon.Object{}, "invalid_arg", "give `card{...}` (the chassis renders it) or `vcard` (text), not both"
	}
	uidW := strings.TrimSpace(entry.Get("uid").String())
	nameW := strings.TrimSpace(entry.Get("name").String())
	if nameW != "" && !chcon.ValidObjectName(nameW) {
		return chcon.Object{}, "invalid_arg", "`name` must be a URL segment ([A-Za-z0-9._~-], up to 255 chars)"
	}
	var o chcon.Object
	var err error
	if cardW.Exists() {
		if !cardW.IsObject() {
			return chcon.Object{}, "invalid_arg", "`card` must be an object"
		}
		c, perr := chcon.CardFromJSON([]byte(cardW.Raw))
		if perr != nil {
			return chcon.Object{}, "invalid_arg", perr.Error()
		}
		if c.UID == "" {
			c.UID = uidW
		}
		if uidW != "" && uidW != c.UID {
			return chcon.Object{}, "invalid_arg", "`uid` disagrees with card.uid"
		}
		o, err = chcon.ObjectFromCard(username, nameW, c, now)
	} else {
		raw := []byte(vcW.String())
		if d.maxBytes > 0 && int64(len(raw)) > d.maxBytes {
			return chcon.Object{}, "too_large", fmt.Sprintf("object is %d bytes, over contacts-object-max-bytes %d", len(raw), d.maxBytes)
		}
		o, err = chcon.ObjectFromVCard(nameW, raw)
		if err == nil && uidW != "" && o.UID != uidW {
			return chcon.Object{}, "invalid_arg", "`uid` disagrees with the UID in `vcard`"
		}
	}
	if err != nil {
		if errors.Is(err, chcon.ErrInvalidCard) {
			return chcon.Object{}, "invalid_arg", strings.TrimPrefix(err.Error(), chcon.ErrInvalidCard.Error()+"\n")
		}
		return chcon.Object{}, "store", err.Error()
	}
	if d.maxBytes > 0 && o.Size > d.maxBytes {
		return chcon.Object{}, "too_large", fmt.Sprintf("object is %d bytes, over contacts-object-max-bytes %d", o.Size, d.maxBytes)
	}
	return o, "", ""
}

func putResultJSON(b *jsonx.Builder, p, prefix, username, abName string, r chcon.PutResult) {
	b.Set(p+".name", r.Name)
	b.Set(p+".path", contactsObjectPath(prefix, username, abName, r.Name))
	b.Set(p+".uid", r.UID)
	b.Set(p+".etag", r.ETag)
	b.Set(p+".created", r.Created)
	b.Set(p+".noop", r.Noop)
	b.Set(p+".modseq", r.ModSeq)
}

func contactsStoreErr(into string, err error) event.Payload {
	switch {
	case errors.Is(err, chcon.ErrInvalidCard):
		return contactsErr(into, "txco_contacts_invalid_arg", strings.TrimPrefix(err.Error(), chcon.ErrInvalidCard.Error()+"\n"))
	case errors.Is(err, chcon.ErrUIDConflict):
		return contactsErr(into, "txco_contacts_conflict", err.Error())
	case errors.Is(err, chcon.ErrPrecondition):
		return contactsErr(into, "txco_contacts_conflict", err.Error())
	case errors.Is(err, chcon.ErrNotFound):
		return contactsErr(into, "txco_contacts_no_addressbook", err.Error())
	}
	return contactsErr(into, "txco_contacts_store", err.Error())
}

// contactsPut materializes one object, addressed by UID. Either `card{}`
// (rendered by the chassis; `uid` optional, else derived from `name`) or
// `vcard` (text, kept as written; the UID is the bytes' own). `name` is the
// resource name on create; on update the object keeps the name it has.
// Same content (REV/PRODID aside) ⇒ noop. Result: {name, path, uid, etag,
// created, noop, modseq}.
func contactsPut(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ab, ep, ok := addressbookFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	o, code, msg := prepareContact(d, acct.Username, gjson.ParseBytes(meta), contactsNow(d))
	if code != "" {
		return contactsErr(into, "txco_contacts_"+code, msg), nil
	}
	blobChargeBytes(ctx, o.Size, in)
	res, err := d.store.PutObject(ctx, ab.ID, o, chcon.PutOpts{ByUID: true})
	if err != nil {
		return contactsStoreErr(into, err), nil
	}
	out := jsonx.NewObject()
	putResultJSON(out, into, d.prefix, acct.Username, ab.Name, res)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// contactsSync applies `put` (a list of put entries, each addressed by UID)
// and `delete` (a list of uids) in one transaction: the bounded write a
// stack without loops needs to make an address book match a list. Any
// invalid entry refuses the whole batch and names it (`index`, `op`).
// Result: {created, updated, noop, deleted, missing, count, items[]}.
func contactsSync(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ab, ep, ok := addressbookFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	putW := gjson.GetBytes(meta, "put")
	delW := gjson.GetBytes(meta, "delete")
	if (putW.Exists() && !putW.IsArray()) || (delW.Exists() && !delW.IsArray()) {
		return contactsErr(into, "txco_contacts_invalid_arg", "`put` must be a list of {card{}|vcard, uid?, name?} and `delete` a list of uids"), nil
	}
	puts := putW.Array()
	dels := delW.Array()
	if len(puts) > syncMaxEntries || len(dels) > syncMaxEntries {
		return contactsErr(into, "txco_contacts_invalid_arg", fmt.Sprintf("at most %d entries per list", syncMaxEntries)), nil
	}
	now := contactsNow(d)
	objs := make([]chcon.Object, 0, len(puts))
	var total int64
	for i, e := range puts {
		if !e.IsObject() {
			return contactsErr(into, "txco_contacts_invalid_arg", fmt.Sprintf("put[%d] must be an object", i)), nil
		}
		o, code, msg := prepareContact(d, acct.Username, e, now)
		if code != "" {
			return contactsErr(into, "txco_contacts_"+code, fmt.Sprintf("put[%d]: %s", i, msg)), nil
		}
		total += o.Size
		objs = append(objs, o)
	}
	uids := make([]string, 0, len(dels))
	for i, e := range dels {
		u := strings.TrimSpace(e.String())
		if u == "" || e.Type != gjson.String {
			return contactsErr(into, "txco_contacts_invalid_arg", fmt.Sprintf("delete[%d] must be a uid", i)), nil
		}
		uids = append(uids, u)
	}
	if total > 0 {
		blobChargeBytes(ctx, total, in)
	}
	res, err := d.store.Batch(ctx, ab.ID, objs, uids)
	if err != nil {
		var be *chcon.BatchError
		if errors.As(err, &be) {
			ep := contactsStoreErr(into, be.Err)
			raw, _ := sjson.Set(ep.Raw, into+".error.op", be.Op)
			raw, _ = sjson.Set(raw, into+".error.index", be.Index)
			return event.Payload{Raw: raw, Type: event.JSON}, nil
		}
		return contactsStoreErr(into, err), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".created", res.Created)
	out.Set(into+".updated", res.Updated)
	out.Set(into+".noop", res.Noop)
	out.Set(into+".deleted", res.Deleted)
	out.Set(into+".missing", res.Missing)
	out.Set(into+".count", len(objs)+len(uids))
	for i, r := range res.Puts {
		putResultJSON(out, fmt.Sprintf("%s.items.%d", into, i), d.prefix, acct.Username, ab.Name, r)
	}
	if len(res.Puts) == 0 {
		out.SetRaw(into+".items", "[]")
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// contactsObjectFor resolves `uid` or `name` to a live object.
func contactsObjectFor(ctx context.Context, d contactsDeps, ab chcon.Addressbook, meta []byte, into string) (chcon.Object, event.Payload, bool) {
	uid := strings.TrimSpace(gjson.GetBytes(meta, "uid").String())
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	var o chcon.Object
	var found bool
	var err error
	switch {
	case uid != "":
		o, found, err = d.store.GetObjectByUID(ctx, ab.ID, uid)
	case name != "":
		o, found, err = d.store.GetObject(ctx, ab.ID, name)
	default:
		return chcon.Object{}, contactsErr(into, "txco_contacts_invalid_arg", "give `uid` or `name`"), false
	}
	if err != nil {
		return chcon.Object{}, contactsErr(into, "txco_contacts_store", err.Error()), false
	}
	if !found {
		return chcon.Object{}, contactsErr(into, "txco_contacts_no_object", "no such object in "+ab.Name), false
	}
	return o, event.Payload{}, true
}

// contactsGet returns one object: its row facts, bytes and parsed card.
func contactsGet(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ab, ep, ok := addressbookFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	o, ep, ok := contactsObjectFor(ctx, d, ab, meta, into)
	if !ok {
		return ep, nil
	}
	out := jsonx.NewObject()
	contactJSON(out, into, d.prefix, acct.Username, ab.Name, o)
	out.Set(into+".vcard", string(o.VCard))
	if c, err := chcon.Parse(o.VCard); err == nil {
		if raw, err := json.Marshal(c); err == nil {
			out.SetRaw(into+".card", string(raw))
		}
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// contactsList lists the account's address books, or — with `addressbook`
// — one book's objects (facts incl. addresses, never the bytes) after a
// modseq cursor (`after`, `limit` ≤ 1000).
func contactsList(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	out := jsonx.NewObject()
	if !gjson.GetBytes(meta, "addressbook").Exists() {
		books, err := d.store.ListAddressbooks(ctx, tenant, acct.Username)
		if err != nil {
			return contactsErr(into, "txco_contacts_store", err.Error()), nil
		}
		for i, ab := range books {
			p := fmt.Sprintf("%s.addressbooks.%d", into, i)
			addressbookJSON(out, p, d.prefix, ab)
			objs, err := d.store.ListObjects(ctx, ab.ID, chcon.ListOpts{})
			if err != nil {
				return contactsErr(into, "txco_contacts_store", err.Error()), nil
			}
			out.Set(p+".objects", len(objs))
		}
		if len(books) == 0 {
			out.SetRaw(into+".addressbooks", "[]")
		}
		out.Set(into+".count", len(books))
		out.Set(into+".home", contactsHomePath(d.prefix, acct.Username))
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	ab, ep, ok := addressbookFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	objs, err := d.store.ListObjects(ctx, ab.ID, chcon.ListOpts{SinceModSeq: gjson.GetBytes(meta, "after").Int()})
	if err != nil {
		return contactsErr(into, "txco_contacts_store", err.Error()), nil
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].ModSeq < objs[j].ModSeq })
	next := int64(0)
	if len(objs) > limit {
		objs = objs[:limit]
		next = objs[len(objs)-1].ModSeq
	}
	for i, o := range objs {
		contactJSON(out, fmt.Sprintf("%s.items.%d", into, i), d.prefix, acct.Username, ab.Name, o)
	}
	if len(objs) == 0 {
		out.SetRaw(into+".items", "[]")
	}
	out.Set(into+".count", len(objs))
	out.Set(into+".next", next)
	out.Set(into+".sync_token", ab.SyncToken)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// contactsDelete tombstones one object by `uid` or `name`. Result:
// {deleted, name, uid}; an absent object is `deleted: false`, not an error.
func contactsDelete(ctx context.Context, d contactsDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := contactsPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := contactsAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ab, ep, ok := addressbookFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	uid := strings.TrimSpace(gjson.GetBytes(meta, "uid").String())
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if uid == "" && name == "" {
		return contactsErr(into, "txco_contacts_invalid_arg", "give `uid` or `name`"), nil
	}
	if name == "" {
		o, found, err := d.store.GetObjectByUID(ctx, ab.ID, uid)
		if err != nil {
			return contactsErr(into, "txco_contacts_store", err.Error()), nil
		}
		if !found {
			out := jsonx.NewObject()
			out.Set(into+".deleted", false)
			out.Set(into+".uid", uid)
			return event.Payload{Raw: out.String(), Type: event.JSON}, nil
		}
		name, uid = o.Name, o.UID
	}
	_, found, err := d.store.DeleteObject(ctx, ab.ID, name)
	if err != nil {
		return contactsErr(into, "txco_contacts_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".deleted", found)
	out.Set(into+".name", name)
	if uid != "" {
		out.Set(into+".uid", uid)
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}
