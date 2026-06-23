// Package bundle walks an on-disk OPS/ tree and produces a flat list of
// (stack, scope, name, txcl, mock_req, mock_res) records suitable for POSTing
// to the admin /v1/ops/import endpoint.
//
// # Layout model: directories organize, the stack root is the only boundary
//
// Under <root>/OPS/, a stack root is the path prefix up to (but excluding) the
// first *numbered* directory. Everything below that numbered directory is for
// developer organization — nested folders never create a new stack. A `.txcl`
// leaf may live at any depth beneath a numbered directory.
//
//	OPS/<stack>/<scope>/<name>.txcl
//	OPS/<stack>/<scope>_<LABEL>/<name>.txcl
//	OPS/<stack>/<scope>_<LABEL>/<org>/<name>.txcl          (org dir is cosmetic)
//	OPS/<stack>/<scope>_<LABEL>/<inner-scope>/<name>.txcl  (nearest number wins)
//
// A numbered directory is any basename starting with digits, optionally
// followed by `_`, `-`, or whitespace and a human-facing label (ignored):
// `1000`, `1000_setup`, `1000-setup`, `1000 setup` all mean step 1000. Leading
// zeros are stripped (`0100` and `100` both mean 100).
//
// Each leaf's effective step is its NEAREST numbered ancestor (the deepest one
// on its path); there is no stride arithmetic. Its name is the path from that
// numbered ancestor down to the file, with separators flattened to `_`, so a
// leaf directly under the numbered dir keeps its bare basename (the common,
// historical case parses byte-identically).
//
// Examples:
//
//	OPS/website/100/resonator.txcl              -> stack=website,        scope=100,  name=resonator
//	OPS/website/0100_SETUP/init.txcl            -> stack=website,        scope=100,  name=init
//	OPS/website/0100_SETUP/audit.txcl           -> stack=website,        scope=100,  name=audit (parallel rule)
//	OPS/website/0100_SETUP/misc/normalize.txcl  -> stack=website,        scope=100,  name=misc_normalize
//	OPS/website/1000_setup/1010_config/load.txcl-> stack=website,        scope=1010, name=load (nearest wins)
//	OPS/website/canary/0100_SETUP/init.txcl     -> stack=website/canary, scope=100,  name=init
//
// A `_`-prefixed organization directory below the numbered dir disables every
// leaf beneath it (park a draft in `_disabled/` to keep it out of the deploy).
//
// A `.txcl` with no numbered ancestor cannot be placed (no step, and no way to
// split stack from path), and two leaves that flatten to the same
// (stack, scope, name) collide. Both are reported via WalkDiag so the deploy
// path can stop loudly instead of silently dropping or failing server-side.
// The plain Walk/WalkFS entry points discard diagnostics and keep their
// historical "skip what doesn't fit" behavior for read-only callers.
//
// Optional sibling files `mock-request.json` and `mock-response.json` in the
// leaf's own directory attach to *all* rules in that directory.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Op is one row in a bundle. Mirrors the admin/ops.go OpRecord shape.
type Op struct {
	Stack   string `json:"stack"`
	Scope   int    `json:"scope"`
	Name    string `json:"name"`
	Txcl    string `json:"txcl"`
	MockReq string `json:"mock_req,omitempty"`
	MockRes string `json:"mock_res,omitempty"`

	// SourcePath is the absolute path to the .txcl file. Set by Walk for
	// diagnostic purposes; not serialized into the wire format.
	SourcePath string `json:"-"`
}

// Diag is a non-fatal-at-walk-time finding about a `.txcl` leaf that could
// not be turned into a well-formed Op (no numbered ancestor) or that would
// collide with another leaf. The deploy path (apply/push) decides whether to
// treat it as an error; read-only callers via Walk/WalkFS ignore them.
type Diag struct {
	// Stack is the resolved stack when known (collisions), or "" when the
	// leaf has no numbered ancestor and therefore no derivable stack.
	Stack string
	// Path is the source `.txcl` path (within fsys).
	Path string
	// Msg is a human-facing, already-formatted explanation.
	Msg string
}

