package ipp

import (
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// printerLabel is what may follow /p/: a DNS-label-ish name, lowercase. It
// is free-form on purpose — the chassis keeps no registry of printers; the
// label is an operation selector the tenant's `_ipp` stack interprets
// ("research", "summarize", "expenses").
var printerLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// target is where one request is aimed, as far as the URL can say.
type target struct {
	x       string // the host minus its leading `ipp.` label, canonical
	host    string // the full ipp host, canonical (no port)
	printer string
	job     int64 // 0 = the printer itself; >0 = /p/<printer>/jobs/<n>
}

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

// ippTarget parses Host + path into a target. ok=false is a 404: not an ipp
// host, or not a printer path.
//
//	Host: ipp.dripl.it        /p/research          → {x: dripl.it, printer: research}
//	Host: ipp.dripl.it:443    /p/research/jobs/42  → {…, job: 42}
func ippTarget(host, path string) (target, bool) {
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
	t := target{x: canon, host: tenants.IPPHostLabel + "." + canon}
	switch len(parts) {
	case 1:
	case 3:
		if parts[1] != "jobs" {
			return target{}, false
		}
		n, err := strconv.ParseInt(parts[2], 10, 32)
		if err != nil || n <= 0 {
			return target{}, false
		}
		t.job = n
	default:
		return target{}, false
	}
	if !printerLabel.MatchString(parts[0]) {
		return target{}, false
	}
	t.printer = parts[0]
	return t, true
}
