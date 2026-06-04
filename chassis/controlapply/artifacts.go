package controlapply

import "github.com/loremlabs/thanks-computer/chassis/controlevent"

// Re-export the artifact payload schemas as locally-named types so
// the existing applier code (and external tests) keep building
// without per-call-site rewrites. The wire shape lives in
// chassis/controlevent so admin handlers (producer side) can build
// these without importing controlapply (which would cycle through
// admin).

type (
	StackActivatedArtifact = controlevent.StackActivatedArtifact
	StackArtifactFile      = controlevent.StackArtifactFile
	RowsArtifact           = controlevent.RowsArtifact
)

const (
	dbRuntime = "runtime"
	dbAuth    = "auth"

	opUpsert = "upsert"
	opDelete = "delete"
)

// tableWhitelist is the ONLY set of tables the feed may write.
// Anything else is rejected. browser_bootstrap/browser_sessions are
// deliberately absent: they are node-local and ephemeral, never
// fleet-synced. Consumer-side policy — stays here.
var tableWhitelist = map[string]map[string]bool{
	dbRuntime: {
		"tenants":              true,
		"tenant_hostnames":     true,
		"tenant_runtime_state": true, // designed-not-shipped; skipped if absent
		"dns_zones":            true, // delegated-zone state; lets a data-plane node re-derive routing hosts + (with the dns head) serve the zone
	},
	dbAuth: {
		"actors":            true,
		"actor_keys":        true,
		"actor_memberships": true,
		"invitations":       true,
	},
}

func tableAllowed(db, table string) bool {
	return tableWhitelist[db] != nil && tableWhitelist[db][table]
}