// numberedDirRE matches a directory basename that denotes a step: digits,
// optionally followed by `_`/`-`/whitespace and a human label. Capture group
// 1 is the digits. A bare numeric basename (`1000`) matches with no label.
var numberedDirRE = regexp.MustCompile(`^(\d+)(?:[ _-].*)?$`)

// numberedDir reports whether seg is a numbered step directory and, if so,
// its integer step.
func numberedDir(seg string) (int, bool) {
	m := numberedDirRE.FindStringSubmatch(seg)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// SystemSegment splits a stack name whose first path segment is a
// `_`-prefixed chassis-local tenant (e.g. "_sys/boot" -> slug="_sys",
// rest="boot"). ok is false for ordinary application stacks and for a
// bare "_slug" with no stack under it. This is the single
// discriminator between the two load paths: `_*` stacks are loaded
// locally/trusted by the chassis (chassis/sysops); everything else is
// an application stack pushed through the admin API.
func SystemSegment(stack string) (slug, rest string, ok bool) {
	i := strings.IndexByte(stack, '/')
	if i <= 0 {
		return "", "", false
	}
	head := stack[:i]
	if !strings.HasPrefix(head, "_") {
		return "", "", false
	}
	tail := stack[i+1:]
	if tail == "" {
		return "", "", false
	}
	return head, tail, true
}

// Walk discovers application rules under <root>/OPS/ on the local
// filesystem. `_`-prefixed system stacks (e.g. OPS/_sys/...) are
// excluded — they are loaded locally by the chassis, never pushed via
// the admin API, so every CLI/apply caller transparently skips them.
// Returns an empty slice (not an error) when OPS/ is missing.
// Diagnostics are discarded; use WalkDiag to surface them.
func Walk(root string) ([]Op, error) {
	return WalkFS(os.DirFS(root), ".")
}

// WalkDiag is Walk plus the diagnostics (no-step leaves, name collisions) the
// deploy path surfaces as errors. ops are still returned alongside.
func WalkDiag(root string) ([]Op, []Diag, error) {
	return walkFS(os.DirFS(root), ".", false)
}

// WalkFS returns application ops only (system stacks excluded). Diagnostics
// are discarded; use WalkFSDiag to surface them.
func WalkFS(fsys fs.FS, root string) ([]Op, error) {
	ops, _, err := walkFS(fsys, root, false)
	return ops, err
}

// WalkFSDiag is WalkFS plus diagnostics.
func WalkFSDiag(fsys fs.FS, root string) ([]Op, []Diag, error) {
	return walkFS(fsys, root, false)
}

// WalkSystemFS returns ONLY `_`-prefixed system ops (op.Stack keeps
// the full "_slug/stack" form; callers split via SystemSegment).
// Used by chassis/sysops to load trusted on-disk system opstacks from
// the same OPS/ tree. Diagnostics are discarded.
func WalkSystemFS(fsys fs.FS, root string) ([]Op, error) {
	ops, _, err := walkFS(fsys, root, true)
	return ops, err
}

// walkFS is the fs.FS-based core. root is the directory *within* fsys
// that contains the `OPS/` tree (use "." for the fsys root). wantSystem
// selects which half of the tree to return: app stacks or `_`-prefixed
// system stacks. Returns (nil, nil, nil) when OPS/ is absent so an
// empty/optional bundle is not an error.
func walkFS(fsys fs.FS, root string, wantSystem bool) ([]Op, []Diag, error) {
	opsRoot := path.Join(root, "OPS")
	info, err := fs.Stat(fsys, opsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("expected directory at %s, found file", opsRoot)
	}

	var ops []Op
	var diags []Diag
	// seen maps a resolved (stack, scope, name) identity to the first source
	// path that produced it, so a second producer becomes a collision diag.
	seen := map[string]string{}

	err = fs.WalkDir(fsys, opsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".txcl") {
			return nil
		}

		rel := strings.TrimPrefix(p, opsRoot+"/")
		segs := strings.Split(rel, "/")
		fileIdx := len(segs) - 1
		base := strings.TrimSuffix(segs[fileIdx], ".txcl")
		dirs := segs[:fileIdx]

		// Find the shallowest numbered dir (delimits the stack root) and the
		// deepest one (sets the step). They coincide in the common one-level
		// layout.
		firstNum, lastNum := -1, -1
		var scope int
		for i, seg := range dirs {
			if n, ok := numberedDir(seg); ok {
				if firstNum < 0 {
					firstNum = i
				}
				lastNum = i
				scope = n
			}
		}

		// No numbered ancestor: the leaf has no step and no way to split a
		// stack from its path. Only the app walk reports it (a system loader
		// shouldn't error on application strays); historically silently dropped.
		if firstNum < 0 {
			if !wantSystem {
				diags = append(diags, Diag{Path: p, Msg: fmt.Sprintf(
					"%s: no numbered step directory in its path; place it under "+
						"<stack>/<NNNN>_label/ — it was NOT deployed", rel)})
			}
			return nil
		}

		stack := strings.Join(dirs[:firstNum], "/")
		if stack == "" {
			// A top-level numeric directory reads as a step, leaving no stack.
			if !wantSystem {
				diags = append(diags, Diag{Path: p, Msg: fmt.Sprintf(
					"%s: no stack name before the first numbered directory %q — a "+
						"top-level numeric directory reads as a step, not a stack", rel, dirs[firstNum])})
			}
			return nil
		}

		// A `_`-prefixed organization dir below the numbered step disables
		// everything beneath it (the park-a-draft affordance). Dirs above the
		// first numbered dir are stack segments (`_mail`, `_sys`) — not checked.
		for _, seg := range dirs[firstNum+1:] {
			if strings.HasPrefix(seg, "_") {
				return nil
			}
		}

		// Split the tree by the `_`-prefix discriminator (on the stack root).
		if _, _, isSys := SystemSegment(stack); isSys != wantSystem {
			return nil
		}

		// Name = path from the nearest numbered ancestor down to the file,
		// flattened to the op-name charset (separators -> `_`). A leaf directly
		// under the numbered dir keeps its bare basename.
		nameParts := append(append([]string{}, dirs[lastNum+1:]...), base)
		name := strings.Join(nameParts, "_")

		txcl, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", p, rerr)
		}

		key := stack + "\x00" + strconv.Itoa(scope) + "\x00" + name
		if prior, dup := seen[key]; dup && name != "" {
			diags = append(diags, Diag{Stack: stack, Path: p, Msg: fmt.Sprintf(
				"two files flatten to the same operation %s/%d/%s:\n    %s\n    %s\n"+
					"  rename one or move it under a different step", stack, scope, name, prior, p)})
			return nil
		}
		seen[key] = p

		op := Op{
			Stack:      stack,
			Scope:      scope,
			Name:       name,
			Txcl:       string(txcl),
			SourcePath: p,
		}

		// Read sibling mock files if present. They attach to every rule in the
		// leaf's own directory.
		dir := path.Dir(p)
		if mr, ok := readSibling(fsys, dir, "mock-request.json"); ok {
			op.MockReq = mr
		}
		if mr, ok := readSibling(fsys, dir, "mock-response.json"); ok {
			op.MockRes = mr
		}

		ops = append(ops, op)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return ops, diags, nil
}

func readSibling(fsys fs.FS, dir, name string) (string, bool) {
	b, err := fs.ReadFile(fsys, path.Join(dir, name))
	if err != nil {
		return "", false
	}
	// If it parses as JSON, re-serialize compactly for stable comparison
	// against server state. If it doesn't, pass through as-is.
	var any interface{}
	if json.Unmarshal(b, &any) == nil {
		canon, err := json.Marshal(any)
		if err == nil {
			return string(canon), true
		}
	}
	s := strings.TrimSpace(string(b))
	return s, true
}
