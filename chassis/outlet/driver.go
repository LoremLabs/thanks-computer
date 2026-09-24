package outlet

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"go.uber.org/zap"
)

// The two operations every driver answers. `query` runs read-only (a
// Postgres driver wraps it in BEGIN READ ONLY so the database itself refuses
// a mutation); `exec` may mutate and always runs as an explicit transaction
// so a ceiling crossed by RETURNING rows rolls it back.
const (
	OpQuery = "query"
	OpExec  = "exec"
)

// PoolLimits bounds one outlet's pool on this node. Pools are per node, so
// a customer database sees roughly nodes × MaxConns connections.
type PoolLimits struct {
	// MaxConns caps live connections; the minimum is always zero, so an
	// idle outlet costs nothing.
	MaxConns int
	// IdleTimeout closes a connection unused for this long.
	IdleTimeout time.Duration
}

// OpenParams carries everything a Driver needs to open one pool. The
// credential is live only for the duration of the Open call.
type OpenParams struct {
	// DSN is the materialized secret value. The runtime zeroes it the
	// moment Open returns; a driver that must keep connection parameters
	// (a pool config) copies what it needs and must never log or encode it.
	DSN []byte
	// Guard is the egress policy for every dial. When non-nil the driver
	// MUST route its connections through it (net.Dialer.Control =
	// egress.DialControl(guard)) so a DSN cannot point the chassis at
	// loopback, link-local or private address space. nil = no policy
	// (dev/test).
	Guard egress.Guard
	// Logger is scoped for this outlet. Drivers log codes and durations,
	// never SQL, values, rows or the DSN.
	Logger *zap.Logger
	Pool   PoolLimits
}

// Request is one statement with its bound arguments and ceilings.
type Request struct {
	SQL  string
	Args []any
	// MaxRows and MaxBytes are safety ceilings: crossing one aborts the read
	// with CodeResultTooLarge and no rows. A driver measures while rows
	// stream so a huge result is dropped early rather than buffered.
	MaxRows  int
	MaxBytes int64
}

// Result is what a statement produced. RowsJSON is a JSON array of objects,
// one per row in column order, "[]" when there are none; Columns gives
// SELECT order, which object keys don't. RowsAffected is set by exec.
type Result struct {
	Columns      []string
	RowsJSON     []byte
	Count        int
	RowsAffected int64
	// Bytes is the size of RowsJSON — what fuel is charged on.
	Bytes int64
}

// Conn is one open pool. It is safe for concurrent use; the runtime hands
// the same Conn to every call on the outlet until the pool is superseded or
// idle-closed.
//
// Errors: a Conn returns *Error for every failure it can classify (the
// caller reports Code and Message to the stack verbatim); anything else is
// reported as CodeUnavailable with a fixed message.
type Conn interface {
	Query(ctx context.Context, req Request) (*Result, error)
	Exec(ctx context.Context, req Request) (*Result, error)
	Close() error
}

// Driver opens pools for one protocol. Open must not block on the network:
// the first statement surfaces connect and auth failures, so a database that
// is down never blocks anything but the op that needs it.
type Driver interface {
	Name() string
	Open(ctx context.Context, p OpenParams) (Conn, error)
}

// registry maps driver name → Driver. Drivers self-register via init() and
// the chassis activates one with a blank import, like decide backends.
var (
	registryMu sync.RWMutex
	registry   = map[string]Driver{}
)

// Register adds a driver under its Name. Re-registering overwrites (test
// support).
func Register(d Driver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[d.Name()] = d
}

// Lookup returns the registered driver, or (nil, false).
func Lookup(name string) (Driver, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// Known reports whether a driver of that name is built into this binary.
func Known(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// Registered returns the registered driver names, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
