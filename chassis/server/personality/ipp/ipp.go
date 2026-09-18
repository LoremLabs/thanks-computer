// Package ipp is the chassis's `ipp` personality: a stack presented to a
// user's computer as a PRINTER. File → Print → "Research Pony" in any
// application is one run of the tenant's `_ipp` stack, with the document
// delivered by CAS reference.
//
//	ipps://ipp.<zone>:443/p/<printer>
//	ipps://ipp.<structured suffix>:443/p/<handle>/<printer>   (a tenant with no zone)
//
// IPP is HTTP (POST application/ipp: a binary attribute block, then the
// document), so this head binds no listener of its own — the web head mounts
// Handler() for every request whose Host is `ipp.<something>`, and for no
// other. The tenant comes from the ZONE (`ipp.dripl.it` → whoever delegated
// dripl.it to this chassis); the printer is the path label and means
// whatever the tenant's `_ipp` stack says it means. The chassis implements
// the printer; the stack decides what printing is.
//
// What happens to one print job:
//
//	authenticate → decode the IPP header → stream the document into the CAS
//	(hash known only at EOF) → record tenant ownership → commit a durable job
//	row → answer the client → a dispatcher hands the envelope to the bus.
//
// The job's lifecycle ENDS when the bus accepts the envelope (`delivered`).
// The stack may then create a task, wait three days for a human, vectorize
// the document or throw it away; none of that is a print job, and nothing
// here waits for it or reports on it.
//
// v1 authentication is ONE static credential per tenant, held in the secret
// store (IPP_PASSWORD, optional IPP_USERNAME). A tenant without the secret
// has no printers. Printer-scoped accounts arrive with the credential
// subsystem that is being reworked separately.
package ipp

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

const (
	// PathPrefix is where printers live on an ipp host: /p/<printer>.
	PathPrefix = "/p"

	// SubscriptionStack is the tenant-level stack that receives print jobs —
	// the `_imap` / `_dns` idiom. A tenant without an active one has no
	// printers.
	SubscriptionStack = "_ipp"

	// Secret names of the v1 static credential. The password is required
	// (no secret ⇒ no printers); the username defaults to DefaultUsername.
	SecretPassword  = "IPP_PASSWORD"
	SecretUsername  = "IPP_USERNAME"
	DefaultUsername = "print"

	// loginCacheTTL is how long a verified (tenant, user, secret, password)
	// tuple skips the throttles — a print client re-authenticates on every
	// operation, so without it a busy queue would throttle itself.
	loginCacheTTL = 5 * time.Minute
	loginCacheMax = 10000

	// Streaming budget for one document: a floor plus a per-MiB allowance,
	// capped — the shape the drive and admin-blob uploads use to escape the
	// web listener's 15 s global read timeout for one request.
	bodyFloor  = 60 * time.Second
	bodyPerMiB = time.Second
	bodyCeil   = 2 * time.Hour

	lookupTimeout = 250 * time.Millisecond
)

// HostResolver maps a hostname to its tenant — the routing every web
// request uses (ingress.DBResolver.ResolveErr). Used for the hostname-row
// fallback when `ipp.<X>`'s X is not a zone origin. Tests inject.
type HostResolver interface {
	ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error)
}

// SecretSource reads one tenant secret. found=false means "not set" (the
// tenant has not enabled printing); err is a store/key failure.
type SecretSource func(ctx context.Context, tenantSlug, name string) (cleartext []byte, found bool, err error)

