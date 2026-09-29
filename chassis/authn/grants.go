package authn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// A standing grant says what a principal may EVER ask the chassis for: one
// row, one principal, one resource, the verbs it may use on it
// (db/schema/*/auth/0006_resource_grants.sql). It carries no condition, role,
// inheritance or time limit — a grant is a row a person can read aloud.
//
// A standing grant is the outer bound, not the decision. What one piece of
// work may ask for is a run grant (rungrants.go), which can only name what
// its principal holds here; whether one request goes ahead is decided when
// it is made.
//
// Grants follow the creator-stack rule (owner.go): only the stack that
// manages a principal may grant to it, and the principal must already exist.

// ResourceKind is the kind of thing a grant names.
type ResourceKind string

const (
	// ResourceCapability is a named capability; its verb is VerbInvoke.
	ResourceCapability ResourceKind = "capability"
	// ResourceSecret is a tenant secret, by name; its verb is VerbRelease:
	// the secret itself may be handed to the principal's work.
	ResourceSecret ResourceKind = "secret"
)

// The verbs built.
const (
	VerbInvoke  = "invoke"
	VerbRelease = "release"
)

// MaxGrants bounds the LIVE grants one principal may hold, so a loop cannot
// write rows without end.
const MaxGrants = 1024

// ErrTooManyGrants: the principal already holds MaxGrants live grants.
var ErrTooManyGrants = errors.New("authn: too many grants")

type resourceRule struct {
	name  *regexp.Regexp
	verbs []string
	want  string // the grammar, for an error message
}

// resourceRules is every kind a grant may name. A new kind is one entry.
var resourceRules = map[ResourceKind]resourceRule{
	ResourceCapability: {
		name:  regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}(\.[a-z][a-z0-9_-]{0,63}){0,7}$`),
		verbs: []string{VerbInvoke},
		want:  "lowercase words joined by dots, such as crm.lookup",
	},
	ResourceSecret: {
		// The secret store's own name grammar (chassis/secrets).
		name:  regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`),
		verbs: []string{VerbRelease},
		want:  "a secret name: a letter, then letters, digits and _ (1-128 chars)",
	},
}

// Resource names one thing a grant covers.
type Resource struct {
	Kind ResourceKind
	Name string
}

// String is the form a run grant's allowlist holds: a capability by its bare
// name, anything else as `<kind>:<name>`. A capability name holds no colon,
// so the two never collide.
func (r Resource) String() string {
	if r.Kind == ResourceCapability {
		return r.Name
	}
	return string(r.Kind) + ":" + r.Name
}

// Verb is the one verb the resource's kind admits today.
func (r Resource) Verb() string {
	if rule, ok := resourceRules[r.Kind]; ok && len(rule.verbs) > 0 {
		return rule.verbs[0]
	}
	return ""
}

// NewResource validates a kind and a name.
func NewResource(kind ResourceKind, name string) (Resource, error) {
	rule, ok := resourceRules[kind]
	if !ok {
		return Resource{}, fmt.Errorf("resource kind %q: want %q or %q", kind, ResourceCapability, ResourceSecret)
	}
	name = strings.TrimSpace(name)
	if !rule.name.MatchString(name) {
		return Resource{}, fmt.Errorf("%s %q: want %s", kind, name, rule.want)
	}
	return Resource{Kind: kind, Name: name}, nil
}

// ParseResource reads the String form: `crm.lookup`, `capability:crm.lookup`
// or `secret:CRM_KEY`.
func ParseResource(s string) (Resource, error) {
	s = strings.TrimSpace(s)
	kind, name, ok := strings.Cut(s, ":")
	if !ok {
		return NewResource(ResourceCapability, s)
	}
	return NewResource(ResourceKind(kind), name)
}

