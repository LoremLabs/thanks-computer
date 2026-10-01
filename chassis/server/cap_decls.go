package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// capEntry is one capability of a tenant's catalogue: its name, the stack
// that declares it, and the declaration. Err is set instead of Decl when
// the active row does not resolve or parse.
type capEntry struct {
	Name  string
	Stack string
	Decl  *capdecl.Decl
	Err   error
}

// capDeclSource resolves a capability declaration the way sandboxDeclSource
// resolves a sandbox's — the row from the dbcache snapshot (in-memory
// mirror — no disk on the hot path), the bytes inline (single node) or from
// the CAS (fingerprint-only fleet rows), the parse cached by content hash —
// with one difference: a capability is looked up by NAME across the
// tenant's active stacks, not within one. The activation gate
// (admin.deepValidateCaps) keeps a name to one active stack; the ORDER BY
// makes the answer deterministic if a duplicate ever slips past it.
//
// It is what the capability inlet routes by (capgw.Decls) and what
// txco://caps/list reads.
type capDeclSource struct {
	dbc   *dbcache.DbCache
	fcas  filecas.Store
	cache sync.Map // content hash → *capdecl.Decl
}

// resolve turns a row's (content, hash) into a parsed declaration.
func (s *capDeclSource) resolve(ctx context.Context, content, hash string) (*capdecl.Decl, error) {
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
	d, err := capdecl.ParseDecl(body)
	if err != nil {
		return nil, err
	}
	s.cache.Store(hash, d)
	return d, nil
}

// Lookup resolves (tenant slug, capability name) → the declaring stack and
// its ACTIVE version's declaration, or capdecl.ErrNotDeclared.
func (s *capDeclSource) Lookup(ctx context.Context, tenant, name string) (string, *capdecl.Decl, error) {
	if !sandbox.ValidCapability(name) {
		return "", nil, capdecl.ErrNotDeclared
	}
	var stack, content, hash string
	err := s.dbc.Snapshot().QueryRowContext(ctx, `
		SELECT s.name, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND sf.path = ?
		 ORDER BY s.name
		 LIMIT 1`,
		tenant, capdecl.DeclPath(name)).Scan(&stack, &content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, capdecl.ErrNotDeclared
	}
	if err != nil {
		return "", nil, fmt.Errorf("resolve capability %q: %w", name, err)
	}
	d, rerr := s.resolve(ctx, content, hash)
	if rerr != nil {
		return "", nil, fmt.Errorf("capability %q (stack %s): %w", name, stack, rerr)
	}
	return stack, d, nil
}

// List is the tenant's catalogue: every capability its active stacks
// declare, sorted by name. A row that does not resolve or parse is listed
// with its Err; it does not fail the list.
func (s *capDeclSource) List(ctx context.Context, tenant string) ([]capEntry, error) {
	rows, err := s.dbc.Snapshot().QueryContext(ctx, `
		SELECT s.name, sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND sf.path LIKE ?
		 ORDER BY sf.path, s.name`,
		tenant, capdecl.Dir+"/%")
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	type row struct{ stack, path, content, hash string }
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.stack, &r.path, &r.content, &r.hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list capabilities: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	rows.Close()

	out := make([]capEntry, 0, len(found))
	seen := map[string]bool{}
	for _, r := range found {
		name := capdecl.Name(r.path)
		if name == "" || seen[name] {
			continue // not a declaration; or a duplicate Lookup would not pick
		}
		seen[name] = true
		e := capEntry{Name: name, Stack: r.stack}
		e.Decl, e.Err = s.resolve(ctx, r.content, r.hash)
		out = append(out, e)
	}
	return out, nil
}
