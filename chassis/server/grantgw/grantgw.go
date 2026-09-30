// Package grantgw is the grant gateway: the one place a request made with a
// run grant is decided. Work that was dispatched somewhere else (a command in
// a workspace) opens a SANDBOX — a named bundle, declared by the stack, of
// what a program may hold (chassis/sandbox). Whichever way it was opened — by
// the rule that started the command (`WITH grant, sandbox`) or by the program
// itself (`txco sandbox`) — the request arrives here, and for each secret the
// sandbox names the same things happen in the same order:
//
//  1. HARD CHECKS, the chassis's alone. The grant is genuine and unexpired,
//     its row is live, its generation is current, its tenant is live, and it
//     names the sandbox. A request that fails one is refused and no stack
//     runs.
//  2. THE PROPOSAL, the chassis's own answer. The run grant names the
//     secret, the principal holds a standing grant to release it, the
//     secret's pull policy admits this node, budget remains. All yes:
//     propose to allow.
//  3. THE TENANT'S `_grant` STACK. The request and the proposal are
//     presented to it as an ordinary pipeline run, so admission, tracing
//     and usage apply as they do to every inlet. A rule may write
//     `@grant.res.allow` either way. No rule writes it: the proposal stands.
//  4. THE VERDICT. Refused: one answer, whatever the reason. Allowed: the
//     budget is charged, and only then is the secret handed over.
//
// A sandbox opens whole or not at all: one secret refused, and the program
// is handed nothing. What was already released is discarded; what was
// charged stays charged.
//
// Everything fails closed. An error, a timeout or a refusal of admission in
// the run is a refusal, whatever was proposed.
//
// The trace of the `_grant` run is the record of a request. The gateway
// keeps no journal of its own.
package grantgw

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/rungrant"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// The ways a request is made, stamped as `@grant.via`: a sandbox is opened
// by the rule that starts the command or by the program itself; a
// capability is called over HTTP (chassis/server/capgw).
const (
	ViaExec     = "exec"     // the rule that starts the command: `WITH grant, sandbox`
	ViaLauncher = "launcher" // the program itself: `txco sandbox`
	ViaHTTP     = "http"     // a capability call: POST /v1/cap/<name> with the token
)

// Open is one request to open a sandbox. Exactly one of Token and GrantID
// says which grant: the token the work was handed (the launcher presents
// it), or the row the chassis itself holds (the exec names it, with the
// tenant it belongs to).
type Open struct {
	Token    string
	TenantID string
	GrantID  string
	Sandbox  string
	Via      string
}

// Answer is what the presentation gets back. It tells the program one of
// three things and nothing more: what it holds, a refusal, or that the
// chassis could not answer.
type Answer struct {
	Allowed bool
	// Env is what the sandbox hands over, variable → value, when Allowed.
	// The caller zeroes every value (secrets.Zero) once it has handed them
	// on.
	Env map[string][]byte
	// Unavailable: the chassis could not decide — a store was down, the
	// chassis is stopping. Not a refusal, and not to be retried forever.
	Unavailable bool
	// Reason is why not. For the log and for tests; never for the program.
	Reason string
}

func refused(reason string) Answer     { return Answer{Reason: reason} }
func unavailable(reason string) Answer { return Answer{Unavailable: true, Reason: reason} }

// Zero overwrites every value the answer holds.
func (a Answer) Zero() {
	for _, v := range a.Env {
		secrets.Zero(v)
	}
}

// Gateway decides requests made with run grants.
type Gateway struct {
	ctx     context.Context
	log     *zap.Logger
	ids     *authn.Store
	signer  *rungrant.Signer // nil: no master key, so no token can be signed or checked
	timeout time.Duration
	seen    *refusals
	now     func() time.Time

	// Seams for tests; New wires the real ones.
	tenantSlug func(ctx context.Context, tenantID string) (string, error)
	tenantID   func(ctx context.Context, slug string) (string, error)
	resolve    func(ctx context.Context, tenantID, stack, name string) (*secrets.SecretMetadata, error)
	decrypt    func(ctx context.Context, meta *secrets.SecretMetadata) ([]byte, error)
	runStack   func(ctx context.Context, payload string) (string, error)

	// Where a program opens a sandbox at run time, set by the socket's
	// controller once it is up. Empty: that way is off on this node.
	mu     sync.RWMutex
	socket string
	bin    string
}

// Config is what New needs beyond the stores.
type Config struct {
	// DecideTimeout bounds the `_grant` run. Past it the request is refused.
	DecideTimeout time.Duration
	// RefusalWindow is how long a refusal is remembered. 0 turns that off.
	RefusalWindow time.Duration
}

// errNoTenant: the tenant directory has no live tenant of that id or slug.
var errNoTenant = errors.New("grantgw: no such tenant")

