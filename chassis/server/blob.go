package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// blob.go holds the handler bodies for the runtime blob store ops
// (txco://blob/{put,get,stat,list,delete}): a mutable NAME layer with
// permissions over the content-addressed filecas. Bytes are immutable and
// global by hash in the CAS; names are per-tenant pointers in the blob index
// (chassis/blob over the KV store); the tenant's sha OWNERSHIP rows are the
// fence that keeps the global CAS from being a cross-tenant existence oracle.
//
// Scoping is trusted: tenant from processor.TenantScope(ctx). Errors surface
// ROOT-level as `blob.error.{code,message}` with a nil Go error (the vector /
// dataset shape) so authors branch with `WHEN @blob.error ...`; the kv shape
// (Go error) would drop the output before it reached the envelope.
//
// Addressing: put takes `name`, or `under` + `filename` (the derived name
// <under>/<sha256(NFC(filename))> — total for any real filename, and stable
// so a re-upload REPLACES), or nothing (a nameless, content-only put). get /
// stat take `name` XOR `sha256`; by-sha needs the privileged blob:cas:read.
// Permissions: the op's declared context — `audience` (⇒ blob:<a>:read) and
// `grants` — must cover every permission a name requires, for any access.

// blobDeps is what the handlers need from the boot wiring.
type blobDeps struct {
	fcas     filecas.Store // nil ⇒ every op answers txco_blob_disabled
	ix       blob.Index
	maxBytes int64 // decoded-byte cap for put and get; 0 = unlimited
	now      func() time.Time
}

func blobErr(code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, "blob.error.code", code)
	raw, _ = sjson.Set(raw, "blob.error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func blobStoreErr(err error) event.Payload { return blobErr("txco_blob_store", err.Error()) }

func blobInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_blob"
	}
	return into
}

// blobStrings reads a WITH param that may be a JSON array of strings or one
// comma-separated string (the same shape `omit` accepts). present reports
// whether the param was given at all (so an omitted `permissions` can mean
// "keep the prior ones" while an explicit [] clears them).
func blobStrings(meta []byte, key string) (vals []string, present bool) {
	v := gjson.GetBytes(meta, key)
	if !v.Exists() {
		return nil, false
	}
	if v.IsArray() {
		for _, e := range v.Array() {
			if s := strings.TrimSpace(e.String()); s != "" {
				vals = append(vals, s)
			}
		}
		return vals, true
	}
	for _, s := range strings.Split(v.String(), ",") {
		if s = strings.TrimSpace(s); s != "" {
			vals = append(vals, s)
		}
	}
	return vals, true
}

// blobSubject builds the permission set the op holds from WITH `audience` +
// `grants`.
func blobSubject(meta []byte) ([]string, error) {
	grants, _ := blobStrings(meta, "grants")
	return blob.SubjectGrants(gjson.GetBytes(meta, "audience").String(), grants)
}

// blobStage labels fuel charges with the op identity the processor stamped.
func blobStage(in []byte) string { return gjson.GetBytes(in, "_txc.op").String() }

func blobChargeBytes(ctx context.Context, n int64, in []byte) {
	mib := (n + (1 << 20) - 1) >> 20
	if mib > 0 {
		_ = processor.AddFuel(ctx, mib*processor.FuelCostBlobPerMiB, blobStage(in))
	}
}

