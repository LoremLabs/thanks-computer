package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// sandboxDecls is what txco://delegate/mint reads a sandbox through.
type sandboxDecls interface {
	// Lookup resolves (tenant slug, stack, name) → the ACTIVE version's
	// declaration, or sandbox.ErrNotDeclared.
	Lookup(ctx context.Context, tenant, stack, name string) (*sandbox.Decl, error)
	// Names lists the sandboxes the stack's active version declares, sorted.
	Names(ctx context.Context, tenant, stack string) ([]string, error)
}

// sandboxDeclSource resolves a sandbox declaration the way outletDeclSource
// resolves an outlet's: the row from the dbcache snapshot (in-memory
// mirror — no disk on the hot path), the bytes inline (single node) or from
// the CAS (fingerprint-only fleet rows), the parse cached by content hash.
type sandboxDeclSource struct {
	dbc   *dbcache.DbCache
	fcas  filecas.Store
	cache sync.Map // content hash → *sandbox.Decl
}

func (s *sandboxDeclSource) Lookup(ctx context.Context, tenant, stack, name string) (*sandbox.Decl, error) {
	if !sandbox.ValidName(name) {
		return nil, sandbox.ErrNotDeclared
	}
	var content, hash string
	err := s.dbc.Snapshot().QueryRowContext(ctx, `
		SELECT sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ? AND sf.path = ?`,
		tenant, stack, sandbox.DeclPath(name)).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sandbox.ErrNotDeclared
	}
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox %q: %w", name, err)
	}
	if hash == "" {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}
	if c, ok := s.cache.Load(hash); ok {
		return c.(*sandbox.Decl), nil
	}
	body := []byte(content)
	if len(body) == 0 {
		if s.fcas == nil {
			return nil, fmt.Errorf("resolve sandbox %q: no content store; cannot resolve declaration", name)
		}
		if body, err = s.fcas.Get(ctx, hash); err != nil {
			return nil, fmt.Errorf("resolve sandbox %q: %w", name, err)
		}
	}
	d, perr := sandbox.ParseDecl(body)
	if perr != nil {
		return nil, fmt.Errorf("sandbox %q: %w", name, perr)
	}
	s.cache.Store(hash, d)
	return d, nil
}

func (s *sandboxDeclSource) Names(ctx context.Context, tenant, stack string) ([]string, error) {
	rows, err := s.dbc.Snapshot().QueryContext(ctx, `
		SELECT sf.path
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ? AND sf.path LIKE ?
		 ORDER BY sf.path`,
		tenant, stack, sandbox.Dir+"/%")
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("list sandboxes: %w", err)
		}
		if n := sandbox.Name(p); n != "" {
			names = append(names, n)
		}
	}
	return names, rows.Err()
}
