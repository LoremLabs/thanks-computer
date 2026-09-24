package outlet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"go.uber.org/zap"
)

// SecretSource materializes a declared secret by name for the calling
// tenant and stack, with the store's ordinary scoping: stack-scoped first,
// then tenant-wide. *secrets.Resolver satisfies it.
type SecretSource interface {
	MaterializeForOpSlug(ctx context.Context, tenantSlug, stack, name string) ([]byte, *secrets.SecretMetadata, error)
}

// DeclSource resolves (tenant, stack, name) → the stack's ACTIVE declaration
// and a hash of its bytes. It returns ErrNotDeclared when the stack has no
// outlet of that name. Package server implements it over the dbcache
// snapshot and the content store, the way txco://dataset finds a manifest.
type DeclSource interface {
	Lookup(ctx context.Context, tenant, stack, name string) (*Decl, string, error)
}

// Limits are the node's ceilings and pool defaults; a declaration can only
// tighten them.
type Limits struct {
	MaxRows      int
	MaxBytes     int64
	PoolMaxConns int
	PoolIdle     time.Duration
	// IdleClose is how long an unused pool stays open before the runtime
	// closes it. The next call reopens it.
	IdleClose time.Duration
}

// Deps is everything a Runtime needs. Lookup defaults to the package
// registry; Now to time.Now.
type Deps struct {
	Lookup  func(driver string) (Driver, bool)
	Guard   egress.Guard
	Secrets SecretSource
	Decls   DeclSource
	Logger  *zap.Logger
	Limits  Limits
	Now     func() time.Time
}

// Call is one outlet operation as the processor hands it over: the trusted
// tenant and stack of the dispatching op (never envelope fields), the
// outlet and operation from the EXEC target, the literal statement and the
// converted arguments.
type Call struct {
	Tenant string
	Stack  string
	Outlet string
	Op     string
	SQL    string
	Args   []any
}

// poolKey identifies one pool. A secret rotation (new VersionNo) or a
// redeploy that changes the declaration (new DeclHash) resolves to a new key
// and therefore a new pool; the old one closes once idle.
type poolKey struct {
	Tenant, Stack, Outlet string
	SecretID              string
	Version               int
	DeclHash              string
}

// entry is one pool's place in the open set. ready is closed once the open
// attempt finishes, so concurrent first calls create exactly one pool.
type entry struct {
	key        poolKey
	driver     string
	ready      chan struct{}
	conn       Conn
	err        error
	refs       int
	lastUsed   time.Time
	superseded bool
}

// Runtime owns every open outlet pool on this node.
type Runtime struct {
	deps Deps

	mu     sync.Mutex
	pools  map[poolKey]*entry
	closed bool
	wg     sync.WaitGroup
}

// NewRuntime builds a runtime; call Start to run the idle sweeper.
func NewRuntime(d Deps) *Runtime {
	if d.Lookup == nil {
		d.Lookup = Lookup
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = zap.NewNop()
	}
	if d.Limits.IdleClose <= 0 {
		d.Limits.IdleClose = 5 * time.Minute
	}
	return &Runtime{deps: d, pools: map[poolKey]*entry{}}
}

// Start runs the idle sweeper until ctx ends.
func (r *Runtime) Start(ctx context.Context) {
	period := r.deps.Limits.IdleClose / 2
	if period < time.Second {
		period = time.Second
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Sweep()
			}
		}
	}()
}

// Sweep closes every pool that is idle past the limit or superseded and no
// longer in use. Exposed for tests; the sweeper calls it on a ticker.
func (r *Runtime) Sweep() {
	now := r.deps.Now()
	r.mu.Lock()
	var toClose []*entry
	for k, e := range r.pools {
		if e.refs != 0 || !isReady(e) {
			continue
		}
		if e.superseded || now.Sub(e.lastUsed) > r.deps.Limits.IdleClose {
			delete(r.pools, k)
			toClose = append(toClose, e)
		}
	}
	r.mu.Unlock()
	for _, e := range toClose {
		r.closeEntry(e, "idle")
	}
}

