package drive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Resource is a drive_resources row: one file or directory of a
// collection. ResourceID is the identity (fixed for life); Path is the
// location (changes on move); ETag is the version (sha256 of the current
// bytes, "" for a directory); ModSeq is the collection sync_token value at
// the row's last change.
type Resource struct {
	ResourceID   string
	CollectionID string
	Path         string
	ParentPath   string
	Depth        int
	Kind         string
	ObjectKey    string
	Size         int64
	ContentType  string
	ETag         string
	ModSeq       int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    time.Time
	Deleted      bool
}

// IsDir reports a directory resource.
func (r Resource) IsDir() bool { return r.Kind == KindDir }

// PutOpts steers Put.
type PutOpts struct {
	// IfMatch / IfNoneMatch are the client's conditional headers, unquoted
	// ("*" for IfNoneMatch means create-only; "*" for IfMatch means
	// must-exist).
	IfMatch     string
	IfNoneMatch string
	// ContentType is recorded on the row; empty keeps the existing one on
	// an update and derives one from the extension on a create.
	ContentType string
}

// PutResult reports what Put did.
type PutResult struct {
	ResourceID string
	ETag       string
	Size       int64
	Created    bool
	Noop       bool
	ModSeq     int64
}

// DeleteOpts steers Delete.
type DeleteOpts struct {
	// IfMatch is the client's conditional header, unquoted ("*" = any).
	IfMatch string
}

// ListOpts narrows List.
type ListOpts struct {
	// Path is the directory whose entries are listed ("" = the root).
	Path string
	// Recursive lists every resource below Path instead of its direct
	// children.
	Recursive bool
	// SinceModSeq returns only rows changed after that sync token.
	SinceModSeq int64
	// IncludeDeleted returns tombstones too (a sync consumer's deletions).
	IncludeDeleted bool
	// Limit caps the page (0 = unlimited); After is the exclusive path
	// cursor of the previous page.
	Limit int
	After string
}

const resourceCols = `resource_id, collection_id, path, parent_path, depth, kind, object_key, size, content_type, etag,
	modseq, created_at, updated_at, deleted_at`

func scanResource(r rowScanner) (Resource, error) {
	var o Resource
	var objectKey, deleted sql.NullString
	var created, updated string
	if err := r.Scan(&o.ResourceID, &o.CollectionID, &o.Path, &o.ParentPath, &o.Depth, &o.Kind, &objectKey, &o.Size, &o.ContentType, &o.ETag,
		&o.ModSeq, &created, &updated, &deleted); err != nil {
		return Resource{}, err
	}
	o.ObjectKey = objectKey.String
	o.CreatedAt = parseTime(created)
	o.UpdatedAt = parseTime(updated)
	if deleted.Valid {
		o.Deleted = true
		o.DeletedAt = parseTime(deleted.String)
	}
	return o, nil
}

// NewResourceID mints a resource id (`dr_<hxid>`, time-sortable).
func NewResourceID() string { return "dr_" + hxid.NewTimeSort().String() }

func newVersionID() string { return "dv_" + hxid.NewTimeSort().String() }

// objectKey is the object-store key of one version of one resource.
func objectKey(tenant, collID, resID, verID string) string {
	return tenant + "/" + collID + "/" + resID + "/" + verID
}

