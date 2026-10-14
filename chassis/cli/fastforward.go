package cli

// The fast-forward guard for the code deploy path — git's "! [rejected]
// (non-fast-forward)" for stacks. A workspace records which version it last
// synced (`.txco/<stack>.state.json`, written by pull/apply/push/dev and
// kept current by data apply/activate). If the chassis's active version is
// no longer that one — someone deployed, rolled back, or edited in the admin
// UI — pushing the local tree would silently supersede their change, so
// `apply`/`push`/`data apply` refuse unless --force. The chassis enforces the
// same rule under its row lock via ActivateOpts.ExpectedActive (409
// stack_moved), which closes the race after the pre-check. A workspace with
// no baseline (first apply from a fresh checkout, CI) cannot detect a move
// and proceeds — like a clone that never fetched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
	"github.com/loremlabs/thanks-computer/chassis/computesrc"
	"github.com/loremlabs/thanks-computer/chassis/stackdir"
)

// remoteMove describes a chassis whose active version is not the one this
// workspace last synced.
type remoteMove struct {
	Synced   int64 // the version this workspace last synced
	Active   int64 // the chassis's active version now (0 = none)
	Rollback bool  // active < synced
}

// remoteMoved reports how the chassis moved relative to the workspace's
// baseline, or nil when they agree or no baseline exists.
func remoteMoved(saved *state.State, rec *client.StackRecord) *remoteMove {
	if saved == nil || rec == nil {
		return nil
	}
	var active int64
	if rec.ActiveVersion != nil {
		active = *rec.ActiveVersion
	}
	if active == saved.VersionNumber {
		return nil
	}
	return &remoteMove{Synced: saved.VersionNumber, Active: active, Rollback: active < saved.VersionNumber}
}

// refusedMovedMessage is the operator-facing refusal: what moved, and the
// three ways out (look, take theirs, overwrite).
func refusedMovedMessage(cmd, stack string, m *remoteMove) string {
	what := fmt.Sprintf("chassis active is v%d but this workspace last synced v%d — someone deployed (or edited in the admin UI) since", m.Active, m.Synced)
	if m.Rollback {
		what = fmt.Sprintf("chassis active is v%d, rolled back from the v%d this workspace last synced", m.Active, m.Synced)
	}
	return fmt.Sprintf("%s: %s: refused — %s.\n  `txco diff %s` shows what changed; `txco pull %s --force` takes the chassis version (discards local edits); `%s --force` overwrites the chassis.\n",
		cmd, stack, what, stack, stack, cmd)
}

// isStackMovedErr recognises the chassis-side refusal (the race the
// pre-check can't see).
func isStackMovedErr(err error) bool {
	var he *client.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusConflict && he.Code == "stack_moved"
	}
	return false
}

// expectedActiveFor is the ExpectedActive a deploy should send after its
// pre-check passed: the version it just observed (0 when none), so the
// chassis refuses if the ref moves in the window before the activate.
func expectedActiveFor(rec *client.StackRecord) *int64 {
	var n int64
	if rec != nil && rec.ActiveVersion != nil {
		n = *rec.ActiveVersion
	}
	return &n
}

// stateKeyFor is the per-target state key for the chassis this client
// talks to — every state.Load/Save in the deploy paths goes through it so
// dev and prod each keep their own baseline (the pre-keyed file made every
// dev↔prod alternation read the other environment's number).
func stateKeyFor(c *client.Client) string {
	return state.Key(c.Addr(), c.TenantSlug())
}

// recordSyncedVersion bumps the workspace's synced version after a deploy
// that did not change the code tree (data apply, activate): the workspace
// now "knows" v<n>, so its own action never trips the fast-forward guard.
// The code manifest is left as it was (pull's dirty check keys on it).
// Best-effort: a state-write failure never fails the deploy.
func recordSyncedVersion(dir, stack, key string, n int64) {
	saved, _ := state.Load(dir, stack, key)
	if saved == nil {
		saved = &state.State{}
	}
	saved.VersionNumber, saved.ParentVersionNumber = n, n
	_ = state.Save(dir, stack, key, *saved)
}

// The content check behind the guard. The version number a workspace
// recorded is a hint, not the truth: the chassis keeps every version's
// per-file hashes (GET versions/{n}, no content), so where the local tree
// stands can be worked out from content alone. A new machine has no
// baseline and an old one's goes stale when someone deploys from
// elsewhere; neither should stop a tree that already holds the chassis's
// changes. With B the recorded baseline, A the active version and L the
// local build (push's exact basis):
//
//	theirs = paths that differ between B and A
//	ours   = paths that differ between L and A
//
// ours empty is in sync. theirs ∩ ours empty is a fast-forward: the tree
// has everything the chassis did since B, and its own edits touch none of
// it, so B can move to A and the push go ahead. Anything else is a
// conflict, named path by path. With no usable B, a tree that is exactly an
// older version is behind; otherwise there is no baseline, as before.

