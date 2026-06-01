package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// Options configures the bundled cert manager.
type Options struct {
	// Publisher is where DNS-01 challenges are written — the dns head's
	// ChallengeStore. REQUIRED for issuance to work.
	Publisher ChallengePublisher

	// Email is the ACME account contact (recommended by CAs).
	Email string

	// CA is the ACME directory URL. Empty ⇒ certmagic's default
	// (Let's Encrypt production). Point at LE staging or a local
	// Pebble/step-ca directory for testing.
	CA string

	// CARootFile is a PEM bundle to trust as the ACME CA's root, for a CA
	// whose root isn't in the system pool (Pebble/step-ca). Empty ⇒ system
	// roots — the right default for Let's Encrypt.
	CARootFile string

	// StorageDSN selects the cert/account store; empty ⇒ file at StoragePath.
	// A recognised scheme uses an overlay-registered backend (see storage.go).
	StorageDSN  string
	StoragePath string

	// Resolvers are the DNS servers certmagic uses for zone discovery and
	// DNS-01 propagation checks. Empty ⇒ query the zone's authoritative
	// servers directly (correct in production, where our head is the
	// authority). Point at our own head (e.g. 127.0.0.1:5354) for an
	// offline/localhost solve.
	Resolvers []string

	// PropagationDelay waits before the first propagation check. For a
	// single-node in-process solve the record is visible immediately, so 0
	// is fine; a small delay helps a shared store settle.
	PropagationDelay time.Duration

	Logger *zap.Logger
}

// Manager owns a certmagic Config wired to issue/renew certs for delegated
// zones via in-process DNS-01. Build it once; call Manage as the set of
// delegated zones changes, and hand TLSConfig to the HTTPS listener.
type Manager struct {
	cfg    *certmagic.Config
	logger *zap.Logger
}

// NewManager builds the cert manager. It does not contact the CA or issue
// anything until Manage is called.
func NewManager(opts Options) (*Manager, error) {
	if opts.Publisher == nil {
		return nil, fmt.Errorf("tls: NewManager requires a ChallengePublisher")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	storage, err := storageForDSN(opts.StorageDSN, opts.StoragePath)
	if err != nil {
		return nil, fmt.Errorf("tls: cert storage: %w", err)
	}

	var roots *x509.CertPool
	if opts.CARootFile != "" {
		pem, rerr := os.ReadFile(opts.CARootFile)
		if rerr != nil {
			return nil, fmt.Errorf("tls: read acme ca root %q: %w", opts.CARootFile, rerr)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls: no PEM certificates in acme ca root %q", opts.CARootFile)
		}
	}

	solver := &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider:      challengeProvider{pub: opts.Publisher},
			Resolvers:        opts.Resolvers,
			PropagationDelay: opts.PropagationDelay,
			Logger:           logger,
		},
	}

	// certmagic's cache needs a config getter; the config references the
	// cache. Capture cfg by reference to break the cycle (the standard
	// certmagic wiring).
	var cfg *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return cfg, nil
		},
	})
	cfg = certmagic.New(cache, certmagic.Config{
		Storage: storage,
		Logger:  logger,
	})
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:           opts.CA, // "" ⇒ certmagic default (LE prod)
		Email:        opts.Email,
		Agreed:       true,
		DNS01Solver:  solver,
		TrustedRoots: roots,
		Logger:       logger,
	})
	cfg.Issuers = []certmagic.Issuer{issuer}

	return &Manager{cfg: cfg, logger: logger}, nil
}

// Manage (re)declares the exact set of domains to keep certificates for,
// obtaining missing ones in the background and renewing as needed. Safe to
// call repeatedly as delegated zones are added/removed.
func (m *Manager) Manage(ctx context.Context, domains []string) error {
	if len(domains) == 0 {
		return nil
	}
	return m.cfg.ManageAsync(ctx, domains)
}

// De-provisioning a revoked zone's cert (certmagic Cache.RemoveManaged) is
// deferred: a revoked zone stops resolving regardless, and a lingering
// managed cert is harmless. Revisit if zone churn pressures CA rate limits.

// TLSConfig returns a *tls.Config whose GetCertificate serves the managed
// certificates by SNI. The HTTPS listener on the web head uses this.
func (m *Manager) TLSConfig() *tls.Config {
	t := m.cfg.TLSConfig()
	// Ensure HTTP/2 + HTTP/1.1 are offered alongside certmagic's ACME-TLS
	// entry (certmagic prepends acme-tls/1 for the TLS-ALPN solver, which we
	// don't use but is harmless).
	t.NextProtos = appendUnique(t.NextProtos, "h2", "http/1.1")
	return t
}

// WildcardDomains expands canonical origins (e.g. "ops.example.com") into
// the cert subject set we manage per delegated zone: the apex plus a
// wildcard covering every per-stack host. One cert per zone instead of one
// per host.
func WildcardDomains(origins []string) []string {
	out := make([]string, 0, len(origins)*2)
	for _, o := range origins {
		o = trimDot(o)
		if o == "" {
			continue
		}
		out = append(out, o, "*."+o)
	}
	return out
}

func trimDot(s string) string {
	for len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func appendUnique(list []string, vals ...string) []string {
	seen := make(map[string]struct{}, len(list))
	for _, v := range list {
		seen[v] = struct{}{}
	}
	for _, v := range vals {
		if _, ok := seen[v]; !ok {
			list = append(list, v)
			seen[v] = struct{}{}
		}
	}
	return list
}
