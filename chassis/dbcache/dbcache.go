package dbcache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bep/debounce"
	sqlite3 "github.com/mattn/go-sqlite3" // sqlite driver + the online Backup API
	"github.com/radovskyb/watcher"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

/*

creates an in-memory version of an on-disk db
updates memory on change

new
create in memory db from file
setup a file watcher
on change, reload memory db
mutex lock
*/

// supersededDBCloseGrace is how long a just-replaced in-memory mirror
// is kept open after a Reload swap before it's closed, so any reader
// that captured it via Snapshot() immediately before the swap finishes
// first. It only needs to exceed the longest in-memory snapshot query
// (the ingress resolver caps its lookup at 250ms; processor/admin
// snapshot reads are comparably short). 30s is a ~120x margin and also
// bounds how many superseded mirrors can be alive at once under bursty
// (debounced) reloads.
const supersededDBCloseGrace = 30 * time.Second

// MirrorLoader (re)builds the in-memory read mirror: given a freshly-opened,
// empty :memory: SQLite handle `dst` and the chassis's authoritative runtime
// store `src`, it populates `dst` with the rows the hot read path serves.
//
// The mirror is ALWAYS SQLite (every reader assumes a :memory: SQLite
// snapshot — see chassis/processor and chassis/server/ingress); only the
// SOURCE varies. For a file: SQLite runtime the built-in "sqlite" loader does
// a page-level online backup. For a postgres:// runtime the cloud overlay
// registers a "postgres" loader that applies the SQLite runtime schema to
// `dst` and copies the hot tables out of Postgres — keeping every Postgres
// line out of open-core (open-core compiles no Postgres driver).
type MirrorLoader func(ctx context.Context, dst, src *sql.DB) error

var (
	loaderMu sync.Mutex
	loaders  = map[string]MirrorLoader{}
)

// RegisterLoader records a mirror loader under name (e.g. "postgres").
// Mirrors the feed/scheduled/artifact factory-registry pattern: the cloud
// overlay calls this from its init(), blank-imported by the cloud main, so
// open-core never references the overlay symbol. The built-in "sqlite"
// loader self-registers below.
func RegisterLoader(name string, l MirrorLoader) {
	loaderMu.Lock()
	defer loaderMu.Unlock()
	loaders[name] = l
}

func lookupLoader(name string) (MirrorLoader, bool) {
	loaderMu.Lock()
	defer loaderMu.Unlock()
	l, ok := loaders[name]
	return l, ok
}

func init() { RegisterLoader("sqlite", sqliteBackupLoad) }

// sqliteBackupLoad is the built-in mirror loader for a SQLite runtime: a
// binary, page-level online backup of `src` into the fresh :memory: `dst`
// over a single borrowed source connection (NOT a SQL-text dump+replay,
// which was O(n²) in go-sqlite3's no-args Exec — ~35s for a 1MB DB; the
// backup is O(db size), ~ms). Running over the existing Source *connection*
// keeps the WAL .db-shm race the old dump avoided still avoided.
func sqliteBackupLoad(ctx context.Context, dst, src *sql.DB) error {
	srcConn, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer srcConn.Close()
	destConn, err := dst.Conn(ctx)
	if err != nil {
		return err
	}
	// Closing returns the pinned connection to dst's 1-conn pool (it does
	// NOT destroy the :memory: db, which lives on that connection), so every
	// subsequent mirror read reuses exactly this copy.
	defer destConn.Close()
	return destConn.Raw(func(dc any) error {
		return srcConn.Raw(func(sc any) error {
			bk, err := dc.(*sqlite3.SQLiteConn).Backup("main", sc.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return err
			}
			// Step(-1) copies all remaining pages in one shot; it restarts
			// internally if the source is written mid-copy, returning
			// done=false until it settles. Bounded retry guards a
			// pathological sustained-write source.
			for tries := 0; ; tries++ {
				done, serr := bk.Step(-1)
				if serr != nil {
					_ = bk.Finish()
					return serr
				}
				if done {
					break
				}
				if tries >= 500 {
					_ = bk.Finish()
					return errors.New("dbcache backup did not converge (source under sustained write)")
				}
				time.Sleep(5 * time.Millisecond)
			}
			return bk.Finish()
		})
	})
}

