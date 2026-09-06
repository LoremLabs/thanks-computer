// Package contacts is the chassis's `contacts` personality: a CardDAV
// server over the durable contacts store (chassis/contacts), mounted on the
// web head under a reserved path prefix (--contacts-path-prefix, default
// /carddav) on every hostname the head serves. It is a stateful
// personality: reads — PROPFIND, REPORT, GET — are answered from the store
// and never run a stack; a client mutation (PUT, DELETE, MKCOL, PROPPATCH)
// is governed by the address book's policy and may become one bounded
// envelope into the account tenant's `_contacts` stack
// (chassis/contacts.PolicyMode).
//
// Materialize, then serve: stacks write cards with txco://contacts/*; the
// head serves them to Apple Contacts, Thunderbird and DAVx⁵. The server a
// client uses is the stack's own hostname — paris@<host> connects to
// https://<host>/carddav/. A client's bytes are stored and served as
// written (the head answers PUT, GET and multiget itself); the library
// serves the rest.
package contacts

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
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

const (
	// loginCacheTTL is how long a verified (username, hash, password)
	// triple skips argon2id — a CardDAV client re-authenticates every
	// request, so this is what makes Basic auth affordable.
	loginCacheTTL = 5 * time.Minute
	loginCacheMax = 10000
)

// HostResolver maps a request's Host to its tenant — the same routing
// every web request uses (ingress.DBResolver.ResolveErr). Tests inject.
type HostResolver interface {
	ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error)
}

// Controller owns the head's shared state: the store, the verified-login
// cache, the throttles, the lanes. It binds no listener of its own — the
// web head mounts Handler().
type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	store    *chcon.Store
	resolver HostResolver

	cache     *apppass.LoginCache
	loginIP   *throttle.Throttle
	loginAcct *throttle.Throttle
	lanes     *lanes

	prefix       string
	insecureAuth bool
	maxBytes     int64

	logins metric.Int64Counter
	now    func() time.Time
}

// NewController constructs (but does not start) the head. A nil store
// (contacts personality off) yields an inert controller whose Handler
// answers 404 and whose Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, store *chcon.Store, resolver HostResolver) *Controller {
	c := &Controller{
		ctx:      ctx,
		pu:       pu,
		store:    store,
		resolver: resolver,
		cache:    apppass.NewLoginCache(loginCacheTTL, loginCacheMax),
		prefix:   "/carddav",
		maxBytes: 1 << 20,
		now:      func() time.Time { return time.Now().UTC() },
	}
	if pu != nil {
		c.prefix = cleanPrefix(pu.Conf.ContactsPathPrefix)
		c.insecureAuth = pu.Conf.ContactsInsecureAuth
		c.maxBytes = int64(pu.Conf.ContactsObjectMaxBytes)
		c.loginIP = throttle.New(pu.Conf.ContactsLoginRate, time.Minute)
		c.loginAcct = throttle.New(pu.Conf.ContactsLoginRate, time.Minute)
		c.lanes = newLanes(ctx, pu)
		if pu.Mc != nil && pu.Mc.Meter != nil {
			c.logins, _ = pu.Mc.Meter.Int64Counter("chassis.contacts.logins",
				metric.WithDescription("CardDAV Basic-auth attempts by outcome"),
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
	return c != nil && c.store != nil && c.pu != nil && c.pu.Conf.HasPersonality("contacts")
}

// Start starts the lanes. The head itself is served by the web head, so
// a `contacts` personality without `web` serves nothing — say so.
func (c *Controller) Start() {
	if !c.Enabled() {
		return
	}
	if !c.pu.Conf.HasPersonality("web") {
		c.pu.Logger.Warn("contacts personality is on but the web head is off: CardDAV is served through the web head, so nothing is listening")
	}
	c.lanes.start()
	c.pu.Logger.Info("contacts controller started", zap.String("prefix", c.prefix), zap.Bool("insecure_auth", c.insecureAuth))
}

// Stop stops the lanes.
func (c *Controller) Stop() {
	if c == nil || c.lanes == nil {
		return
	}
	c.lanes.stop()
}

// Handler is the http.Handler the web head mounts on Prefix()/ and
// /.well-known/carddav.
func (c *Controller) Handler() http.Handler { return c }

func (c *Controller) noteLogin(outcome, username, ip string) {
	if c.logins != nil {
		c.logins.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.contacts.outcome", outcome)))
	}
	if c.pu != nil && c.pu.Logger != nil {
		c.pu.Logger.Info("contacts login", zap.String("outcome", outcome), zap.String("user", username), zap.String("ip", ip))
	}
}

func cleanPrefix(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" {
		p = "/carddav"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