// New builds the gateway over the identity store and the chassis's
// processor. signer may be nil (no master key on this node): every request
// that presents a token is then unavailable, and no grant can be handed to
// a command.
func New(ctx context.Context, pu *processor.Unit, ids *authn.Store, signer *rungrant.Signer, cfg Config) *Gateway {
	if cfg.DecideTimeout <= 0 {
		cfg.DecideTimeout = 5 * time.Second
	}
	g := &Gateway{
		ctx: ctx, log: pu.Logger, ids: ids, signer: signer,
		timeout: cfg.DecideTimeout, seen: newRefusals(cfg.RefusalWindow), now: time.Now,
	}
	if g.log == nil {
		g.log = zap.NewNop()
	}
	g.bin, _ = os.Executable()

	snapshot := func() *sql.DB {
		if pu.Dbc == nil {
			return nil
		}
		pu.Dbc.Mu.Lock()
		defer pu.Dbc.Mu.Unlock()
		return pu.Dbc.Db
	}
	g.tenantSlug = func(ctx context.Context, tenantID string) (string, error) {
		return lookupTenant(ctx, snapshot(), `SELECT slug FROM tenants WHERE tenant_id = ? AND revoked_at IS NULL`, tenantID)
	}
	g.tenantID = func(ctx context.Context, slug string) (string, error) {
		return lookupTenant(ctx, snapshot(), `SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug)
	}
	g.resolve = func(ctx context.Context, tenantID, stack, name string) (*secrets.SecretMetadata, error) {
		if pu.Secrets == nil || pu.Secrets.Store() == nil {
			return nil, errors.New("grantgw: no secret store on this node")
		}
		return pu.Secrets.Store().ResolveForRelease(ctx, tenantID, stack, name)
	}
	g.decrypt = func(ctx context.Context, meta *secrets.SecretMetadata) ([]byte, error) {
		if pu.Secrets == nil || pu.Secrets.Store() == nil {
			return nil, errors.New("grantgw: no secret store on this node")
		}
		return pu.Secrets.Store().DecryptResolved(ctx, meta)
	}
	g.runStack = func(ctx context.Context, payload string) (string, error) {
		return RunOnBus(ctx, g.ctx, pu.Bus, payload)
	}
	return g
}

func lookupTenant(ctx context.Context, db *sql.DB, query, arg string) (string, error) {
	if db == nil {
		return "", errors.New("grantgw: no tenant directory on this node")
	}
	qctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	var out string
	err := db.QueryRowContext(qctx, query, arg).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && out == "") {
		return "", errNoTenant
	}
	return out, err
}

// SetClock replaces the gateway's clock (tests).
func (g *Gateway) SetClock(now func() time.Time) { g.now = now }

// ServeSocket records where the launcher reaches this chassis. "" turns it
// off. Called by the socket's controller once it is listening.
func (g *Gateway) ServeSocket(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.socket = path
}

var (
	errShutdown      = errors.New("grantgw: chassis shutting down")
	errStackStreamed = errors.New("grantgw: the _grant stack streamed a response; it has no client to stream to")
)

// RunOnBus sends one envelope through the bus and waits for its final
// payload: the round trip every inlet makes. Exported for the capability
// inlet, which runs the tenant's `_cap` stack the same way. The result channel is buffered
// and, when the wait is abandoned, drained to its end — the processor sends
// on it without a select, so an unread channel would hold its goroutine for
// good.
func RunOnBus(ctx, life context.Context, bus chan<- *event.Envelope, payload string) (string, error) {
	if bus == nil {
		return "", errors.New("grantgw: no bus")
	}
	resCh := make(chan event.Payload, 8)
	select {
	case bus <- event.PackageJSON(ctx, payload, resCh, srcName):
	case <-ctx.Done():
		return "", ctx.Err()
	case <-life.Done():
		return "", errShutdown
	}
	abandon := func(err error) (string, error) {
		go func() {
			for {
				select {
				case p := <-resCh:
					switch p.Type {
					case event.StreamHead, event.StreamChunk:
						continue
					}
					return
				case <-life.Done():
					return
				}
			}
		}()
		return "", err
	}
	streaming := false
	for {
		select {
		case res := <-resCh:
			switch res.Type {
			case event.StreamHead, event.StreamChunk:
				streaming = true
				continue
			case event.StreamEnd:
				return "", errStackStreamed
			case event.ErrorStr:
				return "", errors.New("pipeline error: " + res.Raw)
			default:
				if streaming {
					return "", errStackStreamed
				}
				return res.Raw, nil
			}
		case <-ctx.Done():
			return abandon(ctx.Err())
		case <-life.Done():
			return abandon(errShutdown)
		}
	}
}

// identify finds the run grant a request was made with and runs the checks
// that are the chassis's alone. It returns the grant, or why not.
func (g *Gateway) identify(ctx context.Context, o Open, now time.Time) (authn.RunGrant, Answer, bool) {
	var (
		grant authn.RunGrant
		err   error
	)
	switch {
	case o.Token != "" && o.GrantID != "":
		return grant, refused(reasonToken), false
	case o.Token != "":
		if g.signer == nil {
			return grant, unavailable("no signing key on this node"), false
		}
		claims, verr := g.signer.Verify(o.Token, now)
		switch {
		case errors.Is(verr, rungrant.ErrExpired):
			return grant, refused(reasonExpired), false
		case verr != nil:
			return grant, refused(reasonToken), false
		}
		grant, err = g.ids.ReadRunGrant(ctx, claims.Tenant, claims.Grant)
		if err == nil && (grant.Generation != claims.Generation || grant.Run != claims.Run ||
			grant.Principal.ID != claims.Principal) {
			// Genuine, and not about this row as it is: every claim the
			// token makes must be the row's own.
			return grant, refused(reasonGeneration), false
		}
	case o.GrantID != "" && o.TenantID == "":
		return grant, refused(reasonNotLive), false
	case o.GrantID != "":
		grant, err = g.ids.ReadRunGrant(ctx, o.TenantID, o.GrantID)
	default:
		return grant, refused(reasonToken), false
	}
	switch {
	case errors.Is(err, authn.ErrNotFound):
		return grant, refused(reasonNotLive), false
	case err != nil:
		g.log.Warn("grant: reading the run grant failed", zap.Error(err))
		return grant, unavailable("identity store"), false
	case !grant.Live(now):
		return grant, refused(reasonNotLive), false
	}
	return grant, Answer{}, true
}

// OpenSandbox decides one request to open a sandbox and, when every secret
// it names is allowed, hands them all over under the sandbox's variable
// names. One refused: nothing is handed over, and the answer is that
// refusal.
func (g *Gateway) OpenSandbox(ctx context.Context, o Open) Answer {
	now := g.now()
	grant, ans, ok := g.identify(ctx, o, now)
	if !ok {
		g.log.Info("grant: refused before any rule ran",
			zap.String("reason", ans.Reason), zap.Bool("unavailable", ans.Unavailable),
			zap.String("via", o.Via), zap.String("sandbox", o.Sandbox), zap.String("grant", grant.ID))
		return ans
	}
	fields := []zap.Field{
		zap.String("grant", grant.ID), zap.String("run", grant.Run),
		zap.String("principal", grant.Principal.ID), zap.String("via", o.Via),
		zap.String("sandbox", o.Sandbox),
	}
	hard := func(ans Answer) Answer {
		g.log.Info("grant: refused before any rule ran",
			append(fields, zap.String("reason", ans.Reason), zap.Bool("unavailable", ans.Unavailable))...)
		return ans
	}
	env, ok := grant.Sandbox(o.Sandbox)
	if !ok {
		return hard(refused(reasonSandbox))
	}
	tenant, err := g.tenantSlug(ctx, grant.TenantID)
	switch {
	case errors.Is(err, errNoTenant):
		return hard(refused(reasonTenant))
	case err != nil:
		g.log.Warn("grant: reading the tenant failed", zap.Error(err))
		return hard(unavailable("tenant directory"))
	}

	vars := make([]string, 0, len(env))
	for v := range env {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	out := make(map[string][]byte, len(vars))
	for _, v := range vars {
		kind, name, perr := sandbox.ParseRef(env[v])
		var res authn.Resource
		if perr == nil {
			res, perr = authn.NewResource(authn.ResourceKind(kind), name)
		}
		var ans Answer
		switch {
		case perr != nil:
			// The row was checked when it was written; this is a row that
			// changed under us.
			ans = hard(refused(reasonName))
		case res.Kind != authn.ResourceSecret:
			// A capability is called, not handed over; nothing here can run one.
			ans = hard(refused(reasonKind))
		default:
			ans = g.release(ctx, grant, tenant, res, request{via: o.Via, sandbox: o.Sandbox, variable: v}, now)
		}
		if !ans.Allowed {
			for _, done := range out {
				secrets.Zero(done)
			}
			return ans
		}
		out[v] = ans.Env[v]
	}
	g.log.Info("grant: sandbox opened", append(fields, zap.Int("vars", len(out)))...)
	return Answer{Allowed: true, Env: out}
}

// request is how one secret was asked for: the sandbox being opened and the
// variable it fills, for the trace.
type request struct {
	via, sandbox, variable string
}

// release decides one secret of a sandbox and, when it is allowed, hands it
// over as the one entry of the answer's env.
func (g *Gateway) release(ctx context.Context, grant authn.RunGrant, tenant string, res authn.Resource, rq request, now time.Time) Answer {
	fields := []zap.Field{
		zap.String("grant", grant.ID), zap.String("run", grant.Run),
		zap.String("principal", grant.Principal.ID), zap.String("via", rq.via),
		zap.String("sandbox", rq.sandbox), zap.String("var", rq.variable),
		zap.String("kind", string(res.Kind)), zap.String("name", res.Name), zap.String("tenant", tenant),
	}
	key := refusalKey(grant.ID, res)
	if reason, again := g.seen.get(key, now); again {
		g.log.Debug("grant: refused again, from memory", append(fields, zap.String("reason", reason))...)
		return refused(reason)
	}

	// The chassis's own reading of the request.
	meta, err := g.resolve(ctx, grant.TenantID, grant.Stack, res.Name)
	switch {
	case errors.Is(err, secrets.ErrSecretNotFound):
		meta = nil
	case err != nil:
		g.log.Warn("grant: reading the secret failed", append(fields, zap.Error(err))...)
		return unavailable("secret store")
	}
	standing, err := g.ids.Granted(ctx, grant.TenantID, grant.Principal, res, res.Verb())
	if err != nil {
		g.log.Warn("grant: reading the standing grant failed", append(fields, zap.Error(err))...)
		return unavailable("identity store")
	}
	c := checks{
		exists:    meta != nil,
		allowlist: grant.Allows(res),
		standing:  standing,
		pull:      meta != nil && meta.Pull.Admits(grant.NodeClass == authn.NodeReviewed),
		budget:    grant.Remaining() >= 1,
	}
	f := facts{
		rid: hxid.NewTimeSort().String(), tenant: tenant, grant: grant, res: res,
		rq: rq, meta: meta, checks: c, prop: propose(c), at: now,
	}
	fields = append(fields, zap.String("rid", f.rid), zap.Bool("proposed", f.prop.allow))

	// The tenant's rules. The run gets a context of its own, off the
	// gateway's: the caller's carries the pins of the run that asked — the
	// dispatching rule's tenant and stack, its budget — and the `_grant` run
	// is an inlet of its own, routed from `_sys` like every other. It ends
	// when the caller gives up, and at the deadline.
	rctx, cancel := context.WithTimeout(g.ctx, g.timeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, f.rid)
	stop := context.AfterFunc(ctx, cancel)
	final, runErr := g.runStack(rctx, requestPayload(f))
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	stop()
	cancel()
	if runErr != nil && !timedOut {
		g.log.Warn("grant: the _grant run failed", append(fields, zap.Error(runErr))...)
	}
	var v verdict
	if runErr == nil {
		v = parseVerdict(final)
	}
	d := decide(f.prop, v, runErr, timedOut)
	if d.allow && meta == nil {
		// A rule cannot release what is not there.
		d = decision{reason: reasonNotFound, byRule: d.byRule, note: d.note}
	}
	fields = append(fields, zap.Bool("by_rule", d.byRule))
	if d.note != "" {
		fields = append(fields, zap.String("note", d.note))
	}
	if !d.allow {
		g.seen.put(key, d.reason, now)
		g.log.Info("grant: refused", append(fields, zap.String("reason", d.reason))...)
		return refused(d.reason)
	}

	// Charge, then release: a charge that failed after the fact would
	// recall nothing. The budget is enforced unless a rule allowed a
	// request the chassis proposed to refuse for want of budget — the rule
	// said go ahead, and the spend is still recorded.
	enforce := !(d.byRule && !c.budget)
	switch err := g.ids.ChargeRunGrant(ctx, grant.TenantID, grant.ID, 1, enforce); {
	case errors.Is(err, authn.ErrRunBudget):
		g.seen.put(key, reasonBudget, now)
		g.log.Info("grant: refused", append(fields, zap.String("reason", reasonBudget))...)
		return refused(reasonBudget)
	case errors.Is(err, authn.ErrRunNotLive), errors.Is(err, authn.ErrNotFound):
		g.log.Info("grant: refused", append(fields, zap.String("reason", reasonNotLive))...)
		return refused(reasonNotLive)
	case err != nil:
		g.log.Warn("grant: charging the run grant failed", append(fields, zap.Error(err))...)
		return unavailable("identity store")
	}
	value, err := g.decrypt(ctx, meta)
	if err != nil {
		g.log.Warn("grant: charged, and then could not read the secret", append(fields, zap.Error(err))...)
		return unavailable("secret store")
	}
	g.log.Info("grant: released", append(fields, zap.Int("version", meta.VersionNo))...)
	return Answer{Allowed: true, Env: map[string][]byte{rq.variable: value}}
}
