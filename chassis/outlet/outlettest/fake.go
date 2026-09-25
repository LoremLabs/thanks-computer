// Package outlettest is an in-memory outlet driver for tests: canned rows,
// injectable delays and failures, counters. It dials nothing. A test
// registers it with outlet.Register(d) (or hands it to Deps.Lookup), so a
// production binary never knows the driver "fake".
package outlettest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

// Driver is the fake. Fields are read at Open/Query time, so a test may
// change them between calls; guard concurrent changes with your own lock.
type Driver struct {
	// DriverName defaults to "fake".
	DriverName string
	// Columns and Rows are what every Query returns (before ceilings).
	Columns []string
	Rows    [][]any
	// RowsAffected is what Exec reports.
	RowsAffected int64
	// ExecReturns makes Exec also return Columns/Rows (a RETURNING clause).
	ExecReturns bool
	// RowsAfter makes the first RowsAfter queries return no rows and every
	// later one return Rows — a poll that eventually finds what it waits
	// for. Zero means every query returns Rows.
	RowsAfter int64

	OpenDelay  time.Duration
	QueryDelay time.Duration
	FailOpen   error
	FailQuery  error

	Opens   atomic.Int64
	Closes  atomic.Int64
	Queries atomic.Int64
	Execs   atomic.Int64

	mu sync.Mutex
	// LastDSN is a copy of the DSN the last Open saw — for a test to prove
	// the runtime handed the secret over, and that nothing else did.
	lastDSN    string
	lastEgress outlet.EgressParams
}

// LastDSN returns the DSN of the most recent Open.
func (d *Driver) LastDSN() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastDSN
}

// LastEgress returns the egress the most recent Open was handed.
func (d *Driver) LastEgress() outlet.EgressParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastEgress
}

func (d *Driver) Name() string {
	if d.DriverName == "" {
		return "fake"
	}
	return d.DriverName
}

// Open accepts `fake://host:port/...` DSNs. With a Guard it checks the host
// the way a real driver's dialer would, so the private-address gate is
// testable without a socket.
func (d *Driver) Open(ctx context.Context, p outlet.OpenParams) (outlet.Conn, error) {
	d.Opens.Add(1)
	d.mu.Lock()
	d.lastDSN = string(p.DSN)
	d.lastEgress = p.Egress
	d.mu.Unlock()
	if d.OpenDelay > 0 {
		select {
		case <-time.After(d.OpenDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if d.FailOpen != nil {
		return nil, d.FailOpen
	}
	u, err := url.Parse(string(p.DSN))
	if err != nil || u.Scheme != "fake" {
		return nil, outlet.NewError(outlet.CodeConnectFailed, "fake outlet: DSN must be fake://host:port/...")
	}
	if p.Guard != nil {
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = "5432"
		}
		if gerr := p.Guard.CheckAddr("tcp4", host+":"+port); gerr != nil {
			return nil, outlet.NewError(outlet.CodeConnectFailed, "outlet dial refused by egress policy")
		}
	}
	return &conn{d: d}, nil
}

type conn struct {
	d      *Driver
	closed atomic.Bool
}

func (c *conn) Query(ctx context.Context, req outlet.Request) (*outlet.Result, error) {
	n := c.d.Queries.Add(1)
	if c.d.RowsAfter > 0 && n <= c.d.RowsAfter {
		return &outlet.Result{Columns: append([]string(nil), c.d.Columns...), RowsJSON: []byte("[]"), Bytes: 2}, nil
	}
	return c.run(ctx, req, false)
}

func (c *conn) Exec(ctx context.Context, req outlet.Request) (*outlet.Result, error) {
	c.d.Execs.Add(1)
	return c.run(ctx, req, true)
}

func (c *conn) run(ctx context.Context, req outlet.Request, exec bool) (*outlet.Result, error) {
	if c.closed.Load() {
		return nil, outlet.NewError(outlet.CodeUnavailable, "fake outlet: pool is closed")
	}
	if c.d.QueryDelay > 0 {
		select {
		case <-time.After(c.d.QueryDelay):
		case <-ctx.Done():
			return nil, outlet.NewError(outlet.CodeTimeout, "outlet operation timed out")
		}
	}
	if c.d.FailQuery != nil {
		return nil, c.d.FailQuery
	}
	if strings.TrimSpace(req.SQL) == "" {
		return nil, outlet.NewError(outlet.CodeInvalidRequest, "empty statement")
	}
	res := &outlet.Result{Columns: append([]string(nil), c.d.Columns...), RowsJSON: []byte("[]")}
	if exec {
		res.RowsAffected = c.d.RowsAffected
		if !c.d.ExecReturns {
			res.Columns = nil
			return res, nil
		}
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, row := range c.d.Rows {
		if req.MaxRows > 0 && i+1 > req.MaxRows {
			return nil, &outlet.Error{Code: outlet.CodeResultTooLarge, Message: "result exceeded the row ceiling", Rows: i, Bytes: int64(buf.Len())}
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('{')
		for j, col := range c.d.Columns {
			if j > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(col)
			buf.Write(k)
			buf.WriteByte(':')
			var v any
			if j < len(row) {
				v = row[j]
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, outlet.NewError(outlet.CodeQueryFailed, "fake outlet: unencodable value")
			}
			buf.Write(b)
		}
		buf.WriteByte('}')
		if req.MaxBytes > 0 && int64(buf.Len()) > req.MaxBytes {
			return nil, &outlet.Error{Code: outlet.CodeResultTooLarge, Message: "result exceeded the byte ceiling", Rows: i + 1, Bytes: int64(buf.Len())}
		}
	}
	buf.WriteByte(']')
	res.RowsJSON = buf.Bytes()
	res.Count = len(c.d.Rows)
	res.Bytes = int64(buf.Len())
	return res, nil
}

func (c *conn) Close() error {
	if c.closed.Swap(true) {
		return errors.New("fake outlet: closed twice")
	}
	c.d.Closes.Add(1)
	return nil
}
