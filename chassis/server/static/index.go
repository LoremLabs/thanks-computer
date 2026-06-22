package static

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	radix "github.com/hashicorp/go-immutable-radix"
	"go.uber.org/zap"
)

// entry is a cached file: bytes + precomputed content-type + strong
// ETag. Held in memory so Lookup never reads disk.
type entry struct {
	body  []byte
	ctype string
	etag  string // strong validator: "<sha256-hex>"
}

// layer is one source: the file set plus the set of its top-level
// directory names. A directory's presence makes static OWN that prefix
// (`/assets/**` is static's the moment any assets/* file exists); the
// root is never owned — top-level paths need an explicit file.
type layer struct {
	files *radix.Tree         // rel -> *entry
	dirs  map[string]struct{} // first path segment of every nested file
}

func emptyLayer() layer { return layer{files: radix.New(), dirs: map[string]struct{}{}} }

// metaEntry is a tenant FILES/ asset known only by its content hash; the
// bytes are resolved lazily from the filecas store (kept out of memory).
// The hash doubles as the strong ETag.
type metaEntry struct {
	hash  string // content sha256 hex
	size  int64
	ctype string // derived from the path extension
}

// tenantLayer is one (tenant, stack)'s FILES/ metadata: rel-path → entry,
// plus the owned top-level dir names (same prefix-ownership semantics as
// the inline layers).
type tenantLayer struct {
	files map[string]*metaEntry
	dirs  map[string]struct{}
}

// Result is the outcome of a Lookup.
//
//	Found      → an exact file; serve Body (200) or 304 on ETag match.
//	!Found && Owned → request is under a static-owned directory prefix;
//	                  the op must answer 404 (NOT fall through).
//	!Found && !Owned → not static's; the op returns "{}" (keep routing).
type Result struct {
	Found bool
	Owned bool
	Body  []byte // set for inline (operator) layers
	// Hash is set for a tenant CAS entry instead of Body: the caller
	// resolves the bytes from the filecas store by this content hash.
	Hash  string
	Size  int64
	Ctype string
	ETag  string
}

// Index is the in-memory static set, three layers deep (per routed
// stack → chassis-wide → embedded). Trees are immutable
// (github.com/hashicorp/go-immutable-radix); Rebuild builds fresh ones
// and swaps under mu. Reads snapshot the pointers under mu then use the
// immutable structures lock-free — the processor.Unit.Mux idiom.
type Index struct {
	root string // workspace root (conf.SystemOpstacksDir); "" = embedded only
	log  *zap.Logger

	mu       sync.Mutex
	perStack map[string]layer // stack -> its layer (disk workspace, inline bytes)
	chassis  layer
	embedded layer
	// tenant FILES/ metadata from the runtime DB: slug -> stack -> layer.
	// Bytes live in the filecas store, not here. Rebuilt by RebuildTenant
	// on dbcache reload; nil until the first build (Lookup nil-safe).
	tenant map[string]map[string]tenantLayer
}

// NewIndex builds the index immediately so the chassis serves correctly
// before the first reload.
func NewIndex(root string, log *zap.Logger) *Index {
	ix := &Index{root: root, log: log}
	ix.Rebuild()
	return ix
}

// budget enforces the workspace count/byte caps across all workspace
// layers in one Rebuild pass.
type budget struct {
	files int
	bytes int64
}

func (b *budget) take(n int) bool {
	if b.files+1 > MaxIndexFiles || b.bytes+int64(n) > MaxIndexBytes {
		return false
	}
	b.files++
	b.bytes += int64(n)
	return true
}

// Rebuild reloads every layer from disk + embed into fresh structures
// and atomically swaps them in. Called at startup and on dbcache reload
// — never on the request path.
func (ix *Index) Rebuild() {
	emb := ix.buildEmbedded()

	chassis := emptyLayer()
	perStack := map[string]layer{}
	if ix.root != "" {
		b := &budget{}
		chassis = ix.buildDir(filepath.Join(ix.root, "FILES"), b)
		perStack = ix.buildPerStack(filepath.Join(ix.root, "OPS"), b)
	}

	ix.mu.Lock()
	ix.embedded, ix.chassis, ix.perStack = emb, chassis, perStack
	ix.mu.Unlock()
}