// DefaultContentType derives a content type from a path's extension,
// application/octet-stream when none is known.
func DefaultContentType(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ETagOf is the etag of stored bytes: bare sha256 hex.
func ETagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkPreconditions applies If-Match / If-None-Match (unquoted etags,
// "*" wildcard) against the live state.
func checkPreconditions(ifMatch, ifNoneMatch string, live bool, etag string) error {
	if ifNoneMatch == "*" && live {
		return ErrPrecondition
	}
	if ifNoneMatch != "" && ifNoneMatch != "*" && live && etag == ifNoneMatch {
		return ErrPrecondition
	}
	if ifMatch != "" && (!live || (ifMatch != "*" && etag != ifMatch)) {
		return ErrPrecondition
	}
	return nil
}

// getResourceQ fetches the row at path (live only unless withDeleted; a
// tombstoned path may have several rows — the newest wins).
func (s *Store) getResourceQ(ctx context.Context, q txq, collID, p string, withDeleted, forUpdate bool) (Resource, bool, error) {
	where := `collection_id = ? AND path = ?`
	if !withDeleted {
		where += ` AND deleted_at IS NULL`
	}
	sfx := ` ORDER BY (deleted_at IS NULL) DESC, modseq DESC LIMIT 1`
	if forUpdate {
		sfx += lock(s)
	}
	r, err := scanResource(q.QueryRowContext(ctx, s.rb(`SELECT `+resourceCols+` FROM drive_resources WHERE `+where+sfx), collID, p))
	if errors.Is(err, sql.ErrNoRows) {
		return Resource{}, false, nil
	}
	if err != nil {
		return Resource{}, false, fmt.Errorf("drive: get resource: %w", err)
	}
	return r, true, nil
}

// getResourceByIDQ fetches the live row with that id in the collection.
func (s *Store) getResourceByIDQ(ctx context.Context, q txq, collID, resID string, forUpdate bool) (Resource, bool, error) {
	sfx := ""
	if forUpdate {
		sfx = lock(s)
	}
	r, err := scanResource(q.QueryRowContext(ctx, s.rb(`
		SELECT `+resourceCols+` FROM drive_resources WHERE collection_id = ? AND resource_id = ? AND deleted_at IS NULL`+sfx), collID, resID))
	if errors.Is(err, sql.ErrNoRows) {
		return Resource{}, false, nil
	}
	if err != nil {
		return Resource{}, false, fmt.Errorf("drive: get resource: %w", err)
	}
	return r, true, nil
}

// requireParent checks that a path's parent directory exists (the root
// always does). ErrNoParent when it is missing; ErrNotDirectory when it is a
// file.
func (s *Store) requireParent(ctx context.Context, q txq, collID, p string) error {
	parent := ParentOf(p)
	if parent == "" {
		return nil
	}
	row, live, err := s.getResourceQ(ctx, q, collID, parent, false, false)
	if err != nil {
		return err
	}
	if !live {
		return ErrNoParent
	}
	if row.Kind != KindDir {
		return ErrNotDirectory
	}
	return nil
}

// rootResource is the synthetic directory row for a collection's root.
func rootResource(c Collection) Resource {
	return Resource{
		ResourceID:   c.ID,
		CollectionID: c.ID,
		Path:         "",
		ParentPath:   "",
		Depth:        0,
		Kind:         KindDir,
		ModSeq:       c.SyncToken,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

// countingReader counts what passed through.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// discard deletes an object the index will never reference (a failed put).
// Best-effort: the sweeper reclaims what this misses.
func (s *Store) discard(ctx context.Context, keys ...string) {
	for _, k := range keys {
		if k != "" {
			_ = s.objects.Delete(ctx, k)
		}
	}
}

// Put stores the bytes from r at path p in the collection, creating or
// replacing the file. size is the caller's declared byte count, or -1 when
// the body's length is not known up front (a chunked HTTP PUT — macOS
// Finder streams a dragged file that way): then the body is read up to the
// per-file cap and its length is what was read. Bytes first: the body
// streams to a fresh object key (hashed on the way), then one index
// transaction locks the collection, re-checks parent / kind /
// preconditions / quota, writes the row, advances the sync token and
// reports the mutation. An unchanged body (same etag) is a Noop: no version
// bump, no event, and the freshly written object is discarded. A directory
// at p is ErrIsDirectory; a missing parent ErrNoParent; a body shorter or
// longer than a declared size ErrSizeMismatch; a body over the cap
// ErrTooLarge.
func (s *Store) Put(ctx context.Context, collID, p string, r io.Reader, size int64, opts PutOpts) (PutResult, error) {
	p, err := NormalizePath(p)
	if err != nil {
		return PutResult{}, err
	}
	if p == "" {
		return PutResult{}, fmt.Errorf("%w: the root is not a file", ErrBadPath)
	}
	if size < 0 {
		size = -1
	}
	if size >= 0 && s.limits.MaxFileBytes > 0 && size > s.limits.MaxFileBytes {
		return PutResult{}, ErrTooLarge
	}
	if size >= 0 && s.limits.MaxCollectionBytes > 0 && size > s.limits.MaxCollectionBytes {
		return PutResult{}, ErrQuota
	}
	coll, ok, err := s.GetCollectionByID(ctx, collID)
	if err != nil {
		return PutResult{}, err
	}
	if !ok {
		return PutResult{}, ErrNotFound
	}
	// Pre-flight, non-authoritative (the tx re-checks): refuse before the
	// body streams when the parent is missing, the path is a directory, or
	// a precondition already fails. A 4 GiB upload should not be accepted
	// only to be refused at the row.
	if err := s.requireParent(ctx, s.db, collID, p); err != nil {
		return PutResult{}, err
	}
	pre, preLive, err := s.getResourceQ(ctx, s.db, collID, p, false, false)
	if err != nil {
		return PutResult{}, err
	}
	if preLive && pre.Kind == KindDir {
		return PutResult{}, ErrIsDirectory
	}
	if err := checkPreconditions(opts.IfMatch, opts.IfNoneMatch, preLive, pre.ETag); err != nil {
		return PutResult{}, err
	}

	// Bytes.
	newID := NewResourceID()
	keyResID := newID
	if preLive {
		keyResID = pre.ResourceID
	}
	key := objectKey(coll.Tenant, collID, keyResID, newVersionID())
	ct := opts.ContentType
	if ct == "" && preLive {
		ct = pre.ContentType
	}
	if ct == "" {
		ct = DefaultContentType(p)
	}
	// Read at most one byte past what is allowed, so a long body is caught
	// without being stored: past the declared size when there is one, past
	// the per-file cap when there is not (an unlimited cap reads to EOF).
	h := sha256.New()
	limit := size + 1
	if size < 0 {
		limit = 0
		if s.limits.MaxFileBytes > 0 {
			limit = s.limits.MaxFileBytes + 1
		}
	}
	body := r
	if limit > 0 {
		body = io.LimitReader(r, limit)
	}
	cr := &countingReader{r: body}
	if err := s.objects.Put(ctx, key, io.TeeReader(cr, h), size, ct); err != nil {
		s.discard(ctx, key)
		return PutResult{}, fmt.Errorf("drive: store object: %w", err)
	}
	if size >= 0 && cr.n != size {
		s.discard(ctx, key)
		return PutResult{}, ErrSizeMismatch
	}
	if size < 0 {
		if s.limits.MaxFileBytes > 0 && cr.n > s.limits.MaxFileBytes {
			s.discard(ctx, key)
			return PutResult{}, ErrTooLarge
		}
		size = cr.n
	}
	etag := hex.EncodeToString(h.Sum(nil))

	// Metadata.
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		s.discard(ctx, key)
		return PutResult{}, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := s.lockCollection(ctx, tx, collID)
	if err != nil {
		s.discard(ctx, key)
		return PutResult{}, err
	}
	if err := s.requireParent(ctx, tx, collID, p); err != nil {
		s.discard(ctx, key)
		return PutResult{}, err
	}
	cur, live, err := s.getResourceQ(ctx, tx, collID, p, false, true)
	if err != nil {
		s.discard(ctx, key)
		return PutResult{}, err
	}
	if live && cur.Kind == KindDir {
		s.discard(ctx, key)
		return PutResult{}, ErrIsDirectory
	}
	if err := checkPreconditions(opts.IfMatch, opts.IfNoneMatch, live, cur.ETag); err != nil {
		s.discard(ctx, key)
		return PutResult{}, err
	}
	if live && cur.ETag == etag && cur.Size == size {
		_ = tx.Rollback()
		s.discard(ctx, key)
		return PutResult{ResourceID: cur.ResourceID, ETag: etag, Size: size, Noop: true, ModSeq: cur.ModSeq}, nil
	}
	newBytes := c.BytesUsed + size
	if live {
		newBytes -= cur.Size
	}
	if s.limits.MaxCollectionBytes > 0 && newBytes > s.limits.MaxCollectionBytes {
		s.discard(ctx, key)
		return PutResult{}, ErrQuota
	}
	if !live && s.limits.MaxResources > 0 && c.ResourceCount+1 > s.limits.MaxResources {
		s.discard(ctx, key)
		return PutResult{}, ErrQuota
	}

	c.SyncToken++
	res := PutResult{ETag: etag, Size: size, ModSeq: c.SyncToken}
	if live {
		res.ResourceID = cur.ResourceID
		if _, err := tx.ExecContext(ctx, s.rb(`
			UPDATE drive_resources SET object_key = ?, size = ?, content_type = ?, etag = ?, modseq = ?, updated_at = ?
			 WHERE resource_id = ?`),
			key, size, ct, etag, c.SyncToken, now, cur.ResourceID); err != nil {
			s.discard(ctx, key)
			return PutResult{}, fmt.Errorf("drive: update resource: %w", err)
		}
	} else {
		res.ResourceID = newID
		res.Created = true
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO drive_resources (`+resourceCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`),
			newID, collID, p, ParentOf(p), Depth(p), KindFile, key, size, ct, etag, c.SyncToken, now, now); err != nil {
			s.discard(ctx, key)
			if s.dialect.IsUniqueViolationGeneric(err) {
				return PutResult{}, ErrExists
			}
			return PutResult{}, fmt.Errorf("drive: insert resource: %w", err)
		}
		c.ResourceCount++
	}
	c.BytesUsed = newBytes
	if err := s.advance(ctx, tx, c, now); err != nil {
		s.discard(ctx, key)
		return PutResult{}, err
	}
	m := Mutation{
		Event: EventResourceUpdated, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: res.ResourceID, Kind: KindFile, Path: p, ETag: etag, Size: size, ContentType: ct,
		ModSeq: c.SyncToken, At: s.now(),
	}
	if res.Created {
		m.Event = EventResourceCreated
	}
	if err := s.emitTx(ctx, tx, m); err != nil {
		s.discard(ctx, key)
		return PutResult{}, fmt.Errorf("drive: enqueue mutation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		s.discard(ctx, key)
		return PutResult{}, fmt.Errorf("drive: commit: %w", err)
	}
	// The superseded object (cur.ObjectKey) is now unreferenced; the
	// sweeper reclaims it after the grace period so an in-flight reader
	// finishes.
	s.emitPost(ctx, m)
	return res, nil
}

// Mkdir creates a directory at p. ErrExists when a live resource of either
// kind is there; ErrNoParent when the parent is missing.
func (s *Store) Mkdir(ctx context.Context, collID, p string) (Resource, error) {
	p, err := NormalizePath(p)
	if err != nil {
		return Resource{}, err
	}
	if p == "" {
		return Resource{}, fmt.Errorf("%w: the root already exists", ErrBadPath)
	}
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Resource{}, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := s.lockCollection(ctx, tx, collID)
	if err != nil {
		return Resource{}, err
	}
	if err := s.requireParent(ctx, tx, collID, p); err != nil {
		return Resource{}, err
	}
	if _, live, err := s.getResourceQ(ctx, tx, collID, p, false, true); err != nil {
		return Resource{}, err
	} else if live {
		return Resource{}, ErrExists
	}
	if s.limits.MaxResources > 0 && c.ResourceCount+1 > s.limits.MaxResources {
		return Resource{}, ErrQuota
	}
	c.SyncToken++
	id := NewResourceID()
	if _, err := tx.ExecContext(ctx, s.rb(`
		INSERT INTO drive_resources (`+resourceCols+`)
		VALUES (?, ?, ?, ?, ?, ?, NULL, 0, '', '', ?, ?, ?, NULL)`),
		id, collID, p, ParentOf(p), Depth(p), KindDir, c.SyncToken, now, now); err != nil {
		if s.dialect.IsUniqueViolationGeneric(err) {
			return Resource{}, ErrExists
		}
		return Resource{}, fmt.Errorf("drive: insert directory: %w", err)
	}
	c.ResourceCount++
	if err := s.advance(ctx, tx, c, now); err != nil {
		return Resource{}, err
	}
	m := Mutation{
		Event: EventResourceCreated, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: id, Kind: KindDir, Path: p, ModSeq: c.SyncToken, At: s.now(),
	}
	if err := s.emitTx(ctx, tx, m); err != nil {
		return Resource{}, fmt.Errorf("drive: enqueue mutation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Resource{}, fmt.Errorf("drive: commit: %w", err)
	}
	s.emitPost(ctx, m)
	return Resource{
		ResourceID: id, CollectionID: collID, Path: p, ParentPath: ParentOf(p), Depth: Depth(p), Kind: KindDir,
		ModSeq: c.SyncToken, CreatedAt: parseTime(now), UpdatedAt: parseTime(now),
	}, nil
}

// Stat returns the live resource at p. The root ("") is a synthetic
// directory carrying the collection's sync token.
func (s *Store) Stat(ctx context.Context, collID, p string) (Resource, bool, error) {
	p, err := NormalizePath(p)
	if err != nil {
		return Resource{}, false, err
	}
	if p == "" {
		c, ok, err := s.GetCollectionByID(ctx, collID)
		if err != nil || !ok {
			return Resource{}, false, err
		}
		return rootResource(c), true, nil
	}
	return s.getResourceQ(ctx, s.db, collID, p, false, false)
}

// StatByID returns the live resource with that id in the collection.
func (s *Store) StatByID(ctx context.Context, collID, resID string) (Resource, bool, error) {
	if resID == "" {
		return Resource{}, false, nil
	}
	return s.getResourceByIDQ(ctx, s.db, collID, resID, false)
}

// Open returns a reader over the file at p and its row. ErrNotFound when
// absent; ErrIsDirectory for a directory (the root included).
func (s *Store) Open(ctx context.Context, collID, p string) (io.ReadCloser, Resource, error) {
	r, ok, err := s.Stat(ctx, collID, p)
	if err != nil {
		return nil, Resource{}, err
	}
	if !ok {
		return nil, Resource{}, ErrNotFound
	}
	return s.openResource(ctx, r)
}

// OpenByID is Open addressed by resource id.
func (s *Store) OpenByID(ctx context.Context, collID, resID string) (io.ReadCloser, Resource, error) {
	r, ok, err := s.StatByID(ctx, collID, resID)
	if err != nil {
		return nil, Resource{}, err
	}
	if !ok {
		return nil, Resource{}, ErrNotFound
	}
	return s.openResource(ctx, r)
}

func (s *Store) openResource(ctx context.Context, r Resource) (io.ReadCloser, Resource, error) {
	if r.Kind == KindDir {
		return nil, r, ErrIsDirectory
	}
	rc, _, err := s.objects.Get(ctx, r.ObjectKey)
	if err != nil {
		return nil, r, fmt.Errorf("drive: open object %s: %w", r.ObjectKey, err)
	}
	return rc, r, nil
}

// List returns resources per opts, ordered by path. Without Recursive the
// direct children of Path; with it every resource strictly below Path. The
// directory itself is never included. Whether Path exists is the caller's
// question (Stat).
func (s *Store) List(ctx context.Context, collID string, opts ListOpts) ([]Resource, error) {
	p, err := NormalizePath(opts.Path)
	if err != nil {
		return nil, err
	}
	where := []string{"collection_id = ?"}
	args := []any{collID}
	switch {
	case !opts.Recursive:
		where = append(where, "parent_path = ?")
		args = append(args, p)
	case p != "":
		where = append(where, "path LIKE ?"+likeEscape)
		args = append(args, LikePrefix(p))
	}
	if !opts.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if opts.SinceModSeq > 0 {
		where = append(where, "modseq > ?")
		args = append(args, opts.SinceModSeq)
	}
	if opts.After != "" {
		where = append(where, "path > ?")
		args = append(args, opts.After)
	}
	q := `SELECT ` + resourceCols + ` FROM drive_resources WHERE ` + strings.Join(where, " AND ") + ` ORDER BY path`
	if opts.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, s.rb(q), args...)
	if err != nil {
		return nil, fmt.Errorf("drive: list resources: %w", err)
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("drive: scan resource: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// tombstoneSubtree tombstones the live row at p and, for a directory,
// everything below it, all at modseq token. Returns the rows and bytes it
// removed (for the collection counters).
func (s *Store) tombstoneSubtree(ctx context.Context, tx txq, collID, p, kind string, token int64, now string) (int64, int64, error) {
	where := `collection_id = ? AND deleted_at IS NULL AND path = ?`
	args := []any{collID, p}
	if kind == KindDir {
		where = `collection_id = ? AND deleted_at IS NULL AND (path = ? OR path LIKE ?` + likeEscape + `)`
		args = append(args, LikePrefix(p))
	}
	var n, bytes int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT COUNT(*), CAST(COALESCE(SUM(size), 0) AS BIGINT) FROM drive_resources WHERE `+where), args...).Scan(&n, &bytes); err != nil {
		return 0, 0, fmt.Errorf("drive: count subtree: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE drive_resources SET deleted_at = ?, modseq = ?, updated_at = ? WHERE `+where),
		append([]any{now, token, now}, args...)...); err != nil {
		return 0, 0, fmt.Errorf("drive: tombstone subtree: %w", err)
	}
	return n, bytes, nil
}

// Delete tombstones the resource at p (a directory with everything below
// it). ErrNotFound when absent.
func (s *Store) Delete(ctx context.Context, collID, p string, opts DeleteOpts) (Resource, error) {
	p, err := NormalizePath(p)
	if err != nil {
		return Resource{}, err
	}
	if p == "" {
		return Resource{}, fmt.Errorf("%w: the root cannot be deleted", ErrBadPath)
	}
	return s.deleteWhere(ctx, collID, opts, func(tx txq) (Resource, bool, error) {
		return s.getResourceQ(ctx, tx, collID, p, false, true)
	})
}

// DeleteByID is Delete addressed by resource id.
func (s *Store) DeleteByID(ctx context.Context, collID, resID string, opts DeleteOpts) (Resource, error) {
	return s.deleteWhere(ctx, collID, opts, func(tx txq) (Resource, bool, error) {
		return s.getResourceByIDQ(ctx, tx, collID, resID, true)
	})
}

func (s *Store) deleteWhere(ctx context.Context, collID string, opts DeleteOpts, find func(txq) (Resource, bool, error)) (Resource, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Resource{}, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := s.lockCollection(ctx, tx, collID)
	if err != nil {
		return Resource{}, err
	}
	cur, live, err := find(tx)
	if err != nil {
		return Resource{}, err
	}
	if !live {
		return Resource{}, ErrNotFound
	}
	if opts.IfMatch != "" && opts.IfMatch != "*" && cur.ETag != opts.IfMatch {
		return Resource{}, ErrPrecondition
	}
	c.SyncToken++
	// The files below a directory, listed before the tombstone hides them:
	// each gets its own deleted event after the directory's.
	var below []Resource
	if cur.Kind == KindDir {
		if below, err = s.listQ(ctx, tx, collID, cur.Path); err != nil {
			return Resource{}, err
		}
	}
	n, bytes, err := s.tombstoneSubtree(ctx, tx, collID, cur.Path, cur.Kind, c.SyncToken, now)
	if err != nil {
		return Resource{}, err
	}
	c.ResourceCount -= n
	c.BytesUsed -= bytes
	if c.ResourceCount < 0 {
		c.ResourceCount = 0
	}
	if c.BytesUsed < 0 {
		c.BytesUsed = 0
	}
	if err := s.advance(ctx, tx, c, now); err != nil {
		return Resource{}, err
	}
	at := s.now()
	ms := []Mutation{{
		Event: EventResourceDeleted, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: cur.ResourceID, Kind: cur.Kind, Path: cur.Path, ETag: cur.ETag, Size: cur.Size, ContentType: cur.ContentType,
		ModSeq: c.SyncToken, At: at,
	}}
	for _, r := range below {
		if r.Kind == KindFile {
			ms = append(ms, fileMutation(EventResourceDeleted, c, r, at))
		}
	}
	if err := s.emitTxAll(ctx, tx, ms); err != nil {
		return Resource{}, fmt.Errorf("drive: enqueue mutation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Resource{}, fmt.Errorf("drive: commit: %w", err)
	}
	s.emitPostAll(ctx, ms)
	cur.ModSeq = c.SyncToken
	cur.Deleted = true
	cur.DeletedAt = parseTime(now)
	return cur, nil
}

// checkMoveCopyPaths normalizes and validates a move/copy pair.
func checkMoveCopyPaths(from, to string) (string, string, error) {
	from, err := NormalizePath(from)
	if err != nil {
		return "", "", err
	}
	to, err = NormalizePath(to)
	if err != nil {
		return "", "", err
	}
	if from == "" || to == "" {
		return "", "", fmt.Errorf("%w: the root cannot be moved or copied", ErrBadPath)
	}
	if from == to || IsInside(to, from) {
		return "", "", ErrCycle
	}
	return from, to, nil
}

// replaceDestination tombstones whatever is live at `to` when overwrite is
// set (RFC 4918 §9.8.4 / §9.9.3: a DELETE with Depth infinity first), or
// refuses with ErrExists. The counters on c are adjusted. Returns the
// deleted events the replacement owes: the destination's own, then one per
// file below it — the caller emits them before its own event.
func (s *Store) replaceDestination(ctx context.Context, tx txq, c *collState, to string, overwrite bool, now string) ([]Mutation, error) {
	dst, dlive, err := s.getResourceQ(ctx, tx, c.ID, to, false, true)
	if err != nil {
		return nil, err
	}
	if !dlive {
		return nil, nil
	}
	if !overwrite {
		return nil, ErrExists
	}
	var below []Resource
	if dst.Kind == KindDir {
		if below, err = s.listQ(ctx, tx, c.ID, to); err != nil {
			return nil, err
		}
	}
	n, bytes, err := s.tombstoneSubtree(ctx, tx, c.ID, to, dst.Kind, c.SyncToken, now)
	if err != nil {
		return nil, err
	}
	c.ResourceCount -= n
	c.BytesUsed -= bytes
	at := s.now()
	ms := []Mutation{{
		Event: EventResourceDeleted, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: dst.ResourceID, Kind: dst.Kind, Path: dst.Path, ETag: dst.ETag, Size: dst.Size, ContentType: dst.ContentType,
		ModSeq: c.SyncToken, At: at,
	}}
	for _, r := range below {
		if r.Kind == KindFile {
			ms = append(ms, fileMutation(EventResourceDeleted, *c, r, at))
		}
	}
	return ms, nil
}

// Move renames the resource at from to to, keeping its resource id (and,
// for a directory, every descendant's). ErrCycle when to is from or inside
// it; ErrNoParent when to's parent is missing; ErrExists when to is live
// and overwrite is false — with overwrite the destination subtree is
// deleted first (its deleted events come first too). Then one moved event
// for the root and one per file below it.
func (s *Store) Move(ctx context.Context, collID, from, to string, overwrite bool) (Resource, error) {
	from, to, err := checkMoveCopyPaths(from, to)
	if err != nil {
		return Resource{}, err
	}
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Resource{}, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := s.lockCollection(ctx, tx, collID)
	if err != nil {
		return Resource{}, err
	}
	src, live, err := s.getResourceQ(ctx, tx, collID, from, false, true)
	if err != nil {
		return Resource{}, err
	}
	if !live {
		return Resource{}, ErrNotFound
	}
	if err := s.requireParent(ctx, tx, collID, to); err != nil {
		return Resource{}, err
	}
	c.SyncToken++
	replaced, err := s.replaceDestination(ctx, tx, &c, to, overwrite, now)
	if err != nil {
		return Resource{}, err
	}
	var below []Resource
	if src.Kind == KindDir {
		if below, err = s.listQ(ctx, tx, collID, from); err != nil {
			return Resource{}, err
		}
		// Descendants: rewrite the `from` prefix of path and parent_path
		// (a descendant's parent_path is `from` or `from/…`), shift depth.
		// SUBSTR(x, n) is 1-based and `||` concatenates on both engines.
		if _, err := tx.ExecContext(ctx, s.rb(`
			UPDATE drive_resources
			   SET path = CAST(? AS TEXT) || SUBSTR(path, ?), parent_path = CAST(? AS TEXT) || SUBSTR(parent_path, ?), depth = depth + ?,
			       modseq = ?, updated_at = ?
			 WHERE collection_id = ? AND deleted_at IS NULL AND path LIKE ?`+likeEscape),
			to, len(from)+1, to, len(from)+1, Depth(to)-Depth(from), c.SyncToken, now, collID, LikePrefix(from)); err != nil {
			if s.dialect.IsUniqueViolationGeneric(err) {
				return Resource{}, ErrExists
			}
			return Resource{}, fmt.Errorf("drive: move subtree: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE drive_resources SET path = ?, parent_path = ?, depth = ?, modseq = ?, updated_at = ? WHERE resource_id = ?`),
		to, ParentOf(to), Depth(to), c.SyncToken, now, src.ResourceID); err != nil {
		if s.dialect.IsUniqueViolationGeneric(err) {
			return Resource{}, ErrExists
		}
		return Resource{}, fmt.Errorf("drive: move resource: %w", err)
	}
	if err := s.advance(ctx, tx, c, now); err != nil {
		return Resource{}, err
	}
	at := s.now()
	ms := append(replaced, Mutation{
		Event: EventResourceMoved, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: src.ResourceID, Kind: src.Kind, Path: to, FromPath: from, ETag: src.ETag, Size: src.Size, ContentType: src.ContentType,
		ModSeq: c.SyncToken, At: at,
	})
	for _, r := range below {
		if r.Kind == KindFile {
			fm := fileMutation(EventResourceMoved, c, r, at)
			fm.FromPath, fm.Path = r.Path, to+r.Path[len(from):]
			ms = append(ms, fm)
		}
	}
	if err := s.emitTxAll(ctx, tx, ms); err != nil {
		return Resource{}, fmt.Errorf("drive: enqueue mutation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Resource{}, fmt.Errorf("drive: commit: %w", err)
	}
	s.emitPostAll(ctx, ms)
	src.Path, src.ParentPath, src.Depth, src.ModSeq, src.UpdatedAt = to, ParentOf(to), Depth(to), c.SyncToken, parseTime(now)
	return src, nil
}

// copied is one object duplicated for a Copy, keyed by the source row.
type copied struct {
	srcKey string
	newID  string
	newKey string
}

// Copy duplicates the resource at from (a directory with everything below
// it) to to. Every copy gets a new resource id; the bytes are copied
// server-side when the object store can (ObjectCopier), else streamed.
// Bytes first: the objects are duplicated from a snapshot of the source
// rows, then the index transaction re-reads the source and inserts the
// rows — a source row that changed in between is copied again inline; a
// failed transaction leaves orphans the sweeper reclaims. ErrCycle /
// ErrNoParent / ErrExists as Move. Events: the replaced destination's
// (if any), then created for the copied root, then created per copied file.
func (s *Store) Copy(ctx context.Context, collID, from, to string, overwrite bool) (Resource, error) {
	from, to, err := checkMoveCopyPaths(from, to)
	if err != nil {
		return Resource{}, err
	}
	coll, ok, err := s.GetCollectionByID(ctx, collID)
	if err != nil {
		return Resource{}, err
	}
	if !ok {
		return Resource{}, ErrNotFound
	}
	// Snapshot + object copies, outside the tx.
	root, live, err := s.getResourceQ(ctx, s.db, collID, from, false, false)
	if err != nil {
		return Resource{}, err
	}
	if !live {
		return Resource{}, ErrNotFound
	}
	if err := s.requireParent(ctx, s.db, collID, to); err != nil {
		return Resource{}, err
	}
	snapshot := []Resource{root}
	if root.Kind == KindDir {
		desc, err := s.List(ctx, collID, ListOpts{Path: from, Recursive: true})
		if err != nil {
			return Resource{}, err
		}
		snapshot = append(snapshot, desc...)
	}
	copies := map[string]copied{}
	var newKeys []string
	dup := func(r Resource) (copied, error) {
		cp := copied{srcKey: r.ObjectKey, newID: NewResourceID()}
		if r.Kind == KindFile {
			cp.newKey = objectKey(coll.Tenant, collID, cp.newID, newVersionID())
			if err := copyObject(ctx, s.objects, r.ObjectKey, cp.newKey); err != nil {
				return copied{}, fmt.Errorf("drive: copy object %s: %w", r.ObjectKey, err)
			}
			newKeys = append(newKeys, cp.newKey)
		}
		return cp, nil
	}
	for _, r := range snapshot {
		cp, err := dup(r)
		if err != nil {
			s.discard(ctx, newKeys...)
			return Resource{}, err
		}
		copies[r.ResourceID] = cp
	}

	// Metadata.
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		s.discard(ctx, newKeys...)
		return Resource{}, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	fail := func(err error) (Resource, error) {
		_ = tx.Rollback()
		s.discard(ctx, newKeys...)
		return Resource{}, err
	}

	c, err := s.lockCollection(ctx, tx, collID)
	if err != nil {
		return fail(err)
	}
	src, live, err := s.getResourceQ(ctx, tx, collID, from, false, true)
	if err != nil {
		return fail(err)
	}
	if !live {
		return fail(ErrNotFound)
	}
	if err := s.requireParent(ctx, tx, collID, to); err != nil {
		return fail(err)
	}
	rows := []Resource{src}
	if src.Kind == KindDir {
		desc, err := s.listQ(ctx, tx, collID, from)
		if err != nil {
			return fail(err)
		}
		rows = append(rows, desc...)
	}
	var addBytes, addCount int64
	for _, r := range rows {
		addCount++
		addBytes += r.Size
	}
	if s.limits.MaxCollectionBytes > 0 && c.BytesUsed+addBytes > s.limits.MaxCollectionBytes {
		return fail(ErrQuota)
	}
	if s.limits.MaxResources > 0 && c.ResourceCount+addCount > s.limits.MaxResources {
		return fail(ErrQuota)
	}
	c.SyncToken++
	replaced, err := s.replaceDestination(ctx, tx, &c, to, overwrite, now)
	if err != nil {
		return fail(err)
	}
	at := s.now()
	var newRoot Resource
	var files []Mutation // created, one per copied file below the root
	for _, r := range rows {
		cp, have := copies[r.ResourceID]
		if !have || cp.srcKey != r.ObjectKey {
			// Changed (or appeared) since the snapshot: copy its bytes now.
			var err error
			if cp, err = dup(r); err != nil {
				return fail(err)
			}
		}
		np := to + r.Path[len(from):]
		var key any
		if r.Kind == KindFile {
			key = cp.newKey
		}
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO drive_resources (`+resourceCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`),
			cp.newID, collID, np, ParentOf(np), Depth(np), r.Kind, key, r.Size, r.ContentType, r.ETag, c.SyncToken, now, now); err != nil {
			if s.dialect.IsUniqueViolationGeneric(err) {
				return fail(ErrExists)
			}
			return fail(fmt.Errorf("drive: insert copy: %w", err))
		}
		if r.ResourceID == src.ResourceID {
			newRoot = r
			newRoot.ResourceID, newRoot.Path, newRoot.ParentPath, newRoot.Depth = cp.newID, np, ParentOf(np), Depth(np)
			newRoot.ObjectKey, newRoot.ModSeq = cp.newKey, c.SyncToken
			newRoot.CreatedAt, newRoot.UpdatedAt = parseTime(now), parseTime(now)
		} else if r.Kind == KindFile {
			files = append(files, fileMutation(EventResourceCreated, c,
				Resource{ResourceID: cp.newID, Path: np, ETag: r.ETag, Size: r.Size, ContentType: r.ContentType}, at))
		}
	}
	c.ResourceCount += addCount
	c.BytesUsed += addBytes
	if err := s.advance(ctx, tx, c, now); err != nil {
		return fail(err)
	}
	ms := append(replaced, Mutation{
		Event: EventResourceCreated, Tenant: c.Tenant, CollectionID: c.ID, Collection: c.Name,
		ResourceID: newRoot.ResourceID, Kind: newRoot.Kind, Path: to, ETag: newRoot.ETag, Size: newRoot.Size, ContentType: newRoot.ContentType,
		ModSeq: c.SyncToken, At: at,
	})
	ms = append(ms, files...)
	if err := s.emitTxAll(ctx, tx, ms); err != nil {
		return fail(fmt.Errorf("drive: enqueue mutation: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("drive: commit: %w", err))
	}
	s.emitPostAll(ctx, ms)
	return newRoot, nil
}

// listQ is the recursive live listing below p inside a tx (Copy's
// authoritative re-read).
func (s *Store) listQ(ctx context.Context, q txq, collID, p string) ([]Resource, error) {
	rows, err := q.QueryContext(ctx, s.rb(`
		SELECT `+resourceCols+` FROM drive_resources
		 WHERE collection_id = ? AND deleted_at IS NULL AND path LIKE ?`+likeEscape+` ORDER BY path`), collID, LikePrefix(p))
	if err != nil {
		return nil, fmt.Errorf("drive: list subtree: %w", err)
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("drive: scan resource: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
