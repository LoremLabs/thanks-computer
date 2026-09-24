// Package open registers the "open" egress policy: it allows every
// outbound op dial. `txco dev` selects it so local development and testing
// reach any address; `serve` defaults to "private" (--egress-policy).
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
