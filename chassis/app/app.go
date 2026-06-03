// Package app is the chassis entrypoint, factored out of cmd/txco so a
// downstream build (e.g. an overlay that blank-imports extra store
// backends) can reuse the exact boot orchestration instead of forking
// main. cmd/txco/main.go is now a thin shim that calls Run; behavior is
// byte-for-byte unchanged.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abronan/valkeyrie"
	"github.com/abronan/valkeyrie/store"
	"github.com/abronan/valkeyrie/store/boltdb"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/artifact"
	_ "github.com/loremlabs/thanks-computer/chassis/artifact/filestore" // registers the "file" backend
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/cli"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/logging"
	"github.com/loremlabs/thanks-computer/chassis/repl"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/server"
	"github.com/loremlabs/thanks-computer/chassis/snapshot"
	"github.com/loremlabs/thanks-computer/chassis/sysops"
	dbschemas "github.com/loremlabs/thanks-computer/db"
)

// BuildInfo carries the ldflag-injected build identity. The values live
// in the `main` package of whichever binary links this (so
// `-X main.Version` etc. keep working unchanged); the shim passes them
// in here.
type BuildInfo struct {
	Version        string
	CommitId       string
	BuildTimestamp string
	InstallMethod  string
	// Chassis is the embedded open-core pin for a wrapping distribution
	// (e.g. txco-saas stamps the core pseudo-version here while
	// Version/CommitId describe the overlay build). Empty for open-core.
	Chassis string
}

func init() {
	boltdb.Register()
}

