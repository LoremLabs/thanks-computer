// Package egress is the outbound op-dial policy seam. When an op does
// EXEC "http(s)://...", the chassis dials it through one shared HTTP
// transport. A Guard is consulted at the dial step — after DNS
// resolution, with the concrete IP about to be connected — so an op
// cannot use the chassis to reach addresses it should not (loopback,
// private networks, link-local cloud-metadata, etc.).
//
// The default backend ("open") allows everything, so local development
// and testing are unaffected. The map in factory.go is the extension
// seam: an additional backend in a separate package registers itself
// the same way as the built-ins, with no change to callers.
package egress

import "syscall"

// Guard decides whether an outbound op dial may proceed.
type Guard interface {
	// CheckAddr is called after DNS resolution with the concrete network
	// ("tcp4"/"tcp6"/...) and the resolved "ip:port" about to be dialed.
	// A non-nil error blocks the connection. Implementations must be
	// pure in-memory (no DNS/FS/DB): the dial's DNS lookup has already
	// happened and this runs on the request hot path.
	CheckAddr(network, address string) error
	// Name is the backend identity (for logs).
	Name() string
}

// DialControl adapts a Guard to a net.Dialer.Control function. Control
// runs once per resolved candidate address, before the socket connects,
// so checking here inspects the IP actually being dialed and defeats
// DNS-rebinding.
func DialControl(g Guard) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		return g.CheckAddr(network, address)
	}
}
