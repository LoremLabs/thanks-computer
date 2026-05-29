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

// StackArtifactFile is one (path, content) pair in a
// StackActivatedArtifact. Path is "<scope>/<name>.txcl" plus the
// well-known "mock-request.json" / "mock-response.json".
type StackArtifactFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
