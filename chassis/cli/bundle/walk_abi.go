package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/txcl/include"
)

// WalkABI reads the ops/ tree of a Web ABI directory (fsys rooted at it)
// as ops of stack. It is stricter than the OPS/ walk, because a producer
// generates this tree and a mistake in it should stop the build:
//
//   - the top of ops/ holds only numbered scope directories (labels allowed:
//     900000_spa/), so an ABI build can never create a nested stack;
//   - a "_" directory is an error, not the OPS/ walk's silent disable;
//   - an op name must be a valid operation name;
//   - &include stays inside ops/;
//   - an op may not use $TXCO_STACK_DIR (the stack tree is the author's).
//
// Each op's SourcePath is sourcePrefix + "/" + its path in fsys, so
// diagnostics and colocated computes resolve against the real file (pass
// the ABI directory relative to the workspace root, or absolute). An absent
// ops/ is no ops.
func WalkABI(fsys fs.FS, stack, sourcePrefix string) ([]Op, error) {
	const root = "ops"
	info, err := fs.Stat(fsys, root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s/ops: not a directory", sourcePrefix)
	}
	src := func(p string) string { return path.Join(sourcePrefix, p) }

	var ops []Op
	seen := map[string]string{}
	err = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		rel := strings.TrimPrefix(p, root+"/")
		segs := strings.Split(rel, "/")
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(segs) == 1 {
			if _, ok := numberedDir(segs[0]); !d.IsDir() || !ok {
				return fmt.Errorf("%s: the top of ops/ holds only numbered scope directories (e.g. ops/900000/); an ABI build can't create a nested stack", src(p))
			}
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), "_") {
				return fmt.Errorf("%s: a \"_\" directory under ops/ (in an OPS/ tree that disables a draft; a build has no drafts)", src(p))
			}
			return nil
		}
		if !strings.HasSuffix(p, ".txcl") {
			return nil // mocks and other siblings
		}

		dirs := segs[:len(segs)-1]
		lastNum := 0
		scope := 0
		for i, seg := range dirs {
			if n, ok := numberedDir(seg); ok {
				lastNum, scope = i, n
			}
		}
		name := strings.Join(append(append([]string{}, dirs[lastNum+1:]...), strings.TrimSuffix(segs[len(segs)-1], ".txcl")), "_")
		if err := opname.Valid(name); err != nil {
			return fmt.Errorf("%s: %w", src(p), err)
		}
		key := strconv.Itoa(scope) + "\x00" + name
		if prior, dup := seen[key]; dup {
			return fmt.Errorf("two files flatten to the same operation %s/%d/%s:\n    %s\n    %s", stack, scope, name, src(prior), src(p))
		}
		seen[key] = p

		raw, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", src(p), rerr)
		}
		text, deps, ierr := include.Expand(fsys, root, p, string(raw))
		if ierr != nil {
			return fmt.Errorf("%s: %w", src(p), ierr)
		}
		if strings.Contains(text, "TXCO_STACK_DIR") {
			return fmt.Errorf("%s: an ABI build's op can't use $TXCO_STACK_DIR (the stack tree is the author's, not the build's)", src(p))
		}
		includes := make([]string, len(deps))
		for i, dep := range deps {
			includes[i] = src(dep)
		}
		op := Op{
			Stack:      stack,
			Scope:      scope,
			Name:       name,
			Txcl:       text,
			SourcePath: src(p),
			Includes:   includes,
			Origin:     OriginABI,
		}
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
