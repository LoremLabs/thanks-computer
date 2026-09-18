package ipp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// ErrJobNotFound is returned when no job matches.
var ErrJobNotFound = errors.New("ipp: job not found")

// Job is one print job. Number is the int32 `job-id` the IPP client sees
// (a per-tenant sequence); ID is the opaque durable identity the stack sees
// as `@ipp.job_id` and may key idempotence on.
type Job struct {
	ID             string
	Tenant         string
	Printer        string
	Number         int64
	RequestingUser string // client-claimed requesting-user-name: untrusted
	JobName        string
	DocumentName   string
	DocumentFormat string
	SHA256         string // "" until committed; bare 64-hex
	Size           int64
	State          string
	StateReason    string
	Attempts       int
	Rid            string
	Host           string // the ipp.<zone> host the job arrived on
	// URIPath is the printer's path on that host: "/p/<printer>", or
	// "/p/<handle>/<printer>" through the platform's shared front door.
	// Host + URIPath is the printer URI the client configured.
	URIPath     string
	ClientIP    string
	CreatedAt   time.Time
	CommittedAt time.Time
	DeliveredAt time.Time
}

// NewJob is what a Print-Job / Create-Job request knows before any document
// byte has been read.
type NewJob struct {
	Tenant         string
	Printer        string
	RequestingUser string
	JobName        string
	DocumentName   string
	DocumentFormat string
	Host           string
	URIPath        string
	ClientIP       string
}

// NewJobID mints a durable job identity.
func NewJobID() string { return "ipj_" + hxid.New().String() }

const jobCols = `id, tenant, printer, job_number, requesting_user, job_name, document_name,
	document_format, COALESCE(sha256, ''), size, state, state_reason, attempts, COALESCE(rid, ''),
	host, uri_path, client_ip, created_at, COALESCE(committed_at, ''), COALESCE(delivered_at, '')`

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (Job, error) {
	var j Job
	var created, committed, delivered string
	if err := row.Scan(&j.ID, &j.Tenant, &j.Printer, &j.Number, &j.RequestingUser, &j.JobName,
		&j.DocumentName, &j.DocumentFormat, &j.SHA256, &j.Size, &j.State, &j.StateReason,
		&j.Attempts, &j.Rid, &j.Host, &j.URIPath, &j.ClientIP, &created, &committed, &delivered); err != nil {
		return Job{}, err
	}
	j.CreatedAt = parseTS(created)
	j.CommittedAt = parseTS(committed)
	j.DeliveredAt = parseTS(delivered)
	return j, nil
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(tsLayout, s)
	return t
}

