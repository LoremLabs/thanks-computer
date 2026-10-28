// Package git is the chassis's `git` personality: a git smart-HTTP server
// (clone, fetch, push) over the chassis's own repository store, mounted on
// the web head under a reserved path prefix (--git-path-prefix, default
// /git) on every hostname the head serves — https://<host>/git/<repo>.git
// is what a developer clones. A stock git client speaks to it; nothing is
// installed on the machine, no repository is materialised on disk.
//
// The head knows repositories, refs and pushes, and nothing about what a
// repository holds. What a push MEANS is a tenant stack's: before a ref
// moves the head asks the tenant's `_git` stack (the answer lane, default
// deny) and after the reply it tells it (the observe lane) — the contacts
// head's two lanes [G1]. Accepting a push and acting on it are two
// different refs moved by two different parties (docs/0059 §7).
//
// The wire protocol is git's v0 "smart" protocol over HTTP, the one every
// client speaks (v2 is not advertised, so clients fall back). go-git
// supplies pkt-lines, the packp messages, packfile parsing and encoding,
// the object model and the reachability walk; the framing, the per-ref
// compare-and-swap, the hook point between "objects stored" and "refs
// moved", and the limits are this package's — go-git's own server
// (plumbing/transport/server) updates refs with a plain SetReference and
// carries a TODO for atomic, so it could not be the server.
//
// Limits are enforced at the door, not by the application later: a pack
// (--git-max-pack-bytes), an object (--git-max-object-bytes), a count
// (--git-max-objects), a repository (--git-max-repo-bytes), and, by
// default, text only (--git-text-only): a blob that is not valid UTF-8, a
// symlink, an executable or a submodule is refused with a one-line reason
// the client prints.
package git

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/edgeproxy"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

const (
	// Streaming budget for one request or response body: a floor plus a
	// per-MiB allowance, capped — the WebDAV head's shape, which escapes the
	// web listener's 15 s global timeouts for one request.
	bodyFloor  = 60 * time.Second
	bodyPerMiB = time.Second
	bodyCeil   = 2 * time.Hour

	uploadPackService  = "git-upload-pack"
	receivePackService = "git-receive-pack"

	// agentName is what the head says in the `agent` capability.
	agentName = "txco"
)

// HostResolver maps a request's Host to its tenant — the same routing
// every web request uses (ingress.DBResolver.ResolveErr). Tests inject.
type HostResolver interface {
	ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error)
}

// LimitsFromConfig reads the --git-* caps.
func LimitsFromConfig(conf config.Config) Limits {
	return Limits{
		MaxPackBytes:   int64(conf.GitMaxPackBytes),
		MaxObjectBytes: int64(conf.GitMaxObjectBytes),
		MaxObjects:     conf.GitMaxObjects,
		MaxRepoBytes:   int64(conf.GitMaxRepoBytes),
		TextOnly:       conf.GitTextOnly,
	}
}

// Controller owns the head's shared state: the repository loader, the
// login resolver and the limits. It binds no listener of its own — the web
// head mounts Handler().
type Controller struct {
	// clients resolves a request's client address behind the operator's
	// HTTP proxies (--web-trusted-proxies).
	clients *edgeproxy.Clients

	ctx      context.Context
	pu       *processor.Unit
	repos    RepoLoader
	resolver HostResolver

	// auth verifies passwords: the resolver the heads share, with its
	// login cache and throttles (--login-rate). nil ⇒ every request is
	// answered "temporary failure" — unless openWithoutAuth, a test
	// harness's switch for driving the protocol without a door.
	auth            *authn.Resolver
	openWithoutAuth bool

	prefix       string
	insecureAuth bool
	limits       Limits

	// lanes dispatch a push into the tenant's `_git` stack: the answer lane
	// before the refs move, the observe lane after the reply.
	lanes *lanes

	// beforeRefs, when set, stands in for the answer lane: it runs on a
	// push after the objects are stored and the response headers are
	// flushed, and before the refs move. A non-nil error refuses every ref
	// with its text. Tests use it to prove the early flush.
	beforeRefs func(ctx context.Context, p *Push) error

	logins metric.Int64Counter
	now    func() time.Time
}

