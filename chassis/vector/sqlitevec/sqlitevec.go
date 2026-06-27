// Package sqlitevec is the bundled vector.Store backend: SQLite + sqlite-vec,
// in a dedicated database file (never the config-reloaded runtime DB).
//
// v1 is exact, not approximate. Each collection is a normal table holding a
// serialized float32 BLOB per item plus a JSON metadata sidecar; search
// brute-force-ranks with sqlite-vec's vec_distance_cosine() scalar function
// and pushes the metadata filter into a parameterized WHERE before ranking.
// No vec0 virtual table, no ANN, no recall tuning — fine into the ~100k-vector
// range, and swappable for vec0/ANN or pgvector behind the vector.Store
// interface when HA or scale demands it.
package sqlitevec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // sqlite3 driver

	"github.com/loremlabs/thanks-computer/chassis/vector"
)

func init() {
	// Register sqlite-vec as an auto-extension so every sqlite3 connection
	// (this store's pool) exposes vec_*() functions. Process-global and
	// idempotent; harmless to other DBs, which simply gain unused functions.
	sqlite_vec.Auto()
}

// Store is the sqlite-vec-backed vector.Store.
type Store struct {
	db *sql.DB

	mu    sync.RWMutex
	cache map[string]cachedColl // key: tenant\x00name
}

type cachedColl struct {
	coll  vector.Collection
	table string
}

// New opens (creating if absent) the vector database at dbPath and verifies
// sqlite-vec is loaded. The DSN mirrors the chassis runtime convention (WAL +
// busy_timeout, no cache=shared).
func New(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlitevec: create dir %s: %w", dir, err)
		}
	}
	dsn := "file:" + dbPath + "?mode=rwc&_journal_mode=WAL&_busy_timeout=15000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: open %s: %w", dbPath, err)
	}

	var vecVersion string
	if err := db.QueryRow("select vec_version()").Scan(&vecVersion); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitevec: sqlite-vec not loaded (vec_version failed): %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS collections (
			tenant          TEXT NOT NULL,
			name            TEXT NOT NULL,
			embedding_model TEXT NOT NULL,
			dimensions      INTEGER NOT NULL,
			metric          TEXT NOT NULL,
			table_name      TEXT NOT NULL,
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (tenant, name)
		)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitevec: create collections table: %w", err)
	}

	return &Store{db: db, cache: map[string]cachedColl{}}, nil
}

func cacheKey(tenant, name string) string { return tenant + "\x00" + name }

// tableNameFor derives a deterministic, injection-safe table name from
// (tenant, collection). Hex only — never interpolates user input into SQL.
func tableNameFor(tenant, name string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + name))
	return "vi_" + hex.EncodeToString(h[:12])
}

func (s *Store) EnsureCollection(ctx context.Context, tenant string, c vector.Collection) error {
	if c.Name == "" {
		return &vector.InvalidArgError{Reason: "collection name required"}
	}
	metric := c.Metric
	if metric == "" {
		metric = vector.MetricCosine
	}
	if metric != vector.MetricCosine {
		return &vector.InvalidArgError{Reason: fmt.Sprintf("unsupported metric %q (v1 supports cosine)", metric)}
	}

	existing, found, err := s.DescribeCollection(ctx, tenant, c.Name)
	if err != nil {
		return err
	}
	if found {
		if c.Dimensions != 0 && existing.Dimensions != c.Dimensions {
			return &vector.CollectionConflictError{Collection: c.Name, Field: "dimensions",
				Existing: fmt.Sprint(existing.Dimensions), Requested: fmt.Sprint(c.Dimensions)}
		}
		if c.EmbeddingModel != "" && existing.EmbeddingModel != c.EmbeddingModel {
			return &vector.CollectionConflictError{Collection: c.Name, Field: "embedding_model",
				Existing: existing.EmbeddingModel, Requested: c.EmbeddingModel}
		}
		if existing.Metric != metric {
			return &vector.CollectionConflictError{Collection: c.Name, Field: "metric",
				Existing: string(existing.Metric), Requested: string(metric)}
		}
		return nil
	}

	if c.Dimensions <= 0 {
		return &vector.InvalidArgError{Reason: "dimensions must be > 0 to create a collection"}
	}
	table := tableNameFor(tenant, c.Name)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO collections (tenant, name, embedding_model, dimensions, metric, table_name) VALUES (?,?,?,?,?,?)`,
		tenant, c.Name, c.EmbeddingModel, c.Dimensions, string(metric), table); err != nil {
		return fmt.Errorf("sqlitevec: register collection: %w", err)
	}
	// table is a hex-only identifier (tableNameFor) — safe to interpolate.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id        TEXT PRIMARY KEY,
			embedding BLOB NOT NULL,
			metadata  TEXT NOT NULL DEFAULT '{}',
			text      TEXT NOT NULL DEFAULT ''
		)`, table)); err != nil {
		return fmt.Errorf("sqlitevec: create items table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	s.cache[cacheKey(tenant, c.Name)] = cachedColl{
		coll:  vector.Collection{Name: c.Name, EmbeddingModel: c.EmbeddingModel, Dimensions: c.Dimensions, Metric: metric},
		table: table,
	}
	s.mu.Unlock()
	return nil
}