// CreateJob allocates the tenant's next job number and inserts the row in
// `receiving`, in one transaction. The number exists BEFORE the document is
// read so a client polling Get-Jobs (or canceling) sees an upload in flight.
func (s *Store) CreateJob(ctx context.Context, nj NewJob) (Job, error) {
	if nj.Tenant == "" || nj.Printer == "" {
		return Job{}, errors.New("ipp: create job: tenant and printer are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("ipp: create job: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// One statement allocates the number on both engines: the first job of a
	// tenant inserts 1, every later one bumps and returns the new value.
	var number int64
	if err := tx.QueryRowContext(ctx, s.rb(
		`INSERT INTO ipp_job_seq (tenant, next_number) VALUES (?, 1)
		 ON CONFLICT (tenant) DO UPDATE SET next_number = ipp_job_seq.next_number + 1
		 RETURNING next_number`), nj.Tenant).Scan(&number); err != nil {
		return Job{}, fmt.Errorf("ipp: allocate job number: %w", err)
	}

	now := s.ts()
	j := Job{
		ID: NewJobID(), Tenant: nj.Tenant, Printer: nj.Printer, Number: number,
		RequestingUser: nj.RequestingUser, JobName: nj.JobName, DocumentName: nj.DocumentName,
		DocumentFormat: nj.DocumentFormat, State: StateReceiving, Host: nj.Host, URIPath: nj.URIPath, ClientIP: nj.ClientIP,
		CreatedAt: parseTS(now),
	}
	if _, err := tx.ExecContext(ctx, s.rb(
		`INSERT INTO ipp_jobs (id, tenant, printer, job_number, requesting_user, job_name,
		     document_name, document_format, state, host, uri_path, client_ip, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		j.ID, j.Tenant, j.Printer, j.Number, j.RequestingUser, j.JobName, j.DocumentName,
		j.DocumentFormat, j.State, j.Host, j.URIPath, j.ClientIP, now, now); err != nil {
		return Job{}, fmt.Errorf("ipp: insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("ipp: create job: %w", err)
	}
	return j, nil
}

// SetCommitted moves a receiving job to committed: the document is in the
// CAS under sha and the tenant owns it. Returns false when the row was no
// longer `receiving` — canceled (or reaped) while the body streamed.
func (s *Store) SetCommitted(ctx context.Context, id, sha string, size int64, format, documentName string) (bool, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs
		    SET state = 'committed', sha256 = ?, size = ?, document_format = ?,
		        document_name = CASE WHEN ? = '' THEN document_name ELSE ? END,
		        committed_at = ?, updated_at = ?
		  WHERE id = ? AND state = 'receiving'`),
		sha, size, format, documentName, documentName, now, now, id)
	if err != nil {
		return false, fmt.Errorf("ipp: commit job: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkFailed moves a not-yet-delivered job to the terminal `failed` state:
// IPP infrastructure could not receive or deliver it. Never used for
// anything the stack did.
func (s *Store) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET state = 'failed', state_reason = ?, lease_node = NULL, lease_at = NULL, updated_at = ?
		  WHERE id = ? AND state IN ('receiving', 'committed')`),
		reason, s.ts(), id)
	if err != nil {
		return fmt.Errorf("ipp: mark failed: %w", err)
	}
	return nil
}

// Cancel cancels a job the client no longer wants. Only a job that has not
// been delivered (and is not mid-handoff under a lease) can be canceled;
// the returned Job tells the caller why when it could not.
func (s *Store) Cancel(ctx context.Context, tenant, printer string, number int64) (Job, bool, error) {
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET state = 'canceled', state_reason = 'canceled by client', updated_at = ?
		  WHERE tenant = ? AND printer = ? AND job_number = ?
		    AND state IN ('receiving', 'committed') AND lease_at IS NULL`),
		s.ts(), tenant, printer, number)
	if err != nil {
		return Job{}, false, fmt.Errorf("ipp: cancel: %w", err)
	}
	n, _ := res.RowsAffected()
	j, err := s.GetJob(ctx, tenant, printer, number)
	if err != nil {
		return Job{}, false, err
	}
	return j, n > 0, nil
}

// GetJob loads one job by the number the client knows it by.
func (s *Store) GetJob(ctx context.Context, tenant, printer string, number int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+jobCols+` FROM ipp_jobs WHERE tenant = ? AND printer = ? AND job_number = ?`),
		tenant, printer, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("ipp: get job: %w", err)
	}
	return j, nil
}

// GetJobByID loads one job by its durable id.
func (s *Store) GetJobByID(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, s.rb(`SELECT `+jobCols+` FROM ipp_jobs WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("ipp: get job: %w", err)
	}
	return j, nil
}

// ListJobs returns a printer's jobs, newest first. completed selects the
// terminal states (IPP which-jobs=completed); otherwise the live ones.
func (s *Store) ListJobs(ctx context.Context, tenant, printer string, completed bool, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	states := `('receiving', 'committed')`
	if completed {
		states = `('delivered', 'canceled', 'failed')`
	}
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT `+jobCols+` FROM ipp_jobs
		  WHERE tenant = ? AND printer = ? AND state IN `+states+`
		  ORDER BY job_number DESC LIMIT ?`), tenant, printer, limit)
	if err != nil {
		return nil, fmt.Errorf("ipp: list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return out, fmt.Errorf("ipp: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountReceiving is the tenant's uploads in flight (the busy gate).
func (s *Store) CountReceiving(ctx context.Context, tenant string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rb(
		`SELECT COUNT(*) FROM ipp_jobs WHERE tenant = ? AND state = 'receiving'`), tenant).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ipp: count receiving: %w", err)
	}
	return n, nil
}