// Close stops accepting calls and closes every pool. The server calls it
// after in-flight runs have drained.
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	all := make([]*entry, 0, len(r.pools))
	for k, e := range r.pools {
		delete(r.pools, k)
		all = append(all, e)
	}
	r.mu.Unlock()
	for _, e := range all {
		<-e.ready
		r.closeEntry(e, "shutdown")
	}
	r.wg.Wait()
}

// OpenPools reports how many pools are open now (tests, later metrics).
func (r *Runtime) OpenPools() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pools)
}

// Outcome is what the processor reports: the result or the classified
// error, plus the driver name and duration for the trace.
type Outcome struct {
	Result   *Result
	Err      *Error
	Driver   string
	Duration time.Duration
}

// Call runs one operation end to end: declaration, access, secret, pool,
// statement. Every failure is an *Error the processor writes to the
// envelope; nothing here returns a Go error.
func (r *Runtime) Call(ctx context.Context, c Call) Outcome {
	start := r.deps.Now()
	out := r.call(ctx, c)
	out.Duration = r.deps.Now().Sub(start)
	if out.Err != nil {
		lvl := r.deps.Logger.Debug
		switch out.Err.Code {
		case CodeConnectFailed, CodeUnavailable, CodeOutcomeUnknown, CodeAuthFailed:
			lvl = r.deps.Logger.Warn
		}
		lvl("outlet call failed",
			zap.String("outlet", c.Outlet), zap.String("driver", out.Driver), zap.String("op", c.Op),
			zap.String("code", out.Err.Code), zap.String("sqlstate", out.Err.SQLState),
			zap.Duration("duration", out.Duration))
	}
	return out
}

func (r *Runtime) call(ctx context.Context, c Call) Outcome {
	if r.deps.Decls == nil {
		return Outcome{Err: NewError(CodeUnavailable, "outlet runtime has no declaration source")}
	}
	decl, hash, err := r.deps.Decls.Lookup(ctx, c.Tenant, c.Stack, c.Outlet)
	if err != nil {
		if errors.Is(err, ErrNotDeclared) {
			return Outcome{Err: NewError(CodeNotDeclared, "outlet is not declared in this stack (add "+DeclPath(c.Outlet)+")")}
		}
		r.deps.Logger.Warn("outlet declaration lookup failed", zap.String("outlet", c.Outlet), zap.Error(err))
		return Outcome{Err: NewError(CodeUnavailable, "outlet declaration could not be read")}
	}
	out := Outcome{Driver: decl.Driver}
	if c.Op == OpExec && !decl.Writable() {
		out.Err = NewError(CodeInvalidRequest, "outlet is declared access: read; exec is refused")
		return out
	}
	if c.Op != OpQuery && c.Op != OpExec {
		out.Err = NewError(CodeInvalidRequest, "operation must be query or exec")
		return out
	}
	drv, ok := r.deps.Lookup(decl.Driver)
	if !ok {
		out.Err = NewError(CodeUnavailable, "outlet driver is not built into this chassis")
		return out
	}
	if r.deps.Secrets == nil {
		out.Err = NewError(CodeMissingSecret, "no secret store is configured on this node, so outlet secret "+decl.Secret+" cannot be read")
		return out
	}
	dsn, meta, err := r.deps.Secrets.MaterializeForOpSlug(ctx, c.Tenant, c.Stack, decl.Secret)
	if err != nil {
		if errors.Is(err, secrets.ErrSecretNotFound) {
			out.Err = NewError(CodeMissingSecret, "outlet secret "+decl.Secret+" is not set for this stack or tenant")
			return out
		}
		r.deps.Logger.Warn("outlet secret materialize failed", zap.String("outlet", c.Outlet), zap.Error(err))
		out.Err = NewError(CodeUnavailable, "outlet secret "+decl.Secret+" could not be read")
		return out
	}
	key := poolKey{Tenant: c.Tenant, Stack: c.Stack, Outlet: c.Outlet, SecretID: meta.SecretID, Version: meta.VersionNo, DeclHash: hash}
	e, err := r.acquire(ctx, key, decl.Driver, drv, dsn)
	secrets.Zero(dsn)
	if err != nil {
		if ctx.Err() != nil {
			out.Err = NewError(CodeTimeout, "outlet operation timed out")
			return out
		}
		out.Err = AsError(err, CodeConnectFailed, "outlet could not be opened")
		return out
	}
	defer r.release(e)

	if decl.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(decl.Timeout)*time.Millisecond)
		defer cancel()
	}
	req := Request{SQL: c.SQL, Args: c.Args, MaxRows: r.deps.Limits.MaxRows, MaxBytes: r.deps.Limits.MaxBytes}
	if decl.MaxRows > 0 && (req.MaxRows == 0 || decl.MaxRows < req.MaxRows) {
		req.MaxRows = decl.MaxRows
	}
	var res *Result
	if c.Op == OpExec {
		res, err = e.conn.Exec(ctx, req)
	} else {
		res, err = e.conn.Query(ctx, req)
	}
	if err != nil {
		oe := AsError(err, CodeUnavailable, "outlet operation failed")
		// Whatever the driver reported, a passed deadline is a timeout —
		// one code a stack can dispatch on — unless the driver already
		// knows the outcome is unknown, which is the more important fact.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && oe.Code != CodeOutcomeUnknown {
			oe = &Error{Code: CodeTimeout, Message: "outlet operation timed out", Rows: oe.Rows, Bytes: oe.Bytes}
		}
		out.Err = oe
		return out
	}
	if res.RowsJSON == nil {
		res.RowsJSON = []byte("[]")
	}
	out.Result = res
	return out
}

