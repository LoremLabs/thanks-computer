package chat

import (
	"fmt"
	"sort"
	"sync"
)

// registry maps backend name → constructor. Backends self-register via
// init() (see chat/openrouter). The map is the extension seam — an
// additional backend in a separate package registers itself the same
// way, with no change to callers or this package. Same pattern as
// compute.RegisterEngine and egress.Register.
var (
	registryMu        sync.RWMutex
	registry          = map[string]Constructor{}
	registrationOrder []string
)

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
//
// Re-registering an existing name overwrites the constructor (test
// support); the registration order is preserved on first-registration
// only, so a re-registered backend keeps its original priority slot.
func Register(name string, c Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; !dup {
		registrationOrder = append(registrationOrder, name)
	}
	registry[name] = c
}

// Open constructs the named backend. Unknown name is an error listing
// what is available; a backend may also return an error for invalid
// config so misconfiguration fails loudly at boot.
func Open(name string, cfg Config) (Backend, error) {
	registryMu.RLock()
	c, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("chat: unknown backend %q (available: %v)", name, registered())
	}
	return c(cfg)
}

// Resolve picks a backend for one ai://chat dispatch. v1 logic is
// intentionally minimal:
//
//   - providerHint non-empty → look up by name in the registry; error
//     with NoBackendError if unknown. Trace records
//     routing_decision = "provider-override".
//   - providerHint empty → return the first-registered backend; trace
//     records routing_decision = "default". With a single v1 backend
//     (OpenRouter), this is unambiguous.
//
// Capability-superset matching across multiple registered backends is
// its own design exercise (ordering, ties, no-match fallback semantics)
// and lands alongside the second backend in a follow-up PR.
// Backend.Capabilities() is still captured in the trace today as
// descriptive metadata.
func Resolve(providerHint string, cfg Config) (Backend, string, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	if providerHint != "" {
		c, ok := registry[providerHint]
		if !ok {
			return nil, "", &NoBackendError{
				ProviderHint: providerHint,
				Registered:   registeredLocked(),
			}
		}
		b, err := c(cfg)
		if err != nil {
			return nil, "", err
		}
		return b, "provider-override", nil
	}

	if len(registrationOrder) == 0 {
		return nil, "", &NoBackendError{Registered: nil}
	}
	first := registrationOrder[0]
	b, err := registry[first](cfg)
	if err != nil {
		return nil, "", err
	}
	return b, "default", nil
}

// Registered returns the names of currently registered backends, sorted
// for deterministic display. Used by error messages and observability.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registeredLocked()
}

// registered is a helper for callers that don't already hold the lock.
func registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registeredLocked()
}

func registeredLocked() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resetForTests clears the registry. Used by package tests in this
// package and chat/openrouter so they can register fresh stubs without
// cross-test contamination. NOT for production use.
func resetForTests() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Constructor{}
	registrationOrder = nil
}
