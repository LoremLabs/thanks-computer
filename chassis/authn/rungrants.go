package authn

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// A run grant says what ONE piece of work may ask the chassis for, until
// when, and within what budget (db/schema/*/auth/0007_run_grants.sql). A
// stack mints one when it dispatches the work; the work presents a token
// that names the row (chassis/rungrant), and every request reads the row.
//
// Authority only narrows on the way down:
//
//   - a run grant names nothing its principal holds no standing grant for;
//   - a grant narrowed from another names nothing the parent does not,
//     expires no later, and takes its budget out of the parent's;
//   - nothing here widens or extends a grant. For longer work, mint again.
//
// A grant stops working when it expires, when it is closed or revoked, when
// the same run is minted again, or when its budget is spent. Ending a grant
// ends every grant narrowed from it.

// NodeClass says what is allowed to run where the work is sent. It is a
// promise by whoever dispatches the work; the chassis cannot prove it.
type NodeClass string

const (
	// NodeUnreviewed runs anything, including code written a moment ago.
	// The default.
	NodeUnreviewed NodeClass = "unreviewed"
	// NodeReviewed runs only what was reviewed before it was installed.
	NodeReviewed NodeClass = "reviewed"
)

const (
	// MaxRunAllow bounds one run grant's allowlist.
	MaxRunAllow = 64
	// MaxRunDepth bounds how many times a grant may be narrowed into
	// another: work that dispatches work that dispatches work.
	MaxRunDepth = 4
	// MaxRunTTL and MaxRunBudget are the store's own ceilings, behind the
	// operator's (--run-grant-ttl-max, --run-grant-budget-max).
	MaxRunTTL    = 24 * time.Hour
	MaxRunBudget = 1_000_000

	maxCloseReason = 128
)

var (
	// ErrExceedsStanding: the allowlist names something the principal holds
	// no standing grant for. The error names it.
	ErrExceedsStanding = errors.New("authn: the run grant exceeds the principal's standing grants")
	// ErrExceedsParent: the allowlist names something the parent grant does
	// not, or the budget is more than the parent has left.
	ErrExceedsParent = errors.New("authn: the run grant exceeds its parent")
	// ErrParentNotLive: the parent grant has expired, ended or been revoked.
	ErrParentNotLive = errors.New("authn: the parent run grant is not live")
	// ErrDepth: the grant would be narrowed more than MaxRunDepth times.
	ErrDepth = errors.New("authn: run grants are nested too deep")
	// ErrStaleGeneration: the run already has this generation or a later one.
	ErrStaleGeneration = errors.New("authn: the run already has this generation or a later one")
	// ErrRunNotLive: the run grant has expired, ended or been revoked.
	ErrRunNotLive = errors.New("authn: the run grant is not live")
	// ErrRunBudget: the run grant's budget is spent.
	ErrRunBudget = errors.New("authn: the run grant's budget is spent")
)