// Run is the full chassis boot. It returns a process exit code; the
// caller's main does the single os.Exit so deferred cleanup runs on the
// normal-shutdown path. Fatal config/db errors still exit in place via
// log.Fatalf / logger.Fatal exactly as before.
func Run(bi BuildInfo) int {
	semver := fmt.Sprintf("%s+%s", bi.Version, bi.CommitId) // set via ldflag at build time

	// Surface the ldflag-injected build identity to the CLI surface BEFORE
	// dispatch so `txco --version` / `txco help` (logo line) can read it.
	// cli.BuildInfo is a structural mirror of this type — separate to avoid
	// an import cycle (chassis/app imports chassis/cli).
	cli.Build = cli.BuildInfo{
		Version:        bi.Version,
		CommitId:       bi.CommitId,
		BuildTimestamp: bi.BuildTimestamp,
		InstallMethod:  bi.InstallMethod,
		Chassis:        bi.Chassis,
	}

	// CLI subcommand dispatch (txco init / apply / diff / help). Runs before
	// config.Load so the server flag namespace doesn't collide with the
	// subcommands. Falls through to server boot for `serve` or no subcommand.
	if status, ok := cli.Dispatch(os.Args, os.Stdout, os.Stderr); ok {
		return status
	}

	// If args[1] is "serve", drop it so config.Load's pflag parser doesn't
	// see it as a stray positional arg.
	if len(os.Args) >= 2 && os.Args[1] == "serve" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}

	// load in our runtime config
	conf, err := config.Load()
	if err != nil {
		log.Fatalf("Unknown command line option %v", err)
	}

	// Stamp the build identity onto the config so server surfaces (admin
	// /healthz JSON) can report it. Kept off chassis/cli to avoid an import
	// cycle (files under chassis/cli import chassis/server).
	conf.Build = config.BuildIdentity{
		Version:        bi.Version,
		Commit:         bi.CommitId,
		Chassis:        bi.Chassis,
		BuildTimestamp: bi.BuildTimestamp,
		InstallMethod:  bi.InstallMethod,
	}

	// setup logger
	logger, err := logging.NewForConfig(&conf)
	if err != nil {
		log.Fatalf("Log Setup Error %v", err)
	}

	// server or repl?
	if conf.Repl {
		logger.Info("-repl mode-", zap.String("v", semver), zap.String("build", bi.BuildTimestamp), zap.String("fqdn", conf.Fqdn))
		repl.Start(os.Stdin, os.Stdout)
		return 0
	}
	logger.Info("-starting thanks computer chassis-", zap.String("v", semver), zap.String("build", bi.BuildTimestamp), zap.String("fqdn", conf.Fqdn))

	// Main context. OTel resource attributes (env, host, version) are set on
	// the SDK Resource in metrics.New, so they don't need to ride on context.
	// Cancellable so background watchers (dbcache.Watch) can be torn down on
	// shutdown via the same propagation server.Start uses.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyVersion, semver)

	// Migrations are read from the on-disk DbSchemaDir if it exists, else
	// from the schemas embedded into the binary at build time. This means
	// `txco serve` (and `txco dev`) work from any working directory without
	// needing the repo checkout on disk.
	schemaFS, schemaBase, schemaSrc := selectSchemaSource(conf.DbSchemaDir)
	logger.Info("db schema source", zap.String("source", schemaSrc))

	// Fleet bootstrap (off by default). If a snapshot ref is configured
	// AND the runtime DB is fresh, restore it BEFORE open/migrate using
	// the safe path: verify → restore into a temp DB → migrate that temp
	// DB → sanity-check → atomic rename into place. A non-fresh DB is left
	// untouched (no silent downgrade — use `txco snapshot import --force`).
	// Empty ref ⇒ this whole block is skipped and boot is byte-for-byte
	// unchanged from single-node.
	if conf.SnapshotBootstrapRef != "" {
		runtimeDBPath := strings.TrimPrefix(conf.DbRuntimeDsn, "file:")
		if !snapshot.IsFresh(runtimeDBPath) {
			logger.Info("snapshot bootstrap skipped — runtime DB not fresh (use `txco snapshot import --force` to replace)",
				zap.String("ref", conf.SnapshotBootstrapRef))
		} else {
			astore, aerr := artifact.Open(conf.ArtifactStore, artifact.StoreConfig{
				FileDir: conf.ArtifactStoreFileDir,
			})
			if aerr != nil {
				logger.Fatal("snapshot bootstrap: artifact store", zap.String("err", aerr.Error()))
			}
			data, manBytes, gerr := astore.Get(ctx, conf.SnapshotBootstrapRef)
			if gerr != nil {
				logger.Fatal("snapshot bootstrap: fetch artifact",
					zap.String("ref", conf.SnapshotBootstrapRef), zap.String("err", gerr.Error()))
			}
			var man snapshot.Manifest
			if jerr := json.Unmarshal(manBytes, &man); jerr != nil {
				logger.Fatal("snapshot bootstrap: bad manifest",
					zap.String("ref", conf.SnapshotBootstrapRef), zap.String("err", jerr.Error()))
			}
			migrateFn := func(dbPath string) error {
				mdb := openSQLiteOrDie(logger, "file:"+dbPath, "runtime-bootstrap")
				defer mdb.Close()
				applyMigrationsOrDie(ctx, logger, mdb, registry.SQLite, schemaFS,
					path.Join(schemaBase, "runtime"), "txco-db-changeset-runtime", "runtime")
				return nil
			}
			if berr := snapshot.Bootstrap(data, man, runtimeDBPath, migrateFn, false); berr != nil {
				logger.Fatal("snapshot bootstrap failed",
					zap.String("ref", conf.SnapshotBootstrapRef), zap.String("err", berr.Error()))
			}
			logger.Info("snapshot bootstrap applied",
				zap.String("ref", conf.SnapshotBootstrapRef),
				zap.Uint64("control_version", man.ControlVersion),
				zap.String("db_migration_version", man.DBMigrationVersion))
		}
	}

	// runtime.db is opened on every chassis. It carries the content the
	// runtime reads from (via dbcache) plus the tenants table for
	// future hostname routing on the data plane.
	runtimeDB := openSQLiteOrDie(logger, conf.DbRuntimeDsn, "runtime")
	defer runtimeDB.Close()
	// runtime.db is always SQLite (scope: only auth moves to Postgres
	// for an HA control plane; runtime-DB HA is the architecture's
	// separately-deferred decision).
	applyMigrationsOrDie(ctx, logger, runtimeDB, registry.SQLite, schemaFS,
		path.Join(schemaBase, "runtime"), "txco-db-changeset-runtime", "runtime")

	// Per-tenant secret store. Auto-minted on first boot at the
	// configured path (default: ./chassis/data/secrets/txco-master.key,
	// matching the runtime DB pattern). Set SecretMasterKeyPath=""
	// in the config to opt out entirely — that's the library /
	// embedder escape hatch for chassis instances that want the
	// feature off.
	//
	// First-mint is logged loudly (back-this-up obligation); load
	// failure (malformed file, bad perms) WARNs and leaves the
	// resolver nil so the chassis still boots — any op with `secrets`
	// in its WITH clause then fails loud with `secret_store_unavailable`
	// rather than silently no-op.
	//
	// See internal docs/todo-secret-store.md §3 + docs/runbook-secret-store.md.
	var secretsResolver *secrets.Resolver
	if conf.SecretMasterKeyPath != "" {
		mk, mkErr := secrets.LoadOrMintFileMasterKey(conf.SecretMasterKeyPath, func(path string) {
			logger.Info("secret store: minted new master key — BACK THIS UP; losing it makes every stored secret unrecoverable",
				zap.String("path", path))
		})
		if mkErr != nil {
			logger.Warn("secret store disabled: master key load failed",
				zap.String("path", conf.SecretMasterKeyPath),
				zap.String("err", mkErr.Error()))
		} else {
			store := secrets.NewStore(runtimeDB, mk)
			// Slug→id lookup against the same runtime DB. Used by the
			// processor splice (PR 3), which has the tenant SLUG pinned
			// on context but needs the tenant_id (hxid) to query the
			// secret store.
			slugToID := func(ctx context.Context, slug string) (string, error) {
				var id string
				err := runtimeDB.QueryRowContext(ctx,
					`SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`,
					slug).Scan(&id)
				if err != nil {
					return "", fmt.Errorf("tenant slug %q not found: %w", slug, err)
				}
				return id, nil
			}
			secretsResolver = secrets.NewResolver(store, slugToID)
			logger.Info("secret store enabled",
				zap.String("path", conf.SecretMasterKeyPath),
				zap.Int("key_version", mk.Version()))
		}
	}

	// auth.db is identity-side only. Data-plane-only chassis (no admin
	// personality) never open it — they have no actors, sessions, or
	// invitations to read or write. An HA control plane points
	// --db-auth-dsn at a shared Postgres so every replica sees the same
	// actors/keys/memberships/sessions.
	var authDB *sql.DB
	authDialect := registry.SQLite
	if strings.Contains(conf.Personalities, "admin") {
		authDB, authDialect = openAuthDBOrDie(logger, conf.DbAuthDsn)
		defer authDB.Close()
		applyMigrationsOrDie(ctx, logger, authDB, authDialect, schemaFS,
			authSchemaRoot(schemaBase, authDialect), "txco-db-changeset-auth", "auth")
	} else {
		logger.Info("skipping auth.db open — admin personality not active",
			zap.String("personalities", conf.Personalities))
	}

	logger.Info("db setup") // feedback here proved helpful in debugging file locking for db

	// Setup read-only db cache. The cache dumps THROUGH the chassis's
	// own runtime *sql.DB handle (not by re-opening the file) so its
	// dump doesn't race against this same handle's WAL/shm state on
	// reload — see chassis/dbcache.DbCache.Source.
	var dbc *dbcache.DbCache
	dbc, err = dbcache.New(conf, logger, ctx, runtimeDB)
	if err != nil {
		logger.Fatal("db cache new error", zap.String("err", err.Error()))
	}

	// Trusted system opstacks (_-prefixed, chassis-local). Loaded from
	// the embedded default plus an optional operator-controlled dir.
	// The OnReload hook re-applies the overlay after every dbcache
	// rebuild (Reload dumps runtime.db into a fresh :memory: DB and
	// would otherwise drop the system ops). Set BEFORE the first
	// Reload so the initial snapshot already carries _sys/boot.
	sysCfg := sysops.Config{Dir: conf.SystemOpstacksDir}
	sysLoader, sysErr := sysops.Load(sysCfg)
	if sysErr != nil {
		logger.Fatal("system opstacks load error", zap.String("err", sysErr.Error()))
	}
	if sysLoader.BootOpCount() == 0 {
		logger.Error("no _sys/boot ops loaded — unrouted requests will fall to the bare 404 safety net; check --system-opstacks-dir or the embedded bundle")
	}
	var sysMu sync.Mutex
	activeSys := sysLoader
	dbc.OnReload = func(db *sql.DB) error {
		sysMu.Lock()
		l := activeSys
		sysMu.Unlock()
		return l.Apply(db)
	}

	if err = dbc.Reload(); err != nil {
		logger.Fatal("db cache load error", zap.String("err", err.Error()))
	}
	go dbc.Watch()

	// Hot-reload of system opstacks is dev-only (txco serve stays
	// static after boot). txco dev sets --system-opstacks-watch.
	if conf.SystemOpstacksWatch {
		sysops.Watch(ctx, sysCfg, logger, func(nl *sysops.Loader) error {
			sysMu.Lock()
			activeSys = nl
			sysMu.Unlock()
			dbc.Mu.Lock()
			defer dbc.Mu.Unlock()
			return nl.Apply(dbc.Db)
		})
		logger.Info("system opstacks hot-reload enabled", zap.String("dir", conf.SystemOpstacksDir))
	}

	// Setup KeyValue Store
	kv, err := valkeyrie.NewStore(store.Backend(conf.KVStore), conf.KVStoreAddrs, &store.Config{Bucket: conf.KVStoreBucket})
	if err != nil {
		logger.Fatal("KVStore connection error", zap.String("kvstoreError", err.Error()))
	}

	// Start chassis Personalities
	ctx, stopWork, err := server.Start(ctx, conf, logger, kv, runtimeDB, authDB, dbc, secretsResolver)
	if err != nil {
		// Include the underlying error so operators can see what
		// failed (missing env, unreachable broker, bad DSN, etc.)
		// instead of an opaque "stop error" with only a stacktrace.
		logger.Fatal("server.Start failed", zap.String("err", err.Error()))
	}

	// Loop and wait for any shutdown / events
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP,
		syscall.SIGUSR1, syscall.SIGUSR2)

	logger.Info("-ready-")

	for {
		exiting := false
		select {
		case <-ctx.Done(): // server shutdown
			logger.Info("Server done")
			exiting = true
		case sig := <-signalChannel: // received OS signal
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				{
					logger.Info("Received signal. Shutting down.", zap.String("signal", sig.String()))
					// shut down chassis controllers and OTel exporters
					stopWork("signal")
					// cancel background watchers (dbcache.Watch et al)
					cancel()

					exiting = true
				}
			case syscall.SIGUSR1:
				// Drain: bleed this node out of its load balancer.
				// /healthz starts returning 503 and new requests get a
				// 503 + Retry-After; in-flight requests finish. SIGUSR2
				// resumes. The process keeps running until SIGTERM.
				admission.SetDraining(true)
				logger.Info("drain enabled (SIGUSR1): /healthz 503, new requests 503; SIGUSR2 to resume")
			case syscall.SIGUSR2:
				admission.SetDraining(false)
				logger.Info("drain disabled (SIGUSR2): resuming normal traffic")
			default:
				{
					// TODO: this could be used for graceful restarts?
					logger.Info("Other signal received", zap.String("signal", sig.String()))
				}
			}
		}
		if exiting {
			break
		}
	}
	return 0
}

