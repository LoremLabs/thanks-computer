package egress

import (
	"net"
	"net/http"
	"time"
)

// Transport is the HTTP transport for outbound requests the guard
// polices: a clone of http.DefaultTransport whose dials go through g, and
// which never uses a proxy.
//
// The default transport takes its proxy from the environment
// (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY). Through a proxy the dial is to the
// proxy, so the guard would check the proxy's address instead of the
// destination's, and a request could reach anything the proxy can. No
// deployment sets those variables today; this keeps it that way for every
// guarded client. Callers tune the pool on the copy they get.
func Transport(g Guard) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   DialControl(g),
	}).DialContext
	return tr
}