// Lookup resolves a request to a static file, layered first-match-wins,
// with try_files-style fallback so prerendered routes serve their own
// HTML for the extension-less URL a browser actually requests:
//
//	exact            /assets/x.css → assets/x.css
//	clean URL        /about        → about.html
//	directory index  /blog         → blog/index.html
//	root             /             → index.html
//
// A path whose last segment already has an extension only probes the
// exact match (so /app.js never tries app.js.html or app.js/index.html).
// Backward-compatible: a path with no matching file still misses and, if
// it sits under a static-owned directory, reports Owned (→ 404) so the op
// answers 404 rather than leaking to app routing; a bare top-level miss
// passes through. Mirrors nginx try_files / Caddy / the layout
// adapter-static emits. Pure in-memory; no filesystem access.
func (ix *Index) Lookup(tenant, stack, reqPath string) Result {
	rel := safeRel(reqPath)

	ix.mu.Lock()
	emb, ch, ps, tn := ix.embedded, ix.chassis, ix.perStack, ix.tenant
	ix.mu.Unlock()

	st := safeStack(stack)

	// Operator/inline layers first (disk workspace → chassis → embedded),
	// highest precedence, returning bytes directly. An operator override of
	// a tenant file still wins.
	opLayers := make([]layer, 0, 3)
	if st != "" {
		if l, ok := ps[st]; ok {
			opLayers = append(opLayers, l)
		}
	}
	opLayers = append(opLayers, ch, emb)

	// Tenant layer (metadata only; the caller resolves bytes by Hash). Keyed
	// (slug, stack) so colliding stack names across tenants never merge.
	var tl tenantLayer
	haveTenant := false
	if ten := safeSeg(tenant); ten != "" && st != "" {
		if stacks, ok := tn[ten]; ok {
			if l, ok := stacks[st]; ok {
				tl, haveTenant = l, true
			}
		}
	}

	// try_files: exact, then .html, then /index.html (and root → index.html).
	for _, cand := range indexCandidates(rel) {
		if r, ok := lookupExact(cand, opLayers, tl, haveTenant); ok {
			return r
		}
	}

	// No file resolved. Static owns the path only if it's under a directory
	// that exists in an applicable layer (the root / a bare top-level name is
	// never prefix-owned). Checked on the original rel, not a candidate.
	if seg, _, nested := strings.Cut(rel, "/"); nested {
		for _, l := range opLayers {
			if _, ok := l.dirs[seg]; ok {
				return Result{Owned: true}
			}
		}
		if haveTenant {
			if _, ok := tl.dirs[seg]; ok {
				return Result{Owned: true}
			}
		}
	}
	return Result{}
}

// Asset resolves an EXACT stack-relative path across the same layers as
// Lookup (routed stack → chassis → embedded → tenant CAS) but WITHOUT
// try_files probing and WITHOUT the `_`-private HTTP filter — so ops
// (e.g. txco://read-file) can read FILES/_mail/ templates as data. Bytes
// are inline (Result.Body) or a tenant CAS entry (Result.Hash, the caller
// resolves via filecas). Returns (_, false) on miss. Pure in-memory: the
// index holds the bytes, so this never touches the filesystem.
func (ix *Index) Asset(tenant, stack, rel string) (Result, bool) {
	cand := safeRel(rel)
	if cand == "" {
		return Result{}, false
	}

	ix.mu.Lock()
	emb, ch, ps, tn := ix.embedded, ix.chassis, ix.perStack, ix.tenant
	ix.mu.Unlock()

	st := safeStack(stack)

	opLayers := make([]layer, 0, 3)
	if st != "" {
		if l, ok := ps[st]; ok {
			opLayers = append(opLayers, l)
		}
	}
	opLayers = append(opLayers, ch, emb)

	var tl tenantLayer
	haveTenant := false
	if ten := safeSeg(tenant); ten != "" && st != "" {
		if stacks, ok := tn[ten]; ok {
			if l, ok := stacks[st]; ok {
				tl, haveTenant = l, true
			}
		}
	}

	return lookupExact(cand, opLayers, tl, haveTenant)
}

// lookupExact resolves a single candidate path across the operator/inline
// layers (bytes inline) then the tenant layer (bytes resolved by content
// hash downstream). Returns (_, false) on miss.
func lookupExact(rel string, opLayers []layer, tl tenantLayer, haveTenant bool) (Result, bool) {
	if rel == "" {
		return Result{}, false
	}
	for _, l := range opLayers {
		if v, found := l.files.Get([]byte(rel)); found {
			e := v.(*entry)
			return Result{Found: true, Body: e.body, Ctype: e.ctype, ETag: e.etag}, true
		}
	}
	if haveTenant {
		if me, ok := tl.files[rel]; ok {
			return Result{Found: true, Hash: me.hash, Size: me.size,
				Ctype: me.ctype, ETag: `"` + me.hash + `"`}, true
		}
	}
	return Result{}, false
}

