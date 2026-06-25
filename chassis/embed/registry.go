package embed

import (
	"fmt"
	"sort"
	"sync"
)

// registry maps backend name → constructor. Backends self-register via init()
// (see embed/ollama). The map is the extension seam — an additional backend
// in a separate package registers itself the same way, with no change to
// callers or this package. Same pattern as chat.Register.
var (
	registryMu        sync.RWMutex
	registry          = map[string]Constructor{}
	registrationOrder []string
)

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
//
// Re-registering an existing name overwrites the constructor (test support);
// registration order is preserved on first-registration only.
func Register(name string, c Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; !dup {
		registrationOrder = append(registrationOrder, name)
	}
	registry[name] = c
}

// Open constructs the named backend. Unknown name is an error listing what is
// available.
func Open(name string, cfg Config) (Backend, error) {
	registryMu.RLock()
	c, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("embed: unknown backend %q (available: %v)", name, registered())
	}
	return c(cfg)
}

// Resolve picks a backend for one ai://embed dispatch. v1 logic mirrors
// chat.Resolve:
//
//   - providerHint non-empty → look up by name; NoBackendError if unknown.
//     routing_decision = "provider-override".
//   - providerHint empty → first-registered backend. routing_decision =
//     "default". With a single v1 backend (ollama), this is unambiguous.
func Resolve(providerHint string, cfg Config) (Backend, string, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	if providerHint != "" {
		c, ok := registry[providerHint]
		if !ok {
			return nil, "", &NoBackendError{ProviderHint: providerHint, Registered: registeredLocked()}
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

// Registered returns the names of currently registered backends, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registeredLocked()
}

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

// resetForTests clears the registry. Used by package tests so they can
// register fresh stubs without cross-test contamination. NOT for production.
func resetForTests() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Constructor{}
	registrationOrder = nil
}
