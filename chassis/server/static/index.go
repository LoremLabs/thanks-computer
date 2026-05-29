package static

import (
	"crypto/sha256"
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

// Result is the outcome of a Lookup.
//
//	Found      → an exact file; serve Body (200) or 304 on ETag match.
//	!Found && Owned → request is under a static-owned directory prefix;
//	                  the op must answer 404 (NOT fall through).
//	!Found && !Owned → not static's; the op returns "{}" (keep routing).
type Result struct {
	Found bool
	Owned bool
	Body  []byte
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
	perStack map[string]layer // stack -> its layer
	chassis  layer
	embedded layer
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

// Lookup resolves a request, layered first-match-wins for an exact
// file, and otherwise reports whether the path is under a static-owned
// directory prefix. Pure in-memory; no filesystem access.
func (ix *Index) Lookup(stack, reqPath string) Result {
	rel := safeRel(reqPath)
	if rel == "" {
		return Result{}
	}

	ix.mu.Lock()
	emb, ch, ps := ix.embedded, ix.chassis, ix.perStack
	ix.mu.Unlock()

	st := safeSeg(stack)
	layers := make([]layer, 0, 3)
	if st != "" {
		if l, ok := ps[st]; ok {
			layers = append(layers, l)
		}
	}
	layers = append(layers, ch, emb)

	for _, l := range layers {
		if v, found := l.files.Get([]byte(rel)); found {
			e := v.(*entry)
			return Result{Found: true, Body: e.body, Ctype: e.ctype, ETag: e.etag}
		}
	}

	// No exact file. Static owns it only if the request is under a
	// directory that exists in an applicable layer (the root / a bare
	// top-level name is never prefix-owned — it needs an explicit file).
	if seg, _, nested := strings.Cut(rel, "/"); nested {
		for _, l := range layers {
			if _, ok := l.dirs[seg]; ok {
				return Result{Owned: true}
			}
		}
	}
	return Result{}
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
