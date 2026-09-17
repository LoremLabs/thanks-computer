package drive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

var collectionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// ValidCollectionName reports whether name is a collection name: a URL
// segment of up to 128 chars (ops use short lowercase names; a pony's
// collection is its slug).
func ValidCollectionName(name string) bool {
	return collectionNameRE.MatchString(name) && name != "." && name != ".."
}

// Collection is a live drive_collections row.
type Collection struct {
	ID            string
	Tenant        string
	Name          string
	SyncToken     int64
	BytesUsed     int64
	ResourceCount int64
	// Policy is what a WebDAV CLIENT may do, by subtree (policy.go). The
	// stack's own txco://drive/* ops are never subject to it.
	Policy    Policy
	CreatedAt time.Time
	UpdatedAt time.Time
}

const collectionCols = `id, tenant, name, sync_token, bytes_used, resource_count, created_at, updated_at, policy`

func scanCollection(r rowScanner) (Collection, error) {
	var c Collection
	var created, updated string
	var policy sql.NullString
	if err := r.Scan(&c.ID, &c.Tenant, &c.Name, &c.SyncToken, &c.BytesUsed, &c.ResourceCount, &created, &updated, &policy); err != nil {
		return Collection{}, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	// A stored policy that no longer parses is dropped rather than fatal:
	// the head then allows, which is every collection's default. Validate()
	// at the write door is what keeps a bad policy from landing.
	c.Policy, _ = ParsePolicy(policy.String)
	return c, nil
}

// SetCollectionPolicy replaces a collection's client policy; an empty
// policy clears it (every verb allowed again).
func (s *Store) SetCollectionPolicy(ctx context.Context, tenant, name string, p Policy) (Collection, error) {
	if err := p.Validate(); err != nil {
		return Collection{}, err
	}
	res, err := s.db.ExecContext(ctx, s.rb(`
		UPDATE drive_collections SET policy = ?, updated_at = ?
		 WHERE tenant = ? AND name = ? AND deleted_at IS NULL`),
		p.String(), fmtTime(s.now()), tenant, strings.TrimSpace(name))
	if err != nil {
		return Collection{}, fmt.Errorf("drive: set collection policy: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return Collection{}, ErrNotFound
	}
	c, found, err := s.GetCollection(ctx, tenant, name)
	if err != nil {
		return Collection{}, err
	}
	if !found {
		return Collection{}, ErrNotFound
	}
	return c, nil
}

// EnsureCollection returns the tenant's live collection of that name,
// creating it when absent. A soft-deleted collection of the same name is
// NOT resurrected — its resources are tombstoned under the old id and a new
// collection starts empty. created reports a fresh row.
func (s *Store) EnsureCollection(ctx context.Context, tenant, name string) (Collection, bool, error) {
	name = strings.TrimSpace(name)
	if tenant == "" {
		return Collection{}, false, errors.New("drive: empty tenant")
	}
	if !ValidCollectionName(name) {
		return Collection{}, false, fmt.Errorf("drive: collection name %q must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)", name)
	}
	// Two attempts: a concurrent creator can win the INSERT between our
	// SELECT and ours (there is no row to lock yet); on Postgres the failed
	// statement aborts the transaction, so the recovery is a fresh one that
	// finds theirs.
	for attempt := 0; ; attempt++ {
		c, created, err := s.ensureCollectionOnce(ctx, tenant, name)
		if err != nil && attempt == 0 && s.dialect.IsUniqueViolationGeneric(err) {
			continue
		}
		if err != nil {
			return Collection{}, false, err
		}
		return c, created, nil
	}
}

func (s *Store) ensureCollectionOnce(ctx context.Context, tenant, name string) (Collection, bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Collection{}, false, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := scanCollection(tx.QueryRowContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections
		 WHERE tenant = ? AND name = ? AND deleted_at IS NULL`+lock(s)), tenant, name))
	created := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id := "dc_" + hxid.NewTimeSort().String()
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO drive_collections (`+collectionCols+`, deleted_at)
			VALUES (?, ?, ?, 0, 0, 0, ?, ?, '', NULL)`),
			id, tenant, name, now, now); err != nil {
			if s.dialect.IsUniqueViolationGeneric(err) {
				return Collection{}, false, err // caller retries
			}
			return Collection{}, false, fmt.Errorf("drive: insert collection: %w", err)
		}
		created = true
		c, err = scanCollection(tx.QueryRowContext(ctx, s.rb(`SELECT `+collectionCols+` FROM drive_collections WHERE id = ?`), id))
		if err != nil {
			return Collection{}, false, fmt.Errorf("drive: reread collection: %w", err)
		}
	case err != nil:
		return Collection{}, false, fmt.Errorf("drive: lookup collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Collection{}, false, fmt.Errorf("drive: commit: %w", err)
	}
	return c, created, nil
}

// GetCollection returns the tenant's live collection of that name.
func (s *Store) GetCollection(ctx context.Context, tenant, name string) (Collection, bool, error) {
	c, err := scanCollection(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections
		 WHERE tenant = ? AND name = ? AND deleted_at IS NULL`), tenant, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, false, nil
	}
	if err != nil {
		return Collection{}, false, fmt.Errorf("drive: get collection: %w", err)
	}
	return c, true, nil
}

// GetCollectionByID returns a live collection by id.
func (s *Store) GetCollectionByID(ctx context.Context, id string) (Collection, bool, error) {
	return s.getCollectionByIDQ(ctx, s.db, id, false)
}

func (s *Store) getCollectionByIDQ(ctx context.Context, q txq, id string, forUpdate bool) (Collection, bool, error) {
	sfx := ""
	if forUpdate {
		sfx = lock(s)
	}
	c, err := scanCollection(q.QueryRowContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections WHERE id = ? AND deleted_at IS NULL`+sfx), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, false, nil
	}
	if err != nil {
		return Collection{}, false, fmt.Errorf("drive: get collection: %w", err)
	}
	return c, true, nil
}

