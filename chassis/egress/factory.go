package egress

import "fmt"

// Config carries backend-selecting options resolved from chassis config.
// The "private" backend uses DenyCIDRs/AllowCIDRs to extend/override its
// built-in private-range set. Additional backends may extend this struct
// with their own fields; existing backends and callers are unaffected by
// added fields.
type Config struct {
	// DenyCIDRs are extra CIDRs the policy also blocks (deployment-
	// specific internal ranges).
	DenyCIDRs []string
	// AllowCIDRs are CIDRs allowed even if otherwise blocked — an
	// explicit escape hatch. Allow wins over deny.
	AllowCIDRs []string
}

// Constructor builds a Guard from resolved config.
type Constructor func(Config) (Guard, error)

// registry maps backend name → constructor. Backends self-register via
// init() (see open, private). The map is the extension seam — an
// additional backend in a separate package registers itself the same
// way, with no change to callers or this package.
var registry = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named backend. Unknown name is a startup error
// listing what is available; a backend may also return an error for
// invalid config (e.g. a malformed CIDR) so misconfiguration fails
// loudly at boot.
func Open(name string, cfg Config) (Guard, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("egress: unknown policy %q (available: %v)", name, avail)
	}
	return c(cfg)
}
