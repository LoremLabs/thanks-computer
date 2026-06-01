// Package tls is the chassis's bundled TLS/ACME layer: an embedded ACME
// client (certmagic) that obtains and renews certificates for delegated
// zones, solving the ACME DNS-01 challenge IN-PROCESS against the chassis's
// own authoritative DNS head. Because the ACME client and the nameserver
// share one process, the challenge is published with a direct function call
// — no RFC2136, no external DNS provider, no plugin. The same challenge
// substrate (the dns head's ChallengeStore) also accepts challenges written
// over RFC2136 by an external ACME client; this package is the bundled,
// self-contained writer.
//
// Two seams keep it deployable at any scale, each with a local default in
// core and a shared backend registered by a downstream overlay (mirroring
// chassis/auth/registry/dialect.go):
//   - cert/account storage (certmagic.Storage) — file by default, see storage.go
//   - the challenge substrate (ChallengePublisher) — the in-memory dns head
//     store by default; a shared store lets any node answer the challenge.
package tls

import (
	"context"
	"strings"

	"github.com/libdns/libdns"
)

// ChallengePublisher is the minimal write surface the DNS-01 solver needs:
// publish and remove an `_acme-challenge` TXT value at an owner FQDN.
// Satisfied structurally by the dns head's ChallengeStore (Present/CleanUp),
// so this package never imports the dns personality.
type ChallengePublisher interface {
	Present(fqdn, value string)
	CleanUp(fqdn, value string)
}

// challengeProvider adapts a ChallengePublisher to the libdns
// RecordAppender/RecordDeleter interfaces certmagic's DNS-01 solver drives.
// It writes ONLY into the transient challenge store — never durable zone
// data — so an in-process ACME solve publishes a TXT the same chassis's DNS
// head then serves, and removes it on cleanup.
type challengeProvider struct {
	pub ChallengePublisher
}

// AppendRecords publishes each challenge value. certmagic only ever asks us
// to append `_acme-challenge` TXT records; we reconstruct the absolute owner
// and publish. The input records are returned verbatim (certmagic expects
// exactly the records it asked to create back).
func (p challengeProvider) AppendRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, r := range recs {
		rr := r.RR()
		p.pub.Present(ownerFQDN(rr.Name, zone), rr.Data)
	}
	return recs, nil
}

// DeleteRecords removes each challenge value (idempotent in the store).
func (p challengeProvider) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, r := range recs {
		rr := r.RR()
		p.pub.CleanUp(ownerFQDN(rr.Name, zone), rr.Data)
	}
	return recs, nil
}

// ownerFQDN reconstructs the absolute, lowercased, trailing-dot owner name
// from a libdns (relative-name, zone) pair — the exact form the dns head
// keys challenges on (strings.ToLower(dns.Fqdn(name))).
func ownerFQDN(name, zone string) string {
	abs := strings.ToLower(libdns.AbsoluteName(name, zone))
	if !strings.HasSuffix(abs, ".") {
		abs += "."
	}
	return abs
}
