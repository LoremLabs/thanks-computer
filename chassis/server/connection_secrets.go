package server

import (
	"context"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// connectionSecrets is the tenant secret store as the connection service
// reads a provider's client credentials through: in the declaring stack's
// scope first, the tenant's otherwise (the resolver's own rule).
type connectionSecrets struct{ r *secrets.Resolver }

func (s connectionSecrets) Materialize(ctx context.Context, tenant, stack, name string) ([]byte, error) {
	cleartext, _, err := s.r.MaterializeForOpSlug(ctx, tenant, stack, name)
	return cleartext, err
}