// DbCache structure
type DbCache struct {
	Conf   config.Config
	Ctx    context.Context
	Db     *sql.DB
	Logger *zap.Logger
	Mu     sync.Mutex

	// reloadMu serializes Reload() end-to-end. It is deliberately SEPARATE
	// from Mu (which Snapshot() takes): the multi-second dump+replay runs
	// under reloadMu only, so readers are never blocked by it — just the
	// brief swap+overlay critical section takes Mu. See Reload for why the
	// serialization is still required for correctness.
	reloadMu sync.Mutex

	// Source is the chassis's runtime *sql.DB handle — the live,
	// configured connection pool that the rest of the chassis writes
	// through. Reload() copies from this handle (via the SQLite online
	// backup API over a borrowed Source connection) rather than opening
	// its own connection to the file: in WAL mode a second uncoordinated
	// connection races the main one's .db-shm state and fails with
	// "database is locked" on first boot. Going through the same pool
	// means there is no second connection to race.
	Source *sql.DB

	// OnReload, if set, runs at the end of every Reload against the
	// freshly-built in-memory DB while the cache lock is still held —
	// so no request ever observes the snapshot before the overlay is
	// applied. Used by chassis/sysops to re-apply the trusted system
	// opstacks (Reload rebuilds :memory: from the runtime.db dump and
	// would otherwise drop them). A hook error is logged, not fatal:
	// the previous snapshot stays live rather than going dark.
	OnReload func(*sql.DB) error

	// gen counts IN-PLACE mutations of the live snapshot. Reload never
	// touches it — a reload swaps Db to a fresh handle, and derived
	// caches (the processor's ops index) already invalidate on pointer
	// identity. The only in-place writer is the dev-only system-
	// opstacks hot-reload (app.go, --system-opstacks-watch), which must
	// call BumpGen() after applying so pointer-identical caches rebuild.
	gen atomic.Uint64

	// loaderName selects the registered MirrorLoader Reload() uses to
	// (re)build the :memory: mirror from Source — "sqlite" (built-in
	// online backup) for a file: runtime, "postgres" (overlay-registered)
	// for a postgres:// runtime. Resolved once in New from the runtime DSN.
	loaderName string
}

// Gen returns the in-place mutation generation of the live snapshot.
// Pair with the Db pointer: a derived cache is fresh only while BOTH
// the handle and the generation it captured are unchanged.
func (dbc *DbCache) Gen() uint64 {
	return dbc.gen.Load()
}

// BumpGen invalidates derived caches after mutating the LIVE snapshot
// in place (dev-only system-opstacks hot-reload). Not needed around
// Reload — the handle swap is the invalidation there.
func (dbc *DbCache) BumpGen() {
	dbc.gen.Add(1)
}

// New Create a new in-memory DB cache.
//
// `source` is the chassis's runtime *sql.DB — the live connection
// pool that Reload() reads from. Required: passing nil here would
// fail at the first Reload. See DbCache.Source for the WAL rationale.
//
// Critical: go-sqlite3 gives each *connection* in the pool its own
// `:memory:` database. So if connection #1 loads the schema and a later
// concurrent query opens connection #2, that second connection sees an
// empty DB and the query fails with "no such table: ops".
//
// To avoid that, pin the in-memory cache to a single connection. Reads
// are fast and the cache is read-only on the hot path; serializing
// through one connection costs nothing visible but guarantees
// consistency under concurrent load.
func New(conf config.Config, logger *zap.Logger, ctx context.Context, source *sql.DB) (*DbCache, error) {

	var dbc = &DbCache{}
	dbc.Mu = sync.Mutex{}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	// Pick the mirror loader from the runtime DSN. A file: runtime uses the
	// built-in "sqlite" online-backup loader; a postgres:// runtime needs
	// the cloud overlay's "postgres" loader (blank-imported into the cloud
	// binary). Fail closed here — an open-core binary pointed at a Postgres
	// runtime has no loader and must not boot into a panic at first Reload.
	loaderName := "sqlite"
	if registry.DialectForDSN(conf.DbRuntimeDsn) == registry.Postgres {
		loaderName = "postgres"
	}
	if _, ok := lookupLoader(loaderName); !ok {
		return nil, fmt.Errorf("dbcache: no %q mirror loader registered (a postgres:// runtime needs the cloud overlay's pgmirror; open-core serves only file: runtimes)", loaderName)
	}

	dbc.Conf = conf
	dbc.Db = db
	dbc.Source = source
	dbc.Logger = logger
	dbc.Ctx = ctx
	dbc.loaderName = loaderName

	return dbc, nil
}

