package tenants

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// SystemStructuredHostCreatedBy is the created_by sentinel stamped on
// chassis-minted hostname rows. It marks a row as managed (so list/UX
// can flag it, `hostnames remove` can refuse it, and a config-disable
// won't touch operator vanity rows) and is the idempotency key for
// "does this stack already have an auto-minted hostname?".
const SystemStructuredHostCreatedBy = "system:structured-host"

// ensureMintRetries bounds the regenerate-on-collision loop. The random
// part is 48 bits, so a collision is astronomically unlikely; the loop
// exists only so a freak duplicate can't fail an activation.
const ensureMintRetries = 5

// Auto-minted structured-hostname handles. See
// internal docs/todo-structured-stack-hostnames.md. MintHandle returns just the
// leftmost DNS *label* ("<hint>-<rand>"); the caller appends the
// configured suffix and validates the whole host with IsValidHostname.
// Routing never parses the hint — the random part is the only
// identity-bearing piece (the handle is not an authorization boundary).

const (
	handleHintMaxLen = 30 // keep the label well under the 63-char DNS limit
	handleRandBytes  = 6  // 48 bits of entropy → 10 base32 chars
)

// handleNonLabel matches any run of characters that isn't a DNS-label
// alphanumeric (input is lowercased first). Stack names may contain
// '/', '_', uppercase, etc.; all collapse to a single '-'.
var handleNonLabel = regexp.MustCompile(`[^a-z0-9]+`)

// base32 RFC-4648, no padding. Its alphabet is A–Z2–7; lowercased that
// is a–z2–7 — all valid DNS-label characters, no '=' padding to strip.
var handleB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// MintHandle builds the label for a stack's auto-minted hostname.
// Hinted form ("<sanitized-stack>-<rand>") per the design doc; for the
// opaque alternative, return randLabel() alone (one-line swap).
func MintHandle(stack string) string {
	r := randLabel()
	hint := sanitizeHint(stack)
	if hint == "" {
		return r
	}
	return hint + "-" + r
}

// sanitizeHint reduces an arbitrary stack name to a safe, cosmetic DNS
// label fragment: lowercase, non-alnum runs → '-', no leading/trailing
// or doubled '-', truncated. May return "" (e.g. stack was all
// punctuation) — MintHandle then uses the random part alone.
func sanitizeHint(stack string) string {
	s := strings.ToLower(strings.TrimSpace(stack))
	s = handleNonLabel.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > handleHintMaxLen {
		s = strings.Trim(s[:handleHintMaxLen], "-")
	}
	return s
}

// SanitizeSlugHint exposes sanitizeHint for callers outside the package
// (e.g. the cloud-enroll handler deriving a suggested tenant slug from an
// OIDC subject). Same rules: lowercase, non-alnum runs → '-', trimmed,
// truncated; may return "".
func SanitizeSlugHint(s string) string { return sanitizeHint(s) }

// RandLabel exposes randLabel for callers that need a fresh
// always-alphanumeric DNS label (e.g. a fallback tenant slug when no
// usable hint can be derived).
func RandLabel() string { return randLabel() }

// StackLabel is the deterministic leftmost DNS label for a stack within
// a per-tenant delegated DNS zone — the sanitized stack name with NO
// random suffix (the zone is tenant-scoped, so the label is already
// unique). Shared by the synthesized DNS pattern
// (chassis/server/personality/dns) and the activation-path routing-host
// mint so the resolved name and the routing hostname never diverge.
// Returns "" for an all-punctuation stack name; callers skip those.
func StackLabel(stack string) string {
	return sanitizeHint(stack)
}

// randLabel is the identity-bearing part: crypto/rand → lowercased
// base32, always alphanumeric so it's a valid label start/end on its
// own.
func randLabel() string {
	var b [handleRandBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails in practice; if it ever does, return
		// an obviously-bad-but-valid label so the caller's collision
		// retry / activation-resilience path handles it rather than
		// emitting an empty label.
		return "x"
	}
	return strings.ToLower(handleB32.EncodeToString(b[:]))
}

