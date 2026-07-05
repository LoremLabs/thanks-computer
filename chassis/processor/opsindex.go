package processor

// opsindex caches the CURRENT dbcache snapshot's ops — scope-sorted and
// with each rule's txcl parsed ONCE — so the per-scope hot path
// (OpsForStage, its getnext look-ahead, and advanceAfterScope's
// corrected re-query) stops paying a SQL round-trip per call and a
// txcl parse per op per request. The snapshot connection is pinned to
// MaxOpenConns(1) (each go-sqlite3 :memory: conn is a private DB), so
// every uncached lookup on the node serializes through ONE connection;
// a request that walks a sparse stack to its fallback scope was issuing
// ~200 queries and re-parsing every rule it passed. See the dbcache.New
// rationale — "serializing through one connection costs nothing
// visible" — which stops holding at hot-path query volumes.
//
// Validity contract: an index is usable only for the exact snapshot
// handle it was built from AND the DbCache generation at build time.
//   - Reload() swaps dbc.Db to a fresh *sql.DB → pointer mismatch →
//     lazy rebuild. Requests still pinned to the superseded handle
//     (ctxKeyOpstackSnap, ≤30s grace) fall back to the SQL path.
//   - The dev-only system-opstacks hot-reload mutates the LIVE snapshot
//     in place; it calls dbc.BumpGen() so gen mismatch forces a rebuild.
//   - Continuation resumes pin a throwaway frozen-opstack DB; its
//     pointer never matches, so resumes keep querying their frozen
//     snapshot directly — the immutability guarantee is untouched.
//
// Sharing contract: cached templates carry a parsed *resonator.Resonator
// shared across requests and goroutines. That is safe by design —
// WhenMatches uses a value receiver and never writes back into the
// shared When tree (see resonator.go), and the processor only reads
// Resonator fields after parse. Everything per-request (Input, Output,
// Meta, OpID, Secrets) lives on the per-request copy that atOrAbove
// returns, built with operation.Copy() (fresh OpID, zero secret bag).

