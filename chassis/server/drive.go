package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/signedurl"
)

// drive.go holds the shared plumbing for the drive store ops
// (txco://drive/{collection,account,put,get,stat,list,delete,mkdir,move,
// copy,sign}): the mutable document store the `webdav` personality serves. A
// stack provisions a collection and an account, writes and reads files by
// path or resource id, and lists changes since a sync token; a WebDAV
// client sees the same collection at https://<host>/drive/.
//
// Scoping is trusted: tenant from processor.TenantScope(ctx); a collection
// is addressed by NAME and must belong to the tenant. Errors surface under
// `<into>.error.{code,message}` with a nil Go error (the contacts /
// calendar shape) so authors branch with `WHEN ._drive.error ...`.
//
// The handler bodies are in drive_ops.go.

// driveDeps is what the handlers need from the boot wiring.
type driveDeps struct {
	store *chdrive.Store // nil ⇒ txco_drive_disabled
	ids   *authn.Store   // the identity store drive/account binds in
	// snap returns the mirror DB the domain-ownership rule reads (dbcache
	// snapshot); nil ⇒ every domain is refused.
	snap     func() *sql.DB
	dialect  registry.Dialect
	maxBytes int64 // --drive-op-max-bytes: cap on buffered put/get content
	prefix   string
	now      func() time.Time
	// ix + fcas are the blob index and content store `drive/put from_sha`
	// reads from (an object the tenant already owns, streamed, never
	// buffered in the envelope); either nil ⇒ from_sha answers
	// txco_drive_disabled.
	ix   blob.Index
	fcas filecas.Store
	// signer + signBase mint `drive/sign` URLs (drive_sign.go): the fleet
	// key's signer and the public origin the URLs live on. A nil signer ⇒
	// txco_drive_sign_unavailable (no master key on this node).
	signer   *signedurl.Signer
	signBase string
}

// driveEnsureParents creates the missing ancestors of path (`parents =
// true` on put/mkdir/move/copy): Mkdir per level, top down, an existing
// entry ignored — a FILE in the way is left for the write itself to refuse
// (ErrNotDirectory). Concurrent creators race benignly to ErrExists.
func driveEnsureParents(ctx context.Context, d driveDeps, collID, path, into string) (event.Payload, bool) {
	norm, err := chdrive.NormalizePath(path)
	if err != nil {
		return driveStoreErr(into, err), false
	}
	var ancestors []string
	for p := chdrive.ParentOf(norm); p != ""; p = chdrive.ParentOf(p) {
		ancestors = append([]string{p}, ancestors...)
	}
	for _, p := range ancestors {
		if _, err := d.store.Mkdir(ctx, collID, p); err != nil && !errors.Is(err, chdrive.ErrExists) {
			return driveStoreErr(into, err), false
		}
	}
	return event.Payload{}, true
}

// driveListMax caps one txco://drive/list page.
const driveListMax = 1000

// driveListDefault is the page when no limit is given.
const driveListDefault = 200

func driveInto(meta []byte) string {
	into := intoPath(meta, "_drive")
	return into
}

func driveErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// driveStoreErr maps a store error to its op code: every typed error has
// one; anything else is txco_drive_store.
func driveStoreErr(into string, err error) event.Payload {
	code := "txco_drive_store"
	switch {
	case errors.Is(err, chdrive.ErrNotFound):
		code = "txco_drive_not_found"
	case errors.Is(err, chdrive.ErrExists):
		code = "txco_drive_exists"
	case errors.Is(err, chdrive.ErrNoParent):
		code = "txco_drive_no_parent"
	case errors.Is(err, chdrive.ErrIsDirectory):
		code = "txco_drive_is_directory"
	case errors.Is(err, chdrive.ErrNotDirectory):
		code = "txco_drive_not_directory"
	case errors.Is(err, chdrive.ErrPrecondition):
		code = "txco_drive_precondition"
	case errors.Is(err, chdrive.ErrQuota):
		code = "txco_drive_quota"
	case errors.Is(err, chdrive.ErrTooLarge):
		code = "txco_drive_too_large"
	case errors.Is(err, chdrive.ErrBadPath):
		code = "txco_drive_invalid_arg"
	case errors.Is(err, chdrive.ErrCycle):
		code = "txco_drive_cycle"
	case errors.Is(err, chdrive.ErrUsernameTaken):
		code = "txco_drive_username_taken"
	case errors.Is(err, chdrive.ErrNotEmpty):
		code = "txco_drive_not_empty"
	case errors.Is(err, chdrive.ErrSizeMismatch):
		code = "txco_drive_invalid_arg"
	}
	return driveErr(into, code, err.Error())
}

// drivePrelude is the common head of every handler: tenant, store, meta.
func drivePrelude(ctx context.Context, d driveDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = driveInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, driveErr(into, "txco_drive_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", nil, into, driveErr(into, "txco_drive_disabled", "no drive store on this node (webdav personality off and --drive-store=sqlite, or the shared store failed to open at boot)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

func (d driveDeps) domainOwned(ctx context.Context, tenant, domain string) (bool, error) {
	return imapDeps{snap: d.snap, dialect: d.dialect}.domainOwned(ctx, tenant, domain)
}

// driveCollectionFor resolves the WITH `collection` (a name) to the
// tenant's live collection.
func driveCollectionFor(ctx context.Context, d driveDeps, tenant string, meta []byte, into string) (chdrive.Collection, event.Payload, bool) {
	name := strings.TrimSpace(gjson.GetBytes(meta, "collection").String())
	if name == "" {
		return chdrive.Collection{}, driveErr(into, "txco_drive_invalid_arg", "missing `collection`"), false
	}
	if !chdrive.ValidCollectionName(name) {
		return chdrive.Collection{}, driveErr(into, "txco_drive_invalid_arg", "`collection` must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)"), false
	}
	c, found, err := d.store.GetCollection(ctx, tenant, name)
	if err != nil {
		return chdrive.Collection{}, driveStoreErr(into, err), false
	}
	if !found {
		return chdrive.Collection{}, driveErr(into, "txco_drive_not_found", fmt.Sprintf("no drive collection %q for this tenant", name)), false
	}
	return c, event.Payload{}, true
}

// driveChargeBytes meters moved bytes per MiB, rounded up.
func driveChargeBytes(ctx context.Context, n int64, in []byte) {
	mib := (n + (1 << 20) - 1) >> 20
	if mib > 0 {
		_ = processor.AddFuel(ctx, mib*processor.FuelCostDrivePerMiB, gjson.GetBytes(in, "_txc.op").String())
	}
}

// driveResourceJSON is the wire shape of one resource in stat/list/get
// results.
func driveResourceJSON(r chdrive.Resource) map[string]any {
	m := map[string]any{
		"resource_id":  r.ResourceID,
		"kind":         r.Kind,
		"path":         r.Path,
		"name":         chdrive.BaseOf(r.Path),
		"parent":       r.ParentPath,
		"size":         r.Size,
		"content_type": r.ContentType,
		"etag":         r.ETag,
		"modseq":       r.ModSeq,
		"created_at":   r.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":   r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.Deleted {
		m["deleted"] = true
		m["deleted_at"] = r.DeletedAt.UTC().Format(time.RFC3339)
	}
	return m
}
