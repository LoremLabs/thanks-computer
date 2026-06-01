// Package sysops loads trusted, chassis-local opstacks into the
// in-memory ops snapshot at startup.
//
// "System" opstacks live under `_`-prefixed tenant slugs (`_sys`,
// `_playground`, …). They are NOT authored through the admin API —
// they ship embedded in the binary (the bundled default) and may be
// overridden/extended from a local directory the operator controls.
// Filesystem control == operator trust, so these bypass the
// tenant-level admin auth path entirely; they are the *second* writer
// of the `ops` table (admin activation is the first), scoped to their
// own `_`-prefixed tenants which the admin API can never create
// (tenants.ReservedSlug).
//
// `_sys` owns the ingress-fallback `boot` stack. Shape mirrors
// continuation.Open: Load() resolves+validates, Apply(db) overlays.
// Apply is idempotent and re-run after every dbcache reload (the
// snapshot is rebuilt from runtime.db and would otherwise drop the
// overlay).
package sysops

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/db"
)

// embedRoot is the directory inside db.SystemOpstacksFS that contains
// the embedded `OPS/` tree (opstacks/OPS/_sys/boot/...).
const embedRoot = "opstacks"

// Config selects the optional on-disk source. Dir is a workspace root
// that contains an `OPS/` tree; the loader reads only its `_`-prefixed
// system stacks (OPS/_sys/...). Dir == "" means embedded-only (the
// production default — `txco serve` in production). Mirrors
// continuation.StoreConfig in spirit.
type Config struct {
	Dir string
}

// Loader is a resolved, validated system bundle ready to overlay onto
// an ops snapshot. ops carry the full "_slug/stack" name (e.g.
// "_sys/boot"); Apply splits that into (tenant, stack). Construct via
// Load; apply via Apply.
type Loader struct {
	ops []bundle.Op
}

// systemTenantID maps a system slug to a stable tenant_id. `_sys` is
// the migration-seeded constant; any other `_x` gets `tnt_x` (the
// loader is the trusted creator of these rows in the snapshot).
func systemTenantID(slug string) string {
	if slug == tenants.SystemTenantSlug {
		return tenants.SystemTenantID
	}
	return "tnt_" + strings.TrimPrefix(slug, "_")
}

// Load resolves the embedded default, overlays the optional on-disk
// bundle (merged per stack/scope/name — extend or override individual
// rules), validates every rule's txcl, and returns a Loader. A parse
// error anywhere is fatal — a broken system bundle must fail the
// chassis at startup, not silently misroute.
func Load(cfg Config) (*Loader, error) {
	// 1. Embedded default: opstacks/OPS/_sys/boot/... → "_sys/boot".
	merged, err := bundle.WalkSystemFS(db.SystemOpstacksFS, embedRoot)
	if err != nil {
		return nil, fmt.Errorf("sysops: walk embedded: %w", err)
	}

	// 2. Optional on-disk overlay from the workspace's OPS/ tree
	// (cfg.Dir is the dir containing OPS/). Merged at
	// (stack, scope, name) granularity onto the embedded base: a
	// same-key file overrides the default, a new key extends it. So
	// dropping `OPS/_sys/boot/10/limit.txcl` adds a rule without
	// copying the shipped boot/0 + boot/20, and editing
	// `OPS/_sys/boot/0/detect.txcl` overrides just that rule.
	if cfg.Dir != "" {
		disk, derr := bundle.WalkSystemFS(os.DirFS(cfg.Dir), ".")
		if derr != nil {
			return nil, fmt.Errorf("sysops: walk %s: %w", cfg.Dir, derr)
		}
		merged = mergeOps(merged, disk)
	}

	// 3. Validate every rule. Fail fast on a broken system rule. Same
	// loud-at-load contract as a txcl parse error, so a malformed
	// operator OPS/_sys overlay is rejected here (and the watcher keeps
	// the previous good bundle); shipped defaults all conform.
	for _, op := range merged {
		if err := opname.ValidStack(op.Stack); err != nil {
			return nil, fmt.Errorf("sysops: %w", err)
		}
		if op.Name != "" && !strings.HasPrefix(op.Name, "_legacy_") {
			if err := opname.Valid(op.Name); err != nil {
				return nil, fmt.Errorf("sysops: %s/%d: %w", op.Stack, op.Scope, err)
			}
		}
		if _, perr := txcl.Resonator(op.Txcl); perr != nil {
			return nil, fmt.Errorf("sysops: invalid txcl (%s/%d/%s): %w",
				op.Stack, op.Scope, op.Name, perr)
		}
	}

	return &Loader{ops: merged}, nil
}