// indexCandidates returns the try_files probe order for a request path:
// exact, then a clean-URL ".html" sibling, then a "/index.html" directory
// index. Root ("") → "index.html". A path whose last segment already has
// an extension only probes the exact match — so a missing asset like
// "/app/x.js" stays a miss (→ Owned 404 under its dir) instead of
// resolving to a stray "x.js/index.html".
func indexCandidates(rel string) []string {
	if rel == "" {
		return []string{"index.html"}
	}
	if lastSegHasDot(rel) {
		return []string{rel}
	}
	return []string{rel, rel + ".html", rel + "/index.html"}
}

// lastSegHasDot reports whether rel's final path segment contains a "."
// (i.e. it looks like a file with an extension).
func lastSegHasDot(rel string) bool {
	return strings.LastIndexByte(rel, '.') > strings.LastIndexByte(rel, '/')
}

// RebuildTenant reloads the tenant FILES/ metadata layer (path → content
// hash + size, no bytes) from the runtime DB and atomically swaps it in.
// Modeled on admission.Rebuild: nil-safe, and it keeps the prior layer on
// any error so a transient DB hiccup never blanks tenant serving. db is the
// handle passed by dbcache.OnReload (or dbc.Snapshot() for the initial
// build) — never capture dbc.Db.
func (ix *Index) RebuildTenant(db *sql.DB) error {
	if ix == nil || db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT t.slug, s.name, f.path, f.content_hash, length(f.content) AS sz
		  FROM stack_files f
		  JOIN stacks  s ON s.active_version = f.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE f.path LIKE 'FILES/%' AND t.revoked_at IS NULL`)
	if err != nil {
		if ix.log != nil {
			ix.log.Warn("static: tenant filecas index query failed", zap.Error(err))
		}
		return err // keep prior layer
	}
	defer rows.Close()

	out := map[string]map[string]tenantLayer{}
	for rows.Next() {
		var slug, name, p, hash string
		var sz int64
		if err := rows.Scan(&slug, &name, &p, &hash, &sz); err != nil {
			if ix.log != nil {
				ix.log.Warn("static: tenant filecas index scan failed", zap.Error(err))
			}
			return err // keep prior layer
		}
		if hash == "" {
			continue
		}
		rel := safeRel(strings.TrimPrefix(p, "FILES/"))
		if rel == "" {
			continue
		}
		sl, st := safeSeg(slug), safeStack(name)
		if sl == "" || st == "" {
			continue
		}
		stacks := out[sl]
		if stacks == nil {
			stacks = map[string]tenantLayer{}
			out[sl] = stacks
		}
		tl, ok := stacks[st]
		if !ok {
			tl = tenantLayer{files: map[string]*metaEntry{}, dirs: map[string]struct{}{}}
			stacks[st] = tl
		}
		tl.files[rel] = &metaEntry{hash: hash, size: sz, ctype: contentType(rel, nil)}
		if seg, _, nested := strings.Cut(rel, "/"); nested {
			tl.dirs[seg] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		if ix.log != nil {
			ix.log.Warn("static: tenant filecas index rows error", zap.Error(err))
		}
		return err // keep prior layer
	}

	// DIAG (driplit read-file found:false): report what got indexed plus the
	// join state of every `_mail` stack on THIS node, so a dropped FILES set
	// (stale active_version / empty content_hash / missing-or-revoked tenant
	// row) is visible in the log instead of a silent miss. Cheap; runs once
	// per cachedb reload.
	if ix.log != nil {
		kept := 0
		for _, stacks := range out {
			for _, tl := range stacks {
				kept += len(tl.files)
			}
		}
		ix.log.Info("static: tenant index rebuilt",
			zap.Int("tenants", len(out)), zap.Int("kept_files", kept))
		if drows, derr := db.Query(`
			SELECT s.name, s.tenant_id, COALESCE(t.slug,''), COALESCE(t.revoked_at,''),
			       s.active_version,
			       (SELECT count(*) FROM stack_files f
			          WHERE f.version_id=s.active_version AND f.path LIKE 'FILES/%') nfiles,
			       (SELECT count(*) FROM stack_files f
			          WHERE f.version_id=s.active_version AND f.path LIKE 'FILES/%'
			            AND f.content_hash<>'') nhash
			  FROM stacks s LEFT JOIN tenants t ON t.tenant_id=s.tenant_id
			 WHERE s.name LIKE '%/_mail'`); derr == nil {
			for drows.Next() {
				var name, tid, slug, rev string
				var av sql.NullInt64
				var nf, nh int
				if drows.Scan(&name, &tid, &slug, &rev, &av, &nf, &nh) == nil {
					// cross-check: did this stack actually land in the index we
					// just built? (key = safeSeg(slug)/safeStack(name), the SAME
					// keys ix.Asset looks up with). indexed<files => the builder
					// dropped rows the DB has — the smoking gun.
					indexed := -1
					if sl := safeSeg(slug); sl != "" {
						if st := safeStack(name); st != "" {
							if tl, ok := out[sl][st]; ok {
								indexed = len(tl.files)
							} else {
								indexed = 0
							}
						}
					}
					ix.log.Info("static: _mail diag",
						zap.String("name", name), zap.String("tenant_id", tid),
						zap.String("tenant_slug", slug), zap.String("revoked_at", rev),
						zap.Int64("active_version", av.Int64),
						zap.Int("db_files", nf), zap.Int("db_files_with_hash", nh),
						zap.String("index_key", safeSeg(slug)+"/"+safeStack(name)),
						zap.Int("indexed_files", indexed))
				}
			}
			drows.Close()
		}
	}

	ix.mu.Lock()
	ix.tenant = out
	ix.mu.Unlock()
	return nil
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (ix *Index) buildEmbedded() layer {
	l := emptyLayer()
	_ = fs.WalkDir(embeddedFS, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel := safeRel(strings.TrimPrefix(p, "files/"))
		if rel == "" {
			return nil
		}
		b, rerr := embeddedFS.ReadFile(p)
		if rerr != nil || len(b) > MaxFileBytes {
			return nil
		}
		l.files, _, _ = l.files.Insert([]byte(rel), &entry{body: b, ctype: contentType(rel, b), etag: etagOf(b)})
		if seg, _, nested := strings.Cut(rel, "/"); nested {
			l.dirs[seg] = struct{}{}
		}
		return nil
	})
	return l
}

// buildPerStack indexes <opsDir>/<stack>/FILES/** for every stack dir.
func (ix *Index) buildPerStack(opsDir string, b *budget) map[string]layer {
	out := map[string]layer{}
	ents, err := os.ReadDir(opsDir)
	if err != nil {
		return out
	}
	for _, de := range ents {
		if !de.IsDir() {
			continue
		}
		stack := safeSeg(de.Name())
		if stack == "" {
			continue
		}
		filesDir := filepath.Join(opsDir, de.Name(), "FILES")
		if fi, serr := os.Stat(filesDir); serr != nil || !fi.IsDir() {
			continue
		}
		l := emptyLayer()
		ix.walkInto(&l, filesDir, b)
		out[stack] = l
	}
	return out
}

// buildDir indexes a single FILES root (the chassis-wide layer).
func (ix *Index) buildDir(dir string, b *budget) layer {
	l := emptyLayer()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return l
	}
	ix.walkInto(&l, dir, b)
	return l
}

// walkInto reads every regular file under root into l, enforcing depth,
// per-file size, and the shared count/byte budget. Overflow / oversize
// / unsafe paths are logged and skipped; already-indexed files keep
// serving.
func (ix *Index) walkInto(l *layer, root string, b *budget) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			if slash != "." && strings.Count(slash, "/")+1 > MaxIndexDepth {
				ix.warn("static: skipping deep dir", root, slash)
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		safe := safeRel(slash)
		if safe == "" {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > MaxFileBytes {
			ix.warn("static: skipping oversize file", root, slash)
			return nil
		}
		if !b.take(int(info.Size())) {
			ix.warn("static: index budget exhausted, skipping", root, slash)
			return nil
		}
		body, berr := os.ReadFile(p)
		if berr != nil {
			return nil
		}
		l.files, _, _ = l.files.Insert([]byte(safe),
			&entry{body: body, ctype: contentType(safe, body), etag: etagOf(body)})
		if seg, _, nested := strings.Cut(safe, "/"); nested {
			l.dirs[seg] = struct{}{}
		}
		return nil
	})
}

func (ix *Index) warn(msg, root, p string) {
	if ix.log != nil {
		ix.log.Warn(msg, zap.String("root", root), zap.String("path", p))
	}
}