// A run is named by whoever dispatches it: a task id, a job id, or a path of
// them (`task-42/lookup`) for work dispatched by other work.
var runNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+:/-]{0,127}$`)

// RunGrant mirrors a run_grants row.
type RunGrant struct {
	ID          string // rgr_<hxid>
	TenantID    string
	Principal   Principal
	MintedBy    string // base name of the minting stack
	Stack       string // the minting rule's app stack
	Run         string
	Generation  int64
	WorkspaceID string
	Workspace   string
	NodeClass   NodeClass
	// FileKey names the run's directory where grants are presented as
	// files. It is for the chassis alone: never put it in an envelope.
	FileKey     string
	Allow       []Resource
	BudgetCalls int64
	SpentCalls  int64
	ParentGrant string
	Depth       int
	TraceID     string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	ClosedAt    *time.Time
	CloseReason string
	RevokedAt   *time.Time
}

// Live reports whether the grant still holds at now.
func (g RunGrant) Live(now time.Time) bool {
	return g.RevokedAt == nil && g.ClosedAt == nil && now.Before(g.ExpiresAt)
}

// Allows reports whether the allowlist names res.
func (g RunGrant) Allows(res Resource) bool {
	for _, a := range g.Allow {
		if a == res {
			return true
		}
	}
	return false
}

// Remaining is the budget left.
func (g RunGrant) Remaining() int64 {
	if g.SpentCalls >= g.BudgetCalls {
		return 0
	}
	return g.BudgetCalls - g.SpentCalls
}

// AllowStrings is the allowlist in its stored / displayed form.
func (g RunGrant) AllowStrings() []string {
	out := make([]string, len(g.Allow))
	for i, a := range g.Allow {
		out[i] = a.String()
	}
	return out
}

// NewRunGrant is what txco://rungrant/mint supplies.
type NewRunGrant struct {
	// Run names the work: a task id, a job id. Minting the same run again
	// replaces the grant it had.
	Run string
	// Generation orders the grants of one run. Zero means the next one; a
	// caller with a counter of its own (a lease) passes it, and it must be
	// later than any the run has had.
	Generation int64
	// Stack is the minting rule's app stack.
	Stack string
	// Workspace and WorkspaceID say where the work runs; both empty for
	// work that runs nowhere.
	Workspace, WorkspaceID string
	NodeClass              NodeClass // "" ⇒ NodeUnreviewed
	// Allow is the allowlist, each entry in Resource's String form.
	Allow       []string
	BudgetCalls int64
	TTL         time.Duration
	// Parent is the run grant this one is narrowed from, or "".
	Parent  string
	TraceID string
}

func parseAllow(in []string) ([]Resource, error) {
	seen := map[Resource]bool{}
	var out []Resource
	for _, raw := range in {
		res, err := ParseResource(raw)
		if err != nil {
			return nil, invalid("allow: %v", err)
		}
		if !seen[res] {
			seen[res] = true
			out = append(out, res)
		}
	}
	if len(out) == 0 {
		return nil, invalid("allow: name at least one capability or secret")
	}
	if len(out) > MaxRunAllow {
		return nil, invalid("allow: at most %d names", MaxRunAllow)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func encodeAllow(allow []Resource) string {
	names := make([]string, len(allow))
	for i, a := range allow {
		names[i] = a.String()
	}
	b, _ := json.Marshal(names)
	return string(b)
}

// decodeAllow reads the column form back. A row that does not parse is an
// error, never an empty allowlist.
func decodeAllow(col string) ([]Resource, error) {
	var raw []string
	if err := json.Unmarshal([]byte(col), &raw); err != nil {
		return nil, fmt.Errorf("stored allowlist: %w", err)
	}
	out := make([]Resource, 0, len(raw))
	for _, r := range raw {
		res, err := ParseResource(r)
		if err != nil {
			return nil, fmt.Errorf("stored allowlist: %w", err)
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		return nil, errors.New("stored allowlist: empty")
	}
	return out, nil
}

func (in NewRunGrant) normalize() (NewRunGrant, []Resource, error) {
	in.Run = strings.TrimSpace(in.Run)
	if !runNameRE.MatchString(in.Run) {
		return in, nil, invalid("run %q: want letters, digits and . _ @ + : / - (1-128 chars)", in.Run)
	}
	if in.Generation < 0 {
		return in, nil, invalid("generation must be positive")
	}
	if in.Stack = strings.Trim(strings.TrimSpace(in.Stack), "/"); in.Stack == "" {
		return in, nil, ErrNoStack
	}
	switch in.NodeClass {
	case "":
		in.NodeClass = NodeUnreviewed
	case NodeUnreviewed, NodeReviewed:
	default:
		return in, nil, invalid("node class %q: want %q or %q", in.NodeClass, NodeReviewed, NodeUnreviewed)
	}
	if (in.Workspace == "") != (in.WorkspaceID == "") {
		return in, nil, invalid("a workspace needs both its name and its id")
	}
	if in.BudgetCalls < 1 || in.BudgetCalls > MaxRunBudget {
		return in, nil, invalid("budget must be 1 to %d calls", MaxRunBudget)
	}
	if in.TTL < time.Second || in.TTL > MaxRunTTL {
		return in, nil, invalid("ttl must be 1 second to %s", MaxRunTTL)
	}
	if len(in.TraceID) > 128 {
		in.TraceID = in.TraceID[:128]
	}
	allow, err := parseAllow(in.Allow)
	return in, allow, err
}

const runGrantCols = `id, tenant_id, principal_id, minted_by, stack, run, generation,
	workspace_id, workspace, node_class, file_key, allowlist, budget_calls, spent_calls,
	COALESCE(parent_grant, ''), depth, trace_id, issued_at, expires_at, closed_at, close_reason, revoked_at`

func scanRunGrant(row interface{ Scan(...any) error }) (RunGrant, error) {
	var (
		g               RunGrant
		pid, allow      string
		issued, expires string
		closed, revoked sql.NullString
	)
	err := row.Scan(&g.ID, &g.TenantID, &pid, &g.MintedBy, &g.Stack, &g.Run, &g.Generation,
		&g.WorkspaceID, &g.Workspace, &g.NodeClass, &g.FileKey, &allow, &g.BudgetCalls, &g.SpentCalls,
		&g.ParentGrant, &g.Depth, &g.TraceID, &issued, &expires, &closed, &g.CloseReason, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return RunGrant{}, ErrNotFound
	}
	if err != nil {
		return RunGrant{}, err
	}
	kind, _, _ := strings.Cut(pid, ":")
	g.Principal = Principal{ID: pid, Kind: PrincipalKind(kind)}
	if g.Allow, err = decodeAllow(allow); err != nil {
		return RunGrant{}, err
	}
	g.IssuedAt, g.ExpiresAt = parseStamp(issued), parseStamp(expires)
	g.ClosedAt, g.RevokedAt = parseStampPtr(closed), parseStampPtr(revoked)
	return g, nil
}

// liveRun is the WHERE clause of a grant that still holds; its one
// placeholder is the current time.
const liveRun = `revoked_at IS NULL AND closed_at IS NULL AND expires_at > ?`

func newFileKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// MintRunGrant writes the grant for one piece of work that acts for p, on
// behalf of stack, which must manage p. Minting a run that already has a
// grant closes the older one: the newest generation is the only live one.
//
// It refuses an allowlist that names anything p holds no standing grant for
// (ErrExceedsStanding), and — narrowing from a parent — anything the parent
// does not name or a budget the parent does not have left (ErrExceedsParent).
// A parent's expiry caps the child's; it is shortened, not refused.
//
// The child's whole budget is taken from the parent at mint, spent or not.
func (s *Store) MintRunGrant(ctx context.Context, tenantID, stack string, p Principal, in NewRunGrant) (RunGrant, error) {
	if err := requireTenant(tenantID); err != nil {
		return RunGrant{}, err
	}
	in, allow, err := in.normalize()
	if err != nil {
		return RunGrant{}, err
	}
	base, err := s.RequireOwner(ctx, tenantID, stack, p)
	if err != nil {
		return RunGrant{}, err
	}
	if err := s.requireActive(ctx, s.DB, tenantID, p); err != nil {
		return RunGrant{}, err
	}
	held, err := s.heldBy(ctx, s.DB, tenantID, p)
	if err != nil {
		return RunGrant{}, err
	}
	for _, res := range allow {
		ok := false
		for _, v := range held[res.String()] {
			ok = ok || v == res.Verb()
		}
		if !ok {
			return RunGrant{}, fmt.Errorf("%w: %s holds no grant to %s %s", ErrExceedsStanding, p.ID, res.Verb(), res)
		}
	}

	now := s.now().UTC()
	expires := now.Add(in.TTL)
	g := RunGrant{
		TenantID: tenantID, Principal: p, MintedBy: base, Stack: in.Stack, Run: in.Run,
		WorkspaceID: in.WorkspaceID, Workspace: in.Workspace, NodeClass: in.NodeClass,
		Allow: allow, BudgetCalls: in.BudgetCalls, TraceID: in.TraceID,
	}
	if in.Parent != "" {
		parent, err := s.ReadRunGrant(ctx, tenantID, in.Parent)
		switch {
		case errors.Is(err, ErrNotFound):
			return RunGrant{}, fmt.Errorf("%w: no run grant %s", ErrParentNotLive, in.Parent)
		case err != nil:
			return RunGrant{}, err
		case parent.MintedBy != base:
			return RunGrant{}, &OwnerError{Principal: parent.Principal, Owner: parent.MintedBy}
		case !parent.Live(now):
			return RunGrant{}, ErrParentNotLive
		case parent.Depth+1 > MaxRunDepth:
			return RunGrant{}, ErrDepth
		case parent.Run == in.Run:
			// Minting the parent's own run again would replace the parent.
			return RunGrant{}, invalid("run %q is the parent's: work dispatched by a run needs a run name of its own", in.Run)
		}
		for _, res := range allow {
			if !parent.Allows(res) {
				return RunGrant{}, fmt.Errorf("%w: the parent does not name %s", ErrExceedsParent, res)
			}
		}
		if parent.ExpiresAt.Before(expires) {
			expires = parent.ExpiresAt
		}
		g.ParentGrant, g.Depth = parent.ID, parent.Depth+1
	}

	return s.insertRunGrant(ctx, g, in.Generation, now, expires)
}

func (s *Store) insertRunGrant(ctx context.Context, g RunGrant, generation int64, now, expires time.Time) (RunGrant, error) {
	tx, err := s.Dialect.BeginWrite(ctx, s.DB)
	if err != nil {
		return RunGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now.Format(time.RFC3339)
	g.ID = "rgr_" + hxid.NewTimeSort().String()

	// Take the run's head row first (0007, run_heads). Two mints of one run
	// then take turns: the second waits here for the first to commit, and
	// everything below sees what the first wrote. It is also a WRITE, so
	// SQLite takes its write lock before anything is read — a transaction
	// that reads and then writes can lose the lock upgrade to another writer.
	if generation == 0 {
		if _, err := s.ex(ctx, tx,
			`INSERT INTO run_heads (tenant_id, minted_by, run, generation, grant_id) VALUES (?, ?, ?, 1, ?)
			 ON CONFLICT (tenant_id, minted_by, run)
			 DO UPDATE SET generation = run_heads.generation + 1, grant_id = excluded.grant_id`,
			g.TenantID, g.MintedBy, g.Run, g.ID); err != nil {
			return RunGrant{}, err
		}
		if err := s.qr(ctx, tx,
			`SELECT generation FROM run_heads WHERE tenant_id = ? AND minted_by = ? AND run = ?`,
			g.TenantID, g.MintedBy, g.Run).Scan(&generation); err != nil {
			return RunGrant{}, err
		}
	} else {
		res, err := s.ex(ctx, tx,
			`INSERT INTO run_heads (tenant_id, minted_by, run, generation, grant_id) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (tenant_id, minted_by, run)
			 DO UPDATE SET generation = excluded.generation, grant_id = excluded.grant_id
			 WHERE run_heads.generation < excluded.generation`,
			g.TenantID, g.MintedBy, g.Run, generation, g.ID)
		if err != nil {
			return RunGrant{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return RunGrant{}, ErrStaleGeneration
		}
	}

	// Take the budget from the parent BEFORE touching this run's own rows:
	// every mint that involves a grant locks that grant's row first, so two
	// of them cannot wait on each other.
	if g.ParentGrant != "" {
		res, err := s.ex(ctx, tx,
			`UPDATE run_grants SET spent_calls = spent_calls + ?
			 WHERE id = ? AND tenant_id = ? AND `+liveRun+` AND spent_calls + ? <= budget_calls`,
			g.BudgetCalls, g.ParentGrant, g.TenantID, stamp, g.BudgetCalls)
		if err != nil {
			return RunGrant{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Ended by someone else a moment ago, or without the budget.
			parent, err := scanRunGrant(s.qr(ctx, tx,
				`SELECT `+runGrantCols+` FROM run_grants WHERE id = ? AND tenant_id = ?`, g.ParentGrant, g.TenantID))
			if err != nil || !parent.Live(now) {
				return RunGrant{}, ErrParentNotLive
			}
			return RunGrant{}, fmt.Errorf("%w: it has %d calls left", ErrExceedsParent, parent.Remaining())
		}
	}

	// Close the run's older grants, and every grant narrowed from them.
	older, err := s.ids(ctx, tx,
		`SELECT id FROM run_grants
		 WHERE tenant_id = ? AND minted_by = ? AND run = ? AND revoked_at IS NULL AND closed_at IS NULL`,
		g.TenantID, g.MintedBy, g.Run)
	if err != nil {
		return RunGrant{}, err
	}
	if _, err := s.ex(ctx, tx,
		`UPDATE run_grants SET closed_at = ?, close_reason = 'superseded'
		 WHERE tenant_id = ? AND minted_by = ? AND run = ? AND revoked_at IS NULL AND closed_at IS NULL`,
		stamp, g.TenantID, g.MintedBy, g.Run); err != nil {
		return RunGrant{}, err
	}
	if err := s.endDescendants(ctx, tx, g.TenantID, older,
		`closed_at = ?, close_reason = 'superseded'`, stamp); err != nil {
		return RunGrant{}, err
	}

	g.Generation = generation
	g.IssuedAt, g.ExpiresAt = parseStamp(stamp), parseStamp(expires.Format(time.RFC3339))
	if g.FileKey, err = newFileKey(); err != nil {
		return RunGrant{}, err
	}
	if _, err := s.ex(ctx, tx,
		`INSERT INTO run_grants
		   (id, tenant_id, principal_id, minted_by, stack, run, generation, workspace_id, workspace,
		    node_class, file_key, allowlist, budget_calls, parent_grant, depth, trace_id, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.TenantID, g.Principal.ID, g.MintedBy, g.Stack, g.Run, g.Generation, g.WorkspaceID, g.Workspace,
		string(g.NodeClass), g.FileKey, encodeAllow(g.Allow), g.BudgetCalls, nullIfEmpty(g.ParentGrant),
		g.Depth, g.TraceID, stamp, expires.Format(time.RFC3339)); err != nil {
		// The head row makes a duplicate generation impossible; should the
		// index ever catch one, the caller is stale, not the store broken.
		if s.Dialect.IsUniqueViolationGeneric(err) {
			return RunGrant{}, ErrStaleGeneration
		}
		return RunGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrant{}, err
	}
	return g, nil
}