// CountQueued is a printer's not-yet-delivered jobs (queued-job-count).
func (s *Store) CountQueued(ctx context.Context, tenant, printer string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rb(
		`SELECT COUNT(*) FROM ipp_jobs WHERE tenant = ? AND printer = ? AND state IN ('receiving', 'committed')`),
		tenant, printer).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ipp: count queued: %w", err)
	}
	return n, nil
}

// --- dispatcher side ---------------------------------------------------------

// LeaseCommitted leases up to limit committed, unleased jobs to node in ONE
// statement and returns the rows this call won (the scheduled store's claim
// shape: on Postgres the subselect takes FOR UPDATE SKIP LOCKED so competing
// dispatchers partition the set; on SQLite the single statement is atomic
// under the single writer). The state stays `committed` — a lease is a
// dispatcher's business, not the client's.
func (s *Store) LeaseCommitted(ctx context.Context, node string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	now := s.ts()
	rows, err := s.db.QueryContext(ctx, s.rb(
		`UPDATE ipp_jobs
		    SET lease_node = ?, lease_at = ?, attempts = attempts + 1, updated_at = ?
		  WHERE state = 'committed' AND lease_at IS NULL
		    AND id IN (SELECT id FROM ipp_jobs
		                WHERE state = 'committed' AND lease_at IS NULL
		                ORDER BY committed_at
		                LIMIT ?`+s.dialect.SkipLockedClause()+`)
		  RETURNING `+jobCols), node, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("ipp: lease committed: %w", err)
	}
	defer rows.Close()
	var won []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return won, fmt.Errorf("ipp: scan leased: %w", err)
		}
		won = append(won, j)
	}
	return won, rows.Err()
}

// MarkDelivered is the END of a print job's lifecycle: the envelope was
// accepted onto the TxCo execution path. rid names the run for tracing.
func (s *Store) MarkDelivered(ctx context.Context, id, rid string) error {
	now := s.ts()
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs
		    SET state = 'delivered', rid = ?, delivered_at = ?, lease_node = NULL, lease_at = NULL, updated_at = ?
		  WHERE id = ? AND state = 'committed'`), rid, now, now, id)
	if err != nil {
		return fmt.Errorf("ipp: mark delivered: %w", err)
	}
	return nil
}

// ReleaseLease hands a committed job back for another attempt (the bus did
// not accept it in time).
func (s *Store) ReleaseLease(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET lease_node = NULL, lease_at = NULL, updated_at = ?
		  WHERE id = ? AND state = 'committed'`), s.ts(), id)
	if err != nil {
		return fmt.Errorf("ipp: release lease: %w", err)
	}
	return nil
}

// ReleaseStaleLeases clears leases older than staleAfter — crash recovery
// for a node that died between leasing and delivering. Returns the count.
func (s *Store) ReleaseStaleLeases(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := s.now().Add(-staleAfter).Format(tsLayout)
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET lease_node = NULL, lease_at = NULL, updated_at = ?
		  WHERE state = 'committed' AND lease_at IS NOT NULL AND lease_at < ?`), s.ts(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("ipp: release stale leases: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// FailExhausted fails committed jobs the bus never accepted after
// maxAttempts leases — the one way delivery itself fails.
func (s *Store) FailExhausted(ctx context.Context, maxAttempts int) (int64, error) {
	if maxAttempts <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET state = 'failed', state_reason = 'delivery attempts exhausted', updated_at = ?
		  WHERE state = 'committed' AND lease_at IS NULL AND attempts >= ?`), s.ts(), maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("ipp: fail exhausted: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReapReceiving fails `receiving` rows older than olderThan: a Create-Job
// whose document never came, or an upload whose node died mid-body.
func (s *Store) ReapReceiving(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := s.now().Add(-olderThan).Format(tsLayout)
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE ipp_jobs SET state = 'failed', state_reason = 'document never arrived', updated_at = ?
		  WHERE state = 'receiving' AND created_at < ?`), s.ts(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("ipp: reap receiving: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Purge deletes terminal rows older than retention. The documents they
// point at stay in the CAS (there is no blob GC); only the job history goes.
func (s *Store) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := s.now().Add(-retention).Format(tsLayout)
	res, err := s.db.ExecContext(ctx, s.rb(
		`DELETE FROM ipp_jobs
		  WHERE state IN ('delivered', 'canceled', 'failed') AND updated_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("ipp: purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