// acquire returns the pool for key, opening it when absent. Concurrent
// first calls wait for one open; a failed open is never cached. Older pools
// for the same outlet are marked superseded so they close once unused.
func (r *Runtime) acquire(ctx context.Context, key poolKey, driver string, drv Driver, dsn []byte) (*entry, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, NewError(CodeUnavailable, "outlet runtime is shutting down")
	}
	if e, ok := r.pools[key]; ok {
		e.refs++
		r.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			r.release(e)
			return nil, ctx.Err()
		}
		if e.err != nil {
			r.release(e)
			return nil, e.err
		}
		return e, nil
	}
	e := &entry{key: key, driver: driver, ready: make(chan struct{}), refs: 1, lastUsed: r.deps.Now()}
	r.pools[key] = e
	var superseded []*entry
	for k, o := range r.pools {
		if o != e && k.Tenant == key.Tenant && k.Stack == key.Stack && k.Outlet == key.Outlet && !o.superseded {
			o.superseded = true
			if o.refs == 0 && isReady(o) {
				delete(r.pools, k)
				superseded = append(superseded, o)
			}
		}
	}
	r.mu.Unlock()
	for _, o := range superseded {
		r.closeEntry(o, "superseded")
	}

	// Open outside the lock: other outlets stay reachable meanwhile. The
	// DSN is live only for this call; the caller zeroes it on return.
	e.conn, e.err = drv.Open(ctx, OpenParams{
		DSN:    dsn,
		Guard:  r.deps.Guard,
		Logger: r.deps.Logger.With(zap.String("outlet", key.Outlet), zap.String("driver", driver)),
		Pool:   PoolLimits{MaxConns: r.deps.Limits.PoolMaxConns, IdleTimeout: r.deps.Limits.PoolIdle},
	})
	close(e.ready)
	if e.err != nil {
		r.release(e)
		return nil, e.err
	}
	r.deps.Logger.Info("outlet pool opened", zap.String("outlet", key.Outlet), zap.String("driver", driver), zap.String("stack", key.Stack))
	return e, nil
}

func (r *Runtime) release(e *entry) {
	r.mu.Lock()
	e.refs--
	e.lastUsed = r.deps.Now()
	closeNow := false
	if e.refs == 0 && r.pools[e.key] == e && (e.err != nil || e.superseded) {
		delete(r.pools, e.key) // a failed open is never cached
		closeNow = true
	}
	r.mu.Unlock()
	if closeNow {
		r.closeEntry(e, "superseded")
	}
}

func (r *Runtime) closeEntry(e *entry, why string) {
	if e.conn == nil {
		return
	}
	if err := e.conn.Close(); err != nil {
		r.deps.Logger.Debug("outlet pool close", zap.String("outlet", e.key.Outlet), zap.String("why", why), zap.Error(err))
		return
	}
	r.deps.Logger.Info("outlet pool closed", zap.String("outlet", e.key.Outlet), zap.String("driver", e.driver), zap.String("why", why))
}

func isReady(e *entry) bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}