// EnsureSystemHostnameTx makes sure the (tenantID, stack) pair has an
// active chassis-minted hostname row, creating one if absent. It runs
// inside the caller's activation transaction (tx) so the row is atomic
// with the version flip; the caller owns commit and dbcache reload.
//
// Idempotent: re-activations reuse the existing row (the URL never
// churns). Returns the canonical hostname (without scheme). suffix is
// the configured apex, e.g. ".stacks.example.com" or ".localhost" — a
// missing leading dot is tolerated. An empty suffix means the feature
// is off → ("", nil), a no-op (the caller also guards this).
func EnsureSystemHostnameTx(ctx context.Context, tx *sql.Tx, tenantID, stack, suffix, now string, d registry.Dialect) (string, error) {
	if suffix == "" || tenantID == "" || stack == "" {
		return "", nil
	}
	d = orSQLite(d)
	if !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}

	// Idempotency: reuse the existing active managed row for this (tenant, stack)
	// if present. Reused on the initial check AND after a mint conflict below.
	lookupExisting := func() (string, bool, error) {
		var h string
		err := tx.QueryRowContext(ctx,
			d.Rebind(`SELECT hostname FROM tenant_hostnames
			  WHERE tenant_id = ? AND stack = ? AND created_by = ?
			    AND revoked_at IS NULL
			  LIMIT 1`),
			tenantID, stack, SystemStructuredHostCreatedBy).Scan(&h)
		switch {
		case err == nil:
			return h, true, nil
		case errors.Is(err, sql.ErrNoRows):
			return "", false, nil
		default:
			return "", false, err
		}
	}
	if h, found, err := lookupExisting(); err != nil {
		return "", err
	} else if found {
		return h, nil
	}

	// Per-host DKIM keypair: each structured host signs `d=<host>` with its OWN
	// key, and the dns head publishes `txco._domainkey.<host>` — so sending
	// reputation is isolated per host (one bad stack can't poison the shared
	// suffix). Generated once here; reused across label-collision retries.
	priv, pub, gerr := GenerateDKIM()
	if gerr != nil {
		return "", gerr
	}

	for attempt := 0; attempt < ensureMintRetries; attempt++ {
		canon, ok := CanonicalizeHost(MintHandle(stack) + suffix)
		if !ok || !IsValidHostname(canon) {
			// A generated label is always valid, so this means the
			// configured suffix is malformed — regenerating won't help.
			return "", errors.New("tenants: structured-host suffix yields an invalid hostname: " + suffix)
		}
		id := "thn_" + hxid.New().String()
		// Wrap the INSERT in a SAVEPOINT so a unique-violation is recoverable
		// without poisoning the enclosing activation tx: on Postgres a 23505
		// aborts the whole tx until ROLLBACK TO SAVEPOINT; on SQLite the savepoint
		// is real but the recover path never fires (a constraint error there is
		// localized). RunInSavepoint returns the INSERT error for us to classify.
		ierr := registry.RunInSavepoint(ctx, tx, "ehs", func() error {
			_, e := tx.ExecContext(ctx,
				d.Rebind(`INSERT INTO tenant_hostnames
				     (id, hostname, tenant_id, stack, created_at, created_by, verified_at,
				      dkim_selector, dkim_private_pem, dkim_public_b64)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				id, canon, tenantID, stack, now, SystemStructuredHostCreatedBy, now,
				DKIMSelector, priv, pub)
			return e
		})
		if ierr == nil {
			return canon, nil
		}
		if !d.IsUniqueViolationGeneric(ierr) {
			return "", ierr
		}
		// A conflict here is normally a freak random-label collision. But a
		// concurrent activation may have just minted THIS (tenant, stack) managed
		// host — re-check and adopt it rather than spinning or masking the error.
		if h, found, lerr := lookupExisting(); lerr != nil {
			return "", lerr
		} else if found {
			return h, nil
		}
		// No managed row yet → genuine label collision → regenerate.
	}
	return "", errors.New("tenants: could not mint a unique structured hostname after retries")
}
