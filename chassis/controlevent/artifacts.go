package controlevent

// Artifact payload schemas — the bytes a producer writes to the
// artifact store and the consumer fetches via `Event.ArtifactRef`.
// Kept here (alongside the event contract) so both producer and
// consumer can depend on them without cycling through chassis/admin
// or chassis/controlapply. The consumer-side apply policy
// (whitelisted tables, dispatch by Type) stays in controlapply.

// StackActivatedArtifact is the payload for a TypeStackActivated
// event: the full file set of the activated stack version. The node
// upserts these into stack_versions/stack_files then materialises
// them into ops.
type StackActivatedArtifact struct {
	TenantID string              `json:"tenant_id"`
	Stack    string              `json:"stack"`
	Version  int64               `json:"version"`
	Files    []StackArtifactFile `json:"files"`
}

// StackArtifactFile is one file of a StackActivatedArtifact. Path is
// "<scope>/<name>.txcl", the well-known "mock-request.json" /
// "mock-response.json", or a "FILES/**" static asset.
//
// Rule/fixture files carry their bytes inline in Content. FILES/** static
// assets instead carry only ContentHash (the sha256 fingerprint): their
// bytes live in the shared content-addressed store (filecas), so the event
// stays small and data-plane nodes never inline tenant file bytes into the
// in-memory runtime DB. Both fields are omitempty so old (all-inline) and
// new (fingerprint) events parse in either direction.
type StackArtifactFile struct {
	Path        string `json:"path"`
	Content     string `json:"content,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// RowsArtifact is the payload for the generic row event types
// (tenant.created, hostname.*, membership.changed,
// entitlement.updated, and the auth.* types). It carries
// authoritative rows for one table and how to apply them. DB selects
// the target database ("runtime" or "auth"); the table is checked
// against a hard whitelist on the consumer side before any SQL runs.
type RowsArtifact struct {
	DB    string           `json:"db"`    // "runtime" | "auth"
	Table string           `json:"table"` // must be whitelisted
	Op    string           `json:"op"`    // "upsert" | "delete"
	PK    []string         `json:"pk"`    // primary-key columns (delete)
	Rows  []map[string]any `json:"rows"`
}
