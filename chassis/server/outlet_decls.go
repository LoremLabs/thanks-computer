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
	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

// outletDeclSource resolves (tenant slug, stack, outlet) → the ACTIVE
// version's OUTLETS/<name>.yaml, reading the row from the dbcache snapshot
// (in-memory mirror — no disk on the hot path) and the bytes inline
// (single node) or from the CAS (fingerprint-only fleet rows). The parse is
// cached by content hash: a declaration is immutable per hash, so a
// redeploy simply introduces a new one. Mirrors resolveDataset.
type outletDeclSource struct {
	dbc   *dbcache.DbCache
	fcas  filecas.Store
	cache sync.Map // content hash → *outlet.Decl
}

type cachedDecl struct {
	decl *outlet.Decl
	hash string
}

// Lookup implements outlet.DeclSource.
func (s *outletDeclSource) Lookup(ctx context.Context, tenant, stack, name string) (*outlet.Decl, string, error) {
	if !outlet.ValidName(name) {
		return nil, "", outlet.ErrNotDeclared
	}
	var content, hash string
	err := s.dbc.Snapshot().QueryRowContext(ctx, `
		SELECT sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ? AND sf.path = ?`,
		tenant, stack, outlet.DeclPath(name)).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", outlet.ErrNotDeclared
	}
	if err != nil {
		return nil, "", fmt.Errorf("resolve outlet %q: %w", name, err)
	}
	if hash == "" {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}
	if c, ok := s.cache.Load(hash); ok {
		cd := c.(cachedDecl)
		return cd.decl, cd.hash, nil
	}
	body := []byte(content)
	if len(body) == 0 {
		if s.fcas == nil {
			return nil, "", fmt.Errorf("resolve outlet %q: no content store; cannot resolve declaration", name)
		}
		if body, err = s.fcas.Get(ctx, hash); err != nil {
			return nil, "", fmt.Errorf("resolve outlet %q: %w", name, err)
		}
	}
	// Activation validated the driver; nil skips that check here so a
	// binary built without the driver still reports it at the pool step.
	d, perr := outlet.ParseDecl(body, nil)
	if perr != nil {
		return nil, "", fmt.Errorf("outlet %q: %w", name, perr)
	}
	s.cache.Store(hash, cachedDecl{decl: d, hash: hash})
	return d, hash, nil
}