// Controller owns the head's shared state. It binds no listener — the web
// head mounts Handler() — but it does own one goroutine: the dispatcher.
type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	store    *chipp.Store
	fcas     filecas.Store
	ix       blob.Index
	resolver HostResolver
	secret   SecretSource

	cache      *apppass.LoginCache
	authIP     *throttle.Throttle
	authTenant *throttle.Throttle

	// sharedZone is the platform's structured-host suffix, bare
	// ("stacks.example"): `ipp.<sharedZone>` is the front door every tenant
	// without a zone of its own shares — see ippTarget.
	sharedZone string

	formats        []string
	maxBytes       int64
	maxInflight    int
	insecureAuth   bool
	anonAttrs      bool
	wireDebug      bool
	receiveTimeout time.Duration

	pollInterval    time.Duration
	dispatchTimeout time.Duration
	runTimeout      time.Duration
	leaseStale      time.Duration
	retention       time.Duration
	maxAttempts     int

	nodeID  string
	upSince time.Time
	nudge   chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// Seams the defaults in tenant.go fill; tests replace them.
	tenantFor  func(ctx context.Context, x string) (slug, ingressKey string, ok bool, err error)
	subscribed func(ctx context.Context, slug string) (bool, error)

	auths metric.Int64Counter
	jobs  metric.Int64Counter
	now   func() time.Time
}

// NewController constructs (but does not start) the head. A nil store (ipp
// personality off) yields an inert controller: Enabled() is false, Start is
// a no-op and the web head is never asked to mount it.
func NewController(ctx context.Context, pu *processor.Unit, store *chipp.Store, resolver HostResolver) *Controller {
	c := &Controller{
		ctx:      ctx,
		pu:       pu,
		store:    store,
		resolver: resolver,
		cache:    apppass.NewLoginCache(loginCacheTTL, loginCacheMax),
		formats:  []string{chipp.FormatPDF},
		nudge:    make(chan struct{}, 1),
		upSince:  time.Now().UTC(),
		now:      func() time.Time { return time.Now().UTC() },
	}
	c.tenantFor = c.lookupTenant
	c.subscribed = c.snapshotSubscribed
	if pu == nil {
		return c
	}
	conf := pu.Conf
	c.sharedZone = normalizeZone(conf.StructuredHostSuffix)
	if f := cleanFormats(conf.IPPFormats); len(f) > 0 {
		c.formats = f
	}
	c.maxBytes = int64(conf.IPPMaxJobBytes)
	c.maxInflight = conf.IPPMaxInflight
	c.insecureAuth = conf.IPPInsecureAuth
	c.anonAttrs = conf.IPPAnonymousAttributes
	c.wireDebug = conf.IPPWireDebug
	c.receiveTimeout = seconds(conf.IPPReceiveTimeout, 600)
	c.pollInterval = seconds(conf.IPPPollInterval, 1)
	c.dispatchTimeout = seconds(conf.IPPDispatchTimeout, 10)
	c.leaseStale = seconds(conf.IPPLeaseStaleAfter, 600)
	c.retention = seconds(conf.IPPRetention, 604800)
	c.maxAttempts = conf.IPPMaxAttempts
	c.authIP = throttle.New(conf.IPPAuthRate, time.Minute)
	c.authTenant = throttle.New(conf.IPPAuthRate, time.Minute)
	c.nodeID = resolveNodeID(conf.Fqdn)
	// The run a delivered job starts gets the chassis's ordinary sync-op
	// ceiling as its context budget — NOT the dispatch timeout. The
	// dispatcher is done with a job the moment the bus takes it.
	c.runTimeout = 10 * time.Minute
	if d, err := time.ParseDuration(conf.OpTimeoutMax); err == nil && d > 0 {
		c.runTimeout = d
	}
	if pu.Mc != nil && pu.Mc.Meter != nil {
		c.auths, _ = pu.Mc.Meter.Int64Counter("chassis.ipp.auth",
			metric.WithDescription("IPP Basic-auth attempts by outcome"), metric.WithUnit("1"))
		c.jobs, _ = pu.Mc.Meter.Int64Counter("chassis.ipp.jobs",
			metric.WithDescription("IPP print jobs by outcome"), metric.WithUnit("1"))
	}
	return c
}

// SetFileCAS hands the head the content-addressed store documents stream into.
func (c *Controller) SetFileCAS(s filecas.Store) { c.fcas = s }

// SetBlobIndex hands the head the blob index that records which tenant owns
// a document's hash (so the stack can read it by sha, and nobody else can).
func (c *Controller) SetBlobIndex(ix blob.Index) { c.ix = ix }