// selectSchemaSource picks where migration SQL files come from.
// Precedence: explicit on-disk DbSchemaDir (when it exists) > embedded
// schemas baked into the binary. The embedded path means a fresh `txco
// serve` (and `txco dev`) works anywhere without the repo on disk.
//
// Returns the fs.FS to read from, the base path inside it (which holds
// the `auth/` and `runtime/` subdirectories), and a short label
// describing the source (used in startup logs).
func selectSchemaSource(dbSchemaDir string) (fs.FS, string, string) {
	if dbSchemaDir != "" {
		if info, err := os.Stat(dbSchemaDir); err == nil && info.IsDir() {
			return os.DirFS(dbSchemaDir), ".", "filesystem:" + dbSchemaDir
		}
	}
	return dbschemas.FS, "schema/sqlite", "embedded"
}

// authSchemaRoot resolves the auth migration directory for the chosen
// dialect. SQLite (the default) keeps <base>/auth. Postgres swaps the
// embedded sqlite tree for the parallel postgres tree
// (schema/postgres/auth), or, for an on-disk --db-schema-dir (base
// "."), the `postgres/auth` subdir. Runtime is unaffected — it is
// always SQLite.
func authSchemaRoot(schemaBase string, d registry.Dialect) string {
	if d == registry.Postgres {
		if schemaBase == "schema/sqlite" {
			return "schema/postgres/auth"
		}
		return path.Join(schemaBase, "postgres", "auth")
	}
	return path.Join(schemaBase, "auth")
}

