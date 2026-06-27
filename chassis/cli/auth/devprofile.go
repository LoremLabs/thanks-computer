package auth

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// DevProfileName is the conventional profile that `txco dev` registers for
// its local chassis, so a developer gets a named target (`--target dev`,
// `txco apply dev`, `txco auth … dev`) without bootstrapping a signing key.
const DevProfileName = "dev"

// DevProfileAction reports what EnsureDevProfile did, for a tidy one-line log.
type DevProfileAction string

const (
	DevProfileWrote   DevProfileAction = "wrote"   // created or refreshed the keyless meta
	DevProfileCurrent DevProfileAction = "current" // keyless meta already matched
	DevProfileKept    DevProfileAction = "kept"    // a real enrolled profile owns the name — left untouched
)

// EnsureDevProfile writes a KEYLESS profile pointing at a local chassis so
// `txco dev` users get a named target without enrolling a key. It pairs with
// the local-chassis relaxations elsewhere (the deploy resolver and
// buildSignedTarget both send unsigned when a profile resolves to a local
// chassis with no signer), so `--target dev` Just Works against the open dev
// chassis — no `bootstrap-local`.
//
// It deliberately does NOT touch the active-profile pointer: the dev profile
// is available as an explicit selector but never hijacks the operator's
// prod/cloud commands.
//
// Idempotent and safe:
//   - If a profile of this name already exists WITH an enrolled key (ActorID
//     set), it's left untouched — we never clobber a real signing identity the
//     user set up themselves. Returns DevProfileKept.
//   - If our keyless meta already matches (same chassis URL + tenant), nothing
//     is written. Returns DevProfileCurrent.
//   - Otherwise the keyless meta is (re)written so a changed dev admin addr
//     stays current. Returns DevProfileWrote.
//
// chassisURL must be local; a non-local URL is rejected, since a keyless
// profile only works against an open chassis and writing one for a remote
// endpoint would be a footgun (silent unsigned → 401).
func EnsureDevProfile(name, chassisURL, tenant string) (DevProfileAction, string, error) {
	if !LocalChassis(chassisURL) {
		return "", "", fmt.Errorf("auth: refusing to register keyless profile %q for non-local chassis %q", name, chassisURL)
	}
	metaPath, err := MetaPath(name)
	if err != nil {
		return "", "", err
	}
	switch existing, err := LoadMeta(metaPath); {
	case err == nil && existing.ActorID != "":
		// A real enrolled profile owns this name — don't touch it.
		return DevProfileKept, metaPath, nil
	case err == nil && existing.ChassisURL == chassisURL && existing.DefaultTenant == tenant:
		// Already our keyless meta and nothing changed.
		return DevProfileCurrent, metaPath, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", "", err
	}
	if err := SaveMeta(metaPath, Meta{
		ChassisURL:    chassisURL,
		DefaultTenant: tenant,
		Label:         "txco dev (local, keyless)",
		EnrolledAt:    time.Now().UTC(),
	}); err != nil {
		return "", "", err
	}
	return DevProfileWrote, metaPath, nil
}
