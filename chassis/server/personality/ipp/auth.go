package ipp

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
)

const challenge = `Basic realm="ipp", charset="UTF-8"`

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// secure reports whether the request reached us over TLS — the chassis's
// own listener, or a front proxy that terminated it.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// demand answers a request that offered no credential: 403 over plaintext
// (never invite a password onto an unencrypted connection), else the Basic
// challenge.
func (c *Controller) demand(w http.ResponseWriter, r *http.Request) {
	if !secure(r) && !c.insecureAuth {
		http.Error(w, "TLS required", http.StatusForbidden)
		return
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// authenticate checks the request's Basic credential against the tenant's
// static one. Order matters (it is the drive head's): TLS FIRST, before a
// credential is even read; then the verified cache; the throttles on a miss
// only; the constant-time compare; admission last. It writes the HTTP
// answer itself on failure and returns false.
//
// It reads HEADERS ONLY, and ServeHTTP calls it before the first byte of the
// body is read. That ordering is load-bearing, not tidiness — see ServeHTTP.
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request, site printerSite) bool {
	ip := clientIP(r)
	if !secure(r) && !c.insecureAuth {
		http.Error(w, "TLS required", http.StatusForbidden)
		return false
	}
	header := r.Header.Get("Authorization")
	user, pass, ok := r.BasicAuth()
	if !ok || header == "" {
		c.demand(w, r)
		return false
	}

	// The cache key binds the PRESENTED credential to the CURRENT secret (by
	// digest), so rotating IPP_PASSWORD invalidates every cached login by
	// construction — no explicit flush.
	digest := sha256.Sum256(append([]byte(site.username+"\x00"), site.password...))
	key := apppass.LoginKey(site.tenant+"\x00"+user, hex.EncodeToString(digest[:]), pass)
	if !c.cache.Hit(key) {
		// A miss is either a first login or a guess; both pay the throttle.
		// Counting only misses is what lets a print client re-authenticate
		// on every operation for free while a guesser — who always misses —
		// is capped, correct guess included.
		if c.throttled(ip, site.tenant) {
			c.noteAuth("throttled", site.tenant, ip)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
			return false
		}
		if !apppass.BasicHeaderMatches(header, site.username, site.password) {
			c.noteAuth("failed", site.tenant, ip)
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return false
		}
		c.cache.Put(key)
	}

	if c.pu != nil && c.pu.Admission != nil {
		if d := c.pu.Admission.Decide(site.tenant); !d.Admit {
			c.noteAuth("denied", site.tenant, ip)
			status := d.Status
			if status == 0 {
				status = http.StatusForbidden
			}
			if d.Retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.Retry.Seconds())))
			}
			http.Error(w, "service unavailable for this tenant", status)
			return false
		}
	}
	c.noteAuth("ok", site.tenant, ip)
	return true
}

func (c *Controller) throttled(ip, tenant string) bool {
	if c.authIP != nil && ip != "" {
		if ok, _ := c.authIP.Allow(ip); !ok {
			return true
		}
	}
	if c.authTenant != nil {
		if ok, _ := c.authTenant.Allow(tenant); !ok {
			return true
		}
	}
	return false
}