func (s *Store) DescribeCollection(ctx context.Context, tenant, name string) (vector.Collection, bool, error) {
	s.mu.RLock()
	if cc, ok := s.cache[cacheKey(tenant, name)]; ok {
		s.mu.RUnlock()
		return cc.coll, true, nil
	}
	s.mu.RUnlock()

	var (
		model  string
		dims   int
		metric string
		table  string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT embedding_model, dimensions, metric, table_name FROM collections WHERE tenant=? AND name=?`,
		tenant, name).Scan(&model, &dims, &metric, &table)
	if err == sql.ErrNoRows {
		return vector.Collection{}, false, nil
	}
	if err != nil {
		return vector.Collection{}, false, err
	}
	coll := vector.Collection{Name: name, EmbeddingModel: model, Dimensions: dims, Metric: vector.Metric(metric)}
	s.mu.Lock()
	s.cache[cacheKey(tenant, name)] = cachedColl{coll: coll, table: table}
	s.mu.Unlock()
	return coll, true, nil
}

// lookup returns the cached collection + table name, or CollectionNotFoundError.
func (s *Store) lookup(ctx context.Context, tenant, name string) (vector.Collection, string, error) {
	if _, found, err := s.DescribeCollection(ctx, tenant, name); err != nil {
		return vector.Collection{}, "", err
	} else if !found {
		return vector.Collection{}, "", &vector.CollectionNotFoundError{Tenant: tenant, Collection: name}
	}
	s.mu.RLock()
	cc := s.cache[cacheKey(tenant, name)]
	s.mu.RUnlock()
	return cc.coll, cc.table, nil
}

func (s *Store) Upsert(ctx context.Context, tenant, collection string, items []vector.Item) (int, error) {
	coll, table, err := s.lookup(ctx, tenant, collection)
	if err != nil {
		return 0, err
	}
	for i := range items {
		if items[i].ID == "" {
			return 0, &vector.InvalidArgError{Reason: "item id required"}
		}
		if len(items[i].Vector) != coll.Dimensions {
			return 0, &vector.DimensionMismatchError{Collection: collection, Want: coll.Dimensions, Got: len(items[i].Vector)}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (id, embedding, metadata, text) VALUES (?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET embedding=excluded.embedding, metadata=excluded.metadata, text=excluded.text`, table))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for _, it := range items {
		blob, serr := sqlite_vec.SerializeFloat32(it.Vector)
		if serr != nil {
			return 0, fmt.Errorf("sqlitevec: serialize vector for %q: %w", it.ID, serr)
		}
		md := it.Metadata
		if md == nil {
			md = map[string]any{}
		}
		mdJSON, merr := json.Marshal(md)
		if merr != nil {
			return 0, fmt.Errorf("sqlitevec: marshal metadata for %q: %w", it.ID, merr)
		}
		if _, err := stmt.ExecContext(ctx, it.ID, blob, string(mdJSON), it.Text); err != nil {
			return 0, fmt.Errorf("sqlitevec: upsert %q: %w", it.ID, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) Search(ctx context.Context, tenant, collection string, query []float32, limit int, filter vector.Filter) ([]vector.Match, error) {
	coll, table, err := s.lookup(ctx, tenant, collection)
	if err != nil {
		return nil, err
	}
	if len(query) != coll.Dimensions {
		return nil, &vector.DimensionMismatchError{Collection: collection, Want: coll.Dimensions, Got: len(query)}
	}
	if limit <= 0 {
		limit = 10
	}
	qblob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: serialize query: %w", err)
	}

	where, whereArgs, err := buildWhere(filter)
	if err != nil {
		return nil, err
	}

	// The single ? in the SELECT binds first, then WHERE args, then LIMIT.
	q := fmt.Sprintf("SELECT id, vec_distance_cosine(embedding, ?) AS dist, metadata, text FROM %s%s ORDER BY dist LIMIT ?", table, where)
	args := make([]any, 0, len(whereArgs)+2)
	args = append(args, qblob)
	args = append(args, whereArgs...)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: search: %w", err)
	}
	defer rows.Close()

	var out []vector.Match
	for rows.Next() {
		var (
			id     string
			dist   float64
			mdJSON string
			text   string
		)
		if err := rows.Scan(&id, &dist, &mdJSON, &text); err != nil {
			return nil, err
		}
		m := vector.Match{ID: id, Distance: dist, Score: 1 - dist, Text: text}
		if mdJSON != "" {
			_ = json.Unmarshal([]byte(mdJSON), &m.Metadata)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, tenant, collection string, ids []string) (int, error) {
	_, table, err := s.lookup(ctx, tenant, collection)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", table, placeholders), args...)
	if err != nil {
		return 0, fmt.Errorf("sqlitevec: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) ListIDs(ctx context.Context, tenant, collection string) ([]string, error) {
	_, table, err := s.lookup(ctx, tenant, collection)
	if err != nil {
		return nil, err
	}
	// table is a hex-only identifier (tableNameFor) — safe to interpolate.
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("SELECT id FROM %s", table))
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ListCollections(ctx context.Context, tenant string) ([]vector.Collection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, embedding_model, dimensions, metric FROM collections WHERE tenant=? ORDER BY name`, tenant)
	if err != nil {
		return nil, fmt.Errorf("sqlitevec: list collections: %w", err)
	}
	defer rows.Close()
	var out []vector.Collection
	for rows.Next() {
		var (
			c      vector.Collection
			metric string
		)
		if err := rows.Scan(&c.Name, &c.EmbeddingModel, &c.Dimensions, &metric); err != nil {
			return nil, err
		}
		c.Metric = vector.Metric(metric)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DropCollection(ctx context.Context, tenant, name string) (int, error) {
	_, found, err := s.DescribeCollection(ctx, tenant, name)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil // missing collection → nothing to drop
	}
	s.mu.RLock()
	table := s.cache[cacheKey(tenant, name)].table
	s.mu.RUnlock()

	var n int
	// table is a hex-only identifier (tableNameFor) — safe to interpolate.
	_ = s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", table)); err != nil {
		return 0, fmt.Errorf("sqlitevec: drop items table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM collections WHERE tenant=? AND name=?`, tenant, name); err != nil {
		return 0, fmt.Errorf("sqlitevec: deregister collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	delete(s.cache, cacheKey(tenant, name))
	s.mu.Unlock()
	return n, nil
}

func (s *Store) Close() error { return s.db.Close() }

// buildWhere turns a Filter into a parameterized WHERE clause over the JSON
// metadata column. Field names are bound as json_extract path parameters
// (never interpolated), so arbitrary author-supplied keys are injection-safe.
func buildWhere(f vector.Filter) (string, []any, error) {
	if len(f.Conditions) == 0 {
		return "", nil, nil
	}
	var clauses []string
	var args []any
	for _, c := range f.Conditions {
		if c.Field == "" || strings.ContainsRune(c.Field, 0) {
			return "", nil, &vector.InvalidArgError{Reason: "filter field name required"}
		}
		// `id` filters the item identity (the id column) — that's what the
		// "exclude already-seen" pattern needs. Every other field reads the
		// JSON metadata sidecar, with the path bound (never interpolated).
		col := "json_extract(metadata, ?)"
		var colArgs []any
		if c.Field == "id" {
			col = "id"
		} else {
			colArgs = []any{"$." + c.Field}
		}
		switch c.Op {
		case vector.OpEq:
			clauses = append(clauses, col+" = ?")
			args = append(args, colArgs...)
			args = append(args, c.Value)
		case vector.OpGte, vector.OpLte, vector.OpGt, vector.OpLt:
			cmp := map[vector.Op]string{vector.OpGte: ">=", vector.OpLte: "<=", vector.OpGt: ">", vector.OpLt: "<"}[c.Op]
			clauses = append(clauses, col+" "+cmp+" ?")
			args = append(args, colArgs...)
			args = append(args, c.Value)
		case vector.OpIn:
			vals := asSlice(c.Value)
			if len(vals) == 0 {
				clauses = append(clauses, "0") // IN () matches nothing
				continue
			}
			ph := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
			clauses = append(clauses, col+" IN ("+ph+")")
			args = append(args, colArgs...)
			args = append(args, vals...)
		case vector.OpNotIn:
			vals := asSlice(c.Value)
			if len(vals) == 0 {
				clauses = append(clauses, "1") // NOT IN () excludes nothing
				continue
			}
			ph := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
			// Absent metadata fields (json_extract → NULL) pass not_in, the
			// intuitive reading ("value is not one of these"). The id column
			// is never NULL, so the IS NULL arm is simply inert for it.
			clauses = append(clauses, "("+col+" IS NULL OR "+col+" NOT IN ("+ph+"))")
			args = append(args, colArgs...)
			args = append(args, colArgs...)
			args = append(args, vals...)
		default:
			return "", nil, &vector.InvalidArgError{Reason: fmt.Sprintf("unsupported filter op %q", c.Op)}
		}
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return []any{v}
}
