package connection

import (
	"net/http"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private" // the guard every credentialed dial goes through
)

// A credentialed request — a token exchange, a renewal, an API call that
// carries a connection's token — dials through the node's own egress guard
// AND the private-range guard, whatever the node's policy: a declaration
// must not be able to carry a credential into this machine or its network.
// The one exception is a loopback origin on a development node (the node's
// policy is open), where a test provider listens. And it follows no
// redirect, to any origin: a 3xx is the answer.
//
// The transport never takes a proxy from the environment (egress.Transport).

// RequestTimeout bounds one credentialed request. Shorter than the renewal
// claim's lease (claimLease), so a claimant that is still waiting on the
// provider has not lost its claim.
const RequestTimeout = 10 * time.Second

type clients struct {
	strict   *http.Client // node guard + private guard
	loopback *http.Client // node guard only; nil unless the node is open
}

type bothGuards struct{ a, b egress.Guard }

func (g bothGuards) Name() string { return g.a.Name() + "+" + g.b.Name() }
func (g bothGuards) CheckAddr(network, address string) error {
	if err := g.a.CheckAddr(network, address); err != nil {
		return err
	}
	return g.b.CheckAddr(network, address)
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newClient(g egress.Guard) *http.Client {
	tr := egress.Transport(g)
	tr.MaxIdleConnsPerHost = 8
	tr.IdleConnTimeout = 30 * time.Second
	return &http.Client{Transport: tr, Timeout: RequestTimeout, CheckRedirect: noRedirect}
}

// newClients builds the clients over the node's guard. open reports whether
// the node's policy is open (development), which is what allows a loopback
// origin.
func newClients(node egress.Guard, open bool) (*clients, error) {
	private, err := egress.Open("private", egress.Config{})
	if err != nil {
		return nil, err
	}
	c := &clients{strict: newClient(bothGuards{node, private})}
	if open {
		c.loopback = newClient(node)
	}
	return c, nil
}

// For picks the client an origin is dialed with.
func (c *clients) For(o Origin) *http.Client {
	if c.loopback != nil && Loopback(o.Host) {
		return c.loopback
	}
	return c.strict
}