// Snapshot returns the current mirror handle under the lock. Callers
// that live longer than one reload (e.g. the ingress resolver) MUST
// call this per use rather than capturing dbc.Db once: Reload() swaps
// dbc.Db to a fresh *sql.DB, so a captured handle goes stale and never
// sees rows written after it was captured. The returned *sql.DB stays
// valid for the caller's immediate query (the old handle isn't closed
// on swap); at worst it's one reload-cycle stale, which is the same
// guarantee the rest of the read path has.
func (dbc *DbCache) Snapshot() *sql.DB {
	dbc.Mu.Lock()
	defer dbc.Mu.Unlock()
	return dbc.Db
}

// Reload a db file into Memory. Sources from the runtime DB only — the
// auth DB (when present) is owned exclusively by the admin role and is
// never mirrored into the read cache.
//
// Concurrency: the dump+replay+swap runs under reloadMu (serializing
// reloads end-to-end) while only the swap+overlay touches Mu — so the
// expensive dump never blocks Snapshot() readers. Serialization is still
// required: two concurrent writers each calling Reload after their
// commits would otherwise dump in parallel (each capturing a snapshot
// before some of the OTHER writer's commits land), and the reload that
// finishes its dump LAST would publish a STALE snapshot, silently
// clobbering durably-committed rows from the mirror. Symptom: a row on
// disk but missing from the resolver until the next (unrelated) reload
// happens to dump after every commit settled. reloadMu held across
// dump+swap keeps the second reload's dump strictly after the first's
// swap. (This costs serial reloads under write bursts, but the dump was
// the dominant cost regardless — concurrent dumps were a parallelism
// mirage.)
func (dbc *DbCache) Reload() error {
	// Serialize reloads under reloadMu — NOT Mu. The expensive dump+replay
	// below ran under Mu (the lock Snapshot() takes), so a large stack
	// activation stalled every reader (healthz, ingress resolver, DNS) for
	// the whole multi-second dump → edge 502s / "web response timeout".
	// reloadMu keeps reloads strictly serial without blocking readers; only
	// the brief swap+overlay critical section below takes Mu.
	dbc.reloadMu.Lock()
	defer dbc.reloadMu.Unlock()

	// Build the fresh mirror by handing an empty :memory: SQLite handle to the
	// configured MirrorLoader (built-in "sqlite" online backup for a file:
	// runtime; the overlay's "postgres" logical copy for a shared-Postgres
	// runtime). The loader runs entirely under reloadMu — off the Snapshot()
	// path — so a slow build never blocks readers. On loader failure the old
	// mirror stays live (below), which is also the Postgres availability
	// buffer: a Neon blip fails the reload but keeps serving the last snapshot.
	dbNew, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		dbc.Logger.Warn("reload cachedb open err", zap.String("err", err.Error()))
		return err
	}
	// Pin to one connection: each go-sqlite3 :memory: connection is its OWN db,
	// so the mirror must live on a single pinned connection (see New()).
	dbNew.SetMaxOpenConns(1)

	bctx := dbc.Ctx
	if bctx == nil {
		bctx = context.Background()
	}
	loader, ok := lookupLoader(dbc.loaderName)
	if !ok {
		// Guarded at New(); defensive here so a mis-set loaderName surfaces
		// loudly instead of a nil-call panic.
		_ = dbNew.Close()
		return fmt.Errorf("dbcache: mirror loader %q not registered", dbc.loaderName)
	}
	if berr := loader(bctx, dbNew, dbc.Source); berr != nil {
		dbc.Logger.Warn("reload cachedb load err",
			zap.String("loader", dbc.loaderName), zap.String("err", berr.Error()))
		_ = dbNew.Close()
		return berr
	}
	// Brief critical section under Mu — the only part Snapshot() readers can
	// wait on. Publish the mirror and run the OnReload overlay here, in the
	// SAME order as before, so no reader observes dbNew before sysops has
	// re-applied the trusted opstacks into it. Milliseconds (a pointer swap +
	// the bounded overlay), not the multi-second dump above.
	dbc.Mu.Lock()
	old := dbc.Db
	dbc.Db = dbNew
	if dbc.OnReload != nil {
		if herr := dbc.OnReload(dbNew); herr != nil {
			// Non-fatal: log loudly and keep serving with the snapshot
			// as-is. A broken overlay must not take the chassis dark.
			dbc.Logger.Error("dbcache OnReload hook failed",
				zap.String("err", herr.Error()))
		}
	}
	dbc.Mu.Unlock()

	// Close the superseded mirror after a grace window that dwarfs any
	// in-flight Snapshot() query. The PREVIOUS handle must be closed or every
	// Reload leaks a whole :memory: SQLite DB (database/sql does not close it
	// on GC); it can't be closed synchronously because a reader that captured
	// it just before the swap may still be mid-query. The grace need only
	// exceed the longest in-memory snapshot query (ingress resolver caps at
	// 250ms; others are similarly short), or close immediately on shutdown.
	if old != nil && old != dbNew {
		go func(prev *sql.DB) {
			t := time.NewTimer(supersededDBCloseGrace)
			defer t.Stop()
			var ctxDone <-chan struct{}
			if dbc.Ctx != nil {
				ctxDone = dbc.Ctx.Done()
			}
			select {
			case <-t.C:
			case <-ctxDone: // nil channel never fires; timer still does
			}
			if cerr := prev.Close(); cerr != nil {
				dbc.Logger.Debug("closing superseded dbcache mirror",
					zap.String("err", cerr.Error()))
			}
		}(old)
	}

	dbc.Logger.Info("reload cachedb complete")

	return nil
}

