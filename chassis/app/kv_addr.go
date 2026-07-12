package app

import (
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/kv/redisstore"
)

// redisConfigFromAddr turns a KV address into a redisstore Config plus the
// bare host:port to hand to valkeyrie.
//
// A bare host:port is returned unchanged (today's behaviour): no TLS, password
// from the fallback arg (TXCO_KVSTORE_PASSWORD). A redis:// or rediss:// URL is
// parsed for host/port, userinfo, /db, and — for the rediss scheme — TLS. This
// is what lets TXCO_KVSTORE_ADDRS point at a managed Redis (e.g. Upstash) that
// mandates TLS: rediss://default:<token>@<db>.upstash.io:6379.
//
// The scheme, not a separate flag, selects TLS — mirroring how the auth/runtime
// DSN seam carries security inside postgres://…?sslmode=… (see
// auth/registry.DialectForDSN + config.RedactDSN).
func redisConfigFromAddr(addr, fallbackPassword string) (*redisstore.Config, string, error) {
	rc := &redisstore.Config{Password: fallbackPassword}
	if !strings.HasPrefix(addr, "redis://") && !strings.HasPrefix(addr, "rediss://") {
		return rc, addr, nil // bare host:port — unchanged
	}
	opt, err := goredis.ParseURL(addr)
	if err != nil {
		// A malformed rediss://user:<token>@… must never reach the fatal log.
		// RedactDSN scrubs the shown URL; the underlying ParseURL error is a
		// *url.Error that embeds the raw URL verbatim, so scrub that too (and
		// use %s, not %w, so the raw text can't ride along the error chain).
		redacted := config.RedactDSN(addr)
		reason := strings.ReplaceAll(err.Error(), addr, redacted)
		return nil, "", fmt.Errorf("invalid redis url %q: %s", redacted, reason)
	}
	rc.TLS = opt.TLSConfig // non-nil for rediss:// (ServerName set from host)
	rc.Username = opt.Username
	if opt.Password != "" {
		rc.Password = opt.Password // URL userinfo wins over TXCO_KVSTORE_PASSWORD
	}
	rc.DB = opt.DB
	return rc, opt.Addr, nil
}
