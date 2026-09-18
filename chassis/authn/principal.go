// Package authn is the stack-plane identity store: the users, principal
// bindings and credentials a TENANT's stacks manage, and that the protocol
// heads authenticate against.
//
// Two planes share auth.db and nothing else. The ACCOUNT plane (chassis/auth:
// actors, keys, memberships, browser sessions) says who may administer a
// tenant. The STACK plane — this package — says who a tenant's own product
// knows about: its people, its ponies, its services. The tenant is the
// boundary between them. No row here references an actor and this package
// never imports the account plane's registry types (only its SQL dialect
// seam), so the two cannot be joined by accident.
//
// Three tables (db/schema/*/auth/0005_identity.sql):
//
//	users               durable state about a human
//	principal_bindings  identifier (an email, an OIDC subject) → principal
//	credentials         a revocable secret that authenticates AS a principal
//
// Every row records the stack that wrote it, and only that stack may change
// a principal's rows — see owner.go.
package authn

import (
	"fmt"
	"regexp"
	"strings"
)

// PrincipalKind is the part of a principal id before the colon. `user` is
// minted by the chassis; every other kind is named by the product.
type PrincipalKind string

// KindUser is the one kind the chassis mints: `user:usr_<hxid>`, backed by a
// users row. A product cannot name a user principal into existence.
const KindUser PrincipalKind = "user"

// Principal is the authenticated actor in TxCo: the thing a credential
// authenticates AS, and the thing authorization will be asked about.
//
// It is not the DAV notion of a principal (the `principal` types in the
// calendar, contacts and webdav heads name a DAV resource URL).
type Principal struct {
	// ID is the full `<kind>:<name>` form — what rows store and what
	// `_txc.principal` will carry: `user:usr_7Hq…`, `pony:paris`.
	ID   string
	Kind PrincipalKind
}

func (p Principal) String() string { return p.ID }

// IsZero reports whether p is the zero Principal (no one).
func (p Principal) IsZero() bool { return p.ID == "" }

// Name is the part of the id after the kind: `usr_7Hq…`, `paris`.
func (p Principal) Name() string {
	return strings.TrimPrefix(p.ID, string(p.Kind)+":")
}

// UserID returns the users-table id of a `user` principal.
func (p Principal) UserID() (string, bool) {
	if p.Kind != KindUser {
		return "", false
	}
	return p.Name(), true
}

// UserPrincipal is the principal of a users row.
func UserPrincipal(userID string) Principal {
	return Principal{ID: string(KindUser) + ":" + userID, Kind: KindUser}
}

var (
	kindRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// A product-chosen name: a slug, an address, an id. No colon (the
	// separator), no whitespace, nothing that needs quoting in a log line.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]{0,127}$`)
	// usr_ + an hxid (a base58 ULID).
	userIDRE = regexp.MustCompile(`^usr_[1-9A-HJ-NP-Za-km-z]{8,40}$`)
)

// ParsePrincipal validates a `<kind>:<name>` principal id. Ids are
// case-sensitive and compared as written: `usr_` ids are base58, and a
// product's own ids may be too.
func ParsePrincipal(s string) (Principal, error) {
	kind, name, ok := strings.Cut(s, ":")
	if !ok || !kindRE.MatchString(kind) {
		return Principal{}, fmt.Errorf("principal %q: want <kind>:<name>, kind in lowercase letters, digits, _ and -", s)
	}
	if PrincipalKind(kind) == KindUser {
		if !userIDRE.MatchString(name) {
			return Principal{}, fmt.Errorf("principal %q: a user principal is user:usr_<id>", s)
		}
	} else if !nameRE.MatchString(name) {
		return Principal{}, fmt.Errorf("principal %q: name may hold letters, digits and . _ @ + - (1-128 chars)", s)
	}
	return Principal{ID: s, Kind: PrincipalKind(kind)}, nil
}
