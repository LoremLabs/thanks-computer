package tenants

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"regexp"
	"strings"

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
func EnsureSystemHostnameTx(ctx context.Context, tx *sql.Tx, tenantID, stack, suffix, now string) (string, error) {
	if suffix == "" || tenantID == "" || stack == "" {
		return "", nil
	}
	if !strings.HasPrefix(suffix, ".") {
		suffix = "." + suffix
	}

	// Idempotency: reuse the existing active managed row if present.
	var existing string
	err := tx.QueryRowContext(ctx,
		`SELECT hostname FROM tenant_hostnames
		  WHERE tenant_id = ? AND stack = ? AND created_by = ?
		    AND revoked_at IS NULL
		  LIMIT 1`,
		tenantID, stack, SystemStructuredHostCreatedBy).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}

	for attempt := 0; attempt < ensureMintRetries; attempt++ {
		canon, ok := CanonicalizeHost(MintHandle(stack) + suffix)
		if !ok || !IsValidHostname(canon) {
			// A generated label is always valid, so this means the
			// configured suffix is malformed — regenerating won't help.
			return "", errors.New("tenants: structured-host suffix yields an invalid hostname: " + suffix)
		}
		id := "thn_" + hxid.New().String()
		_, ierr := tx.ExecContext(ctx,
			`INSERT INTO tenant_hostnames
			     (id, hostname, tenant_id, stack, created_at, created_by, verified_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, canon, tenantID, stack, now, SystemStructuredHostCreatedBy, now)
		if ierr == nil {
			return canon, nil
		}
		if isUniqueViolation(ierr) {
			continue // freak collision on the random part — regenerate
		}
		return "", ierr
	}
	return "", errors.New("tenants: could not mint a unique structured hostname after retries")
}