// blobBytes resolves the bytes to store from `from` (envelope path) XOR
// `value` (literal), decoded per `encoding` (base64 default | utf8).
func blobBytes(meta, in []byte) ([]byte, error) {
	var src gjson.Result
	switch {
	case gjson.GetBytes(meta, "from").Exists():
		from := normReadFilePath(gjson.GetBytes(meta, "from").String())
		src = gjson.GetBytes(in, from)
		if !src.Exists() {
			return nil, fmt.Errorf("source path %q is absent", from)
		}
	case gjson.GetBytes(meta, "value").Exists():
		src = gjson.GetBytes(meta, "value")
	default:
		return nil, errors.New("need `from` or `value`")
	}
	enc := gjson.GetBytes(meta, "encoding").String()
	switch enc {
	case "", "base64":
		if src.Type != gjson.String {
			return nil, errors.New("base64 content must be a string (use encoding=\"utf8\" to store JSON as text)")
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(src.String()))
		if err != nil {
			return nil, fmt.Errorf("base64 decode: %w", err)
		}
		return data, nil
	case "utf8":
		if src.Type == gjson.String {
			return []byte(src.String()), nil
		}
		return []byte(src.Raw), nil // non-string JSON stored as its text
	default:
		return nil, fmt.Errorf("encoding %q must be base64 or utf8", enc)
	}
}

// blobAddress resolves `name` XOR `sha256` for get/stat.
func blobAddress(meta []byte) (name, sha string, errPayload event.Payload, ok bool) {
	name = gjson.GetBytes(meta, "name").String()
	sha = gjson.GetBytes(meta, "sha256").String()
	switch {
	case name != "" && sha != "":
		return "", "", blobErr("txco_blob_invalid_arg", "give `name` or `sha256`, not both"), false
	case name == "" && sha == "":
		return "", "", blobErr("txco_blob_invalid_arg", "need `name` or `sha256`"), false
	case name != "":
		if err := blob.ValidName(name); err != nil {
			return "", "", blobErr("txco_blob_invalid_name", err.Error()), false
		}
	default:
		if !blob.ValidSha256(sha) {
			return "", "", blobErr("txco_blob_invalid_arg", "sha256 must be 64 lowercase hex chars"), false
		}
	}
	return name, sha, event.Payload{}, true
}

// blobPrelude is the common head of every handler: tenant, store, subject.
func blobPrelude(ctx context.Context, d blobDeps) (tenant string, meta []byte, grants []string, errPayload event.Payload, ok bool) {
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, nil, blobErr("txco_blob_no_tenant", "no tenant in request scope"), false
	}
	if d.fcas == nil || d.ix == nil {
		return "", nil, nil, blobErr("txco_blob_disabled", "no blob store on this node (filecas not configured)"), false
	}
	meta = []byte(operation.MetaFromContext(ctx))
	grants, err := blobSubject(meta)
	if err != nil {
		return "", nil, nil, blobErr("txco_blob_invalid_arg", err.Error()), false
	}
	return tenant, meta, grants, event.Payload{}, true
}

func blobNow(d blobDeps) time.Time {
	if d.now == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return d.now().UTC().Truncate(time.Second)
}

