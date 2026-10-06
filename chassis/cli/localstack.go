package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	computeapi "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/cli/update"
	"github.com/loremlabs/thanks-computer/chassis/webabi"
)

// localWorkspace is the deployable view of a workspace: the author's OPS/
// tree plus the Web ABI builds txco.yaml binds to stacks. A bound stack's
// build is laid over its own tree (or is the whole stack, for a pure
// framework app with no OPS/<stack>/).
type localWorkspace struct {
	Dir      string
	Ops      []bundle.Op   // the OPS/ walk's ops, then each build's ops/
	Diags    []bundle.Diag // the OPS/ walk's diagnostics
	Bindings map[string]stackBinding
	ABI      map[string]*webabi.Dir // loaded builds, by stack
	Broken   map[string]error       // bound stacks whose build can't be installed
}

// readWorkspace walks dir's OPS/ tree and loads every bound Web ABI build.
// A broken binding (txco.yaml itself) is an error; a build that can't be
// installed (not built yet, a bad manifest) is recorded in Broken, so a
// command that doesn't touch that stack still works.
func readWorkspace(dir string) (*localWorkspace, error) {
	ops, diags, err := bundle.WalkDiag(dir)
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	bindings, err := loadStackBindings(dir)
	if err != nil {
		return nil, err
	}
	ws := &localWorkspace{Dir: dir, Ops: ops, Diags: diags, Bindings: bindings,
		ABI: map[string]*webabi.Dir{}, Broken: map[string]error{}}
	names := make([]string, 0, len(bindings))
	for n := range bindings {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		b := bindings[name]
		d, err := webabi.Load(b.Abs)
		if err != nil {
			ws.Broken[name] = fmt.Errorf("its Web ABI build %s can't be installed (build it first?): %w", b.ABI, err)
			continue
		}
		// SourcePaths are relative to the root, like the OPS/ walk's, unless
		// the build sits outside the workspace.
		prefix := b.ABI
		if strings.HasPrefix(prefix, "..") {
			prefix = b.Abs
		}
		abiOps, err := bundle.WalkABI(os.DirFS(d.Path), name, prefix)
		if err != nil {
			ws.Broken[name] = fmt.Errorf("its Web ABI build %s: %w", b.ABI, err)
			continue
		}
		ws.ABI[name] = d
		ws.Ops = append(ws.Ops, abiOps...)
	}
	return ws, nil
}

