package tenants

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Verifier runs hostname-ownership checks for the admin /verify
// endpoint. Two methods are supported:
//
//   - DNS TXT: look up `_txco-verify.<hostname>` and match any TXT
//     value against the challenge token. Works before the hostname's
//     A/CNAME points at the chassis.
//   - HTTP-01: build a per-attempt http.Client with a pinned-IP
//     dialer and no redirect-following, then fetch
//     `http://<hostname><port>/.well-known/txco-verify/<token>` and
//     compare the body to the token.
//
// Both methods accept a context and apply their own timeouts on top.
// All errors returned from this package are safe to surface verbatim
// to the operator (they describe what was attempted, not what the
// chassis saw internally).
type Verifier struct {
	// AllowPrivateAddresses bypasses the SSRF blocklist when running
	// HTTP-01 against a hostname that resolves to a private,
	// loopback, link-local, or otherwise non-public address. Default
	// false (production-safe); `txco dev` flips it true in
	// devDefaults so localhost workflows function.
	AllowPrivateAddresses bool
}

// ErrAddressNotPublic signals an SSRF defense rejection: the
// hostname resolves to one or more non-public IPs and
// AllowPrivateAddresses is false.
var ErrAddressNotPublic = errors.New("address_not_public")

// ErrUnexpectedRedirect signals that the HTTP-01 target returned a
// 3xx. We never follow — a malicious server could 302 us into an
// internal URL, defeating the pinned dialer.
var ErrUnexpectedRedirect = errors.New("unexpected_redirect")

// ErrTokenMismatch signals the response body (or TXT record) didn't
// match the expected token.
var ErrTokenMismatch = errors.New("token_mismatch")

// VerifyDNS resolves `_txco-verify.<hostname>` TXT records and
// matches any value (after TrimSpace) against either
// "txco-verify=<token>" or the bare "<token>". Uses
// `net.Resolver{PreferGo: true}` so /etc/hosts and cgo-only
// resolvers don't accidentally satisfy verification in dev.
func (v *Verifier) VerifyDNS(ctx context.Context, hostname, token string) error {
	if hostname == "" || token == "" {
		return errors.New("verify: empty hostname or token")
	}
	resolver := &net.Resolver{PreferGo: true}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	records, err := resolver.LookupTXT(lookupCtx, "_txco-verify."+hostname)
	if err != nil {
		return fmt.Errorf("lookup_txt: %w", err)
	}
	wantPrefixed := "txco-verify=" + token
	for _, r := range records {
		got := strings.TrimSpace(r)
		if got == wantPrefixed || got == token {
			return nil
		}
	}
	return ErrTokenMismatch
}

// VerifyHTTP fetches `http://<hostname><portSuffix>/.well-known/
// txco-verify/<token>` with SSRF defenses (pinned-IP dialer +
// redirect refusal) and confirms the response body equals the
// token. `portSuffix` is "" for default port 80, ":<n>" otherwise —
// production behind an LB on :80 uses "", dev with the chassis on
// :8080 uses ":8080".
func (v *Verifier) VerifyHTTP(ctx context.Context, hostname, portSuffix, token string) error {
	if hostname == "" || token == "" {
		return errors.New("verify: empty hostname or token")
	}
	// Resolve and check.
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, hostname)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	if len(addrs) == 0 {
		return errors.New("no_addresses")
	}
	if !v.AllowPrivateAddresses {
		for _, a := range addrs {
			if !isPublicGlobalUnicast(a.IP) {
				return fmt.Errorf("%w: %s", ErrAddressNotPublic, a.IP)
			}
		}
	}
	pinned := addrs[0].IP

	// Build a per-attempt client. NOT pu.HTTPClient — that follows
	// redirects and shares a transport pool with op-dispatch. The
	// verifier is a slow path; allocating one client per attempt is
	// fine.
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// addr is "<hostname>:<port>" — replace the host
				// with the pre-checked IP, keep the port. The
				// Request.Host header still carries the original
				// hostname so the server sees the right vhost.
				_, port, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					return nil, splitErr
				}
				return (&net.Dialer{Timeout: 3 * time.Second}).
					DialContext(ctx, network, net.JoinHostPort(pinned.String(), port))
			},
		},
	}

	url := fmt.Sprintf("http://%s%s/.well-known/txco-verify/%s",
		hostname, portSuffix, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build_request: %w", err)
	}
	req.Header.Set("User-Agent", "txco-hostname-verifier/1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return ErrUnexpectedRedirect
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("read_body: %w", err)
	}
	if strings.TrimSpace(string(body)) != token {
		return ErrTokenMismatch
	}
	return nil
}

// isPublicGlobalUnicast reports whether ip is safe to fetch from in
// the SSRF-defense sense: not loopback, not private, not link-local,
// not multicast, not unspecified, not CGNAT, not IPv6 ULA. Used by
// VerifyHTTP to reject hostnames whose A/AAAA records point at
// internal addresses.
//
// Defense-in-depth: even though we pin the connect IP, an attacker
// who controls the DNS could otherwise direct the chassis at
// 169.254.169.254 (AWS/GCE metadata), an internal service via
// RFC1918, or the loopback. This blocklist is the first line.
func isPublicGlobalUnicast(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		// RFC 1918.
		if ip4[0] == 10 {
			return false
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return false
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return false
		}
		// CGNAT (RFC 6598).
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		// 0.0.0.0/8 (current network) — also caught by IsUnspecified
		// for the literal 0.0.0.0 but the /8 block is reserved too.
		if ip4[0] == 0 {
			return false
		}
		// 192.0.0.0/24 IETF protocol assignments, 192.0.2.0/24
		// TEST-NET-1, 198.51.100.0/24 TEST-NET-2, 203.0.113.0/24
		// TEST-NET-3 — operators sometimes use these in lab setups
		// and they shouldn't accept production routing.
		if ip4[0] == 192 && ip4[1] == 0 && (ip4[2] == 0 || ip4[2] == 2) {
			return false
		}
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19 || (ip4[1] == 51 && ip4[2] == 100)) {
			return false
		}
		if ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113 {
			return false
		}
		// 240.0.0.0/4 reserved + 255.255.255.255 broadcast.
		if ip4[0] >= 240 {
			return false
		}
		return true
	}
	// IPv6.
	// fd00::/8 unique local addresses (ULA).
	if ip[0] == 0xfd || ip[0] == 0xfc {
		return false
	}
	// fec0::/10 deprecated site-local — defensively block.
	if ip[0] == 0xfe && (ip[1]&0xc0) == 0xc0 {
		return false
	}
	return true
}
