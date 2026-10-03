package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/stackdir"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// TreeBundleSource reads a stack tree's bundle by its sha256: the chassis
// file store (filecas.Store satisfies it).
type TreeBundleSource interface {
	Get(ctx context.Context, hash string) ([]byte, error)
}

// cwdHome is the workspace root, as `cwd` may name it.
const cwdHome = "$HOME"

// workspaceCwd resolves `WITH cwd` into what a provider is handed, and says
// whether it names the run's stack tree:
//
//	omitted, "foo", "$HOME", "$HOME/foo"        beneath the workspace: "" or a clean relative path
//	"$TXCO_STACK_DIR", "$TXCO_STACK_DIR/foo"    beneath the stack tree: "" or a clean relative path, inTree
//	"/foo"                                      an absolute path on the computer, cleaned
//
// The two roots are tokens the chassis resolves; txcl does no `$` expansion,
// and no other `$` value is accepted. ".." is refused only when it would
// climb out of its root: "$HOME/a/../b" is fine, "$HOME/../etc" is not, and an
// absolute path is simply cleaned ("/var/tmp/../tmp" is "/tmp"). This is
// resolution, not authorization: an absolute cwd grants nothing a `cd` in the
// command could not, and what the command may touch is the computer's
// business.
func workspaceCwd(raw string) (cwd string, inTree bool, err error) {
	if raw == "" {
		return "", false, nil
	}
	if strings.HasPrefix(raw, "/") {
		return path.Clean(raw), false, nil
	}
	rest, root := raw, "the workspace"
	switch {
	case raw == stackdir.Token || strings.HasPrefix(raw, stackdir.Token+"/"):
		rest, root, inTree = strings.TrimPrefix(raw, stackdir.Token), "the stack tree", true
	case raw == cwdHome || strings.HasPrefix(raw, cwdHome+"/"):
		rest = strings.TrimPrefix(raw, cwdHome)
	case strings.HasPrefix(raw, "$"):
		return "", false, fmt.Errorf("WITH cwd %q: the roots are %s and %s; otherwise give a path relative to the workspace, or an absolute one", raw, cwdHome, stackdir.Token)
	}
	// "./" first, so a leading "/" after a root ("$HOME//x") stays beneath it.
	clean := path.Clean("./" + rest)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("WITH cwd %q climbs out of %s", raw, root)
	}
	if clean == "." {
		clean = ""
	}
	return clean, inTree, nil
}

// stackTree finds the run's own stack tree: the STACKDIR/ row of the stack's
// active version in the opstack snapshot this run pinned — the live mirror it
// started with, or a continuation's frozen copy — so an activation mid-run
// never changes the tree it execs against. A failure is a code and a message
// for workspace.error.
func (pu *Unit) stackTree(ctx context.Context, tenant, stack string) (*workspace.Tree, string, string) {
	const code = workspace.CodeStackDirUnavailable
	if pu.TreeBundles == nil {
		return nil, code, "this chassis has no file store to read stack trees from"
	}
	var hash string
	err := pu.opstackDB(ctx).QueryRowContext(ctx, `
		SELECT sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND s.name = ? AND sf.path LIKE 'STACKDIR/%'
		 ORDER BY sf.path
		 LIMIT 1`, tenant, stack).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, code, fmt.Sprintf("stack %s has no stack tree in this run: apply it with a txco that packs %s, or start the run again if it predates the tree", stack, stackdir.Token)
	}
	if err != nil {
		return nil, code, "look up the stack tree: " + err.Error()
	}
	if !stackdir.ValidDigest(hash) {
		return nil, code, "the stack tree's row is malformed"
	}
	bundles := pu.TreeBundles
	return &workspace.Tree{
		Digest: hash,
		Open:   func(ctx context.Context) ([]byte, error) { return bundles.Get(ctx, hash) },
	}, "", ""
}