import (
	"context"
	"database/sql"
	"sort"
	"sync"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// opsIndex is one snapshot's lazily-populated ladder cache.
type opsIndex struct {
	snap *sql.DB // the dbcache snapshot handle this index was built from
	gen  uint64  // DbCache generation at build time

	mu      sync.RWMutex
	ladders map[string]*stackLadder // key: tenant + "\x00" + stack pattern
}

// stackLadder holds one (tenant, stack)'s ops grouped by scope, with
// scopes sorted for the floor lookup. Ops are TEMPLATES: shared parsed
// Resonator, no per-request state. A rule whose txcl fails to parse is
// cached with Resonator == nil so ResonatingOps re-parses it per
// request and fails exactly as the uncached path does.
type stackLadder struct {
	scopes []int
	ops    map[int][]operation.Operation
}

// currentOpsIndex returns the index for the request's pinned snapshot,
// building/swapping a fresh one when the snapshot or generation moved.
// Returns nil when the pinned snapshot is not the CURRENT dbcache
// mirror (superseded handle mid-reload, or a continuation's frozen
// opstack DB) — callers then use the SQL path against their pinned
// handle, which is exactly the pre-index behavior.
func (pu *Unit) currentOpsIndex(snap *sql.DB) *opsIndex {
	if pu.Dbc == nil || snap == nil {
		return nil
	}
	if snap != pu.Dbc.Snapshot() {
		return nil
	}
	gen := pu.Dbc.Gen()
	idx := pu.opsIdx.Load()
	if idx != nil && idx.snap == snap && idx.gen == gen {
		return idx
	}
	fresh := &opsIndex{snap: snap, gen: gen, ladders: make(map[string]*stackLadder)}
	if pu.opsIdx.CompareAndSwap(idx, fresh) {
		return fresh
	}
	// Lost a swap race; use the winner if it matches, else skip the
	// index for this call (next call re-evaluates).
	if idx = pu.opsIdx.Load(); idx != nil && idx.snap == snap && idx.gen == gen {
		return idx
	}
	return nil
}

// opsForStageIndexed resolves the same contract as the SQL loop in
// OpsForStage — floor lookup per stack prefix, peeling to stackParent
// only when the floor result at that prefix is empty — entirely from
// the ladder cache. Wildcard patterns never reach here (OpsForStage
// gates on them); they keep the SQL path.
func (idx *opsIndex) opsForStage(ctx context.Context, pu *Unit, tenant, stack string, scope int) ([]operation.Operation, error) {
	prefix := stack
	for {
		ladder, err := idx.ladderFor(ctx, pu, tenant, prefix)
		if err != nil {
			return make([]operation.Operation, 0), err
		}
		if rows := ladder.atOrAbove(scope); len(rows) > 0 {
			if prefix != stack {
				pu.Logger.Debug("ops-for-stage fallback", zap.String("stack", stack), zap.String("matched", prefix), zap.Int("scope", scope))
			}
			return rows, nil
		}
		next, ok := stackParent(prefix)
		if !ok {
			return make([]operation.Operation, 0), nil
		}
		prefix = next
	}
}

// ladderFor returns the cached ladder for (tenant, stack), building it
// on first miss. The build runs OUTSIDE the write lock (it does SQL +
// parses); a lost insert race just discards the duplicate build.
func (idx *opsIndex) ladderFor(ctx context.Context, pu *Unit, tenant, stack string) (*stackLadder, error) {
	key := tenant + "\x00" + stack

	idx.mu.RLock()
	l := idx.ladders[key]
	idx.mu.RUnlock()
	if l != nil {
		return l, nil
	}

	l, err := buildStackLadder(ctx, idx.snap, tenant, stack, pu.Logger)
	if err != nil {
		return nil, err
	}

	idx.mu.Lock()
	if won := idx.ladders[key]; won != nil {
		l = won
	} else {
		idx.ladders[key] = l
	}
	idx.mu.Unlock()
	return l, nil
}

// buildStackLadder loads every op for (tenant, stack) in one query and
// parses each txcl once. The WHERE clause mirrors lookupOpsExact
// byte-for-byte (LIKE + tenantPredicate) minus the MIN(scope) floor —
// the floor becomes an in-memory binary search — so tenant isolation
// and LIKE's case-insensitive match semantics are identical to the SQL
// path. ORDER BY scope, rowid preserves the SQL path's within-scope
// row order.
func buildStackLadder(ctx context.Context, db *sql.DB, tenant, stack string, logger *zap.Logger) (*stackLadder, error) {
	tenantPred, tenantArgs := tenantPredicate(tenant)
	query := `SELECT stack, scope, name, txcl, mock_res FROM ops WHERE stack LIKE ?` + tenantPred + ` ORDER BY scope, rowid`
	args := append([]any{stack}, tenantArgs...)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	l := &stackLadder{ops: make(map[int][]operation.Operation)}
	for rows.Next() {
		op := operation.New()
		if err := rows.Scan(&op.Stack, &op.Scope, &op.Name, &op.Txcl, &op.MockRes); err != nil {
			return nil, err
		}
		// Parse once. On failure keep Resonator nil: ResonatingOps
		// re-parses per request and surfaces the same error the
		// uncached path would (a broken rule stays exactly as broken).
		if res, perr := txcl.Resonator(op.Txcl); perr == nil && res != nil {
			op.Resonator = res
		} else if perr != nil {
			logger.Debug("opsindex parse (deferred to request path)",
				zap.String("stack", op.Stack), zap.Int("scope", op.Scope),
				zap.String("name", op.Name), zap.String("err", perr.Error()))
		}
		if _, seen := l.ops[op.Scope]; !seen {
			l.scopes = append(l.scopes, op.Scope)
		}
		l.ops[op.Scope] = append(l.ops[op.Scope], *op)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Ints(l.scopes)
	return l, nil
}

// atOrAbove returns per-request copies of the ops at the lowest scope
// ≥ floor — the in-memory equivalent of lookupOpsExact's MIN(scope)
// subquery. Copies get a fresh OpID and their own zero secret bag;
// the parsed Resonator pointer is shared (read-only by contract).
func (l *stackLadder) atOrAbove(floor int) []operation.Operation {
	i := sort.SearchInts(l.scopes, floor)
	if i >= len(l.scopes) {
		return nil
	}
	templates := l.ops[l.scopes[i]]
	out := make([]operation.Operation, 0, len(templates))
	for t := range templates {
		out = append(out, *templates[t].Copy())
	}
	return out
}
