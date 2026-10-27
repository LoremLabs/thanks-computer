package connection

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// An Origin is where a credential may be sent: scheme, host and port,
// exactly — never a host name alone, so a port the declaration did not name
// is refused. `https://` only, except `http://` to a loopback host on a
// development node.
type Origin struct {
	Scheme string
	Host   string // lower case, IDNA ASCII
	Port   string // always set: the scheme's default when the URL had none
}

// ParseOrigin reads "https://api.github.com" or "https://host:8443". A path
// other than "/" (or none), a query, a fragment or user info is refused: an
// origin names a place, not a resource.
func ParseOrigin(s string) (Origin, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil {
		return Origin{}, fmt.Errorf("origin %q: %v", s, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Origin{}, fmt.Errorf("origin %q: want https://host[:port]", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return Origin{}, fmt.Errorf("origin %q: an origin is scheme://host[:port], nothing more", s)
	}
	host, err := normalizeHost(u.Hostname())
	if err != nil {
		return Origin{}, fmt.Errorf("origin %q: %v", s, err)
	}
	if u.Scheme == "http" && !Loopback(host) {
		return Origin{}, fmt.Errorf("origin %q: http:// is allowed only to localhost", s)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return Origin{Scheme: u.Scheme, Host: host, Port: port}, nil
}

// OriginOf is the origin a request URL reaches, normalized the same way, so
// a declaration and a request compare exactly.
func OriginOf(u *url.URL) (Origin, error) {
	if u == nil {
		return Origin{}, fmt.Errorf("no url")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Origin{}, fmt.Errorf("url %q: not http(s)", u.Redacted())
	}
	host, err := normalizeHost(u.Hostname())
	if err != nil {
		return Origin{}, fmt.Errorf("url %q: %v", u.Redacted(), err)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return Origin{Scheme: u.Scheme, Host: host, Port: port}, nil
}

// String is the canonical form: the default port left out.
func (o Origin) String() string {
	if o.Port == defaultPort(o.Scheme) {
		return o.Scheme + "://" + o.Host
	}
	return o.Scheme + "://" + net.JoinHostPort(o.Host, o.Port)
}

// Loopback reports whether host is the local machine by name or literal:
// the one place an http:// origin, and a dial the private guard would
// refuse, is allowed on a development node.
func Loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func normalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", fmt.Errorf("no host")
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", fmt.Errorf("host %q: %v", h, err)
	}
	return ascii, nil
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// originSet is a parsed, deduplicated list of origins.
type originSet map[Origin]bool

func (s originSet) has(o Origin) bool { return s[o] }

func parseOrigins(field string, raw []string) (originSet, []Origin, error) {
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("%s: at least one origin is required", field)
	}
	set := originSet{}
	var list []Origin
	for i, r := range raw {
		o, err := ParseOrigin(r)
		if err != nil {
			return nil, nil, fmt.Errorf("%s[%d]: %v", field, i, err)
		}
		if set[o] {
			continue
		}
		set[o] = true
		list = append(list, o)
	}
	return set, list, nil
}
