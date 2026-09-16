// Package webdav is the chassis's `webdav` personality: a WebDAV server
// over the drive store (chassis/drive), mounted on the web head under a
// reserved path prefix (--drive-path-prefix, default /drive) on every
// hostname the head serves. It is a stateful personality: every request is
// answered from the drive index and object store and never runs a stack —
// mutations reach stacks as `_scheduled` events the store emits (see
// docs/advanced/drive.md).
//
// The mount URL is https://<host>/drive/. A Basic-auth account (minted by
// txco://drive/account) is bound to ONE collection, and that collection is
// the root the client sees — there is no username segment in the URL, no
// discovery and no principal: WebDAV has none of that, and a login that
// resolves to exactly one tree needs none.
//
// The head owns what the library (go-webdav v0.7.0) does not: Basic auth
// over the account table, OPTIONS (class 2 must be advertised or macOS
// Finder mounts read-only), LOCK/UNLOCK (a stateless shim — see lock.go),
// streaming PUT with a size-scaled read deadline, and the prefix the
// library's hrefs must carry.
package webdav

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

const (
	// loginCacheTTL is how long a verified (username, hash, password)
	// triple skips argon2id — a WebDAV client re-authenticates every
	// request, so this is what makes Basic auth affordable.
	loginCacheTTL = 5 * time.Minute
	loginCacheMax = 10000

	// Streaming budget for one PUT or GET body: a floor plus a per-MiB
	// allowance, capped — the same shape the admin blob upload uses to
	// escape the web listener's 15 s global timeouts for one request.
	bodyFloor  = 60 * time.Second
	bodyPerMiB = time.Second
	bodyCeil   = 2 * time.Hour

	// lockTimeout is the lifetime LOCK advertises. Nothing enforces it —
	// see lock.go.
	lockTimeout = time.Hour
)

// HostResolver maps a request's Host to its tenant — the same routing
// every web request uses (ingress.DBResolver.ResolveErr). Tests inject.
type HostResolver interface {
	ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error)
}

// Controller owns the head's shared state: the store, the verified-login
// cache and the throttles. It binds no listener of its own — the web head
// mounts Handler().
type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	store    *chdrive.Store
	resolver HostResolver

	cache     *apppass.LoginCache
	loginIP   *throttle.Throttle
	loginAcct *throttle.Throttle

	prefix       string
	insecureAuth bool
	maxBytes     int64

	logins metric.Int64Counter
	now    func() time.Time
}

// NewController constructs (but does not start) the head. A nil store
// (webdav personality off) yields an inert controller whose Handler
// answers 404 and whose Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, store *chdrive.Store, resolver HostResolver) *Controller {
	c := &Controller{
		ctx:      ctx,
		pu:       pu,
		store:    store,
		resolver: resolver,
		cache:    apppass.NewLoginCache(loginCacheTTL, loginCacheMax),
		prefix:   "/drive",
		now:      func() time.Time { return time.Now().UTC() },
	}
	if pu != nil {
		c.prefix = cleanPrefix(pu.Conf.DrivePathPrefix)
		c.insecureAuth = pu.Conf.DriveInsecureAuth
		c.maxBytes = int64(pu.Conf.DriveMaxFileBytes)
		c.loginIP = throttle.New(pu.Conf.DriveLoginRate, time.Minute)
		c.loginAcct = throttle.New(pu.Conf.DriveLoginRate, time.Minute)
		if pu.Mc != nil && pu.Mc.Meter != nil {
			c.logins, _ = pu.Mc.Meter.Int64Counter("chassis.webdav.logins",
				metric.WithDescription("WebDAV Basic-auth attempts by outcome"),
				metric.WithUnit("1"))
		}
	}
	return c
}

// Prefix is the reserved path prefix (no trailing slash).
func (c *Controller) Prefix() string { return c.prefix }

// Enabled reports whether the head serves anything: the personality is
// on and the store opened.
func (c *Controller) Enabled() bool {
	return c != nil && c.store != nil && c.pu != nil && c.pu.Conf.HasPersonality("webdav")
}

// Start logs the mount. The head itself is served by the web head, so a
// `webdav` personality without `web` serves nothing — say so.
func (c *Controller) Start() {
	if !c.Enabled() {
		return
	}
	if !c.pu.Conf.HasPersonality("web") {
		c.pu.Logger.Warn("webdav personality is on but the web head is off: WebDAV is served through the web head, so nothing is listening")
	}
	c.pu.Logger.Info("webdav controller started", zap.String("prefix", c.prefix), zap.Bool("insecure_auth", c.insecureAuth),
		zap.Int64("max_file_bytes", c.maxBytes))
}

// Stop is a no-op: the head holds no goroutines of its own.
func (c *Controller) Stop() {}

// Handler is the http.Handler the web head mounts on Prefix()/.
func (c *Controller) Handler() http.Handler { return c }

func (c *Controller) noteLogin(outcome, username, ip string) {
	if c.logins != nil {
		c.logins.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.webdav.outcome", outcome)))
	}
	if c.pu != nil && c.pu.Logger != nil {
		c.pu.Logger.Info("webdav login", zap.String("outcome", outcome), zap.String("user", username), zap.String("ip", ip))
	}
}

func cleanPrefix(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" {
		p = "/drive"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// bodyBudget is the streaming deadline for a body of size bytes.
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