// ffKind is what the content check concluded about one stack.
type ffKind int

const (
	ffInSync   ffKind = iota // the tree is the active version
	ffForward                // the tree has every change since the baseline; its edits touch none
	ffConflict               // the chassis changed paths since the baseline that the tree differs on
	ffBehind                 // no baseline, and the tree is an older version exactly
	ffNoBase                 // no usable baseline, and the tree differs from the active version
)

// ffResult is the content check's verdict for one stack. Paths are the
// chassis's (numeric scope, flattened name); displayPaths maps them back.
type ffResult struct {
	Kind   ffKind
	Base   int64 // the baseline the check used: the recorded one, or the version the tree matched (0 = none)
	Active int64
	// Edits are the paths the tree differs from the active version on.
	Edits []string
	// Conflicts are the paths the chassis changed since Base that the tree
	// differs on (ffConflict), or everything it changed since (ffBehind).
	Conflicts []string
}

// ffRecentVersions bounds the versions the behind check reads.
const ffRecentVersions = 20

// stackDirMember prefixes the files of a STACKDIR/ bundle once expanded.
const stackDirMember = "STACKDIR:"

// isDataPackPath reports whether a stack_files path is a store-seed pack
// row: data, deployed by `txco data apply`, outside the code a push
// compares. The same five prefixes the chassis leaves out of a stack's
// code manifest (server/admin/stacks.go codeManifestHash).
func isDataPackPath(p string) bool {
	for _, pre := range []string{"VECTORS/", "KV/", "BLOBS/", "CALENDARS/", "CONTACTS/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// fileHash is a stack_files row's content hash: the one it carries, or the
// sha256 of its content (ops carry none; their content is the raw text).
func fileHash(f client.StackFile) string {
	if f.ContentHash != "" {
		return f.ContentHash
	}
	sum := sha256.Sum256([]byte(f.Content))
	return hex.EncodeToString(sum[:])
}

// codeHashes maps a file set's code rows to their hashes. Data packs are
// left out (not code), and so are COMPUTES/ rows: they are named by wasm
// digest, and a compute that changed already shows as a changed
// compute://sha256/... reference in the op row that uses it.
func codeHashes(files []client.StackFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		if isDataPackPath(f.Path) || computesrc.IsPath(f.Path) {
			continue
		}
		out[f.Path] = fileHash(f)
	}
	return out
}

// diffPaths is the set of paths whose hash differs between a and b,
// including a path present on one side only.
func diffPaths(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for p, h := range a {
		if b[p] != h {
			out[p] = true
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out[p] = true
		}
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ffVerdict is the rule on three hash maps. base nil = no usable baseline.
func ffVerdict(base, active, local map[string]string) (kind ffKind, edits, conflicts []string) {
	ours := diffPaths(local, active)
	if len(ours) == 0 {
		return ffInSync, nil, nil
	}
	edits = sortedSet(ours)
	if base == nil {
		return ffNoBase, edits, nil
	}
	both := map[string]bool{}
	for p := range diffPaths(base, active) {
		if ours[p] {
			both[p] = true
		}
	}
	if len(both) == 0 {
		return ffForward, edits, nil
	}
	return ffConflict, edits, sortedSet(both)
}

// fastForward runs the content check for one stack against the version the
// chassis has active. saved may be nil (no baseline). It reads hash lists
// only — never content — except a STACKDIR/ bundle whose digest differs
// between the sides, which it opens to compare the stack's tree file by
// file. An error means the check could not run; callers fall back to the
// version-number rule.
func fastForward(ctx context.Context, c *client.Client, dir, stack string, saved *state.State, rec *client.StackRecord, build *stackBuild) (*ffResult, error) {
	if rec == nil || rec.ActiveVersion == nil {
		return nil, errors.New("the stack has no active version")
	}
	res := &ffResult{Active: *rec.ActiveVersion}
	av, err := c.GetVersion(ctx, stack, res.Active, false)
	if err != nil {
		return nil, fmt.Errorf("read v%d: %w", res.Active, err)
	}
	active, local := codeHashes(av.Files), codeHashes(build.Files)
	var base map[string]string
	if saved != nil {
		if bv, berr := c.GetVersion(ctx, stack, saved.VersionNumber, false); berr == nil {
			base, res.Base = codeHashes(bv.Files), saved.VersionNumber
		}
	}
	trees := newTreeReader(ctx, c, dir, build)
	if err := trees.expand(base, active, local); err != nil {
		return nil, err
	}
	res.Kind, res.Edits, res.Conflicts = ffVerdict(base, active, local)
	if res.Kind != ffNoBase {
		return res, nil
	}
	// No usable baseline: a tree that is exactly an activated older version
	// is behind it. The version list carries each manifest, so one call
	// finds it; a version minted by a code push records the same hash the
	// local build has.
	vs, err := c.ListVersions(ctx, stack)
	if err != nil {
		return res, nil
	}
	localHash := build.Hash()
	for i, v := range vs {
		if i >= ffRecentVersions {
			break
		}
		if v.VersionNumber == res.Active || v.ActivatedAt == nil || v.ManifestHash != localHash {
			continue
		}
		bv, berr := c.GetVersion(ctx, stack, v.VersionNumber, false)
		if berr != nil {
			break
		}
		matched := codeHashes(bv.Files)
		activeAgain := codeHashes(av.Files)
		if err := trees.expand(matched, activeAgain); err != nil {
			break
		}
		res.Kind, res.Base = ffBehind, v.VersionNumber
		res.Conflicts = sortedSet(diffPaths(matched, activeAgain))
		break
	}
	return res, nil
}

// treeReader opens STACKDIR/ bundles by digest: the workspace's own cache
// (.txco/stackdir, where every bundle this machine packed is kept) first,
// then the chassis's file store.
type treeReader struct {
	ctx   context.Context
	c     *client.Client
	dir   string
	local map[string]string // digest → a bundle file on disk
	seen  map[string]map[string]string
}

func newTreeReader(ctx context.Context, c *client.Client, dir string, build *stackBuild) *treeReader {
	t := &treeReader{ctx: ctx, c: c, dir: dir, local: map[string]string{}, seen: map[string]map[string]string{}}
	if build != nil {
		for _, u := range build.treeUploads {
			t.local[u.Hash] = u.LocalPath
		}
	}
	return t
}

// expand replaces each map's STACKDIR/ row with the bundle's files
// (STACKDIR:<path> → hash) when the maps don't all carry the same bundle.
// Identical rows compare equal as they are, so nothing is read then. A nil
// map is skipped.
func (t *treeReader) expand(maps ...map[string]string) error {
	digests := map[string]bool{}
	carriers := 0
	for _, m := range maps {
		if m == nil {
			continue
		}
		d := ""
		for p := range m {
			if stackdir.IsPath(p) {
				d = stackdir.DigestFromPath(p)
			}
		}
		digests[d] = true
		if d != "" {
			carriers++
		}
	}
	if carriers == 0 || len(digests) < 2 {
		return nil
	}
	for _, m := range maps {
		if m == nil {
			continue
		}
		for p := range m {
			if !stackdir.IsPath(p) {
				continue
			}
			members, err := t.members(stackdir.DigestFromPath(p))
			if err != nil {
				return fmt.Errorf("read the stack tree %s: %w", p, err)
			}
			delete(m, p)
			for mp, h := range members {
				m[mp] = h
			}
		}
	}
	return nil
}

// members lists one bundle's files with their hashes (an executable file's
// hash is marked, so a mode change is a change).
func (t *treeReader) members(digest string) (map[string]string, error) {
	if m, ok := t.seen[digest]; ok {
		return m, nil
	}
	data, err := t.bundle(digest)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("bundle does not match its digest")
	}
	m := map[string]string{}
	if err := stackdir.Unpack(data, func(f stackdir.File) error {
		fs := sha256.Sum256(f.Content)
		h := hex.EncodeToString(fs[:])
		if f.Exec {
			h += "+x"
		}
		m[stackDirMember+f.Path] = h
		return nil
	}); err != nil {
		return nil, err
	}
	t.seen[digest] = m
	return m, nil
}

func (t *treeReader) bundle(digest string) ([]byte, error) {
	for _, p := range []string{t.local[digest], filepath.Join(t.dir, state.Dir, "stackdir", digest+stackdir.Ext)} {
		if p == "" {
			continue
		}
		if b, err := os.ReadFile(p); err == nil {
			return b, nil
		}
	}
	rc, _, err := t.c.GetBlob(t.ctx, digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// A bundle holds at most MaxTreeBytes of files; the tar headers add a
	// little. Read one byte past a generous cap to notice a bigger one.
	const limit = 2 * stackdir.MaxTreeBytes
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errors.New("bundle is over the tree cap")
	}
	return b, nil
}

// authorPaths turns chassis paths into what the author recognises: an op
// row becomes the file it was walked from, and anything else (a tree
// member, FILES/, a declaration) its place under OPS/<stack>/.
func authorPaths(stack string, paths []string, ops []bundle.Op) []string {
	src := map[string]string{}
	for _, op := range ops {
		if op.SourcePath != "" {
			src[fmt.Sprintf("%d/%s.txcl", op.Scope, op.Name)] = op.SourcePath
		}
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if s := src[p]; s != "" {
			out = append(out, filepath.ToSlash(s))
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Join("OPS", stack, strings.TrimPrefix(p, stackDirMember))))
	}
	return out
}

// indentList renders paths one per line under a message, at most limit,
// then a count of the rest.
func indentList(paths []string, limit int) string {
	var b strings.Builder
	for i, p := range paths {
		if i == limit {
			fmt.Fprintf(&b, "    … and %d more\n", len(paths)-limit)
			break
		}
		fmt.Fprintf(&b, "    %s\n", p)
	}
	return b.String()
}

// forceHint is the command that overwrites the chassis anyway.
func forceHint(cmd, stack string) string {
	if cmd == "push" {
		return "`txco push " + stack + " --force`"
	}
	return "`txco " + cmd + " --force`"
}

// refusedConflictMessage: the chassis changed files since the baseline, and
// the tree differs on them — pushing would drop those changes.
func refusedConflictMessage(cmd, stack string, ff *ffResult, ops []bundle.Op) string {
	moved := fmt.Sprintf("the chassis moved from v%d (this workspace's baseline) to v%d", ff.Base, ff.Active)
	if ff.Active < ff.Base {
		moved = fmt.Sprintf("the chassis rolled back from v%d (this workspace's baseline) to v%d", ff.Base, ff.Active)
	}
	n := len(ff.Conflicts)
	return fmt.Sprintf("%s: %s: refused — %s and changed %d file%s this tree also differs on:\n%s  Bring those changes in first (git pull; `txco diff %s` shows them), or %s overwrites the chassis.\n",
		cmd, stack, moved, n, pluralS(n), indentList(authorPaths(stack, ff.Conflicts, ops), 10), stack, forceHint(cmd, stack))
}

// refusedBehindMessage: the tree is an older version exactly — pushing it
// would put that version's content back.
func refusedBehindMessage(cmd, stack string, ff *ffResult, ops []bundle.Op) string {
	where := fmt.Sprintf("the chassis has moved on to v%d", ff.Active)
	if ff.Active < ff.Base {
		where = fmt.Sprintf("the chassis rolled back from it to v%d", ff.Active)
	}
	return fmt.Sprintf("%s: %s: refused — this tree is v%d exactly, and %s (%s):\n%s  Bring the newer version in first (git pull), or %s puts v%d's content back on the chassis.\n",
		cmd, stack, ff.Base, where, filesDiffer(len(ff.Conflicts)), indentList(authorPaths(stack, ff.Conflicts, ops), 10), forceHint(cmd, stack), ff.Base)
}

// filesDiffer is "1 file differs" / "3 files differ".
func filesDiffer(n int) string {
	if n == 1 {
		return "1 file differs"
	}
	return fmt.Sprintf("%d files differ", n)
}

// localTreeHash is the stack's hash on `txco status`'s basis (see
// state.State.TreeHash), or "" when it can't be built.
func localTreeHash(ws *localWorkspace, stack string) string {
	b, err := buildStackFiles(ws.Dir, stack, opsForStack(ws.Ops, stack), ws.withABI(stack, collectOpts{}))
	if err != nil {
		return ""
	}
	return b.Hash()
}

// recordInSync saves v<n> as the baseline of a stack whose local tree is
// that version exactly (an unchanged push, a sync): the pointer move that
// lets a fresh checkout's first push establish what only a pull used to.
// A baseline already recorded this way is left alone. Best-effort.
func recordInSync(ws *localWorkspace, key, stack string, n int64, manifest string) {
	if saved, _ := state.Load(ws.Dir, stack, key); saved != nil &&
		saved.VersionNumber == n && saved.ManifestHash == manifest && saved.TreeHash != "" {
		return
	}
	_ = state.Save(ws.Dir, stack, key, state.State{
		VersionNumber: n, ParentVersionNumber: n,
		ManifestHash: manifest, TreeHash: localTreeHash(ws, stack),
	})
}

// recordBaseline saves v<n> as the baseline of a stack whose tree is that
// version plus local edits (a fast-forward, an adopt). The manifest is the
// version's, not the tree's — `apply --changed` must still see the edits
// as changes — and there is no tree hash, so status reads the edits.
func recordBaseline(dir, key, stack string, n int64, manifest string) {
	_ = state.Save(dir, stack, key, state.State{VersionNumber: n, ParentVersionNumber: n, ManifestHash: manifest})
}
