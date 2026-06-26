// Package pgvector is the HA/scale vector.Store backend: PostgreSQL + the
// pgvector extension, for deployments where multiple chassis nodes share one
// durable vector store (the same trigger that sends the auth DB to Postgres).
//
// **No Postgres driver is imported here.** Like the auth registry, this package
// uses database/sql with the driver *name* "pgx"; the actual driver is
// blank-imported by the overlay/production build, never by the open-core
// chassis (SQLite stays the only in-tree driver). pgvector is entirely
// server-side SQL — vectors travel as text literals cast to ::vector — so no
// pgx-specific Go API is needed.
//
// **v1 is exact, not approximate.** Each collection is a table with a
// vector(N) column; search ranks with the cosine-distance operator (`<=>`)
// over an exact scan and pushes metadata/id filters into the WHERE before
// ranking. That already delivers the shared-store HA value. An HNSW index per
// collection is the one-line scale optimisation for >~100k vectors and is
// deliberately deferred (correct-not-optimized).
package pgvector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// Store is the pgvector-backed vector.Store.
type Store struct {
	db *sql.DB

	mu    sync.RWMutex
	cache map[string]cachedColl // key: tenant\x00name
}

type cachedColl struct {
	coll  vector.Collection
	table string
}

// New connects to the Postgres at dsn (driver name "pgx"; the driver must be
// registered by the build — the open-core chassis does not register it),
// ensures the pgvector extension and the collections metadata table exist.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("pgvector: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgvector: ping (is the pgx driver registered + db reachable?): %w", err)
	}
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgvector: CREATE EXTENSION vector (image must ship pgvector; role must be allowed): %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS vector_collections (
			tenant          text NOT NULL,
			name            text NOT NULL,
			embedding_model text NOT NULL,
			dimensions      integer NOT NULL,
			metric          text NOT NULL,
			table_name      text NOT NULL,
			created_at      timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant, name)
		)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgvector: create collections table: %w", err)
	}
	return &Store{db: db, cache: map[string]cachedColl{}}, nil
}

