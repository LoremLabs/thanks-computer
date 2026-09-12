package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Lease is one granted attachment to a workspace: who holds it, on which
// node, until when, and when it last proved it was alive. Rows outlive the
// binding (released_at is set, never deleted) so an admin view can answer
// "who was in this machine, when" — the same posture as the workspaces
// table itself.
//
// A lease is what the reaper will consult (D9 of the PTY design): a
// workspace with a live lease is in use whatever its last_used_at says.
// Until that lands the heartbeat also touches last_used_at, so the reaper
// that exists today never reaps a workspace someone is sitting in.
type Lease struct {
	ID            string // the attachment id (hxid)
	Tenant        string
	Stack         string // the OWNING (app) stack, not the inlet sub-stack
	Name          string
	WorkspaceID   string // ID(tenant, stack, name)
	Kind          string // "pty"
	SessionID     string // the WebSocket session holding the socket
	NodeID        string // the chassis node holding it
	RunID         string
	StartedAt     time.Time
	ExpiresAt     time.Time
	LastHeartbeat time.Time
	ReleasedAt    *time.Time
}

// LeaseHeartbeatInterval is how often a live attachment heartbeats; a
// lease silent for LeaseStaleAfter (two intervals) is dead — the node
// crashed mid-session — and no longer counts as holding the workspace.
const (
	LeaseHeartbeatInterval = 60 * time.Second
	LeaseStaleAfter        = 2 * LeaseHeartbeatInterval
)

// Live reports whether the lease holds the workspace at now: not released,
// not past its expiry, and heard from within LeaseStaleAfter.
func (l Lease) Live(now time.Time) bool {
	if l.ReleasedAt != nil {
		return false
	}
	if !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt) {
		return false
	}
	return now.Sub(l.LastHeartbeat) < LeaseStaleAfter
}

// leaseDDL is the portable DDL the migrations use (open-core sqlite
// runtime 0027 / overlay pg 0030); EnsureSchema stands it up for tests and
// for a connection without the migration runner.
var leaseDDL = []string{
	`CREATE TABLE IF NOT EXISTS workspace_leases (
		lease_id       TEXT PRIMARY KEY,
		tenant         TEXT NOT NULL,
		stack          TEXT NOT NULL,
		name           TEXT NOT NULL,
		workspace_id   TEXT NOT NULL,
		kind           TEXT NOT NULL,
		session_id     TEXT NOT NULL,
		node_id        TEXT NOT NULL,
		run_id         TEXT,
		started_at     TEXT NOT NULL,
		expires_at     TEXT NOT NULL,
		last_heartbeat TEXT NOT NULL,
		released_at    TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS workspace_leases_workspace_idx
		ON workspace_leases (workspace_id, released_at)`,
	`CREATE INDEX IF NOT EXISTS workspace_leases_tenant_idx
		ON workspace_leases (tenant, started_at)`,
}

const leaseCols = `lease_id, tenant, stack, name, workspace_id, kind, session_id, node_id, run_id,
	started_at, expires_at, last_heartbeat, released_at`

func scanLease(sc interface{ Scan(...any) error }) (*Lease, error) {
	var l Lease
	var runID, released sql.NullString
	var started, expires, beat string
	if err := sc.Scan(&l.ID, &l.Tenant, &l.Stack, &l.Name, &l.WorkspaceID, &l.Kind, &l.SessionID, &l.NodeID, &runID,
		&started, &expires, &beat, &released); err != nil {
		return nil, err
	}
	l.RunID = runID.String
	l.StartedAt, _ = time.Parse(time.RFC3339, started)
	l.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	l.LastHeartbeat, _ = time.Parse(time.RFC3339, beat)
	if released.Valid && released.String != "" {
		if t, err := time.Parse(time.RFC3339, released.String); err == nil {
			l.ReleasedAt = &t
		}
	}
	return &l, nil
}

