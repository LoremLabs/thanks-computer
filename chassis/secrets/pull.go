package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PullPolicy is a secret's own answer to one question: may work that was
// dispatched somewhere else ASK for this secret and be handed it?
//
// It is set by whoever stores the secret, and it is one input to the
// decision, never the decision itself: a request also needs a run grant that
// names the secret and a principal that holds a standing grant to release
// it, and the tenant's `_grant` stack may change the answer either way.
//
// A secret used where it is stored — a rule names it and the chassis
// materializes it into that one op — is not a pull, and no policy applies.
type PullPolicy string

const (
	// PullNone: no work may be handed the secret. The default, and every
	// secret's behavior before the policy existed.
	PullNone PullPolicy = "none"
	// PullReviewed: work on a reviewed node may be handed it.
	PullReviewed PullPolicy = "reviewed"
	// PullAny: work on any node may be handed it, including one that runs
	// code nobody has read. For a secret whose loss is tolerable.
	PullAny PullPolicy = "any"
)

// ErrInvalidPull signals a pull policy that is none of the three.
var ErrInvalidPull = errors.New(`secrets: invalid pull policy (want "none", "reviewed" or "any")`)

// ParsePullPolicy validates a policy as written by an operator.
func ParsePullPolicy(s string) (PullPolicy, error) {
	switch p := PullPolicy(s); p {
	case PullNone, PullReviewed, PullAny:
		return p, nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidPull, s)
}

// Admits reports whether the policy lets a node of the given class be handed
// the secret. Anything that is not a known policy admits nothing, so a row
// written by a later release, or by hand, fails closed.
func (p PullPolicy) Admits(reviewedNode bool) bool {
	switch p {
	case PullAny:
		return true
	case PullReviewed:
		return reviewedNode
	}
	return false
}

// ResolveForRelease finds the secret a release would hand over and returns
// its metadata — never its value. It resolves as MaterializeSecretForOp
// does (stack-scoped first, then tenant-wide), but always against the store:
// the pull policy decides whether a secret leaves this chassis, so a change
// to it must take effect on the next request, not when a cache expires.
func (s *Store) ResolveForRelease(ctx context.Context, tenantID, stack, name string) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if stack != "" {
		meta, err := s.lookupMetadataExact(ctx, tenantID, &stack, name)
		if err == nil || !errors.Is(err, ErrSecretNotFound) {
			return meta, err
		}
	}
	return s.lookupMetadataExact(ctx, tenantID, nil, name)
}

// DecryptResolved returns the cleartext of the secret meta names, at the
// version meta names — the one the decision was made about. The caller
// zeroes it.
func (s *Store) DecryptResolved(ctx context.Context, meta *SecretMetadata) ([]byte, error) {
	if meta == nil {
		return nil, ErrSecretNotFound
	}
	if s.MK == nil {
		return nil, fmt.Errorf("secrets: no master key configured")
	}
	return s.decryptActive(ctx, meta)
}

// UpdateSecretPull sets a secret's pull policy, in its exact scope. It
// changes nothing else: the value, its versions and the description stay.
func (s *Store) UpdateSecretPull(ctx context.Context,
	tenantID string, stack *string, name string, pull PullPolicy,
) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if _, err := ParsePullPolicy(string(pull)); err != nil {
		return nil, err
	}
	meta0, err := s.lookupMetadataExact(ctx, tenantID, stack, name)
	if err != nil {
		return nil, err
	}

	// Lock the parent row and re-read under the lock, as
	// UpdateSecretDescription does and for its reason: the fleet artifact
	// carries every parent column, and one built from a pre-lock read would
	// revert a concurrent rotation on the nodes that apply it.
	tx, err := s.dia().BeginWrite(ctx, s.DB)
	if err != nil {
		return nil, fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer tx.Rollback()

	var lockedID string
	if err := tx.QueryRowContext(ctx,
		s.rb(`SELECT secret_id FROM tenant_secrets WHERE secret_id = ?`+s.dia().LockClause()),
		meta0.SecretID).Scan(&lockedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSecretNotFound
		}
		return nil, fmt.Errorf("secrets: lock secret: %w", err)
	}
	meta, err := s.lookupMetadataExactVia(ctx, tx, tenantID, stack, name)
	if err != nil {
		return nil, err
	}
	meta.Pull = pull

	inTx, err := s.publishUpsert(ctx, tenantID, nil, parentRowMap(meta))
	if err != nil {
		return nil, fmt.Errorf("secrets: fleet publish: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.rb(`
		UPDATE tenant_secrets
		SET pull = ?
		WHERE secret_id = ? AND revoked_at IS NULL`),
		string(pull), meta.SecretID)
	if err != nil {
		return nil, fmt.Errorf("secrets: update pull policy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrSecretNotFound
	}
	if inTx != nil {
		if err := inTx(tx); err != nil {
			return nil, fmt.Errorf("secrets: fleet outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("secrets: commit: %w", err)
	}
	s.mat.flush()
	return s.lookupMetadataExact(ctx, tenantID, stack, name)
}