// openSQLiteOrDie opens a SQLite file with the chassis's standard
// connection options. `kind` is a short label ("runtime" / "auth") used
// in the fatal log message so an operator hitting a permissions error
// knows which file is at fault.
//
// Connection options, why each:
//
//   - mode=rwc            — create the file on first run; standard.
//   - _journal_mode=WAL   — concurrent readers alongside one writer.
//     Default rollback-journal serializes BOTH
//     behind an EXCLUSIVE lock; under any real
//     parallelism (demo Runner's per-step seed,
//     fleet apply, parallel CLI ops) that lock
//     is a bottleneck and a 500 source. WAL
//     creates `.db-wal` + `.db-shm` files next
//     to the main `.db`; cleaned on graceful
//     shutdown.
//   - _busy_timeout=5000  — 5s patience on a busy file lock. In WAL
//     mode readers don't block writers and
//     writers serialize via the WAL's commit
//     lock, so realistic contention is short.
//     5s is a generous safety net for the worst
//     case (e.g. brief checkpoint stalls under
//     burst load).
//
// Why NOT cache=shared: shared-cache mode introduces a SECOND lock
// class — TABLE-level (SQLITE_LOCKED, error code 6) — that exists on
// top of the file-level lock and which busy_timeout does NOT retry.
// Under any real concurrency, admin writes that map the same table
// (e.g. parallel POSTs to /stacks/<name>/draft hitting `stacks`) hit
// SQLITE_LOCKED with the error text "database table is locked:
// stacks" and surface as 500s. SQLite's own docs say shared-cache is
// "discouraged"; WAL replaces its concurrency story cleanly.
//
// The WAL move also requires chassis/dbcache.Reload to dump THROUGH
// this same *sql.DB handle (via sqlite3dump.DumpDB), not to open the
// file fresh — a second uncoordinated connection in WAL mode races
// the .db-shm state and 500s on first boot. See dbcache.DbCache.Source.
func openSQLiteOrDie(logger *zap.Logger, dsn, kind string) *sql.DB {
	full := fmt.Sprintf("%s?mode=rwc&_journal_mode=WAL&_busy_timeout=%d", dsn, 5000)
	db, err := sql.Open("sqlite3", full)
	if err != nil {
		logger.Fatal("db open err",
			zap.String("kind", kind),
			zap.String("dsn", dsn),
			zap.String("dberr", err.Error()))
	}
	return db
}