// Grant mirrors a resource_grants row.
type Grant struct {
	ID        string // grt_<hxid>
	TenantID  string
	Principal Principal
	Resource  Resource
	Verbs     []string // sorted
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Revoked reports whether the grant no longer holds.
func (g Grant) Revoked() bool { return g.RevokedAt != nil }

// Allows reports whether the grant names verb.
func (g Grant) Allows(verb string) bool {
	for _, v := range g.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}

// NewGrant is what txco://grant/put supplies.
type NewGrant struct {
	Kind ResourceKind
	Name string
	// Verbs the principal may use. Empty means every verb the kind admits.
	Verbs []string
}

func (in NewGrant) normalize() (Resource, []string, error) {
	res, err := NewResource(in.Kind, in.Name)
	if err != nil {
		return Resource{}, nil, invalid("%v", err)
	}
	rule := resourceRules[res.Kind]
	if len(in.Verbs) == 0 {
		return res, append([]string(nil), rule.verbs...), nil
	}
	seen := map[string]bool{}
	var verbs []string
	for _, raw := range in.Verbs {
		v := strings.TrimSpace(raw)
		known := false
		for _, k := range rule.verbs {
			known = known || k == v
		}
		if !known {
			return Resource{}, nil, invalid("verb %q: a %s grant takes %s", v, res.Kind, strings.Join(rule.verbs, ", "))
		}
		if !seen[v] {
			seen[v] = true
			verbs = append(verbs, v)
		}
	}
	sort.Strings(verbs)
	return res, verbs, nil
}

func encodeVerbs(verbs []string) string {
	b, _ := json.Marshal(verbs)
	return string(b)
}

// decodeVerbs reads the column form back. A row this package wrote always
// parses; one that does not yields an error rather than an empty list, so a
// corrupt row is never read as a grant of nothing — or of everything.
func decodeVerbs(col string) ([]string, error) {
	var verbs []string
	if err := json.Unmarshal([]byte(col), &verbs); err != nil || len(verbs) == 0 {
		return nil, fmt.Errorf("stored grant verbs %q: not a list of verbs", col)
	}
	sort.Strings(verbs)
	return verbs, nil
}

func sameVerbs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const grantCols = `id, tenant_id, principal_id, resource_kind, resource_id, verbs,
	created_by, created_at, revoked_at`

func scanGrant(row interface{ Scan(...any) error }) (Grant, error) {
	var (
		g            Grant
		pid, verbs   string
		kind, name   string
		created      string
		revokedStamp sql.NullString
	)
	err := row.Scan(&g.ID, &g.TenantID, &pid, &kind, &name, &verbs, &g.CreatedBy, &created, &revokedStamp)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	if err != nil {
		return Grant{}, err
	}
	pkind, _, _ := strings.Cut(pid, ":")
	g.Principal = Principal{ID: pid, Kind: PrincipalKind(pkind)}
	g.Resource = Resource{Kind: ResourceKind(kind), Name: name}
	if g.Verbs, err = decodeVerbs(verbs); err != nil {
		return Grant{}, err
	}
	g.CreatedAt = parseStamp(created)
	g.RevokedAt = parseStampPtr(revokedStamp)
	return g, nil
}

func (s *Store) liveGrant(ctx context.Context, q querier, tenantID string, p Principal, res Resource) (Grant, error) {
	return scanGrant(s.qr(ctx, q,
		`SELECT `+grantCols+` FROM resource_grants
		 WHERE tenant_id = ? AND principal_id = ? AND resource_kind = ? AND resource_id = ? AND revoked_at IS NULL`,
		tenantID, p.ID, string(res.Kind), res.Name))
}

// requireActive refuses a `user` principal that is disabled or gone. Any
// other kind of principal has no status to check.
func (s *Store) requireActive(ctx context.Context, q querier, tenantID string, p Principal) error {
	uid, isUser := p.UserID()
	if !isUser {
		return nil
	}
	u, err := s.getUser(ctx, q, tenantID, uid)
	if err != nil {
		return err
	}
	if u.Disabled() {
		return ErrDisabled
	}
	return nil
}

// PutGrant grants p the resource on behalf of stack, which must manage p. It
// is idempotent: putting a grant that already holds returns it with
// created=false. Putting it with different verbs revokes the old row and
// writes a new one.
//
// The principal must already exist (RequireOwner), so a grant can never name
// a principal another stack could later claim. The resource need not: a
// grant may be written before the secret it names is stored.
func (s *Store) PutGrant(ctx context.Context, tenantID, stack string, p Principal, in NewGrant) (Grant, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Grant{}, false, err
	}
	res, verbs, err := in.normalize()
	if err != nil {
		return Grant{}, false, err
	}
	base, err := s.RequireOwner(ctx, tenantID, stack, p)
	if err != nil {
		return Grant{}, false, err
	}
	if err := s.requireActive(ctx, s.DB, tenantID, p); err != nil {
		return Grant{}, false, err
	}
	// Two passes, as in BindPrincipal: a lost insert race trips the live
	// index, and the second pass finds the winner's row.
	for attempt := 0; ; attempt++ {
		existing, err := s.liveGrant(ctx, s.DB, tenantID, p, res)
		switch {
		case err == nil && sameVerbs(existing.Verbs, verbs):
			return existing, false, nil
		case err != nil && !errors.Is(err, ErrNotFound):
			return Grant{}, false, err
		}
		replace := ""
		if err == nil {
			replace = existing.ID
		} else {
			var live int
			if err := s.qr(ctx, s.DB,
				`SELECT COUNT(*) FROM resource_grants WHERE tenant_id = ? AND principal_id = ? AND revoked_at IS NULL`,
				tenantID, p.ID).Scan(&live); err != nil {
				return Grant{}, false, err
			}
			if live >= MaxGrants {
				return Grant{}, false, ErrTooManyGrants
			}
		}
		g, err := s.insertGrant(ctx, tenantID, base, p, res, verbs, replace)
		if err == nil {
			return g, true, nil
		}
		if attempt > 0 || !s.Dialect.IsUniqueViolationGeneric(err) {
			return Grant{}, false, err
		}
	}
}

