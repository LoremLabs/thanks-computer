// Package bundle walks an on-disk OPS/ tree and produces a flat list of
// (stack, scope, name, txcl, mock_req, mock_res) records suitable for POSTing
// to the admin /v1/ops/import endpoint.
//
// Path conventions accepted by the walker, where every leaf is `<name>.txcl`
// and each scope directory may hold any number of them (parallel rules at
// the same stage):
//
//   OPS/<stack>/<scope>/<name>.txcl
//   OPS/<stack>/<scope>_<DESCRIPTION>/<name>.txcl
//
// The scope dir is the integer prefix; an optional `_DESCRIPTION` suffix is
// for human readability and is ignored by the chassis. Leading zeros are
// stripped (`0100` and `100` both mean scope 100).
//
// Examples:
//
//   OPS/website/100/resonator.txcl              -> stack=website,           scope=100, name=resonator
//   OPS/website/0100_SETUP/init.txcl            -> stack=website,           scope=100, name=init
//   OPS/website/0100_SETUP/audit.txcl           -> stack=website,           scope=100, name=audit  (parallel rule)
//   OPS/website/canary/0100_SETUP/init.txcl     -> stack=website/canary,    scope=100, name=init
//
// Optional sibling files `mock-request.json` and `mock-response.json` in the
// same scope directory attach to *all* rules at that scope. (Per-rule mocks
// can come later if there's demand.)
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

// pathRE matches any `<stack>/<scope>[_DESCRIPTION]/<name>.txcl` under OPS/.
//
// The non-greedy `(.+?)` plus the trailing structure ensures the rightmost
// numeric directory wins as the scope, even when the stack name contains
// digits (e.g. `OPS/v2/0100/x.txcl` parses to stack="v2", scope=100).
var pathRE = regexp.MustCompile(`^(.+?)/(\d+)(?:_[^/]*)?/([^/]+)\.txcl$`)

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
func Walk(root string) ([]Op, error) {
	return WalkFS(os.DirFS(root), ".")
}

// WalkFS returns application ops only (system stacks excluded).
func WalkFS(fsys fs.FS, root string) ([]Op, error) {
	return walkFS(fsys, root, false)
}

// WalkSystemFS returns ONLY `_`-prefixed system ops (op.Stack keeps
// the full "_slug/stack" form; callers split via SystemSegment).
// Used by chassis/sysops to load trusted on-disk system opstacks from
// the same OPS/ tree.
func WalkSystemFS(fsys fs.FS, root string) ([]Op, error) {
	return walkFS(fsys, root, true)
}

// walkFS is the fs.FS-based core. root is the directory *within* fsys
// that contains the `OPS/` tree (use "." for the fsys root). wantSystem
// selects which half of the tree to return: app stacks or `_`-prefixed
// system stacks. Returns (nil, nil) when OPS/ is absent so an
// empty/optional bundle is not an error.
func walkFS(fsys fs.FS, root string, wantSystem bool) ([]Op, error) {
	opsRoot := path.Join(root, "OPS")
	info, err := fs.Stat(fsys, opsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("expected directory at %s, found file", opsRoot)
	}

	var ops []Op
	err = fs.WalkDir(fsys, opsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".txcl") {
			return nil
		}

		rel := strings.TrimPrefix(p, opsRoot+"/")

		m := pathRE.FindStringSubmatch(rel)
		if m == nil {
			return nil
		}
		stack := m[1]
		scope, err := strconv.Atoi(m[2])
		if err != nil {
			return nil
		}
		name := m[3]

		// Split the tree by the `_`-prefix discriminator.
		if _, _, isSys := SystemSegment(stack); isSys != wantSystem {
			return nil
		}

		txcl, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}

		op := Op{
			Stack:      stack,
			Scope:      scope,
			Name:       name,
			Txcl:       string(txcl),
			SourcePath: p,
		}

		// Read sibling mock files if present. They attach to every rule in
		// the same scope dir.
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
		return nil, err
	}
	return ops, nil
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
