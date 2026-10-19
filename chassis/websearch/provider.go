package websearch

import (
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/fetch"
)

// Provider is one search engine behind txco://web/search. It builds the
// request and reads the answer; it never dials. The handler fetches
// through chassis/fetch with the request a provider built, so every
// provider gets the same egress guard, caps and deadline.
//
// Build is the only place the key is handled. It goes into the header
// the provider reads it from and nowhere else: not the URL, not a log,
// not an error.
type Provider interface {
	// Name is the registered name, and the `provider` field of the answer.
	Name() string
	// RequiredSecrets names the tenant secrets the provider needs, as the
	// ai:// backends do (BRAVE_SEARCH_KEY). The handler materializes them
	// from the tenant's store, with the env fallback the ai:// ops use.
	RequiredSecrets() []string
	// Build turns a validated request into the one fetch to make.
	Build(req Request, key []byte) (fetch.Request, error)
	// Parse reads a 2xx body into results, in the provider's order, raw:
	// Normalize cleans and caps them. An error means the body was not
	// what the provider sends (invalid_response).
	Parse(body []byte) ([]Result, error)
	// Failure maps a non-2xx answer to the op's error: provider_auth for
	// a refused key, rate_limited with retry_after, invalid_request when
	// the provider refused a parameter, provider_error otherwise.
	Failure(status int, header http.Header) *Error
}

// Config carries what a provider needs from the chassis configuration.
type Config struct {
	// BraveURL is the Brave endpoint (--web-search-brave-url); "" is the
	// public one.
	BraveURL string
}

// Constructor builds a provider from the configuration.
type Constructor func(Config) (Provider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Constructor{}
)

// Register adds a provider under its name. A provider package calls it
// from init(); the chassis links one with a blank import, the way the
// chat backends are linked.
func Register(name string, c Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = c
}

// Open builds the named provider. An unknown name lists what is linked.
func Open(name string, cfg Config) (Provider, error) {
	registryMu.RLock()
	c, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("websearch: unknown provider %q (linked: %v)", name, Registered())
	}
	return c(cfg)
}

// Registered lists the linked providers, sorted.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