// NewController constructs (but does not start) the head. A nil loader
// (git personality off) yields an inert controller whose Handler answers
// 404 and whose Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, repos RepoLoader, resolver HostResolver) *Controller {
	c := &Controller{
		ctx:      ctx,
		pu:       pu,
		repos:    repos,
		resolver: resolver,
		prefix:   "/git",
		now:      func() time.Time { return time.Now().UTC() },
	}
	if pu != nil {
		c.clients, _ = edgeproxy.NewClients(pu.Conf.WebTrustedProxies)
		c.prefix = cleanPrefix(pu.Conf.GitPathPrefix)
		c.insecureAuth = pu.Conf.GitInsecureAuth
		c.limits = LimitsFromConfig(pu.Conf)
		if pu.Mc != nil && pu.Mc.Meter != nil {
			c.logins, _ = pu.Mc.Meter.Int64Counter("chassis.git.logins",
				metric.WithDescription("git Basic-auth attempts by outcome"),
				metric.WithUnit("1"))
		}
		c.lanes = newLanes(ctx, pu)
	}
	return c
}

// SetAuth hands the head the login resolver (chassis/authn).
func (c *Controller) SetAuth(r *authn.Resolver) { c.auth = r }

// Prefix is the reserved path prefix (no trailing slash).
func (c *Controller) Prefix() string { return c.prefix }

// Limits reports the caps in force.
func (c *Controller) Limits() Limits { return c.limits }

// Enabled reports whether the head serves anything: the personality is on
// and a repository loader was given.
func (c *Controller) Enabled() bool {
	return c != nil && c.repos != nil && c.pu != nil && c.pu.Conf.HasPersonality("git")
}

// Start logs the mount. The head itself is served by the web head, so a
// `git` personality without `web` serves nothing — say so.
func (c *Controller) Start() {
	if !c.Enabled() {
		return
	}
	if !c.pu.Conf.HasPersonality("web") {
		c.pu.Logger.Warn("git personality is on but the web head is off: git is served through the web head, so nothing is listening")
	}
	c.lanes.start()
	c.pu.Logger.Info("git controller started", zap.String("prefix", c.prefix), zap.Bool("insecure_auth", c.insecureAuth),
		zap.Int64("max_pack_bytes", c.limits.MaxPackBytes), zap.Int64("max_object_bytes", c.limits.MaxObjectBytes),
		zap.Int("max_objects", c.limits.MaxObjects), zap.Int64("max_repo_bytes", c.limits.MaxRepoBytes),
		zap.Bool("text_only", c.limits.TextOnly), zap.Bool("auth", c.auth != nil))
}

// Stop drains the observe lane's workers.
func (c *Controller) Stop() {
	if c == nil {
		return
	}
	c.lanes.stop()
}

// Handler is the http.Handler the web head mounts on Prefix()/.
func (c *Controller) Handler() http.Handler { return c }

// noteLogin counts a login outcome and logs one line for it.
func (c *Controller) noteLogin(outcome, username, ip string, who ...authn.Authenticated) {
	if c.logins != nil {
		c.logins.Add(context.Background(), 1, metric.WithAttributes(attribute.String("txco.git.outcome", outcome)))
	}
	if c.pu != nil && c.pu.Logger != nil {
		fields := []zap.Field{zap.String("outcome", outcome), zap.String("user", username), zap.String("ip", ip)}
		if len(who) > 0 && !who[0].IsZero() {
			fields = append(fields, zap.String("principal", who[0].Principal.ID), zap.String("credential", who[0].Credential))
		}
		c.pu.Logger.Info("git login", fields...)
	}
}

func (c *Controller) logger() *zap.Logger {
	if c != nil && c.pu != nil && c.pu.Logger != nil {
		return c.pu.Logger
	}
	return zap.NewNop()
}

func cleanPrefix(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" {
		p = "/git"
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
