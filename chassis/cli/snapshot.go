package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	_ "github.com/loremlabs/thanks-computer/chassis/artifact/filestore"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/snapshot"
)

const defaultRuntimeDB = "./chassis/data/db/runtime-dev.db"

// defaultRuntimeDBForEnv resolves the runtime DB path from the
// chassis environment when present, falling back to the dev path.
// The chassis sets TXCO_DB_ROOT_DIR + TXCO_ENV inside its container
// (e.g. /data/db + prod → /data/db/runtime-prod.db), so a `docker
// exec txco-txco-1 txco snapshot publish` Just Works without an
// explicit --db. Local repo invocations see neither env var set and
// fall back to the dev default.
func defaultRuntimeDBForEnv() string {
	root := strings.TrimSpace(os.Getenv("TXCO_DB_ROOT_DIR"))
	if root == "" {
		return defaultRuntimeDB
	}
	env := strings.TrimSpace(os.Getenv("TXCO_ENV"))
	if env == "" {
		env = "dev"
	}
	// Match chassis/app/app.go convention: <db-root>/runtime-<env>.db
	return root + "/runtime-" + env + ".db"
}

// runSnapshot is the `txco snapshot <sub>` namespace: export a verifiable
// snapshot artifact of the runtime DB, safely restore one, or publish
// directly to the configured artifact store for a new fleet chassis to
// fetch as its --snapshot-bootstrap-ref.
func runSnapshot(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printSnapshotUsage(stdout)
		return 0
	}
	switch args[0] {
	case "export":
		return runSnapshotExport(args[1:], stdout, stderr)
	case "import":
		return runSnapshotImport(args[1:], stdout, stderr)
	case "publish":
		return runSnapshotPublish(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printSnapshotUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "snapshot: unknown subcommand %q\n\n", args[0])
		printSnapshotUsage(stderr)
		return 2
	}
}

func printSnapshotUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco snapshot <command> [flags]

Commands:
  export   Dump the runtime DB into a checksummed, versioned artifact
           on the local filesystem.
  import   Safely restore an artifact from a local file (temp DB ->
           sanity-check -> atomic replace). Refuses a populated DB
           unless --force.
  publish  Export + upload to the configured artifact store
           ($TXCO_ARTIFACT_STORE = file | s3 / R2). The artifact ref
           the upload writes is printed on stdout's last line so
           cron/scripts can capture it for $TXCO_SNAPSHOT_BOOTSTRAP_REF.

Export flags:
  --db <path>    runtime DB file (default `+defaultRuntimeDB+`)
  --out <path>   artifact output path; writes <path> and <path>.manifest.json
                 (default ./snapshot.snap)

Import flags:
  --db <path>    runtime DB file to restore into (default `+defaultRuntimeDB+`)
  --force        permit replacing a non-fresh (populated) runtime DB

Publish flags:
  --db <path>          runtime DB file (default `+defaultRuntimeDB+`)
  --key-prefix <str>   artifact key prefix (default snapshots/)
  --alias <str>        ALSO write the same bytes under this fixed key
                       (e.g. snapshots/latest) so a new chassis can boot
                       with TXCO_SNAPSHOT_BOOTSTRAP_REF=snapshots/latest
                       without knowing the latest timestamped key.
                       Empty (default) writes only the timestamped key.