// Pool bounds for a shared-Postgres auth DB (HA control plane). The
// control plane is low-QPS, so a small per-replica cap keeps total
// fleet connections well under a managed-Postgres max_connections while
// leaving plenty of headroom. SQLite ignores all of this — it is only
// applied on the pgx pool.
const (
	authPGMaxOpenConns    = 10
	authPGMaxIdleConns    = 5
	authPGConnMaxIdleTime = 5 * time.Minute
	authPGConnMaxLifetime = 30 * time.Minute
	authPGPingTimeout     = 5 * time.Second
)

// openAuthDBOrDie opens the auth DB and returns its SQL dialect. A
// postgres:// (or postgresql://) DSN selects a shared Postgres store for
// an HA control plane — opened via the `pgx` database/sql driver, which
// a downstream overlay blank-imports (the chassis never compiles a
// Postgres driver; SQLite stays the in-tree default and only built-in
// driver). Anything else is the historical local SQLite file,
// byte-for-byte unchanged.
//
// The DSN is logged redacted (it may carry a Postgres password).
func openAuthDBOrDie(logger *zap.Logger, dsn string) (*sql.DB, registry.Dialect) {
	d := registry.DialectForDSN(dsn)
	if d == registry.Postgres {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			logger.Fatal("db open err",
				zap.String("kind", "auth"),
				zap.String("dsn", config.RedactDSN(dsn)),
				zap.String("dberr", err.Error()))
		}
		// Bound the pool. database/sql defaults to unlimited open
		// connections; against a shared managed Postgres, N
		// control-plane replicas each opening unbounded conns would
		// exhaust server max_connections. The admin/control plane is
		// low-QPS (operators, not end-user traffic), so a small cap is
		// ample. ConnMaxLifetime/IdleTime recycle connections so a
		// managed-PG failover or idle-timeout is picked up cleanly
		// rather than surfacing as stale-connection errors. (SQLite is
		// deliberately left untouched — historical behaviour.)
		db.SetMaxOpenConns(authPGMaxOpenConns)
		db.SetMaxIdleConns(authPGMaxIdleConns)
		db.SetConnMaxIdleTime(authPGConnMaxIdleTime)
		db.SetConnMaxLifetime(authPGConnMaxLifetime)

		// Fail fast with a clear, redacted message if the DSN is set
		// but the server is unreachable, rather than a confusing
		// mid-migration error on the first CREATE TABLE.
		pingCtx, cancel := context.WithTimeout(context.Background(), authPGPingTimeout)
		defer cancel()
		if perr := db.PingContext(pingCtx); perr != nil {
			logger.Fatal("auth Postgres unreachable",
				zap.String("kind", "auth"),
				zap.String("dsn", config.RedactDSN(dsn)),
				zap.String("dberr", perr.Error()))
		}
		logger.Info("auth.db on shared Postgres (HA control plane)",
			zap.String("dsn", config.RedactDSN(dsn)),
			zap.Int("max_open_conns", authPGMaxOpenConns))
		return db, d
	}
	return openSQLiteOrDie(logger, dsn, "auth"), registry.SQLite
}

