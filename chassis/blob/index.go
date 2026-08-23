package blob

import (
	"context"
	"sort"
	"time"
)

// NameRow is one mutable name → sha pointer with its metadata. Name is the
// key (not stored in the value); Permissions are the strings an op must cover
// to touch the name at all (read, repoint, delete).
//
// SeededBy / SeededSHA are the pack bookkeeping: the stack whose BLOBS/ tree
// last shipped this name, and the content it shipped. A runtime put keeps
// both (it only moves SHA256), so a seeded name that the runtime repointed
// is detectable as DRIFT — the same idea as a remote branch having moved
// past your last push. `txco data apply` refuses to push over drift unless
// forced; `txco data pull` brings the live content into the tree first.
type NameRow struct {
	Name        string    `json:"-"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	Permissions []string  `json:"permissions,omitempty"`
	SeededBy    string    `json:"seeded_by,omitempty"`
	SeededSHA   string    `json:"seeded_sha,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Drifted reports whether a pack-seeded name was repointed at runtime since
// the pack last shipped it: the live content is no longer what the tree says.
func (r NameRow) Drifted() bool {
	return r.SeededSHA != "" && r.SHA256 != r.SeededSHA
}

// ShaRow is the tenant's OWNERSHIP record for a hash: it exists iff this
// tenant has put (or seeded) those bytes. It is the tenant fence over the
// global-by-hash CAS — `existed` on put and every by-sha read consult it,
// never the CAS itself, so one tenant can never learn whether another holds
// a given document.
type ShaRow struct {
	SHA256      string    `json:"-"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
}

// ListOpts windows a name listing: names with Prefix (byte-wise), sorted,
// strictly after the After cursor, at most Limit rows (Limit <= 0 = all —
// for internal callers; the op clamps). Every listing reads the tenant's
// whole index (the KV backing has no sub-namespace prefix scan); a SQL
// index can lift that behind the same interface.
type ListOpts struct {
	Prefix string
	After  string
	Limit  int
}

// ListPage is a window of rows (never nil) plus the cursor for the next
// call ("" when exhausted).
type ListPage struct {
	Names []NameRow
	Next  string
}

// Index is the name plane: mutable pointers + the tenant's sha ownership.
// Every method is tenant-scoped by the trusted request tenant (the slug
// processor.TenantScope carries). Implementations need not be transactional
// — callers order writes CAS → sha row → name row so every crash point
// self-heals (see docs/advanced/blobs.md).
type Index interface {
	GetName(ctx context.Context, tenant, name string) (NameRow, bool, error)
	PutName(ctx context.Context, tenant string, row NameRow) error
	DeleteName(ctx context.Context, tenant, name string) error
	ListNames(ctx context.Context, tenant string, opts ListOpts) (ListPage, error)

	GetSha(ctx context.Context, tenant, sha string) (ShaRow, bool, error)
	// PutShaIfAbsent records ownership once; created is false when the row
	// already existed (its FirstSeen is preserved).
	PutShaIfAbsent(ctx context.Context, tenant string, row ShaRow) (created bool, err error)
}

func sortStrings(s []string) { sort.Strings(s) }