// Watch a db file for changes
func (dbc *DbCache) Watch() {
	w := watcher.New()

	// SetMaxEvents to 1 to allow at most 1 event's to be received
	// on the Event channel per watching cycle.
	//
	// If SetMaxEvents is not set, the default is to send all events.
	w.SetMaxEvents(1)

	// Only notify rename and move events.
	// w.FilterOps(watcher.Rename, watcher.Move)

	// Only files that match the regular expression during file listings
	// will be watched.
	r := regexp.MustCompile(`\.db$`)
	w.AddFilterHook(watcher.RegexFilterHook(r, false))

	// Only the runtime DB is mirrored into the read cache (see Reload's
	// doc). The watcher's `\.db$` filter also matches auth-*.db, and a
	// child write bumps the parent dir's mtime → a `DIRECTORY "db"`
	// event. Reloading runtime on either is pure waste and floods the
	// log (an idle admin-ui tab touching browser_sessions in auth-*.db
	// triggers it every poll). Gate on the runtime DB basename so only
	// real runtime writes drive a reload. Synchronous Reload() (admin
	// activation / OnReload) never goes through here and is unaffected.
	runtimeBase := filepath.Base(strings.TrimPrefix(dbc.Conf.DbRuntimeDsn, "file:"))

	go func() {
		// this may miss some updates if they come within the same second?
		debounced := debounce.New(1000 * time.Millisecond)

		for {
			select {
			case event := <-w.Event:
				if filepath.Base(event.Path) != runtimeBase {
					continue
				}
				dbc.Logger.Info("watch event", zap.String("watchEvent", event.String()))
				debounced(func() {
					go func() {
						err := dbc.Reload()
						if err != nil {
							dbc.Logger.Info("dbc reload error", zap.String("err", err.Error()))
						}
					}()
				})
			case err := <-w.Error:
				dbc.Logger.Info("watch error", zap.String("err", err.Error()))
			case <-w.Closed:
				return
			case <-dbc.Ctx.Done():
				dbc.Logger.Info("watch context closed")
				w.Close()
			}
		}
	}()

	// Watch this folder for changes.
	dbRoot := dbc.Conf.DbRoot
	if err := w.Add(dbRoot); err != nil {
		dbc.Logger.Warn("watch unable to open dbroot to read", zap.String("err", err.Error()))
		return
	}

	// Start the watching process - it'll check for changes every 100ms.
	if err := w.Start(time.Millisecond * 2000); err != nil {
		dbc.Logger.Warn("watch unable to start", zap.String("err", err.Error()))
	}

	dbc.Logger.Info("watch shutting down")
}
