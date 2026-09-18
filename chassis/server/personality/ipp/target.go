package ipp

import (
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// printerLabel is what names a printer in the path: a DNS-label-ish name,
// lowercase. It is free-form on purpose — the chassis keeps no registry of
// printers; the label is an operation selector the tenant's `_ipp` stack
// interprets ("research", "summarize", "expenses").
var printerLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// handleLabel is ONE DNS label: the leftmost label of a structured hostname
// (`core-hmhzx2isby` of `core-hmhzx2isby.stacks.example`). Stricter than a
// printer label — no dots — because it is joined to the shared zone to form
// a hostname, and a dot would let a path pick a name at another depth.
var handleLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// target is where one request is aimed, as far as the URL can say.
type target struct {
	x       string // the hostname that decides the TENANT, canonical
	host    string // the ipp host the request arrived on, canonical (no port)
	handle  string // shared front door only: the structured handle from the path
	base    string // the printer's parent path: "/p", or "/p/<handle>"
	printer string
	job     int64 // 0 = the printer itself; >0 = …/<printer>/jobs/<n>
}

// uriPath is the printer's path on its host.
func (t target) uriPath() string { return t.base + "/" + t.printer }

// stripIPPLabel returns the host with its leading `ipp.` label removed.
// THE ONLY PLACE the marker form lives: `ipp.<X>`, exactly one leading
// label, nothing else (never `ipp-<X>`, never a path on an ordinary host).
func stripIPPLabel(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	x, ok := strings.CutPrefix(host, tenants.IPPHostLabel+".")
	if !ok || x == "" {
		return "", false
	}
	return x, true
}

// normalizeZone makes a --structured-host-suffix value (".stacks.example")
// comparable with a canonical hostname.
func normalizeZone(suffix string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
}

// ippTarget parses Host + path into a target. ok=false is a 404: not an ipp
// host, or not a printer path.
//
// A printer hangs off `ipp.<zone>`, and the tenant is whoever owns <zone>:
//
//	Host: ipp.dripl.it        /p/research          → tenant of dripl.it
//	Host: ipp.dripl.it:443    /p/research/jobs/42  → …job 42
//
// THE SHARED FRONT DOOR. One zone belongs to nobody: the platform's
// structured-host suffix (sharedZone — every tenant without a zone of its own
// lives there, as `<handle>.<suffix>`). `ipp.<suffix>` cannot take its tenant
// from the zone, so it takes it from the PATH — the handle of any hostname
// the tenant has under the suffix:
//
//	Host: ipp.stacks.example  /p/core-hmhzx2isby/research
//	                          → tenant of core-hmhzx2isby.stacks.example
//
// That single name sits one label under the suffix, so the suffix's wildcard
// certificate and wildcard DNS already cover it: every tenant gets a printer
// URL with no zone, no record and no certificate of its own. (The obvious
// alternative, `ipp.<handle>.<suffix>`, is two labels deep — no wildcard
// certificate reaches it.)
//
// The two forms are told apart by PATH SHAPE, never by host alone, because
// under `txco dev` the suffix is `localhost` and `ipp.localhost` is also the
// ordinary front door of the hostname `localhost`:
//
//	/p/<printer>                       plain
//	/p/<printer>/jobs/<n>              plain, a job       (middle is "jobs")
//	/p/<handle>/<printer>              shared — only on ipp.<sharedZone>
//	/p/<handle>/<printer>/jobs/<n>     shared, a job
func ippTarget(host, path, sharedZone string) (target, bool) {
	x, ok := stripIPPLabel(host)
	if !ok {
		return target{}, false
	}
	canon, ok := tenants.CanonicalizeHost(x)
	if !ok || !tenants.IsValidHostname(canon) {
		return target{}, false
	}
	rest, ok := strings.CutPrefix(path, PathPrefix+"/")
	if !ok {
		return target{}, false
	}
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	t := target{x: canon, host: tenants.IPPHostLabel + "." + canon, base: PathPrefix}

	// A trailing /jobs/<n> addresses one job of the printer before it.
	if n := len(parts); n >= 3 && parts[n-2] == "jobs" {
		job, err := strconv.ParseInt(parts[n-1], 10, 32)
		if err != nil || job <= 0 {
			return target{}, false
		}
		t.job = job
		parts = parts[:n-2]
	}
	switch len(parts) {
	case 1:
	case 2:
		// The handle form exists on the shared front door and nowhere else:
		// on a tenant's own zone the tenant is already known, and a second
		// segment is not a printer path.
		if sharedZone == "" || canon != sharedZone || !handleLabel.MatchString(parts[0]) {
			return target{}, false
		}
		t.handle = parts[0]
		t.x = t.handle + "." + sharedZone
		t.base = PathPrefix + "/" + t.handle
		parts = parts[1:]
	default:
		return target{}, false
	}
	if !printerLabel.MatchString(parts[0]) {
		return target{}, false
	}
	t.printer = parts[0]
	return t, true
}