// bySlug groups merged ops by system tenant slug, stripping the
// "_slug/" prefix so op.Stack is the bare stack name the data plane
// resolves (e.g. "_sys/boot" → slug "_sys", stack "boot").
func (l *Loader) bySlug() map[string][]bundle.Op {
	out := map[string][]bundle.Op{}
	for _, op := range l.ops {
		slug, rest, ok := bundle.SystemSegment(op.Stack)
		if !ok {
			continue // WalkSystemFS only yields system stacks; defensive.
		}
		o := op
		o.Stack = rest
		out[slug] = append(out[slug], o)
	}
	return out
}

// Apply overlays the system bundle onto db (the in-memory ops
// snapshot). Idempotent: for each system tenant it upserts the tenants
// row, deletes all of that tenant's ops, then re-inserts. Safe to call
// after every dbcache reload.
func (l *Loader) Apply(db *sql.DB) error {
	now := time.Now().UTC().Format(time.RFC3339)
	bySlug := l.bySlug()
	for _, slug := range sortedKeys(bySlug) {
		tid := systemTenantID(slug)

		if _, err := db.Exec(
			`INSERT OR IGNORE INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, ?)`,
			tid, slug, "System ("+slug+")", now); err != nil {
			return fmt.Errorf("sysops: upsert tenant %s: %w", slug, err)
		}
		if _, err := db.Exec(`DELETE FROM ops WHERE tenant_id = ?`, tid); err != nil {
			return fmt.Errorf("sysops: clear ops %s: %w", slug, err)
		}
		for _, op := range bySlug[slug] {
			if _, err := db.Exec(
				`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				tid, op.Stack, op.Scope, op.Name, op.Txcl, op.MockReq, op.MockRes); err != nil {
				return fmt.Errorf("sysops: insert op %s (%s/%d/%s): %w",
					slug, op.Stack, op.Scope, op.Name, err)
			}
		}
	}
	return nil
}

// BootOpCount reports how many `_sys` `boot` ops resolved. Zero means
// the ingress-fallback pipeline is missing — the caller logs loudly and
// the chassis falls back to the synthetic 404 safety net.
func (l *Loader) BootOpCount() int {
	n := 0
	for _, op := range l.ops {
		if slug, rest, ok := bundle.SystemSegment(op.Stack); ok &&
			slug == tenants.SystemTenantSlug && rest == "boot" {
			n++
		}
	}
	return n
}

// mergeOps overlays `over` onto `base`, keyed by (stack, scope, name):
// a matching key is replaced, a new key appended. Result is sorted by
// (stack, scope, name) for a stable Apply order.
func mergeOps(base, over []bundle.Op) []bundle.Op {
	key := func(o bundle.Op) string {
		return fmt.Sprintf("%s\x00%d\x00%s", o.Stack, o.Scope, o.Name)
	}
	idx := map[string]int{}
	merged := make([]bundle.Op, 0, len(base)+len(over))
	for _, o := range base {
		idx[key(o)] = len(merged)
		merged = append(merged, o)
	}
	for _, o := range over {
		if i, ok := idx[key(o)]; ok {
			merged[i] = o
			continue
		}
		idx[key(o)] = len(merged)
		merged = append(merged, o)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Stack != merged[j].Stack {
			return merged[i].Stack < merged[j].Stack
		}
		if merged[i].Scope != merged[j].Scope {
			return merged[i].Scope < merged[j].Scope
		}
		return merged[i].Name < merged[j].Name
	})
	return merged
}

func sortedKeys(m map[string][]bundle.Op) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
