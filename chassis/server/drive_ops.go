package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"

	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// driveCollection ensures (creates) or removes one collection of the
// pinned tenant. Result at `into`: {id, name, sync_token, bytes_used,
// resource_count, created} or {name, removed}. A remove refuses a
// non-empty collection unless `force`.
func driveCollection(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !chdrive.ValidCollectionName(name) {
		return driveErr(into, "txco_drive_invalid_arg", "`name` must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)"), nil
	}
	out := jsonx.NewObject()
	if gjson.GetBytes(meta, "remove").Bool() {
		removed, err := d.store.DeleteCollection(ctx, tenant, name, gjson.GetBytes(meta, "force").Bool())
		if err != nil {
			return driveStoreErr(into, err), nil
		}
		out.Set(into+".name", name)
		out.Set(into+".removed", removed)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	c, created, err := d.store.EnsureCollection(ctx, tenant, name)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	out.Set(into+".id", c.ID)
	out.Set(into+".name", c.Name)
	out.Set(into+".sync_token", c.SyncToken)
	out.Set(into+".bytes_used", c.BytesUsed)
	out.Set(into+".resource_count", c.ResourceCount)
	out.Set(into+".created", created)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveAccount creates or updates a drive account for the pinned tenant:
// a Basic-auth login (<local>@<owned domain>, the calendar / contacts
// rule, so usernames are globally unique by construction) bound to ONE
// collection (`collection`, by name; the DAV root that login sees).
// Result at `into`: {username, created, collection_id, collection, mount,
// password?, rotated?} — password only when generated (create, explicit
// "", or `rotate`). Pass the password the IMAP account got (`password =
// ._imapacct.password`) and the heads share one credential.
func driveAccount(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	username, domain, err := parseUsername(gjson.GetBytes(meta, "username").String())
	if err != nil {
		return driveErr(into, "txco_drive_invalid_arg", err.Error()), nil
	}
	owned, err := d.domainOwned(ctx, tenant, domain)
	if err != nil {
		return driveErr(into, "txco_drive_store", err.Error()), nil
	}
	if !owned {
		return driveErr(into, "txco_drive_domain_not_owned",
			fmt.Sprintf("domain %q is not a verified hostname or delegated zone of this tenant", domain)), nil
	}
	_, exists, err := d.store.GetAccount(ctx, username)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	// The collection is required on create; on update it rebinds the login.
	var coll chdrive.Collection
	collectionID := ""
	if gjson.GetBytes(meta, "collection").Exists() || !exists {
		coll, ep, ok = driveCollectionFor(ctx, d, tenant, meta, into)
		if !ok {
			return ep, nil
		}
		collectionID = coll.ID
	}
	pwr, pcode, pmsg := resolveAccountPassword(meta, func() (bool, error) { return exists, nil })
	if pcode != "" {
		return driveErr(into, "txco_drive_"+pcode, pmsg), nil
	}
	status := gjson.GetBytes(meta, "status").String()
	created, err := d.store.UpsertAccount(ctx, tenant, username, pwr.hash, status, collectionID)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	acct, _, err := d.store.GetAccount(ctx, username)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	if coll.ID == "" {
		if c, found, gerr := d.store.GetCollectionByID(ctx, acct.CollectionID); gerr == nil && found {
			coll = c
		}
	}
	out := jsonx.NewObject()
	out.Set(into+".username", username)
	out.Set(into+".created", created)
	out.Set(into+".collection_id", acct.CollectionID)
	out.Set(into+".collection", coll.Name)
	out.Set(into+".mount", strings.TrimSuffix(d.prefix, "/")+"/")
	if pwr.generated != "" {
		out.Set(into+".password", pwr.generated)
	}
	if pwr.rotated {
		out.Set(into+".rotated", true)
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveAddress resolves `path` XOR `resource_id` for get/stat/delete.
func driveAddress(meta []byte, into string) (path, id string, errPayload event.Payload, ok bool) {
	path = gjson.GetBytes(meta, "path").String()
	id = strings.TrimSpace(gjson.GetBytes(meta, "resource_id").String())
	switch {
	case gjson.GetBytes(meta, "path").Exists() && id != "":
		return "", "", driveErr(into, "txco_drive_invalid_arg", "give `path` or `resource_id`, not both"), false
	case !gjson.GetBytes(meta, "path").Exists() && id == "":
		return "", "", driveErr(into, "txco_drive_invalid_arg", "need `path` or `resource_id`"), false
	}
	return path, id, event.Payload{}, true
}

// driveStatFor is Stat by path or id.
func driveStatFor(ctx context.Context, d driveDeps, collID, path, id string) (chdrive.Resource, bool, error) {
	if id != "" {
		return d.store.StatByID(ctx, collID, id)
	}
	return d.store.Stat(ctx, collID, path)
}

// drivePut stores bytes at `path` in `collection`. Bytes come from `from`
// (an envelope path) XOR `value` (a literal), decoded per `encoding`
// (base64 default | utf8), capped by --drive-op-max-bytes. `content_type`,
// `if_match`, `if_none_match` as the store's PutOpts. Result at `into`:
// {resource_id, path, etag, size, created, noop, modseq}.
func drivePut(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path := gjson.GetBytes(meta, "path").String()
	if strings.TrimSpace(path) == "" {
		return driveErr(into, "txco_drive_invalid_arg", "missing `path`"), nil
	}
	data, err := blobBytes(meta, in)
	if err != nil {
		return driveErr(into, "txco_drive_invalid_arg", "drive/put: "+err.Error()), nil
	}
	size := int64(len(data))
	if d.maxBytes > 0 && size > d.maxBytes {
		return driveErr(into, "txco_drive_too_large",
			fmt.Sprintf("%d bytes exceeds drive-op-max-bytes %d", size, d.maxBytes)), nil
	}
	driveChargeBytes(ctx, size, in)
	res, err := d.store.Put(ctx, coll.ID, path, bytes.NewReader(data), size, chdrive.PutOpts{
		IfMatch:     strings.Trim(gjson.GetBytes(meta, "if_match").String(), `"`),
		IfNoneMatch: strings.Trim(gjson.GetBytes(meta, "if_none_match").String(), `"`),
		ContentType: strings.TrimSpace(gjson.GetBytes(meta, "content_type").String()),
	})
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	norm, _ := chdrive.NormalizePath(path)
	out := jsonx.NewObject()
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", norm)
	out.Set(into+".etag", res.ETag)
	out.Set(into+".size", res.Size)
	out.Set(into+".created", res.Created)
	out.Set(into+".noop", res.Noop)
	out.Set(into+".modseq", res.ModSeq)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveGet reads a file's bytes into the envelope, by `path` or
// `resource_id`, per `encoding` (base64 default | utf8 | auto), capped by
// --drive-op-max-bytes and a lower `max_bytes`. Result at `into`:
// {resource_id, path, etag, size, content_type, modseq, content, encoding}.
func driveGet(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path, id, ep, ok := driveAddress(meta, into)
	if !ok {
		return ep, nil
	}
	enc := gjson.GetBytes(meta, "encoding").String()
	switch enc {
	case "":
		enc = "base64"
	case "base64", "utf8", "auto":
	default:
		return driveErr(into, "txco_drive_invalid_arg", "encoding must be base64, utf8 or auto"), nil
	}
	res, found, err := driveStatFor(ctx, d, coll.ID, path, id)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	if !found {
		return driveErr(into, "txco_drive_not_found", "no such resource"), nil
	}
	if res.IsDir() {
		return driveErr(into, "txco_drive_is_directory", "resource is a directory; use drive/list"), nil
	}
	limit := d.maxBytes
	if mb := gjson.GetBytes(meta, "max_bytes").Int(); mb > 0 && (limit == 0 || mb < limit) {
		limit = mb
	}
	if limit > 0 && res.Size > limit {
		return driveErr(into, "txco_drive_too_large",
			fmt.Sprintf("resource is %d bytes, over the %d-byte read cap", res.Size, limit)), nil
	}
	rc, res, err := d.store.OpenByID(ctx, coll.ID, res.ResourceID)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	var data []byte
	if limit > 0 {
		data, err = io.ReadAll(io.LimitReader(rc, limit+1))
	} else {
		data, err = io.ReadAll(rc)
	}
	_ = rc.Close()
	if err != nil {
		return driveErr(into, "txco_drive_store", err.Error()), nil
	}
	if limit > 0 && int64(len(data)) > limit {
		return driveErr(into, "txco_drive_too_large",
			fmt.Sprintf("resource exceeds the %d-byte read cap", limit)), nil
	}
	driveChargeBytes(ctx, int64(len(data)), in)
	content, encOut := encodeReadFile(data, enc)

	out := jsonx.NewObject()
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", res.Path)
	out.Set(into+".name", chdrive.BaseOf(res.Path))
	out.Set(into+".etag", res.ETag)
	out.Set(into+".size", int64(len(data)))
	out.Set(into+".content_type", res.ContentType)
	out.Set(into+".modseq", res.ModSeq)
	out.Set(into+".content", content)
	out.Set(into+".encoding", encOut)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveStat reports a resource's metadata by `path` or `resource_id`
// without moving bytes; a miss is a result ({exists:false}), not an error.
// The root path ("" or "/") is the collection itself.
func driveStat(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path, id, ep, ok := driveAddress(meta, into)
	if !ok {
		return ep, nil
	}
	res, found, err := driveStatFor(ctx, d, coll.ID, path, id)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".exists", found)
	if found {
		raw, _ := json.Marshal(driveResourceJSON(res))
		out.SetRaw(into+".resource", string(raw))
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveList lists a directory (`path`, "" = root; `recursive` for the
// whole subtree) or, with `since` = a sync token, every resource changed
// after it (`include_deleted` adds tombstones so a consumer sees removals).
// Pages of `limit` (≤ 1000, default 200) by path, `after` = the previous
// page's `next`. Result at `into`: {items[], count, next, sync_token}.
func driveList(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 {
		limit = driveListDefault
	}
	if limit > driveListMax {
		limit = driveListMax
	}
	opts := chdrive.ListOpts{
		Path:           gjson.GetBytes(meta, "path").String(),
		Recursive:      gjson.GetBytes(meta, "recursive").Bool(),
		SinceModSeq:    gjson.GetBytes(meta, "since").Int(),
		IncludeDeleted: gjson.GetBytes(meta, "include_deleted").Bool(),
		Limit:          limit + 1,
		After:          gjson.GetBytes(meta, "after").String(),
	}
	if opts.SinceModSeq > 0 && !gjson.GetBytes(meta, "recursive").Exists() {
		opts.Recursive = true // a change feed is the whole subtree by default
	}
	if opts.Path != "" && !opts.Recursive {
		dir, found, err := d.store.Stat(ctx, coll.ID, opts.Path)
		if err != nil {
			return driveStoreErr(into, err), nil
		}
		if !found {
			return driveErr(into, "txco_drive_not_found", "no such directory"), nil
		}
		if !dir.IsDir() {
			return driveErr(into, "txco_drive_not_directory", "path is a file"), nil
		}
	}
	rows, err := d.store.List(ctx, coll.ID, opts)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].Path
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, driveResourceJSON(r))
	}
	raw, _ := json.Marshal(items)
	out := jsonx.NewObject()
	out.SetRaw(into+".items", string(raw))
	out.Set(into+".count", len(items))
	out.Set(into+".next", next)
	out.Set(into+".sync_token", coll.SyncToken)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveDelete tombstones a resource by `path` or `resource_id` (a
// directory with everything below it); `if_match` guards on the etag. A
// miss is {deleted:false}. Result at `into`: {deleted, resource_id?, path?,
// modseq?}.
func driveDelete(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path, id, ep, ok := driveAddress(meta, into)
	if !ok {
		return ep, nil
	}
	opts := chdrive.DeleteOpts{IfMatch: strings.Trim(gjson.GetBytes(meta, "if_match").String(), `"`)}
	var res chdrive.Resource
	var err error
	if id != "" {
		res, err = d.store.DeleteByID(ctx, coll.ID, id, opts)
	} else {
		res, err = d.store.Delete(ctx, coll.ID, path, opts)
	}
	out := jsonx.NewObject()
	if errors.Is(err, chdrive.ErrNotFound) {
		out.Set(into+".deleted", false)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	out.Set(into+".deleted", true)
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", res.Path)
	out.Set(into+".kind", res.Kind)
	out.Set(into+".modseq", res.ModSeq)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveMkdir creates a directory at `path`; an existing directory there
// is {created:false}, an existing file txco_drive_exists. Result at
// `into`: {resource_id, path, created, modseq}.
func driveMkdir(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path := gjson.GetBytes(meta, "path").String()
	res, err := d.store.Mkdir(ctx, coll.ID, path)
	created := true
	if errors.Is(err, chdrive.ErrExists) {
		cur, found, serr := d.store.Stat(ctx, coll.ID, path)
		if serr != nil {
			return driveStoreErr(into, serr), nil
		}
		if !found || !cur.IsDir() {
			return driveStoreErr(into, err), nil
		}
		res, created, err = cur, false, nil
	}
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", res.Path)
	out.Set(into+".created", created)
	out.Set(into+".modseq", res.ModSeq)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// driveMove renames the resource at `path` to `to` (same collection),
// keeping its resource id; `overwrite` replaces whatever is at `to`.
// Result at `into`: {resource_id, path, from, kind, modseq}.
func driveMove(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	return driveMoveCopy(ctx, d, in, "move")
}

// driveCopy duplicates the resource at `path` to `to` (same collection)
// under a NEW resource id; `overwrite` as move. Result at `into`:
// {resource_id, path, from, kind, etag, modseq}.
func driveCopy(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	return driveMoveCopy(ctx, d, in, "copy")
}

func driveMoveCopy(ctx context.Context, d driveDeps, in []byte, verb string) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	from := gjson.GetBytes(meta, "path").String()
	to := gjson.GetBytes(meta, "to").String()
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return driveErr(into, "txco_drive_invalid_arg", "need `path` and `to`"), nil
	}
	overwrite := gjson.GetBytes(meta, "overwrite").Bool()
	var res chdrive.Resource
	var err error
	if verb == "move" {
		res, err = d.store.Move(ctx, coll.ID, from, to, overwrite)
	} else {
		res, err = d.store.Copy(ctx, coll.ID, from, to, overwrite)
	}
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	normFrom, _ := chdrive.NormalizePath(from)
	out := jsonx.NewObject()
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", res.Path)
	out.Set(into+".from", normFrom)
	out.Set(into+".kind", res.Kind)
	out.Set(into+".etag", res.ETag)
	out.Set(into+".modseq", res.ModSeq)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}
