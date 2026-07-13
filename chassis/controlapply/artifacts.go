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
		"dns_records":          true, // zone override/extra records; ride with dns_zones so the dns head serves what the admin renders
		"cron_settings":        true, // per-tenant cron timezone; lets every node localize @cron.* consistently
		// Per-tenant secret store. The two tables ride together: the parent
		// (tenant_secrets) carries identity + active key_version, the child
		// (tenant_secret_versions) carries the encrypted blob columns. Their
		// ciphertext is decryptable on every node ONLY when the fleet shares one
		// master key (TXCO_SECRET_MASTER_KEY_B64) — see secrets/master_key.go.
		// version rows carry BLOB columns via the {"$b64":…} coerce convention.
		"tenant_secrets":         true,
		"tenant_secret_versions": true,
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