// applyMigrationsOrDie sweeps a per-DB migration directory once and
// brings the DB up to head. Each DB tracks its own changeset row in its
// own `varvals` table (key = changesetVar). On any failure the chassis
// fails fast — there's no recovery from a partially-applied migration.
func applyMigrationsOrDie(ctx context.Context, logger *zap.Logger, db *sql.DB,
	dialect registry.Dialect, fsys fs.FS, root, changesetVar, kind string) {

	if dialect == nil {
		dialect = registry.SQLite
	}

	// Ensure varvals exists before we read the changeset row. This DDL
	// is portable as-is (TEXT + inline UNIQUE) across SQLite/Postgres.
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS varvals (var TEXT, val TEXT, UNIQUE(var));`,
	); err != nil {
		logger.Fatal("db bootstrap err",
			zap.String("kind", kind), zap.String("dberr", err.Error()))
	}

	var current int
	err := db.QueryRowContext(ctx,
		dialect.Rebind(`SELECT val FROM varvals WHERE var = ?`), changesetVar,
	).Scan(&current)
	switch {
	case err == sql.ErrNoRows:
		current = 0
	case err != nil:
		logger.Fatal("db changeset err",
			zap.String("kind", kind), zap.String("dberr", err.Error()))
	}
	logger.Info("database at ChangeId",
		zap.String("kind", kind), zap.Int("dbChangeId", current))

	files, err := fs.ReadDir(fsys, root)
	if err != nil {
		logger.Fatal("db changeset file err",
			zap.String("kind", kind), zap.String("migrationErr", err.Error()))
	}

	sort.Slice(files, func(i, j int) bool {
		na, _ := strconv.Atoi(strings.Split(files[i].Name(), "_")[0])
		nb, _ := strconv.Atoi(strings.Split(files[j].Name(), "_")[0])
		return na < nb
	})

	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".sql") {
			continue
		}
		fileID, err := strconv.Atoi(strings.Split(f.Name(), "_")[0])
		if err != nil {
			logger.Info("skipping filename not in migration format NNNN_desc.sql",
				zap.String("kind", kind), zap.String("skipFile", f.Name()))
			continue
		}
		if fileID <= current {
			logger.Debug("already migrated",
				zap.String("kind", kind), zap.String("skipFile", f.Name()))
			continue
		}
		logger.Info("should migrate db",
			zap.String("kind", kind),
			zap.Int("schemaChangeId", fileID),
			zap.Int("currentDbChangeId", current),
			zap.String("schemaFile", f.Name()))

		body, err := fs.ReadFile(fsys, path.Join(root, f.Name()))
		if err != nil {
			logger.Fatal("can't read db migration",
				zap.String("kind", kind), zap.String("schemaFile", f.Name()))
		}

		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			logger.Fatal("Db migration transaction start error",
				zap.String("kind", kind), zap.String("err", err.Error()))
		}
		// Run the migration SQL and the varvals upsert THROUGH `tx`, not
		// the parent `db`. The earlier shape — db.Exec(body) + tx.Commit
		// of an unused transaction — happened to work because SQLite DDL
		// inside `db.Exec` auto-commits on whatever pool connection it
		// gets, but the `tx.Rollback()` paths on error were dead code:
		// they rolled back an empty transaction while the DDL stayed
		// committed on the other connection. Funnelling both through
		// `tx` makes the rollback meaningful and keeps migration atomic.
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			logger.Fatal("db setup err",
				zap.String("kind", kind), zap.String("dberr", err.Error()),
				zap.String("schemaFile", f.Name()))
		}
		// Portable upsert: `ON CONFLICT(var) DO UPDATE` is valid on both
		// SQLite (≥3.24, the bundled version) and Postgres — replaces
		// SQLite-only `INSERT OR REPLACE`. Rebound for `$N` on Postgres.
		// val is a TEXT column. SQLite silently coerces an int bind;
		// pgx is strict and refuses (OID 25). Bind the changeset id as
		// a string so the same statement works on both engines — the
		// read side scans TEXT→int via database/sql's converter.
		if _, err := tx.ExecContext(ctx,
			dialect.Rebind(`INSERT INTO varvals (var, val) VALUES (?, ?)
			 ON CONFLICT(var) DO UPDATE SET val = excluded.val`),
			changesetVar, strconv.Itoa(fileID),
		); err != nil {
			_ = tx.Rollback()
			logger.Fatal("db update varval err",
				zap.String("kind", kind), zap.String("dberr", err.Error()))
		}
		if err := tx.Commit(); err != nil {
			logger.Fatal("db commit err",
				zap.String("kind", kind), zap.String("dberr", err.Error()))
		}
		logger.Info("migrated db",
			zap.String("kind", kind),
			zap.Int("schemaChangeId", fileID),
			zap.String("schemaFile", f.Name()))
		current = fileID
	}
}