// ListCollections returns the tenant's live collections by name.
func (s *Store) ListCollections(ctx context.Context, tenant string) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections
		 WHERE tenant = ? AND deleted_at IS NULL ORDER BY name`), tenant)
	if err != nil {
		return nil, fmt.Errorf("drive: list collections: %w", err)
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, fmt.Errorf("drive: scan collection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCollection soft-deletes the tenant's collection of that name and
// tombstones its live resources. Refuses (ErrNotEmpty) when the collection
// holds live resources unless force. Returns false (no error) when there is
// no such live collection. No mutation events are emitted: the collection
// itself is the subscription a consumer keys on.
func (s *Store) DeleteCollection(ctx context.Context, tenant, name string, force bool) (bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("drive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := scanCollection(tx.QueryRowContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections
		 WHERE tenant = ? AND name = ? AND deleted_at IS NULL`+lock(s)), tenant, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("drive: lock collection: %w", err)
	}
	if c.ResourceCount > 0 && !force {
		return false, ErrNotEmpty
	}
	token := c.SyncToken + 1
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE drive_resources SET deleted_at = ?, modseq = ?, updated_at = ?
		 WHERE collection_id = ? AND deleted_at IS NULL`), now, token, now, c.ID); err != nil {
		return false, fmt.Errorf("drive: tombstone resources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE drive_collections SET deleted_at = ?, sync_token = ?, bytes_used = 0, resource_count = 0, updated_at = ?
		 WHERE id = ?`), now, token, now, c.ID); err != nil {
		return false, fmt.Errorf("drive: delete collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("drive: commit: %w", err)
	}
	return true, nil
}

// collState is what lockCollection reads: the counters every mutation
// checks and advances.
type collState struct {
	Collection
}

// lockCollection reads (and on Postgres row-locks) the live collection's
// counters inside a write tx.
func (s *Store) lockCollection(ctx context.Context, tx txq, id string) (collState, error) {
	c, err := scanCollection(tx.QueryRowContext(ctx, s.rb(`
		SELECT `+collectionCols+` FROM drive_collections WHERE id = ? AND deleted_at IS NULL`+lock(s)), id))
	if errors.Is(err, sql.ErrNoRows) {
		return collState{}, ErrNotFound
	}
	if err != nil {
		return collState{}, fmt.Errorf("drive: lock collection: %w", err)
	}
	return collState{Collection: c}, nil
}

// advance writes the collection's new sync_token and counters.
func (s *Store) advance(ctx context.Context, tx txq, c collState, now string) error {
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE drive_collections SET sync_token = ?, bytes_used = ?, resource_count = ?, updated_at = ?
		 WHERE id = ?`), c.SyncToken, c.BytesUsed, c.ResourceCount, now, c.ID); err != nil {
		return fmt.Errorf("drive: advance sync token: %w", err)
	}
	return nil
}
