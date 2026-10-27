package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// connectionDeclSource is what the connection service reads declarations
// through (connection.Decls): a provider by NAME across the tenant's active
// stacks (the activation gate keeps a name to one active stack, as for
// capabilities; the ORDER BY makes the answer deterministic if a duplicate
// ever slips past it), and a stack's consumer declaration for a provider
// within that stack. Rows come from the dbcache snapshot, bytes inline or
// from the CAS, parses cached by content hash — the capDeclSource treatment.
type connectionDeclSource struct {
	dbc       *dbcache.DbCache
	fcas      filecas.Store
	providers sync.Map // content hash → *connection.Provider
	consumers sync.Map // content hash → *connection.Consumer
}

func (s *connectionDeclSource) body(ctx context.Context, content, hash string) ([]byte, string, error) {
	if hash == "" {
		sum := sha256.Sum256([]byte(content))
		hash = hex.EncodeToString(sum[:])
	}
	body := []byte(content)
	if len(body) == 0 {
		if s.fcas == nil {
			return nil, hash, errors.New("no content store; cannot resolve declaration")
		}
		b, err := s.fcas.Get(ctx, hash)
		if err != nil {
			return nil, hash, err
		}
		body = b
	}
	return body, hash, nil
}

// Provider resolves (tenant slug, name) → the declaring stack and its
// active declaration, or connection.ErrNotDeclared.
func (s *connectionDeclSource) Provider(ctx context.Context, tenant, name string) (string, *connection.Provider, error) {
	if !connection.ValidName(name) {
		return "", nil, connection.ErrNotDeclared
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
		tenant, connection.ProviderPath(name)).Scan(&stack, &content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, connection.ErrNotDeclared
	}
	if err != nil {
		return "", nil, fmt.Errorf("resolve provider %q: %w", name, err)
	}
	body, hash, err := s.body(ctx, content, hash)
	if err != nil {
		return "", nil, fmt.Errorf("provider %q (stack %s): %w", name, stack, err)
	}
	if c, ok := s.providers.Load(hash); ok {
		return stack, c.(*connection.Provider), nil
	}
	p, perr := connection.ParseProvider(body)
	if perr != nil {
		return "", nil, fmt.Errorf("provider %q (stack %s): %w", name, stack, perr)
	}
	s.providers.Store(hash, p)
	return stack, p, nil
}

// Consumer resolves (tenant slug, stack, provider) → the stack's active
// consumer declaration, or connection.ErrNotDeclared. A declaration whose
// `provider` field names another provider than its file does is an error,
// not a match (the activation gate refuses it; this is the backstop).
func (s *connectionDeclSource) Consumer(ctx context.Context, tenant, stack, provider string) (*connection.Consumer, error) {
	if !connection.ValidName(provider) {
		return nil, connection.ErrNotDeclared
	}
	var content, hash string
	err := s.dbc.Snapshot().QueryRowContext(ctx, `
		SELECT sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ? AND sf.path = ?`,
		tenant, stack, connection.ConsumerPath(provider)).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connection.ErrNotDeclared
	}
	if err != nil {
		return nil, fmt.Errorf("resolve connection %q of stack %s: %w", provider, stack, err)
	}
	body, hash, err := s.body(ctx, content, hash)
	if err != nil {
		return nil, fmt.Errorf("connection %q of stack %s: %w", provider, stack, err)
	}
	var c *connection.Consumer
	if cached, ok := s.consumers.Load(hash); ok {
		c = cached.(*connection.Consumer)
	} else {
		parsed, perr := connection.ParseConsumer(body)
		if perr != nil {
			return nil, fmt.Errorf("connection %q of stack %s: %w", provider, stack, perr)
		}
		s.consumers.Store(hash, parsed)
		c = parsed
	}
	if c.Provider != provider {
		return nil, fmt.Errorf("connection %q of stack %s: the declaration names provider %q", provider, stack, c.Provider)
	}
	return c, nil
}