The chassis applies any newer migrations on next boot, so an artifact from
a slightly older binary restores safely.
`)
}

func runSnapshotExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", defaultRuntimeDB, "runtime DB file")
	out := fs.String("out", "./snapshot.snap", "artifact output path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	data, man, err := snapshot.Export(*db)
	if err != nil {
		fmt.Fprintf(stderr, "snapshot export: %v\n", err)
		return 1
	}
	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "snapshot export: marshal manifest: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "snapshot export: write %s: %v\n", *out, err)
		return 1
	}
	if err := os.WriteFile(*out+".manifest.json", manBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "snapshot export: write manifest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "exported %s (%d bytes, %s, control_version=%d)\n",
		*out, len(data), man.Checksum, man.ControlVersion)
	return 0
}

// runSnapshotPublish exports the runtime DB and uploads the artifact
// to the configured store ($TXCO_ARTIFACT_STORE + backend-specific
// env). On success the artifact key is printed on the FINAL line of
// stdout (preceded by a human-readable summary), so a shell wrapper
// can capture it with `tail -n1`.
//
// Snapshot data + manifest both land at <key> + <key>.manifest.json
// (the latter handled by artifact.Store.Put's two-blob API). When
// --alias is supplied, the SAME bytes are written a second time under
// the alias key — operator workflow:
//
//	$ docker exec txco-txco-1 txco snapshot publish --alias=snapshots/latest
//	published snapshots/20260521-143012-9f2c6e3d.snap
//	  control_version=4837, bytes=2156033
//	  alias: snapshots/latest
//	snapshots/20260521-143012-9f2c6e3d.snap
//
// Then a new chassis boots with TXCO_SNAPSHOT_BOOTSTRAP_REF=
// snapshots/latest and always picks up the freshest available
// snapshot without re-configuring per snapshot cadence.
func runSnapshotPublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Default looks at the current chassis runtime env first — `txco
	// snapshot publish` is overwhelmingly invoked via `docker exec
	// txco-txco-1 …` where TXCO_DB_ROOT_DIR + TXCO_ENV are populated
	// — and falls back to the dev path so a local repo invocation
	// still works.
	db := fs.String("db", defaultRuntimeDBForEnv(), "runtime DB file")
	keyPrefix := fs.String("key-prefix", "snapshots/", "artifact key prefix")
	alias := fs.String("alias", "", "ALSO write the same bytes under this fixed key (e.g. snapshots/latest)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	backend := strings.TrimSpace(os.Getenv("TXCO_ARTIFACT_STORE"))
	if backend == "" {
		backend = "file"
	}
	store, err := artifact.Open(backend, artifact.StoreConfig{
		FileDir: os.Getenv("TXCO_ARTIFACT_STORE_FILE_DIR"),
	})
	if err != nil {
		fmt.Fprintf(stderr, "snapshot publish: open artifact store %q: %v\n", backend, err)
		return 1
	}

	data, man, err := snapshot.Export(*db)
	if err != nil {
		fmt.Fprintf(stderr, "snapshot publish: export: %v\n", err)
		return 1
	}
	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "snapshot publish: marshal manifest: %v\n", err)
		return 1
	}

	// Key: <prefix><utc-stamp>-<sha12>.snap. utc-stamp is lex-sortable
	// so a listing of the prefix is automatically newest-last; sha12
	// disambiguates within a second of clock resolution.
	prefix := *keyPrefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	sum := strings.TrimPrefix(man.Checksum, "sha256:")
	if len(sum) > 12 {
		sum = sum[:12]
	}
	key := fmt.Sprintf("%s%s-%s.snap", prefix, stamp, sum)

	ctx := context.Background()
	if err := store.Put(ctx, key, data, manBytes); err != nil {
		fmt.Fprintf(stderr, "snapshot publish: put %s: %v\n", key, err)
		return 1
	}
	fmt.Fprintf(stdout, "published %s\n", key)
	fmt.Fprintf(stdout, "  control_version=%d, bytes=%d\n", man.ControlVersion, len(data))

	if *alias != "" {
		if err := store.Put(ctx, *alias, data, manBytes); err != nil {
			fmt.Fprintf(stderr, "snapshot publish: write alias %s: %v\n", *alias, err)
			// Don't fail the whole command — the timestamped key is
			// published. Operator can re-run with --alias-only later
			// (which we don't implement yet, but the workflow tolerates).
			return 1
		}
		fmt.Fprintf(stdout, "  alias: %s\n", *alias)
	}

	// FINAL line is the bare key for shell consumption (`tail -n1`).
	fmt.Fprintln(stdout, key)
	return 0
}

func runSnapshotImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", defaultRuntimeDB, "runtime DB file to restore into")
	force := fs.Bool("force", false, "permit replacing a populated runtime DB")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "snapshot import: missing <artifact-file>")
		printSnapshotUsage(stderr)
		return 2
	}
	artFile := fs.Arg(0)

	data, err := os.ReadFile(artFile)
	if err != nil {
		fmt.Fprintf(stderr, "snapshot import: read %s: %v\n", artFile, err)
		return 1
	}
	manBytes, err := os.ReadFile(artFile + ".manifest.json")
	if err != nil {
		fmt.Fprintf(stderr, "snapshot import: read manifest: %v\n", err)
		return 1
	}
	var man snapshot.Manifest
	if err := json.Unmarshal(manBytes, &man); err != nil {
		fmt.Fprintf(stderr, "snapshot import: bad manifest: %v\n", err)
		return 1
	}

	// migrate=nil: the dump is self-contained at its own migration
	// version; the chassis applies any newer migrations on next boot
	// (idempotent via varvals). Verify still gates model/cache compat.
	if err := snapshot.Bootstrap(data, man, *db, nil, *force); err != nil {
		if errors.Is(err, snapshot.ErrNotFresh) {
			fmt.Fprintf(stderr, "snapshot import: %s is populated; re-run with --force to replace it\n", *db)
			return 1
		}
		fmt.Fprintf(stderr, "snapshot import: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s from %s (control_version=%d)\n",
		*db, artFile, man.ControlVersion)
	return 0
}
