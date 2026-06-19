// Package controlevent defines the control-plane event contract: the
// notification a control plane appends and a fleet chassis consumes.
//
// See internal docs/todo-architecture-saas-fleet.md §3.1. An event is a semantic
// invalidation/version record — NEVER the payload itself. It carries
// identity, a content-addressed pointer (ArtifactRef), an integrity seal
// (Checksum) and the fleet position (ControlVersion). The bytes live in the
// artifact store, not in the log.
//
// This package is the shared event contract: the chassis consumes events; a
// control plane produces them. It is intentionally dependency-free so both
// sides can depend on it.
package controlevent

import (
	"errors"
	"fmt"
	"strings"
)

// Model/cache versions. A node rejects an artifact (or event) whose declared
// versions it cannot interpret (see snapshot.Verify). Bump deliberately when
// the on-the-wire contract or the materialised cache shape changes
// incompatibly.
const (
	// ControlModelVersion is the version of the event/contract shape.
	ControlModelVersion = 1
	// CacheSchemaVersion is the version of the materialised local cache
	// shape an artifact restores into.
	CacheSchemaVersion = 1
)

// Event types. These are the existing admin-API mutations, externalised.
const (
	TypeStackActivated     = "stack.activated"
	TypeTenantCreated      = "tenant.created"
	TypeHostnameBound      = "hostname.bound"
	TypeHostnameVerified   = "hostname.verified"
	TypeHostnameRevoked    = "hostname.revoked"
	TypeActorChanged       = "actor.changed"
	TypeKeyChanged         = "key.changed"
	TypeMembershipChanged  = "membership.changed"
	TypeEntitlementUpdated = "entitlement.updated"
	TypeSystemOpstack      = "system.opstack.updated"
	// TypeDNSZoneUpserted carries a dns_zones row (RowsArtifact, op=upsert;
	// revocation is an upsert with revoked_at set). Lets a data-plane node hold
	// the delegated-zone state so it re-derives `<label>.<origin>` routing hosts
	// itself and (with the dns personality) can serve the zone — see
	// internal docs/todo-dns-authority.md §9 fleet note.
	TypeDNSZoneUpserted = "dns.zone.upserted"
	// TypeCronSettingsUpserted carries a cron_settings row (RowsArtifact,
	// op=upsert; clearing a timezone is an upsert with timezone=''). Lets every
	// node localize a tenant's @cron.* wall-clock fields consistently.
	TypeCronSettingsUpserted = "cron.settings.upserted"
)

var knownTypes = map[string]bool{
	TypeStackActivated: true, TypeTenantCreated: true,
	TypeHostnameBound: true, TypeHostnameVerified: true,
	TypeHostnameRevoked: true, TypeActorChanged: true,
	TypeKeyChanged: true, TypeMembershipChanged: true,
	TypeEntitlementUpdated: true, TypeSystemOpstack: true,
	TypeDNSZoneUpserted: true, TypeCronSettingsUpserted: true,
}

// Event is the control-plane event contract.
//
// Field notes (deliberate, see plan):
//   - EventID is the producer-assigned semantic identity (UUID; UUIDv7
//     via chassis/hxid is the conventional choice). It rides the
//     event through retries and replays so consumers can recognise a
//     duplicate even when broker-side dedup (e.g. JetStream
//     Nats-Msg-Id) has expired. Required; an empty EventID fails
//     Validate.
//   - Version is the *stack* version (stack_versions.version_number).
//   - BaseVersion is the producer's view of the prior value of the
//     mutated resource — e.g. the active_version it saw before
//     flipping it for a stack.activated event. Optional in v1; not
//     enforced by the applier. Carried so a later revision can layer
//     CAS-style optimistic concurrency on top without a wire-format
//     change.
//   - ControlVersion is the global monotonic *fleet cursor* — the resume
//     position. Producer-side it is zero (the backend's broker
//     stamps it on publish — JetStream stream_sequence in the NATS
//     overlay; sequential ints in the file backend). Consumer-side
//     Validate requires > 0.
//   - ArtifactRef points at the event's artifact in the artifact store. It
//     is "the artifact for THIS event", not necessarily a full bootstrap
//     SQLite snapshot — hence ArtifactRef, not "snapshot_ref".
//   - There is no "active_version": it was ambiguous against Version and is
//     omitted until/unless it earns a distinct meaning.
type Event struct {
	ID             int64  `json:"id"`
	EventID        string `json:"event_id"`
	Type           string `json:"type"`
	TenantID       string `json:"tenant_id,omitempty"`
	StackID        string `json:"stack_id,omitempty"`
	Version        int64  `json:"version,omitempty"`
	BaseVersion    int64  `json:"base_version,omitempty"`
	ArtifactRef    string `json:"artifact_ref,omitempty"`
	Checksum       string `json:"checksum,omitempty"`
	ControlVersion uint64 `json:"control_version"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// ErrInvalid wraps every Validate failure.
var ErrInvalid = errors.New("controlevent: invalid event")

// Validate checks the structural invariants every consumer relies on. It is
// deliberately strict: a malformed system event must fail loudly, never
// silently misroute.
//
// Validate is consumer-side: it runs after the carrier has stamped
// ControlVersion (broker-assigned). Producers do not need to call it
// before queueing an outbox row — the Sink stamps ControlVersion at
// publish time, and the next applier on any chassis re-runs Validate.
func (e Event) Validate() error {
	if e.EventID == "" {
		return fmt.Errorf("%w: empty event_id", ErrInvalid)
	}
	if e.Type == "" {
		return fmt.Errorf("%w: empty type", ErrInvalid)
	}
	if !knownTypes[e.Type] {
		return fmt.Errorf("%w: unknown type %q", ErrInvalid, e.Type)
	}
	if e.ControlVersion == 0 {
		return fmt.Errorf("%w: control_version must be > 0", ErrInvalid)
	}
	if e.Checksum != "" && !strings.HasPrefix(e.Checksum, "sha256:") {
		return fmt.Errorf("%w: checksum must be sha256:-prefixed", ErrInvalid)
	}
	// An event that names an artifact must seal it.
	if e.ArtifactRef != "" && e.Checksum == "" {
		return fmt.Errorf("%w: artifact_ref without checksum", ErrInvalid)
	}
	if e.Type == TypeStackActivated {
		if e.TenantID == "" || e.StackID == "" || e.Version == 0 {
			return fmt.Errorf("%w: %s requires tenant_id, stack_id, version",
				ErrInvalid, TypeStackActivated)
		}
	}
	return nil
}

// CompatibleModel reports whether this chassis can interpret an artifact/event
// declaring the given control-model version. Same major line only; an older
// binary must refuse a newer model rather than guess.
func CompatibleModel(modelVersion int) bool {
	return modelVersion == ControlModelVersion
}

// CompatibleCacheSchema reports whether this chassis can load an artifact
// declaring the given cache-schema version.
func CompatibleCacheSchema(cacheSchemaVersion int) bool {
	return cacheSchemaVersion == CacheSchemaVersion
}