// blobPut stores bytes (CAS) and, when addressed, points a name at them.
// Result at `into`: {name?, sha256, size, existed, replaced?}.
func blobPut(ctx context.Context, d blobDeps, in []byte) (event.Payload, error) {
	tenant, meta, grants, ep, ok := blobPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	// Address.
	name := gjson.GetBytes(meta, "name").String()
	under := gjson.GetBytes(meta, "under").String()
	filename := gjson.GetBytes(meta, "filename").String()
	switch {
	case name != "" && under != "":
		return blobErr("txco_blob_invalid_arg", "give `name` or `under`+`filename`, not both"), nil
	case name != "":
		if err := blob.ValidName(name); err != nil {
			return blobErr("txco_blob_invalid_name", err.Error()), nil
		}
	case under != "":
		if filename == "" {
			return blobErr("txco_blob_invalid_arg", "`under` needs a `filename` to derive the name from"), nil
		}
		derived, err := blob.DerivedName(under, filename)
		if err != nil {
			return blobErr("txco_blob_invalid_name", err.Error()), nil
		}
		name = derived
	}
	if filename != "" {
		if err := blob.ValidFilename(filename); err != nil {
			return blobErr("txco_blob_invalid_arg", err.Error()), nil
		}
	}
	// Declared permissions: well-formed, and held by the declarer.
	perms, permsGiven := blobStrings(meta, "permissions")
	for _, p := range perms {
		if err := blob.ValidRequirement(p); err != nil {
			return blobErr("txco_blob_invalid_arg", err.Error()), nil
		}
	}
	if !blob.Allowed(grants, perms) {
		return blobErr("txco_blob_denied", "op declares permissions it does not hold"), nil
	}
	// Repointing an existing name needs the access it requires.
	var prior blob.NameRow
	var hasPrior bool
	if name != "" {
		var err error
		prior, hasPrior, err = d.ix.GetName(ctx, tenant, name)
		if err != nil {
			return blobStoreErr(err), nil
		}
		if hasPrior && !blob.Allowed(grants, prior.Permissions) {
			return blobErr("txco_blob_denied", "name requires permissions this op does not hold"), nil
		}
	}
	// Bytes — nothing moved before the checks above.
	data, err := blobBytes(meta, in)
	if err != nil {
		return blobErr("txco_blob_invalid_arg", "blob/put: "+err.Error()), nil
	}
	size := int64(len(data))
	if d.maxBytes > 0 && size > d.maxBytes {
		return blobErr("txco_blob_too_large",
			fmt.Sprintf("%d bytes exceeds blob-max-bytes %d", size, d.maxBytes)), nil
	}
	blobChargeBytes(ctx, size, in)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	present, err := d.fcas.Exists(ctx, sha)
	if err != nil {
		return blobStoreErr(err), nil
	}
	if !present {
		// PutReader bypasses the LRU's admit-clone; the backend verifies the
		// hash before the bytes become visible.
		if err := filecas.PutReader(ctx, d.fcas, sha, bytes.NewReader(data), size); err != nil {
			return blobStoreErr(err), nil
		}
	}
	// Ownership: `existed` is THIS tenant's prior knowledge of the bytes —
	// never the CAS's (which would leak other tenants' holdings).
	_, existed, err := d.ix.GetSha(ctx, tenant, sha)
	if err != nil {
		return blobStoreErr(err), nil
	}
	now := blobNow(d)
	ct := gjson.GetBytes(meta, "content_type").String()
	if ct == "" && hasPrior {
		ct = prior.ContentType
	}
	if ct == "" {
		if filename != "" {
			ct = blob.DefaultContentType(filename)
		} else {
			ct = blob.DefaultContentType(name)
		}
	}
	if !existed {
		if _, err := d.ix.PutShaIfAbsent(ctx, tenant, blob.ShaRow{
			SHA256: sha, Size: size, ContentType: ct, FirstSeen: now,
		}); err != nil {
			return blobStoreErr(err), nil
		}
	}

	resp := jsonx.NewObject()
	into := blobInto(meta)
	replaced := false
	if name != "" {
		replaced = hasPrior && prior.SHA256 != sha
		row := blob.NameRow{
			Name:        name,
			SHA256:      sha,
			Size:        size,
			ContentType: ct,
			Filename:    filename,
			Permissions: perms,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if hasPrior {
			if filename == "" {
				row.Filename = prior.Filename
			}
			if !permsGiven {
				row.Permissions = prior.Permissions // omitted never declassifies
			}
			if !prior.CreatedAt.IsZero() {
				row.CreatedAt = prior.CreatedAt
			}
			// Keep the pack bookkeeping: a runtime repoint of a seeded name
			// is DRIFT the next `txco data apply` must see (and refuse to
			// clobber unless forced), not a silent change of ownership.
			row.SeededBy = prior.SeededBy
			row.SeededSHA = prior.SeededSHA
		}
		if err := d.ix.PutName(ctx, tenant, row); err != nil {
			return blobStoreErr(err), nil
		}
		resp.Set(into+".name", name)
		resp.Set(into+".replaced", replaced)
	}
	resp.Set(into+".sha256", sha)
	resp.Set(into+".size", size)
	resp.Set(into+".existed", existed)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// blobResolve turns an address into the row to read: by name (permission
// checked) or by sha (privileged + tenant-owned).
func blobResolve(ctx context.Context, d blobDeps, tenant string, grants []string, name, sha string) (row blob.NameRow, found bool, errPayload event.Payload, ok bool) {
	if name != "" {
		row, found, err := d.ix.GetName(ctx, tenant, name)
		if err != nil {
			return blob.NameRow{}, false, blobStoreErr(err), false
		}
		if !found {
			return blob.NameRow{}, false, event.Payload{}, true
		}
		if !blob.Allowed(grants, row.Permissions) {
			return blob.NameRow{}, false, blobErr("txco_blob_denied", "name requires permissions this op does not hold"), false
		}
		return row, true, event.Payload{}, true
	}
	if !blob.CanReadByHash(grants) {
		return blob.NameRow{}, false, blobErr("txco_blob_denied", "addressing by sha256 requires "+blob.CASRead), false
	}
	srow, found, err := d.ix.GetSha(ctx, tenant, sha)
	if err != nil {
		return blob.NameRow{}, false, blobStoreErr(err), false
	}
	if !found {
		return blob.NameRow{}, false, event.Payload{}, true
	}
	return blob.NameRow{SHA256: sha, Size: srow.Size, ContentType: srow.ContentType}, true, event.Payload{}, true
}

// blobGet reads a blob's bytes into the envelope. Result at `into`:
// {name?, sha256, size, content_type, filename?, content, encoding}.
func blobGet(ctx context.Context, d blobDeps, in []byte) (event.Payload, error) {
	tenant, meta, grants, ep, ok := blobPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name, sha, ep, ok := blobAddress(meta)
	if !ok {
		return ep, nil
	}
	enc := gjson.GetBytes(meta, "encoding").String()
	switch enc {
	case "":
		enc = "base64"
	case "base64", "utf8", "auto":
	default:
		return blobErr("txco_blob_invalid_arg", "encoding must be base64, utf8 or auto"), nil
	}
	row, found, ep, ok := blobResolve(ctx, d, tenant, grants, name, sha)
	if !ok {
		return ep, nil
	}
	if !found {
		return blobErr("txco_blob_not_found", "no such blob"), nil
	}
	limit := d.maxBytes
	if mb := gjson.GetBytes(meta, "max_bytes").Int(); mb > 0 && (limit == 0 || mb < limit) {
		limit = mb
	}
	if limit > 0 && row.Size > limit {
		return blobErr("txco_blob_too_large",
			fmt.Sprintf("blob is %d bytes, over the %d-byte read cap", row.Size, limit)), nil
	}
	rc, _, err := filecas.GetReader(ctx, d.fcas, row.SHA256)
	if err != nil {
		return blobStoreErr(err), nil
	}
	var data []byte
	if limit > 0 {
		data, err = io.ReadAll(io.LimitReader(rc, limit+1))
	} else {
		data, err = io.ReadAll(rc)
	}
	_ = rc.Close()
	if err != nil {
		return blobStoreErr(err), nil
	}
	if limit > 0 && int64(len(data)) > limit {
		return blobErr("txco_blob_too_large",
			fmt.Sprintf("blob exceeds the %d-byte read cap", limit)), nil
	}
	blobChargeBytes(ctx, int64(len(data)), in)
	content, encOut := encodeReadFile(data, enc)

	resp := jsonx.NewObject()
	into := blobInto(meta)
	if row.Name != "" {
		resp.Set(into+".name", row.Name)
	}
	resp.Set(into+".sha256", row.SHA256)
	resp.Set(into+".size", int64(len(data)))
	resp.Set(into+".content_type", row.ContentType)
	if row.Filename != "" {
		resp.Set(into+".filename", row.Filename)
	}
	resp.Set(into+".content", content)
	resp.Set(into+".encoding", encOut)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// blobStat reports a blob's metadata without moving bytes; a miss is a
// result ({exists:false}), not an error.
func blobStat(ctx context.Context, d blobDeps, in []byte) (event.Payload, error) {
	tenant, meta, grants, ep, ok := blobPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name, sha, ep, ok := blobAddress(meta)
	if !ok {
		return ep, nil
	}
	row, found, ep, ok := blobResolve(ctx, d, tenant, grants, name, sha)
	if !ok {
		return ep, nil
	}
	resp := jsonx.NewObject()
	into := blobInto(meta)
	resp.Set(into+".exists", found)
	if found {
		if row.Name != "" {
			resp.Set(into+".name", row.Name)
			resp.SetRaw(into+".permissions", blobJSONStrings(row.Permissions))
			resp.Set(into+".updated_at", row.UpdatedAt.UTC().Format(time.RFC3339))
		}
		resp.Set(into+".sha256", row.SHA256)
		resp.Set(into+".size", row.Size)
		resp.Set(into+".content_type", row.ContentType)
		if row.Filename != "" {
			resp.Set(into+".filename", row.Filename)
		}
	}
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

type blobListEntry struct {
	Name        string   `json:"name"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	ContentType string   `json:"content_type,omitempty"`
	Filename    string   `json:"filename,omitempty"`
	Permissions []string `json:"permissions"`
	UpdatedAt   string   `json:"updated_at"`
}

// blobList windows the tenant's names under `prefix`, sorted, after the
// `after` cursor, at most `limit` (≤ 200). Rows carry their `permissions`
// (metadata only; reads stay gated) — v1 does not filter by subject.
func blobList(ctx context.Context, d blobDeps, in []byte) (event.Payload, error) {
	tenant, meta, _, ep, ok := blobPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 || limit > kvstore.MaxListLimit {
		limit = kvstore.DefaultListLimit
	}
	page, err := d.ix.ListNames(ctx, tenant, blob.ListOpts{
		Prefix: gjson.GetBytes(meta, "prefix").String(),
		After:  gjson.GetBytes(meta, "after").String(),
		Limit:  limit,
	})
	if err != nil {
		return blobStoreErr(err), nil
	}
	entries := make([]blobListEntry, 0, len(page.Names))
	for _, r := range page.Names {
		perms := r.Permissions
		if perms == nil {
			perms = []string{}
		}
		entries = append(entries, blobListEntry{
			Name: r.Name, SHA256: r.SHA256, Size: r.Size, ContentType: r.ContentType,
			Filename: r.Filename, Permissions: perms, UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	raw, _ := json.Marshal(entries)
	resp := jsonx.NewObject()
	into := blobInto(meta)
	resp.SetRaw(into+".names", string(raw))
	resp.Set(into+".next", page.Next)
	resp.Set(into+".count", len(entries))
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// blobDelete unlinks a NAME; the bytes stay in the CAS (no GC in v1).
// Result at `into`: {deleted}.
func blobDelete(ctx context.Context, d blobDeps, in []byte) (event.Payload, error) {
	tenant, meta, grants, ep, ok := blobPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name := gjson.GetBytes(meta, "name").String()
	if name == "" {
		return blobErr("txco_blob_invalid_arg", "need `name`"), nil
	}
	if err := blob.ValidName(name); err != nil {
		return blobErr("txco_blob_invalid_name", err.Error()), nil
	}
	row, found, err := d.ix.GetName(ctx, tenant, name)
	if err != nil {
		return blobStoreErr(err), nil
	}
	into := blobInto(meta)
	if !found {
		raw, _ := sjson.Set(`{}`, into+".deleted", false)
		return event.Payload{Raw: raw, Type: event.JSON}, nil
	}
	if !blob.Allowed(grants, row.Permissions) {
		return blobErr("txco_blob_denied", "name requires permissions this op does not hold"), nil
	}
	if err := d.ix.DeleteName(ctx, tenant, name); err != nil {
		return blobStoreErr(err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".deleted", true)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func blobJSONStrings(s []string) string {
	if s == nil {
		s = []string{}
	}
	raw, _ := json.Marshal(s)
	return string(raw)
}