// InsertLease records a newly granted attachment.
func (s *Store) InsertLease(ctx context.Context, l Lease) error {
	if l.ID == "" || l.Tenant == "" || l.Stack == "" || l.Name == "" || l.Kind == "" || l.SessionID == "" || l.NodeID == "" {
		return errors.New("workspace: insert lease needs id, tenant, stack, name, kind, session_id, node_id")
	}
	now := s.now()
	if l.StartedAt.IsZero() {
		l.StartedAt = now
	}
	if l.LastHeartbeat.IsZero() {
		l.LastHeartbeat = l.StartedAt
	}
	if l.WorkspaceID == "" {
		l.WorkspaceID = ID(l.Tenant, l.Stack, l.Name)
	}
	_, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO workspace_leases
			(lease_id, tenant, stack, name, workspace_id, kind, session_id, node_id, run_id,
			 started_at, expires_at, last_heartbeat, released_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`),
		l.ID, l.Tenant, l.Stack, l.Name, l.WorkspaceID, l.Kind, l.SessionID, l.NodeID, nullIfEmpty(l.RunID),
		l.StartedAt.UTC().Format(time.RFC3339), l.ExpiresAt.UTC().Format(time.RFC3339),
		l.LastHeartbeat.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("workspace: insert lease: %w", err)
	}
	return nil
}

// Heartbeat records that the attachment is still alive at `at`.
func (s *Store) Heartbeat(ctx context.Context, leaseID string, at time.Time) error {
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE workspace_leases SET last_heartbeat = ? WHERE lease_id = ? AND released_at IS NULL`),
		at.UTC().Format(time.RFC3339), leaseID)
	if err != nil {
		return fmt.Errorf("workspace: heartbeat: %w", err)
	}
	return nil
}

// ReleaseLease ends the lease at `at` (idempotent: a released lease stays
// released at its first release time).
func (s *Store) ReleaseLease(ctx context.Context, leaseID string, at time.Time) error {
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE workspace_leases SET released_at = ? WHERE lease_id = ? AND released_at IS NULL`),
		at.UTC().Format(time.RFC3339), leaseID)
	if err != nil {
		return fmt.Errorf("workspace: release lease: %w", err)
	}
	return nil
}

// GetLease returns one lease by id, or (nil, nil).
func (s *Store) GetLease(ctx context.Context, leaseID string) (*Lease, error) {
	l, err := scanLease(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+leaseCols+` FROM workspace_leases WHERE lease_id = ?`), leaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: get lease: %w", err)
	}
	return l, nil
}

// ListLeases returns leases newest first; tenant "" lists every tenant;
// includeReleased adds ended ones.
func (s *Store) ListLeases(ctx context.Context, tenant string, includeReleased bool, limit int) ([]Lease, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + leaseCols + ` FROM workspace_leases WHERE 1 = 1`
	var args []any
	if tenant != "" {
		q += ` AND tenant = ?`
		args = append(args, tenant)
	}
	if !includeReleased {
		q += ` AND released_at IS NULL`
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.rb(q), args...)
	if err != nil {
		return nil, fmt.Errorf("workspace: list leases: %w", err)
	}
	defer rows.Close()
	return collectLeases(rows)
}

// ListLive returns the leases that hold workspaceID at now (see
// Lease.Live) — what the reaper asks before destroying, and what an admin
// view shows as ATTACHED.
func (s *Store) ListLive(ctx context.Context, workspaceID string, now time.Time) ([]Lease, error) {
	if now.IsZero() {
		now = s.now()
	}
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT `+leaseCols+` FROM workspace_leases
		  WHERE workspace_id = ? AND released_at IS NULL
		  ORDER BY started_at ASC`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list live leases: %w", err)
	}
	defer rows.Close()
	all, err := collectLeases(rows)
	if err != nil {
		return nil, err
	}
	live := all[:0]
	for _, l := range all {
		if l.Live(now) {
			live = append(live, l)
		}
	}
	return live, nil
}

func collectLeases(rows *sql.Rows) ([]Lease, error) {
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("workspace: scan lease: %w", err)
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}