// StackNames is every stack the workspace deploys, sorted: those with ops,
// plus every bound stack (a static build may have none).
func (ws *localWorkspace) StackNames() []string {
	set := map[string]bool{}
	for _, op := range ws.Ops {
		set[op.Stack] = true
	}
	for n := range ws.Bindings {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Has reports whether the workspace deploys stack.
func (ws *localWorkspace) Has(stack string) bool {
	if _, ok := ws.Bindings[stack]; ok {
		return true
	}
	for _, op := range ws.Ops {
		if op.Stack == stack {
			return true
		}
	}
	return false
}

// withABI fills o with stack's Web ABI build, if it has one.
func (ws *localWorkspace) withABI(stack string, o collectOpts) collectOpts {
	o.ABI, o.ABIBroken = ws.ABI[stack], ws.Broken[stack]
	return o
}

// collectOpts chooses the parts of a stack's file set a caller needs. The
// CODE manifest (ops, FILES/, SOURCES/, OUTLETS/, SANDBOXES/, CAPS/,
// DATASETS/) is always collected; the rest is per caller:
//
//	apply / push   Derived           (+ KeepSource when the chassis keeps it)
//	dev            Data + Derived    (a full local mirror)
//	status, drift  nothing extra     (the basis their cleanliness hash
//	                                  compares against)
type collectOpts struct {
	// Data adds the store-seed packs (VECTORS/, KV/, BLOBS/, …), which only
	// `txco dev` deploys with the code; `txco data apply` owns them elsewhere.
	Data bool
	// Derived adds the rows derived from the code: COMPUTES/ (when
	// KeepSource) and STACKDIR/. AllStacks is required with it.
	Derived    bool
	KeepSource bool
	Built      []computeapi.Built
	AllStacks  map[string]bool

	// ABI is the stack's bound Web ABI build, laid over its tree: public/
	// becomes FILES/, plus the markers and the provenance file. ABIBroken,
	// when set, is why the stack's build can't be installed; collecting the
	// stack then fails rather than deploying it without its web half.
	ABI       *webabi.Dir
	ABIBroken error
}

// stackBuild is one stack's file set, plus the bytes the chassis must
// hold before a draft may reference them.
type stackBuild struct {
	Files []client.StackFile

	datasetUploads []casUpload // DATASETS/ artifacts
	blobUploads    []casUpload // store-seed BLOBS/ (Data only)
	sourceUploads  []casUpload // COMPUTES/ sources (Derived + KeepSource)
	treeUploads    []casUpload // the STACKDIR/ bundle (Derived)
}

// buildStackFiles collects one stack's file set from <dir>/OPS/<stack>/,
// with its already-walked (and, for an upload, already-resolved) ops. It is
// the one place that knows what a stack uploads: apply, push, dev, status
// and drift all build through it, so they can't disagree. An error names
// the part that failed ("collect FILES/: …"); the caller adds the stack.
func buildStackFiles(dir, stack string, ops []bundle.Op, o collectOpts) (*stackBuild, error) {
	if o.ABIBroken != nil {
		return nil, o.ABIBroken
	}
	var authorOps, abiOps []bundle.Op
	for _, op := range ops {
		if op.Origin == bundle.OriginABI {
			abiOps = append(abiOps, op)
		} else {
			authorOps = append(authorOps, op)
		}
	}
	stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
	b := &stackBuild{Files: opsToFiles(authorOps)}

	add := func(label string, files []client.StackFile, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		b.Files = append(b.Files, files...)
		return nil
	}

	assets, err := collectFileAssets(stackDir)
	if err := add("collect FILES/", assets, err); err != nil {
		return nil, err
	}
	if o.Data {
		packs, uploads, err := collectStorePacks(stackDir)
		if err := add("collect store packs", packs, err); err != nil {
			return nil, err
		}
		b.blobUploads = uploads
	}
	// SOURCES/ inlet packs are CODE, not data: small, human-authored config
	// that belongs with the ops it feeds, so it deploys alongside them. The
	// server upserts only the declared columns of tenant_sources.
	srcPacks, err := collectSourcePacks(stackDir)
	if err := add("collect SOURCES/", srcPacks, err); err != nil {
		return nil, err
	}
	// OUTLETS/, SANDBOXES/ and CAPS/ declarations are code too: one small
	// YAML each, inline in the draft, checked at validate and activate.
	outletFiles, err := collectOutletFiles(stackDir)
	if err := add("collect OUTLETS/", outletFiles, err); err != nil {
		return nil, err
	}
	sandboxFiles, err := collectSandboxFiles(stackDir)
	if err := add("collect SANDBOXES/", sandboxFiles, err); err != nil {
		return nil, err
	}
	capFiles, err := collectCapFiles(stackDir)
	if err := add("collect CAPS/", capFiles, err); err != nil {
		return nil, err
	}
	// Datasets are CODE (a query and its schema deploy together). Artifacts
	// enter as fingerprint-only rows; their bytes are made resident later.
	dsFiles, dsUploads, err := collectDatasetFiles(stackDir)
	if err := add("collect DATASETS/", dsFiles, err); err != nil {
		return nil, err
	}
	b.datasetUploads = dsUploads

	if o.Derived {
		// COMPUTES/ rows record each colocated compute's source, so the admin
		// can show it, when the chassis is new enough to accept them.
		if o.KeepSource {
			srcFiles, srcUploads := computeSourceRows(ops, o.Built)
			b.Files = append(b.Files, srcFiles...)
			b.sourceUploads = srcUploads
		}
		// STACKDIR/ is the stack's own directory, packed when one of its ops
		// names $TXCO_STACK_DIR (chassis/stackdir).
		treeFiles, treeUploads, err := stackTreeRows(dir, stack, authorOps, o.AllStacks)
		if err := add("stack tree", treeFiles, err); err != nil {
			return nil, err
		}
		b.treeUploads = treeUploads
	}

	if o.ABI != nil {
		abiRows, err := abiStackRows(o.ABI, abiOps)
		if err != nil {
			return nil, err
		}
		author := make([]string, len(b.Files))
		for i, f := range b.Files {
			author[i] = f.Path
		}
		abi := make([]string, len(abiRows))
		for i, f := range abiRows {
			abi[i] = f.Path
		}
		if conflicts := webabi.CheckOverlay(author, abi, o.ABI); len(conflicts) > 0 {
			return nil, fmt.Errorf("the Web ABI build %s collides with the stack's own tree:\n  %s",
				o.ABI.Path, strings.Join(conflicts, "\n  "))
		}
		b.Files = append(b.Files, abiRows...)
	}
	return b, nil
}

// abiStackRows is a Web ABI build's contribution to its stack: its ops,
// public/ as FILES/, a marker per public "_" root and immutable prefix, and
// the provenance file naming all of them.
func abiStackRows(d *webabi.Dir, ops []bundle.Op) ([]client.StackFile, error) {
	rows := opsToFiles(ops)
	pub, err := collectTreeAssetsAs(filepath.Join(d.Path, "public"), "FILES")
	if err != nil {
		return nil, fmt.Errorf("collect %s/public: %w", d.Path, err)
	}
	rows = append(rows, pub...)
	sum := sha256.Sum256([]byte(webabi.MarkerContent))
	for _, m := range d.Markers() {
		rows = append(rows, client.StackFile{Path: m, Content: webabi.MarkerContent, ContentHash: hex.EncodeToString(sum[:])})
	}
	owned := make([]string, len(rows))
	for i, f := range rows {
		owned[i] = f.Path
	}
	prov := webabi.Provenance(owned)
	psum := sha256.Sum256(prov)
	rows = append(rows, client.StackFile{Path: webabi.ProvenancePath, Content: string(prov), ContentHash: hex.EncodeToString(psum[:])})
	return rows, nil
}

// Hash is the stack's local manifest hash, the value the chassis records
// for the version these files make.
func (b *stackBuild) Hash() string { return localManifestHash(b.Files) }

// ensureResident makes every byte the file set references resident in the
// chassis's file store, before any draft names it: dataset artifacts,
// store-seed blobs, compute sources, the stack tree. Call it after the
// unchanged-skip checks, so an in-sync stack costs no probes.
func (b *stackBuild) ensureResident(ctx context.Context, c *client.Client, progress, stderr io.Writer) error {
	if len(b.datasetUploads) > 0 {
		if err := ensureDatasetBlobs(ctx, c, b.datasetUploads, progress, stderr); err != nil {
			return err
		}
	}
	if len(b.blobUploads) > 0 {
		if err := ensureBlobsResident(ctx, c, b.blobUploads, nil, progress, stderr); err != nil {
			return err
		}
	}
	if len(b.sourceUploads) > 0 {
		if err := ensureBlobsResident(ctx, c, b.sourceUploads, nil, progress, stderr); err != nil {
			return fmt.Errorf("compute source: %w", err)
		}
	}
	if len(b.treeUploads) > 0 {
		if err := ensureBlobsResident(ctx, c, b.treeUploads, nil, progress, stderr); err != nil {
			return fmt.Errorf("stack tree: %w", err)
		}
	}
	return nil
}

// abiWarnings lists what's worth telling the author about the selected
// stacks' builds: each build's own load warnings, and an author op in the
// producer band (scope ≥ webabi.ProducerScope) of a bound stack, which
// would race the build's terminal ops — the class of the stale nested
// fallback op that once froze a site.
func abiWarnings(ws *localWorkspace, selected map[string]bool) []string {
	var out []string
	for _, n := range sortedMapKeys(ws.ABI) {
		if !selected[n] {
			continue
		}
		for _, w := range ws.ABI[n].Warnings {
			out = append(out, n+": "+w)
		}
	}
	for _, op := range ws.Ops {
		if op.Origin == bundle.OriginABI || !selected[op.Stack] || op.Scope < webabi.ProducerScope {
			continue
		}
		if _, bound := ws.Bindings[op.Stack]; bound {
			out = append(out, fmt.Sprintf("%s: %s is in the Web ABI producer band (scope %d ≥ %d); the build's ops own that band — move it below %d",
				op.Stack, op.SourcePath, op.Scope, webabi.ProducerScope, webabi.ProducerScope))
		}
	}
	return out
}

// sortedMapKeys returns m's keys, sorted.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// markerFeature is the chassis feature a Web ABI build's markers need.
const markerFeature = "web-abi-markers"

// checkMarkerSupport refuses to send a selected stack's Web ABI markers to a
// chassis that doesn't advertise reading them (its /healthz features).
func checkMarkerSupport(ctx context.Context, ws *localWorkspace, selected map[string]bool, adminAddr string) error {
	need := ""
	for _, n := range sortedMapKeys(ws.ABI) {
		if selected[n] && len(ws.ABI[n].Markers()) > 0 {
			need = n
			break
		}
	}
	if need == "" {
		return nil
	}
	info, err := update.FetchServerInfo(ctx, adminAddr, "txco")
	if err != nil {
		return fmt.Errorf("%s: couldn't confirm the chassis at %s reads Web ABI markers: %v", need, adminAddr, err)
	}
	for _, f := range info.Features {
		if f == markerFeature {
			return nil
		}
	}
	return fmt.Errorf("%s: the chassis at %s (version %s) doesn't read Web ABI markers, so the build's \"_\" files would 404 — upgrade the chassis first", need, adminAddr, info.Version)
}
