package postgres

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

// conn is one open pool.
type conn struct {
	pool   *pgxpool.Pool
	log    *zap.Logger
	closed atomic.Bool
}

// Query runs one statement in a READ ONLY transaction. Postgres decides
// what is legal, so a read op that mutates fails there — including inside
// a data-modifying CTE the leading-keyword lint lets through on purpose.
func (c *conn) Query(ctx context.Context, req outlet.Request) (*outlet.Result, error) {
	return c.run(ctx, req, pgx.TxOptions{AccessMode: pgx.ReadOnly}, false)
}

// Exec runs one statement in an explicit transaction: begin, execute, read
// the RETURNING rows, check them against the ceilings, commit. A ceiling
// crossed by the returned rows rolls back, which gives the invariant an op
// relies on: ok:false from an exec means the chassis did not commit. The
// one case it can't cover — a connection lost while COMMIT is in flight —
// is CodeOutcomeUnknown.
func (c *conn) Exec(ctx context.Context, req outlet.Request) (*outlet.Result, error) {
	return c.run(ctx, req, pgx.TxOptions{}, true)
}

func (c *conn) run(ctx context.Context, req outlet.Request, opts pgx.TxOptions, mutating bool) (*outlet.Result, error) {
	if c.closed.Load() {
		return nil, outlet.NewError(outlet.CodeUnavailable, "outlet pool is closed")
	}
	// The statement runs under its own cancellable context so a byte
	// ceiling can abandon the connection (see readRows) without touching
	// the caller's.
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tx, err := c.pool.BeginTx(qctx, opts)
	if err != nil {
		return nil, classify(err, phaseConnect, mutating)
	}
	rows, err := tx.Query(qctx, req.SQL, req.Args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, classify(err, phaseQuery, mutating)
	}
	res, abandon, rerr := readRows(rows, req)
	if rerr != nil {
		if abandon {
			// Cancelling mid-result makes pgx close this connection, which
			// discards the rest of an arbitrarily large result instead of
			// draining it. The transaction dies with the connection.
			cancel()
		}
		_ = tx.Rollback(ctx)
		return nil, rerr
	}
	if err := tx.Commit(qctx); err != nil {
		return nil, classify(err, phaseCommit, mutating)
	}
	return res, nil
}

// Close releases the pool.
func (c *conn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.pool.Close()
	return nil
}

// readRows streams the result into a JSON array of row objects, one key
// per column in SELECT order, and stops at the first ceiling crossed. It
// always closes rows. abandon is true when the caller should cut the
// connection rather than let pgx drain the remainder: a row ceiling means
// a handful of extra rows (drain them, keep the connection); a byte
// ceiling means the rest may be arbitrarily large.
func readRows(rows pgx.Rows, req outlet.Request) (res *outlet.Result, abandon bool, err error) {
	defer rows.Close()
	fds := rows.FieldDescriptions()
	var cols []string
	if len(fds) > 0 {
		cols = make([]string, len(fds))
		seen := make(map[string]struct{}, len(fds))
		for i, fd := range fds {
			if _, dup := seen[fd.Name]; dup {
				return nil, false, outlet.NewError(outlet.CodeInvalidRequest, "result has two columns named "+quoteIdent(fd.Name)+"; alias one of them")
			}
			seen[fd.Name] = struct{}{}
			cols[i] = fd.Name
		}
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	count := 0
	for rows.Next() {
		if req.MaxRows > 0 && count+1 > req.MaxRows {
			return nil, false, &outlet.Error{Code: outlet.CodeResultTooLarge, Message: "result exceeded the row ceiling; no rows returned", Rows: count, Bytes: int64(buf.Len())}
		}
		vals, verr := rows.Values()
		if verr != nil {
			return nil, false, classify(verr, phaseQuery, false)
		}
		if count > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('{')
		for i, fd := range fds {
			if i > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(fd.Name)
			buf.Write(k)
			buf.WriteByte(':')
			encodeValue(&buf, fd.DataTypeOID, vals[i])
		}
		buf.WriteByte('}')
		count++
		if req.MaxBytes > 0 && int64(buf.Len()) > req.MaxBytes {
			return nil, true, &outlet.Error{Code: outlet.CodeResultTooLarge, Message: "result exceeded the byte ceiling; no rows returned", Rows: count, Bytes: int64(buf.Len())}
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, false, classify(rerr, phaseQuery, false)
	}
	buf.WriteByte(']')
	return &outlet.Result{
		Columns:      cols,
		RowsJSON:     buf.Bytes(),
		Count:        count,
		RowsAffected: rows.CommandTag().RowsAffected(),
		Bytes:        int64(buf.Len()),
	}, false, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// phase says where an error happened, which decides what an unclassified
// one means: before the statement was sent it's a connect failure the op
// may retry; after COMMIT was sent on a mutating transaction nobody knows.
type phase int

const (
	phaseConnect phase = iota
	phaseQuery
	phaseCommit
)

// classify maps a pgx error to an outlet code with a fixed message. Driver
// text is never passed through: connection errors name the host, user and
// database, and query errors quote the statement.
func classify(err error, ph phase, mutating bool) *outlet.Error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		switch {
		case strings.HasPrefix(code, "23"):
			return &outlet.Error{Code: outlet.CodeConstraint, Message: "the database refused the statement: constraint violation", SQLState: code}
		case strings.HasPrefix(code, "28"):
			return &outlet.Error{Code: outlet.CodeAuthFailed, Message: "the database rejected the credentials", SQLState: code}
		case code == "57014":
			return &outlet.Error{Code: outlet.CodeTimeout, Message: "the statement was cancelled", SQLState: code}
		case code == "57P01", code == "57P02", code == "57P03", code == "53300", code == "53400", strings.HasPrefix(code, "08"):
			return &outlet.Error{Code: outlet.CodeUnavailable, Message: "the database is unavailable", SQLState: code}
		case code == "25006":
			return &outlet.Error{Code: outlet.CodeQueryFailed, Message: "a query may not modify data; use outlet://<name>/exec on a write outlet", SQLState: code}
		}
		return &outlet.Error{Code: outlet.CodeQueryFailed, Message: "the database refused the statement", SQLState: code}
	}
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return outlet.NewError(outlet.CodeQueryFailed, "the transaction was rolled back")
	}
	if ph == phaseCommit && mutating {
		// COMMIT was sent and no answer came back: the statement may or
		// may not be durable. Never reported as connect_failed or
		// unavailable — the op must look before acting again.
		return outlet.NewError(outlet.CodeOutcomeUnknown, "commit outcome unknown: the connection was lost while committing; verify before retrying")
	}
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return outlet.NewError(outlet.CodeTimeout, "outlet operation timed out")
	}
	if errors.Is(err, context.Canceled) {
		return outlet.NewError(outlet.CodeTimeout, "outlet operation was cancelled")
	}
	var connErr *pgconn.ConnectError
	var netErr net.Error
	var opErr *net.OpError
	var tlsErr *tls.CertificateVerificationError
	var x509Err x509.UnknownAuthorityError
	switch {
	case errors.As(err, &connErr), errors.As(err, &opErr), errors.As(err, &netErr),
		errors.As(err, &tlsErr), errors.As(err, &x509Err), pgconn.SafeToRetry(err), ph == phaseConnect:
		return outlet.NewError(outlet.CodeConnectFailed, "the database could not be reached (DNS, TLS, refused, or blocked by the egress policy)")
	}
	return outlet.NewError(outlet.CodeUnavailable, "the database connection failed")
}
