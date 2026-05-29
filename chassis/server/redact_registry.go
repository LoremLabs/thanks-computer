package server

import (
	"database/sql"
	"sort"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

// redactRegistry holds the per-(tenant, stack) redact/omit hint lists
// the trace sink applies on the worker thread. Built at chassis
// startup by walking the opstack snapshot and parsing each rule's
// txcl; rebuilt on every dbcache reload (apply, file-watch) by
// chaining onto dbc.OnReload.
//
// Hints come from `WITH redact = "..."` / `WITH omit = "..."` clauses
// on rules. Only literal string values are honored — a non-literal
// expression (`&fn(...)`, `@path`) is silently ignored, since the
// registry must be deterministic at build time.
//
// The snapshot is held behind an atomic pointer so reads (hot path on
// the trace worker) are lock-free. Lookups that miss return the empty
// Hints, which is a true no-op in trace.ApplyHints.
type redactRegistry struct {
	snap   atomic.Pointer[map[string]trace.Hints] // key = tenant + ":" + stack
	logger *zap.Logger
}

func newRedactRegistry(logger *zap.Logger) *redactRegistry {
	r := &redactRegistry{logger: logger}
	empty := map[string]trace.Hints{}
	r.snap.Store(&empty)
	return r
}

// Hints implements trace.HintLookup. Returns the merged
// (deduplicated, omit-wins) hint lists for the (tenant, stack) slot,
// or zero Hints if the slot is empty.
func (r *redactRegistry) Hints(tenant, stack string) trace.Hints {
	if r == nil || stack == "" {
		return trace.Hints{}
	}
	m := r.snap.Load()
	if m == nil {
		return trace.Hints{}
	}
	if tenant == "" {
		tenant = tenants.SystemTenantSlug
	}
	if h, ok := (*m)[tenant+":"+stack]; ok {
		return h
	}
	return trace.Hints{}
}

// Rebuild scans the opstack snapshot for every (tenant_id, stack,
// txcl) row, parses each rule's WITH clause, and rebuilds the
// (tenant, stack) → Hints lookup. Safe to call concurrently with
// readers — readers see either the previous snapshot or the new one,
// never a half-built one.
//
// On any error (SQL or parse-level) the previous snapshot is kept;
// trace redaction stays correct rather than going dark mid-flight.
func (r *redactRegistry) Rebuild(db *sql.DB) error {
	if r == nil || db == nil {
		return nil
	}
	rows, err := db.Query(`
        SELECT
            COALESCE((SELECT slug FROM tenants t WHERE t.tenant_id = o.tenant_id), ?) AS tenant,
            o.stack,
            o.txcl
        FROM ops o
        WHERE o.txcl IS NOT NULL AND o.txcl != ''`,
		tenants.SystemTenantSlug)
	if err != nil {
		r.logger.Warn("redact registry rebuild: query failed", zap.Error(err))
		return err
	}
	defer rows.Close()

	// Accumulate per-key sets, then materialize to sorted slices.
	type acc struct {
		redact map[string]struct{}
		omit   map[string]struct{}
	}
	staging := map[string]*acc{}

	parsed, withRules := 0, 0
	for rows.Next() {
		var tenant, stack, def string
		if err := rows.Scan(&tenant, &stack, &def); err != nil {
			r.logger.Warn("redact registry rebuild: scan", zap.Error(err))
			return err
		}
		parsed++
		// txcl.Resonator is lenient — parser errors return a partially-
		// populated Resonator; we still read whatever WITH clauses
		// were captured. A nil result is the only thing we skip.
		res, _ := txcl.Resonator(def)
		if res == nil || len(res.With) == 0 {
			continue
		}
		r1 := withList(res.With, "redact")
		o1 := withList(res.With, "omit")
		if len(r1) == 0 && len(o1) == 0 {
			continue
		}
		withRules++
		key := tenant + ":" + stack
		a, ok := staging[key]
		if !ok {
			a = &acc{redact: map[string]struct{}{}, omit: map[string]struct{}{}}
			staging[key] = a
		}
		for _, p := range r1 {
			a.redact[p] = struct{}{}
		}
		for _, p := range o1 {
			a.omit[p] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		r.logger.Warn("redact registry rebuild: rows", zap.Error(err))
		return err
	}

	out := make(map[string]trace.Hints, len(staging))
	for k, a := range staging {
		var h trace.Hints
		for p := range a.omit {
			h.Omit = append(h.Omit, p)
		}
		for p := range a.redact {
			if _, dropped := a.omit[p]; dropped {
				continue // omit wins
			}
			h.Redact = append(h.Redact, p)
		}
		sort.Strings(h.Omit)
		sort.Strings(h.Redact)
		out[k] = h
	}

	r.snap.Store(&out)
	r.logger.Debug("redact registry rebuilt",
		zap.Int("rules_scanned", parsed),
		zap.Int("rules_with_hints", withRules),
		zap.Int("slots", len(out)))
	return nil
}

// withList pulls a comma-separated literal-string value out of the
// rule's WITH map and returns the trimmed, deduplicated path list.
// Non-literal values (PathRef, FunctionCall) are skipped: the
// registry must be deterministic at build time, and those resolve
// only against per-request envelope state.
func withList(with map[string]ast.Value, key string) []string {
	v, ok := with[key]
	if !ok {
		return nil
	}
	raw := ast.LiteralOrNil(v)
	if raw == nil {
		return nil
	}
	switch s := raw.(type) {
	case string:
		return splitPaths(s)
	case []interface{}:
		// Allow `WITH redact = ["a.b", "c.d"]` syntax too — arrays of
		// strings flow as []interface{} from the parser.
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				for _, p := range splitPaths(str) {
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}

func splitPaths(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
