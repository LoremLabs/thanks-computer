package dbcache

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	_ "github.com/mattn/go-sqlite3" // add sqlite support to database
	"github.com/radovskyb/watcher"
	"github.com/schollz/sqlite3dump"
	"go.uber.org/zap"

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

// DbCache structure
type DbCache struct {
	Conf   config.Config
	Ctx    context.Context
	Db     *sql.DB
	Logger *zap.Logger
	Mu     sync.Mutex

	// OnReload, if set, runs at the end of every Reload against the
	// freshly-built in-memory DB while the cache lock is still held —
	// so no request ever observes the snapshot before the overlay is
	// applied. Used by chassis/sysops to re-apply the trusted system
	// opstacks (Reload rebuilds :memory: from the runtime.db dump and
	// would otherwise drop them). A hook error is logged, not fatal:
	// the previous snapshot stays live rather than going dark.
	OnReload func(*sql.DB) error
}

// New Create a new in-memory DB cache.
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
func New(conf config.Config, logger *zap.Logger, ctx context.Context) (*DbCache, error) {

	var dbc = &DbCache{}
	dbc.Mu = sync.Mutex{}

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	dbc.Conf = conf
	dbc.Db = db
	dbc.Logger = logger
	dbc.Ctx = ctx

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
func (dbc *DbCache) Reload() error {
	var b bytes.Buffer
	out := bufio.NewWriter(&b)

	dbfile := strings.TrimPrefix(dbc.Conf.DbRuntimeDsn, "file:")
	if err := sqlite3dump.Dump(dbfile, out); err != nil {
		dbc.Logger.Warn("reload cachedb err", zap.String("err", err.Error()))
		return err
	}
	_ = out.Flush()

	dbc.Mu.Lock()
	defer dbc.Mu.Unlock()
	dbNew, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		dbc.Logger.Warn("sql.Open err", zap.String("err", err.Error()))
		return err
	}
	// Pin to one connection. See the rationale in New(): each go-sqlite3
	// connection gets its own `:memory:` DB, so without this any second
	// connection in the pool would be empty.
	dbNew.SetMaxOpenConns(1)
	dbc.Logger.Debug("dbcache reload", zap.String("file", dbfile), zap.Int("dump_bytes", b.Len()))
	_, err = dbNew.Exec(b.String())
	if err != nil {
		dbc.Logger.Warn("reload cachedb open db err", zap.String("err", err.Error()))
		return err
	}

	// Atomic publish of the freshly-built mirror. The PREVIOUS handle
	// must be closed or every Reload leaks a whole :memory: SQLite DB
	// (database/sql does not close it on GC). It can't be closed
	// synchronously here: a reader that captured it via Snapshot()
	// just before this swap may still be mid-query on it. Close it
	// after a grace window that dwarfs any in-memory snapshot query
	// (the ingress resolver's is a 250ms ctx; others are similarly
	// short), or immediately on shutdown. Bounds live mirrors to the
	// current one plus at most a couple in their grace window.
	old := dbc.Db
	dbc.Db = dbNew
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

	if dbc.OnReload != nil {
		if herr := dbc.OnReload(dbNew); herr != nil {
			// Non-fatal: log loudly and keep serving with the snapshot
			// as-is. A broken overlay must not take the chassis dark.
			dbc.Logger.Error("dbcache OnReload hook failed",
				zap.String("err", herr.Error()))
		}
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