// insertGrant writes a grant, revoking the row it replaces in the same
// transaction so the principal is never without one in between.
func (s *Store) insertGrant(ctx context.Context, tenantID, base string, p Principal, res Resource, verbs []string, replace string) (Grant, error) {
	tx, err := s.Dialect.BeginWrite(ctx, s.DB)
	if err != nil {
		return Grant{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.stamp()
	if replace != "" {
		if _, err := s.ex(ctx, tx,
			`UPDATE resource_grants SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
			now, replace, tenantID); err != nil {
			return Grant{}, err
		}
	}
	g := Grant{
		ID: "grt_" + hxid.NewTimeSort().String(), TenantID: tenantID, Principal: p,
		Resource: res, Verbs: verbs, CreatedBy: base, CreatedAt: parseStamp(now),
	}
	if _, err := s.ex(ctx, tx,
		`INSERT INTO resource_grants
		   (id, tenant_id, principal_id, resource_kind, resource_id, verbs, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, tenantID, p.ID, string(res.Kind), res.Name, encodeVerbs(verbs), base, now); err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// ListGrants returns p's grants, newest first, on behalf of stack — the
// owner only. kind narrows the list when it is not empty. Revoked rows are
// included when asked.
func (s *Store) ListGrants(ctx context.Context, tenantID, stack string, p Principal, kind ResourceKind, includeRevoked bool) ([]Grant, error) {
	if err := requireTenant(tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return nil, err
	}
	q := `SELECT ` + grantCols + ` FROM resource_grants WHERE tenant_id = ? AND principal_id = ?`
	args := []any{tenantID, p.ID}
	if kind != "" {
		if _, ok := resourceRules[kind]; !ok {
			return nil, invalid("resource kind %q: want %q or %q", kind, ResourceCapability, ResourceSecret)
		}
		q += ` AND resource_kind = ?`
		args = append(args, string(kind))
	}
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	return s.grants(ctx, q, args...)
}

// GrantsOn returns the live grants on one resource: who can reach this.
// Reads by resource are tenant-wide, as GetUser is; each row names the stack
// that wrote it.
func (s *Store) GrantsOn(ctx context.Context, tenantID string, kind ResourceKind, name string) ([]Grant, error) {
	if err := requireTenant(tenantID); err != nil {
		return nil, err
	}
	res, err := NewResource(kind, name)
	if err != nil {
		return nil, invalid("%v", err)
	}
	return s.grants(ctx,
		`SELECT `+grantCols+` FROM resource_grants
		 WHERE tenant_id = ? AND resource_kind = ? AND resource_id = ? AND revoked_at IS NULL`,
		tenantID, string(res.Kind), res.Name)
}

func (s *Store) grants(ctx context.Context, query string, args ...any) ([]Grant, error) {
	rows, err := s.qy(ctx, s.DB, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	// Newest first, decided here rather than by the database: ids made in
	// the same second tie on created_at, and how a database orders two ids
	// depends on its collation. Byte order is the order ids were made in.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, rows.Err()
}

// RevokeGrant revokes one grant by id, on behalf of stack. It returns the row
// and whether THIS call revoked it (false: it already was). The row stays.
//
// Revoking a standing grant ends every run grant that leans on it at that
// run's next request: each request reads the standing grant again.
func (s *Store) RevokeGrant(ctx context.Context, tenantID, stack, grantID string) (Grant, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Grant{}, false, err
	}
	g, err := scanGrant(s.qr(ctx, s.DB,
		`SELECT `+grantCols+` FROM resource_grants WHERE id = ? AND tenant_id = ?`, grantID, tenantID))
	if err != nil {
		return Grant{}, false, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, g.Principal); err != nil {
		return Grant{}, false, err
	}
	return s.revokeGrant(ctx, g)
}

func (s *Store) revokeGrant(ctx context.Context, g Grant) (Grant, bool, error) {
	if g.Revoked() {
		return g, false, nil
	}
	now := s.stamp()
	res, err := s.ex(ctx, s.DB,
		`UPDATE resource_grants SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		now, g.ID, g.TenantID)
	if err != nil {
		return Grant{}, false, err
	}
	n, _ := res.RowsAffected()
	t := parseStamp(now)
	g.RevokedAt = &t
	return g, n > 0, nil
}

// RevokeGrantOf is RevokeGrant for a caller that knows WHOSE the grant should
// be: it revokes grantID only if it is p's, and answers ErrNotFound
// otherwise. For an id that came from a request (see RevokeCredentialOf).
func (s *Store) RevokeGrantOf(ctx context.Context, tenantID, stack string, p Principal, grantID string) (Grant, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Grant{}, false, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return Grant{}, false, err
	}
	var owner string
	err := s.qr(ctx, s.DB, `SELECT principal_id FROM resource_grants WHERE id = ? AND tenant_id = ?`, grantID, tenantID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != p.ID) {
		return Grant{}, false, ErrNotFound
	}
	if err != nil {
		return Grant{}, false, err
	}
	return s.RevokeGrant(ctx, tenantID, stack, grantID)
}

// RevokeGrantOn revokes p's grant on a resource, named the way it was put. It
// returns ErrNotFound when p never held one, and revoked=false when the last
// one it held is already revoked.
func (s *Store) RevokeGrantOn(ctx context.Context, tenantID, stack string, p Principal, kind ResourceKind, name string) (Grant, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Grant{}, false, err
	}
	res, err := NewResource(kind, name)
	if err != nil {
		return Grant{}, false, invalid("%v", err)
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return Grant{}, false, err
	}
	// The live row if there is one, else the newest revoked one.
	g, err := s.liveGrant(ctx, s.DB, tenantID, p, res)
	if errors.Is(err, ErrNotFound) {
		var past []Grant
		past, err = s.grants(ctx,
			`SELECT `+grantCols+` FROM resource_grants
			 WHERE tenant_id = ? AND principal_id = ? AND resource_kind = ? AND resource_id = ?`,
			tenantID, p.ID, string(res.Kind), res.Name)
		if err == nil && len(past) == 0 {
			err = ErrNotFound
		}
		if err == nil {
			g = past[0]
		}
	}
	if err != nil {
		return Grant{}, false, err
	}
	return s.revokeGrant(ctx, g)
}

// Granted reports whether p holds a live grant of verb on the resource. It is
// the question the chassis asks on every request, so it takes no stack: the
// asker is the chassis, not a rule.
//
// A disabled user holds nothing while disabled. Nothing is revoked, so
// enabling the user restores every grant.
func (s *Store) Granted(ctx context.Context, tenantID string, p Principal, res Resource, verb string) (bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return false, err
	}
	if p.IsZero() {
		return false, nil
	}
	g, err := s.liveGrant(ctx, s.DB, tenantID, p, res)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !g.Allows(verb) {
		return false, nil
	}
	switch err := s.requireActive(ctx, s.DB, tenantID, p); {
	case errors.Is(err, ErrDisabled), errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// heldBy returns every resource p holds a live grant on, keyed by the
// resource's String form, for a caller checking a whole allowlist at once.
func (s *Store) heldBy(ctx context.Context, q querier, tenantID string, p Principal) (map[string][]string, error) {
	rows, err := s.qy(ctx, q,
		`SELECT resource_kind, resource_id, verbs FROM resource_grants
		 WHERE tenant_id = ? AND principal_id = ? AND revoked_at IS NULL`, tenantID, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string][]string{}
	for rows.Next() {
		var kind, name, col string
		if err := rows.Scan(&kind, &name, &col); err != nil {
			return nil, err
		}
		verbs, err := decodeVerbs(col)
		if err != nil {
			return nil, err
		}
		held[Resource{Kind: ResourceKind(kind), Name: name}.String()] = verbs
	}
	return held, rows.Err()
}
