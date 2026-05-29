// Package open registers the "open" egress policy: it allows every
// outbound op dial. This is the default — local development and testing
// reach any address, exactly as before any policy existed.
package open

import "github.com/loremlabs/thanks-computer/chassis/egress"

func init() {
	egress.Register("open", func(egress.Config) (egress.Guard, error) {
		return guard{}, nil
	})
}

type guard struct{}

func (guard) CheckAddr(string, string) error { return nil }
func (guard) Name() string                   { return "open" }
