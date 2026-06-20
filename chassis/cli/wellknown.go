package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
)

// signingKeysWellKnownPath is where a registry publishes the public keys it
// signs packages with. The CLI fetches it during signed-package verification so
// packages from a (blessed) registry verify with ZERO local trust config: the
// trust root is HTTPS + control of the registry domain — the same root that
// already serves you the package bytes — and the discovered keys are scoped to
// that one registry host (never trusted for any other registry).
const signingKeysWellKnownPath = "/.well-known/txco-signing-keys.json"

// wellKnownTimeout bounds the discovery fetch. It's best-effort, so it must
// never stall an install for long.
const wellKnownTimeout = 10 * time.Second

// maxWellKnownBytes caps the discovery response so a hostile/huge endpoint
// can't exhaust memory.
const maxWellKnownBytes = 1 << 20 // 1 MiB

// signingKeysDoc is the JSON document served at signingKeysWellKnownPath.
type signingKeysDoc struct {
	Keys []signingKeyEntry `json:"keys"`
}

type signingKeyEntry struct {
	Name   string `json:"name"`   // display label, e.g. "txco"
	Pubkey string `json:"pubkey"` // an ssh-ed25519 authorized_keys line
}

// fetchRegistrySigningKeys fetches the keys a registry publishes at its
// well-known endpoint and returns them as TrustedKeys SCOPED to that registry
// host. Best-effort by design: a missing endpoint or any transport/parse
// failure yields no keys (never an error that would fail the caller) — an
// unverifiable package simply stays "untrusted". An empty host yields nothing.
// Set TXCO_NO_KEY_DISCOVERY=1 to disable the network fetch entirely (air-gapped
// or strict environments; trust then comes only from `trust:`/--key).
func fetchRegistrySigningKeys(ctx context.Context, registryHost string) []sign.TrustedKey {
	if registryHost == "" || os.Getenv("TXCO_NO_KEY_DISCOVERY") != "" {
		return nil
	}
	url := "https://" + registryHost + signingKeysWellKnownPath
	keys, _ := fetchSigningKeysFrom(ctx, http.DefaultClient, url, registryHost)
	return keys
}

// fetchSigningKeysFrom is the testable core of discovery: GET url, parse the
// doc, and scope every key to registryHost. A non-200 yields (nil, nil) — the
// endpoint is optional, so its absence is not an error.
func fetchSigningKeysFrom(ctx context.Context, client *http.Client, url, registryHost string) ([]sign.TrustedKey, error) {
	ctx, cancel := context.WithTimeout(ctx, wellKnownTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil // optional endpoint; absent is fine
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWellKnownBytes))
	if err != nil {
		return nil, err
	}
	return parseSigningKeys(body, registryHost)
}

// parseSigningKeys decodes the well-known document into registry-scoped
// TrustedKeys. A malformed individual entry is skipped (one bad key shouldn't
// drop the rest); a malformed document as a whole is an error.
func parseSigningKeys(data []byte, registryHost string) ([]sign.TrustedKey, error) {
	var doc signingKeysDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("well-known signing keys: %w", err)
	}
	var out []sign.TrustedKey
	for _, e := range doc.Keys {
		pub, err := sign.ParseTrustedKey(e.Pubkey)
		if err != nil {
			continue
		}
		out = append(out, sign.TrustedKey{
			Name:     e.Name,
			Pub:      pub,
			KeyID:    sign.KeyIDForPub(pub),
			Registry: registryHost,
		})
	}
	return out, nil
}
