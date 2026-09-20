package ipp

import (
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

const challenge = `Basic realm="ipp", charset="UTF-8"`

// clientIP is the address of the client that sent r: the socket peer, or —
// when that peer is one of --web-trusted-proxies — the client the proxies
// recorded in X-Forwarded-For (edgeproxy.Clients). It keys the per-IP auth
// limit, and is what the auth line and the job event report.
func (c *Controller) clientIP(r *http.Request) string { return c.clients.IP(r) }

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

// authenticate signs the request's Basic credential in through the chassis's
// login resolver (chassis/authn) — the one the IMAP and DAV heads use, so a
// guess here spends the same budget as a guess there. Order matters (it is
// the drive head's): TLS FIRST, before a credential is even read; then the
// resolver, which resolves the username to its principal, finds the
// credential by the id in the password, verifies it and checks that its
// scopes cover `ipp:<printer>:print`; then the GRANT — the printer belongs to
// one principal, and only that principal prints to it; admission last. It
// writes the HTTP answer itself on failure and returns ok=false.
//
// It reads HEADERS ONLY, and ServeHTTP calls it before the first byte of the
// body is read. That ordering is load-bearing, not tidiness — see ServeHTTP.
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request, site printerSite) (authn.Authenticated, bool) {
	ip := c.clientIP(r)
	if !secure(r) && !c.insecureAuth {
		http.Error(w, "TLS required", http.StatusForbidden)
		return authn.Authenticated{}, false
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		c.demand(w, r)
		return authn.Authenticated{}, false
	}
	deny := func(outcome string, who ...authn.Authenticated) (authn.Authenticated, bool) {
		c.noteAuth(outcome, site, user, ip, who...)
		w.Header().Set("WWW-Authenticate", challenge)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return authn.Authenticated{}, false
	}

	// The username is the full address its principal was bound under. A name
	// the resolver cannot read as one (a bare "print", the pre-principal
	// username) is answered like any unknown name, at the same cost.
	res := c.auth.Login(r.Context(), authn.Attempt{
		Tenant: site.tenant, Username: user, Password: pass, IP: ip,
		Want: authn.Scope{Domain: "ipp", Instance: site.printer.Label, Action: "print"},
	})
	switch res.Outcome {
	case authn.OutcomeOK:
	case authn.OutcomeThrottled:
		c.noteAuth("throttled", site, user, ip)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return authn.Authenticated{}, false
	case authn.OutcomeError:
		c.pu.Logger.Warn("ipp login failed", zap.String("tenant", site.tenant), zap.Error(res.Err))
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return authn.Authenticated{}, false
	default:
		return deny(string(res.Outcome))
	}

	// The grant. One principal per printer: a valid password for ANOTHER
	// principal is refused exactly like a wrong one, so it learns nothing
	// about whose printer this is. Letting others print here widens this
	// comparison (and the one in sendDocument), nothing else.
	if res.Who.Principal.ID != site.printer.PrincipalID {
		return deny("grant", res.Who)
	}

	if c.pu != nil && c.pu.Admission != nil {
		if d := c.pu.Admission.Decide(site.tenant); !d.Admit {
			c.noteAuth("denied", site, user, ip, res.Who)
			status := d.Status
			if status == 0 {
				status = http.StatusForbidden
			}
			if d.Retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.Retry.Seconds())))
			}
			http.Error(w, "service unavailable for this tenant", status)
			return authn.Authenticated{}, false
		}
	}
	if res.Cached {
		// A print client sends its password with every operation, so a hit is
		// the same client's next request, not a new login. It is counted; the
		// line is logged once per password check. Refusals log every time.
		c.countAuth("ok")
	} else {
		c.noteAuth("ok", site, user, ip, res.Who)
	}
	return res.Who, true
}