// SetSecretSource hands the head the tenant secret reader.
func (c *Controller) SetSecretSource(s SecretSource) { c.secret = s }

// Enabled reports whether the head serves anything: the personality is on,
// the job store opened, and there is somewhere to put documents.
func (c *Controller) Enabled() bool {
	return c != nil && c.store != nil && c.pu != nil && c.pu.Conf.HasPersonality("ipp")
}

// IsIPPHost is the web head's mount predicate: the leftmost label is `ipp`.
// Marker only — no lookup — because it runs on EVERY web request. Whether
// the rest names a tenant is the handler's question.
func IsIPPHost(host string) bool {
	_, ok := stripIPPLabel(host)
	return ok
}

// Handler is the http.Handler the web head mounts for ipp hosts.
func (c *Controller) Handler() http.Handler { return c }

// Start launches the dispatcher. The head itself is served by the web head,
// so an `ipp` personality without `web` receives nothing — say so.
func (c *Controller) Start() {
	if !c.Enabled() {
		return
	}
	if !c.pu.Conf.HasPersonality("web") {
		c.pu.Logger.Warn("ipp personality is on but the web head is off: IPP is served through the web head, so nothing is listening")
	}
	if c.fcas == nil || c.ix == nil {
		c.pu.Logger.Warn("ipp personality has no file CAS / blob index: print jobs will be refused")
	}
	if c.secret == nil {
		c.pu.Logger.Warn("ipp personality has no secret store: no tenant can enable a printer")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.dispatchLoop(ctx)
	}()
	c.pu.Logger.Info("ipp controller started",
		zap.Strings("formats", c.formats), zap.Int64("max_job_bytes", c.maxBytes),
		zap.Bool("insecure_auth", c.insecureAuth), zap.Bool("anonymous_attributes", c.anonAttrs),
		zap.String("shared_front_door", sharedDoor(c.sharedZone)),
		zap.String("node", c.nodeID))
}

// Stop ends the dispatcher. Committed jobs it had not handed over stay
// committed; the next node (or the next boot) delivers them.
func (c *Controller) Stop() {
	if c == nil || c.cancel == nil {
		return
	}
	c.cancel()
	c.wg.Wait()
}

func (c *Controller) noteAuth(outcome, tenant, ip string) {
	if c.auths != nil {
		c.auths.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.ipp.outcome", outcome)))
	}
	if c.pu != nil && c.pu.Logger != nil {
		c.pu.Logger.Info("ipp auth", zap.String("outcome", outcome), zap.String("tenant", tenant), zap.String("ip", ip))
	}
}

func (c *Controller) noteJob(outcome string) {
	if c.jobs != nil {
		c.jobs.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.ipp.outcome", outcome)))
	}
}

// sharedDoor names the shared front door for the startup log ("" = none).
func sharedDoor(zone string) string {
	if zone == "" {
		return ""
	}
	return "ipp." + zone + PathPrefix + "/<handle>/<printer>"
}

func cleanFormats(in []string) []string {
	var out []string
	for _, e := range in {
		for _, f := range strings.Split(e, ",") { // env lists arrive as one element
			f = strings.ToLower(strings.TrimSpace(f))
			// PostScript is a programming language, not a page format: it is
			// not acceptable however the operator configures the list.
			if f == "" || f == "application/postscript" {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

func seconds(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

// resolveNodeID mirrors the scheduled/cron heads: the operator-set FQDN,
// else the OS hostname (distinct per container in a fleet), else "local".
func resolveNodeID(fqdn string) string {
	if fqdn != "" {
		return fqdn
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// bodyBudget is the streaming deadline for a document of size bytes.
func bodyBudget(size int64) time.Duration {
	if size < 0 {
		size = 0
	}
	b := bodyFloor + time.Duration(size>>20)*bodyPerMiB
	if b > bodyCeil {
		b = bodyCeil
	}
	return b
}
