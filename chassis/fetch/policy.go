package fetch

import (
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// maxURLBytes bounds the URL a caller may ask for (and a redirect may
// point at).
const maxURLBytes = 4096

// CheckURL parses rawURL and applies the URL policy: http or https only,
// no userinfo, port 80 or 443 (or none), a host that IDNA-normalizes. It
// returns the normalized URL. The same check runs on every redirect
// target. Where the host resolves is decided later, at the dial.
func CheckURL(rawURL string) (*url.URL, *Error) {
	if rawURL == "" {
		return nil, errf(CodeInvalidURL, "`url` is required")
	}
	if len(rawURL) > maxURLBytes {
		return nil, errf(CodeInvalidURL, "the URL is longer than %d bytes", maxURLBytes)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errf(CodeInvalidURL, "the URL does not parse")
	}
	return u, checkParsed(u)
}

func checkParsed(u *url.URL) *Error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return errf(CodeInvalidURL, "only http and https URLs can be fetched")
	}
	if u.Opaque != "" {
		return errf(CodeInvalidURL, "the URL has no host")
	}
	if u.User != nil {
		return errf(CodeInvalidURL, "a URL with credentials in it can't be fetched")
	}
	host := u.Hostname()
	if host == "" {
		return errf(CodeInvalidURL, "the URL has no host")
	}
	switch u.Port() {
	case "", "80", "443":
	default:
		return errf(CodeInvalidURL, "only ports 80 and 443 can be fetched")
	}
	if net.ParseIP(host) == nil {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil || ascii == "" {
			return errf(CodeInvalidURL, "the URL's host is not a valid hostname")
		}
		if ascii != host {
			if p := u.Port(); p != "" {
				u.Host = net.JoinHostPort(ascii, p)
			} else {
				u.Host = ascii
			}
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return nil
}

// allowedPort reports whether a dial port is one the policy permits.
func allowedPort(port string) bool { return port == "80" || port == "443" }