func cacheKey(tenant, name string) string { return tenant + "\x00" + name }

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
		`INSERT INTO vector_collections (tenant, name, embedding_model, dimensions, metric, table_name) VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, c.Name, c.EmbeddingModel, c.Dimensions, string(metric), table); err != nil {
		return fmt.Errorf("pgvector: register collection: %w", err)
	}
	// table + dim are chassis-derived (hex name, validated int) — safe to format.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id        text PRIMARY KEY,
			embedding vector(%d) NOT NULL,
			metadata  jsonb NOT NULL DEFAULT '{}'::jsonb,
			text      text NOT NULL DEFAULT ''
		)`, table, c.Dimensions)); err != nil {
		return fmt.Errorf("pgvector: create items table: %w", err)
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
		`SELECT embedding_model, dimensions, metric, table_name FROM vector_collections WHERE tenant=$1 AND name=$2`,
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
		`INSERT INTO %s (id, embedding, metadata, text) VALUES ($1, $2::vector, $3::jsonb, $4)
		 ON CONFLICT (id) DO UPDATE SET embedding=excluded.embedding, metadata=excluded.metadata, text=excluded.text`, table))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for _, it := range items {
		md := it.Metadata
		if md == nil {
			md = map[string]any{}
		}
		mdJSON, merr := json.Marshal(md)
		if merr != nil {
			return 0, fmt.Errorf("pgvector: marshal metadata for %q: %w", it.ID, merr)
		}
		if _, err := stmt.ExecContext(ctx, it.ID, encodeVector(it.Vector), string(mdJSON), it.Text); err != nil {
			return 0, fmt.Errorf("pgvector: upsert %q: %w", it.ID, err)
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

	args := []any{encodeVector(query)} // $1 (query vector, referenced twice)
	where, wargs, nextIdx, werr := buildWherePG(filter, 2)
	if werr != nil {
		return nil, werr
	}
	args = append(args, wargs...)
	limitIdx := nextIdx
	args = append(args, limit)

	q := fmt.Sprintf(
		"SELECT id, (embedding <=> $1::vector) AS distance, metadata::text, text FROM %s%s ORDER BY embedding <=> $1::vector LIMIT $%d",
		table, where, limitIdx)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgvector: search: %w", err)
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
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", table, strings.Join(ph, ",")), args...)
	if err != nil {
		return 0, fmt.Errorf("pgvector: delete: %w", err)
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
		return nil, fmt.Errorf("pgvector: list ids: %w", err)
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
		`SELECT name, embedding_model, dimensions, metric FROM vector_collections WHERE tenant=$1 ORDER BY name`, tenant)
	if err != nil {
		return nil, fmt.Errorf("pgvector: list collections: %w", err)
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
		return 0, fmt.Errorf("pgvector: drop items table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vector_collections WHERE tenant=$1 AND name=$2`, tenant, name); err != nil {
		return 0, fmt.Errorf("pgvector: deregister collection: %w", err)
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

// encodeVector renders a float32 slice as pgvector's text literal "[a,b,c]"
// (bound as a parameter, then cast ::vector server-side).
func encodeVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// buildWherePG builds a parameterized WHERE over jsonb metadata + the id
// column, numbering placeholders from start. The metadata KEY is bound (via
// `metadata->>$n`), never interpolated, so author-supplied keys are
// injection-safe. Returns the clause, its args, and the next free placeholder.
func buildWherePG(f vector.Filter, start int) (string, []any, int, error) {
	if len(f.Conditions) == 0 {
		return "", nil, start, nil
	}
	idx := start
	var args []any
	next := func(v any) int { args = append(args, v); i := idx; idx++; return i }

	var clauses []string
	for _, c := range f.Conditions {
		if c.Field == "" || strings.ContainsRune(c.Field, 0) {
			return "", nil, 0, &vector.InvalidArgError{Reason: "filter field name required"}
		}
		// `id` filters the item identity; every other field reads jsonb metadata
		// with the key bound as a parameter.
		col := "id"
		if c.Field != "id" {
			col = fmt.Sprintf("metadata->>$%d", next(c.Field))
		}
		switch c.Op {
		case vector.OpEq:
			clauses = append(clauses, fmt.Sprintf("%s = $%d", col, next(textOf(c.Value))))
		case vector.OpGte, vector.OpLte, vector.OpGt, vector.OpLt:
			cmp := map[vector.Op]string{vector.OpGte: ">=", vector.OpLte: "<=", vector.OpGt: ">", vector.OpLt: "<"}[c.Op]
			clauses = append(clauses, fmt.Sprintf("(%s)::numeric %s $%d::numeric", col, cmp, next(textOf(c.Value))))
		case vector.OpIn:
			vals := asSlice(c.Value)
			if len(vals) == 0 {
				clauses = append(clauses, "false")
				continue
			}
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = fmt.Sprintf("$%d", next(textOf(v)))
			}
			clauses = append(clauses, fmt.Sprintf("%s IN (%s)", col, strings.Join(ph, ",")))
		case vector.OpNotIn:
			vals := asSlice(c.Value)
			if len(vals) == 0 {
				clauses = append(clauses, "true")
				continue
			}
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = fmt.Sprintf("$%d", next(textOf(v)))
			}
			// Absent metadata fields (->> NULL) pass not_in; id is never NULL.
			clauses = append(clauses, fmt.Sprintf("(%s IS NULL OR %s NOT IN (%s))", col, col, strings.Join(ph, ",")))
		default:
			return "", nil, 0, &vector.InvalidArgError{Reason: fmt.Sprintf("unsupported filter op %q", c.Op)}
		}
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, idx, nil
}

// textOf renders a filter value for text comparison against metadata->> (which
// yields text) or for ::numeric casts. Matches what jsonb ->> produces for the
// same JSON scalar (e.g. 12 → "12", true → "true").
func textOf(v any) string { return fmt.Sprintf("%v", v) }

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return []any{v}
}
