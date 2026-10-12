package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/calldecl"
	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// callEntry is one declared entry of a tenant's stacks: the stack, the
// entry's name, and the declaration. Err is set instead of Decl when the
// active row does not resolve or parse.
type callEntry struct {
	Stack string
	Name  string
	Decl  *capdecl.Decl
	Err   error
}

// callDeclSource resolves a call entry's declaration the way capDeclSource
// resolves a capability's — the row from the dbcache snapshot, the bytes
// inline or from the CAS, the parse cached by content hash — with one
// difference: an entry is looked up WITHIN a stack, by (tenant, stack,
// name), because entry names are a stack's own. It is what `WITH entry`
// resolves through (processor.CallLookup) and what txco://calls/list reads.
type callDeclSource struct {
	dbc   *dbcache.DbCache
	fcas  filecas.Store
	cache sync.Map // content hash → *capdecl.Decl
}

func (s *callDeclSource) resolve(ctx context.Context, content, hash string) (*capdecl.Decl, error) {
	if hash == "" {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}
	if c, ok := s.cache.Load(hash); ok {
		return c.(*capdecl.Decl), nil
	}
	body := []byte(content)
	if len(body) == 0 {
		if s.fcas == nil {
			return nil, errors.New("no content store; cannot resolve declaration")
		}
		b, err := s.fcas.Get(ctx, hash)
		if err != nil {
			return nil, err
		}
		body = b
	}
	d, err := calldecl.ParseDecl(body)
	if err != nil {
		return nil, err
	}
	s.cache.Store(hash, d)
	return d, nil
}

// Lookup resolves (tenant slug, stack, entry name) → the stack's ACTIVE
// version's declaration, or calldecl.ErrNotDeclared.
func (s *callDeclSource) Lookup(ctx context.Context, tenant, stack, name string) (*capdecl.Decl, error) {
	if !calldecl.ValidName(name) {
		return nil, calldecl.ErrNotDeclared
	}
	var content, hash string
	err := s.dbc.Snapshot().QueryRowContext(ctx, `
		SELECT sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ? AND sf.path = ?`,
		tenant, stack, calldecl.DeclPath(name)).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, calldecl.ErrNotDeclared
	}
	if err != nil {
		return nil, fmt.Errorf("resolve entry %q of %s: %w", name, stack, err)
	}
	d, rerr := s.resolve(ctx, content, hash)
	if rerr != nil {
		return nil, fmt.Errorf("entry %q of %s: %w", name, stack, rerr)
	}
	return d, nil
}

// List is every entry the tenant's active stacks declare, sorted by stack
// then name. A row that does not resolve or parse is listed with its Err;
// it does not fail the list.
func (s *callDeclSource) List(ctx context.Context, tenant string) ([]callEntry, error) {
	rows, err := s.dbc.Snapshot().QueryContext(ctx, `
		SELECT s.name, sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND sf.path LIKE ?
		 ORDER BY s.name, sf.path`,
		tenant, calldecl.Dir+"/%")
	if err != nil {
		return nil, fmt.Errorf("list call entries: %w", err)
	}
	type row struct{ stack, path, content, hash string }
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.stack, &r.path, &r.content, &r.hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list call entries: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list call entries: %w", err)
	}
	rows.Close()

	out := make([]callEntry, 0, len(found))
	for _, r := range found {
		name := calldecl.Name(r.path)
		if name == "" {
			continue // not a declaration
		}
		e := callEntry{Stack: r.stack, Name: name}
		e.Decl, e.Err = s.resolve(ctx, r.content, r.hash)
		out = append(out, e)
	}
	return out, nil
}