func (s *Store) ids(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := s.qy(ctx, q, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// endDescendants ends every live grant narrowed from roots, and from those,
// down to MaxRunDepth. set is the SET clause; setArgs are its values.
func (s *Store) endDescendants(ctx context.Context, q querier, tenantID string, roots []string, set string, setArgs ...any) error {
	const chunk = 200
	level := roots
	for depth := 0; depth < MaxRunDepth && len(level) > 0; depth++ {
		var next []string
		for i := 0; i < len(level); i += chunk {
			part := level[i:min(i+chunk, len(level))]
			in := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
			args := []any{tenantID}
			for _, id := range part {
				args = append(args, id)
			}
			children, err := s.ids(ctx, q,
				`SELECT id FROM run_grants WHERE tenant_id = ? AND parent_grant IN (`+in+`)`, args...)
			if err != nil {
				return err
			}
			next = append(next, children...)
			if _, err := s.ex(ctx, q,
				`UPDATE run_grants SET `+set+`
				 WHERE tenant_id = ? AND parent_grant IN (`+in+`) AND revoked_at IS NULL AND closed_at IS NULL`,
				append(append([]any{}, setArgs...), args...)...); err != nil {
				return err
			}
		}
		level = next
	}
	return nil
}

// ReadRunGrant reads a run grant for the chassis itself, which asks on
// behalf of no stack. An op reads through GetRunGrant.
func (s *Store) ReadRunGrant(ctx context.Context, tenantID, grantID string) (RunGrant, error) {
	if err := requireTenant(tenantID); err != nil {
		return RunGrant{}, err
	}
	return scanRunGrant(s.qr(ctx, s.DB,
		`SELECT `+runGrantCols+` FROM run_grants WHERE id = ? AND tenant_id = ?`, grantID, tenantID))
}

// GetRunGrant reads a run grant on behalf of stack — the stack that minted
// it only: the row says what a piece of its work may ask for.
func (s *Store) GetRunGrant(ctx context.Context, tenantID, stack, grantID string) (RunGrant, error) {
	g, err := s.ReadRunGrant(ctx, tenantID, grantID)
	if err != nil {
		return RunGrant{}, err
	}
	base := BaseStack(stack)
	switch {
	case base == "":
		return RunGrant{}, ErrNoStack
	case g.MintedBy != base:
		return RunGrant{}, &OwnerError{Principal: g.Principal, Owner: g.MintedBy}
	}
	return g, nil
}

// RevokeRunGrant revokes a run grant and every grant narrowed from it, on
// behalf of the stack that minted it. It returns the row and whether THIS
// call revoked it. The work's next request is refused.
//
// Revoking stops what the work may still ask for. It recalls nothing the
// work was already given.
func (s *Store) RevokeRunGrant(ctx context.Context, tenantID, stack, grantID string) (RunGrant, bool, error) {
	return s.endRunGrant(ctx, tenantID, stack, grantID, false, "")
}

// CloseRunGrant ends a run grant because its work ended, with every grant
// narrowed from it. reason is a short note for whoever reads the row
// ("done", "failed", "timeout"); empty means "done". It returns the row and
// whether THIS call closed it (false: it had already ended).
func (s *Store) CloseRunGrant(ctx context.Context, tenantID, stack, grantID, reason string) (RunGrant, bool, error) {
	reason, err := cleanText("reason", reason, maxCloseReason)
	if err != nil {
		return RunGrant{}, false, err
	}
	if reason == "" {
		reason = "done"
	}
	return s.endRunGrant(ctx, tenantID, stack, grantID, true, reason)
}

func (s *Store) endRunGrant(ctx context.Context, tenantID, stack, grantID string, closing bool, reason string) (RunGrant, bool, error) {
	g, err := s.GetRunGrant(ctx, tenantID, stack, grantID)
	if err != nil {
		return RunGrant{}, false, err
	}
	if g.RevokedAt != nil || (closing && g.ClosedAt != nil) {
		return g, false, nil
	}
	tx, err := s.Dialect.BeginWrite(ctx, s.DB)
	if err != nil {
		return RunGrant{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.stamp()
	set, setArgs := `revoked_at = ?`, []any{now}
	where := `revoked_at IS NULL`
	if closing {
		set, setArgs = `closed_at = ?, close_reason = ?`, []any{now, reason}
		where = `revoked_at IS NULL AND closed_at IS NULL`
	}
	res, err := s.ex(ctx, tx,
		`UPDATE run_grants SET `+set+` WHERE id = ? AND tenant_id = ? AND `+where,
		append(append([]any{}, setArgs...), grantID, tenantID)...)
	if err != nil {
		return RunGrant{}, false, err
	}
	n, _ := res.RowsAffected()
	if err := s.endDescendants(ctx, tx, tenantID, []string{grantID}, set, setArgs...); err != nil {
		return RunGrant{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrant{}, false, err
	}
	t := parseStamp(now)
	if closing {
		g.ClosedAt, g.CloseReason = &t, reason
	} else {
		g.RevokedAt = &t
	}
	return g, n > 0, nil
}

// ChargeRunGrant spends n calls of a run grant's budget, in one statement:
// the grant is charged only if it is live at that moment and, when
// enforceBudget is set, only if the budget covers it. Two requests racing
// for the last call cannot both be charged.
//
// It answers ErrRunNotLive or ErrRunBudget when nothing was charged. Charge
// BEFORE handing over what was asked for: a charge that fails after the
// fact recalls nothing.
func (s *Store) ChargeRunGrant(ctx context.Context, tenantID, grantID string, n int64, enforceBudget bool) error {
	if err := requireTenant(tenantID); err != nil {
		return err
	}
	if n < 1 {
		return invalid("charge at least one call")
	}
	now := s.stamp()
	q := `UPDATE run_grants SET spent_calls = spent_calls + ? WHERE id = ? AND tenant_id = ? AND ` + liveRun
	args := []any{n, grantID, tenantID, now}
	if enforceBudget {
		q += ` AND spent_calls + ? <= budget_calls`
		args = append(args, n)
	}
	res, err := s.ex(ctx, s.DB, q, args...)
	if err != nil {
		return err
	}
	if charged, _ := res.RowsAffected(); charged > 0 {
		return nil
	}
	// Nothing was charged. Say why; the answer is advice, the refusal above
	// is what counts.
	g, err := s.ReadRunGrant(ctx, tenantID, grantID)
	switch {
	case err != nil:
		return err
	case !g.Live(parseStamp(now)):
		return ErrRunNotLive
	}
	return ErrRunBudget
}
