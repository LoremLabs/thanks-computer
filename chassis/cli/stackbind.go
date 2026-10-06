package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/loremlabs/thanks-computer/chassis/opname"
)

// stackBinding ties a stack to a Web ABI directory: the producer's build
// output that `txco apply` lays over the stack's own tree (or that is the
// whole stack, for a pure framework app).
//
//	stacks:
//	  web:
//	    abi: www/txco-web
type stackBinding struct {
	Stack string
	ABI   string // as written: slash form, relative to the workspace root
	Abs   string // resolved, absolute
}

var stacksKeyRE = regexp.MustCompile(`(?m)^stacks\s*:|"stacks"\s*:`)

// loadStackBindings reads the `stacks:` block of the workspace's txco.yaml
// (the same file loadWorkspaceConfig reads), on its own and strictly. A
// mistake here is an error, never a silently dropped binding: a dropped
// binding would deploy a stack without its web half. A workspace with no
// config, or no `stacks:`, has no bindings.
func loadStackBindings(root string) (map[string]stackBinding, error) {
	path, raw, err := readWorkspaceConfigFile(root)
	if err != nil || raw == nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		if stacksKeyRE.Match(raw) {
			return nil, fmt.Errorf("%s: %w (it has a stacks: block, so this can't be ignored)", path, err)
		}
		return nil, nil // as today: an unparseable config is ignored
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	top := doc.Content[0]
	var stacks *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		switch k, v := top.Content[i].Value, top.Content[i+1]; k {
		case "stacks":
			stacks = v
		case "stack":
			if v.Kind == yaml.MappingNode {
				return nil, fmt.Errorf("%s:%d: `stack:` is a single default stack name; did you mean `stacks:`?", path, top.Content[i].Line)
			}
		}
	}
	if stacks == nil || (stacks.Kind == yaml.ScalarNode && stacks.Tag == "!!null") {
		return nil, nil
	}
	if stacks.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s:%d: stacks: must map a stack name to its settings", path, stacks.Line)
	}

	out := map[string]stackBinding{}
	for i := 0; i+1 < len(stacks.Content); i += 2 {
		kn, vn := stacks.Content[i], stacks.Content[i+1]
		name := kn.Value
		where := fmt.Sprintf("%s:%d: stacks.%s", path, kn.Line, name)
		if err := checkBindableStack(name); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if vn.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: must be a mapping, e.g. `abi: www/txco-web`", where)
		}
		var abi string
		for j := 0; j+1 < len(vn.Content); j += 2 {
			key, val := vn.Content[j], vn.Content[j+1]
			if key.Value != "abi" {
				return nil, fmt.Errorf("%s: unknown key %q (known: abi)", where, key.Value)
			}
			if val.Kind != yaml.ScalarNode || val.Value == "" {
				return nil, fmt.Errorf("%s.abi: must be a path to a Web ABI directory", where)
			}
			abi = val.Value
		}
		if abi == "" {
			return nil, fmt.Errorf("%s: no abi: path", where)
		}
		abs, err := resolveABIPath(root, abi)
		if err != nil {
			return nil, fmt.Errorf("%s.abi: %w", where, err)
		}
		out[name] = stackBinding{Stack: name, ABI: filepath.ToSlash(abi), Abs: abs}
	}

	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, a := range names {
		for _, b := range names[i+1:] {
			x, y := out[a].Abs, out[b].Abs
			if x == y || strings.HasPrefix(x, y+string(filepath.Separator)) || strings.HasPrefix(y, x+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: stacks.%s and stacks.%s name the same or nested ABI directories", path, a, b)
			}
		}
	}
	return out, nil
}

// checkBindableStack: a bound stack is a web stack. System and channel
// stacks (any "_" segment) and the boot pipeline are not.
func checkBindableStack(name string) error {
	if err := opname.ValidStack(name); err != nil {
		return err
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, "_") {
			return fmt.Errorf("%q: a system or channel stack can't be bound to a Web ABI build", name)
		}
	}
	if name == "boot" || strings.HasPrefix(name, "boot/") {
		return fmt.Errorf("%q: the boot pipeline can't be bound to a Web ABI build", name)
	}
	return nil
}

// resolveABIPath resolves a binding's path, relative to the workspace root.
// It must lie outside OPS/, where the walker would read it as a nested
// stack, and must not be the root itself. ".." is allowed (a front end
// beside the stack tree).
func resolveABIPath(root, p string) (string, error) {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("%q: must be relative to the workspace root", p)
	}
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(p)))
	if abs == filepath.Clean(root) {
		return "", fmt.Errorf("%q: is the workspace root itself", p)
	}
	ops := filepath.Join(root, "OPS")
	if abs == ops || strings.HasPrefix(abs, ops+string(filepath.Separator)) {
		return "", fmt.Errorf("%q: is inside OPS/, where it would be read as a stack; keep the build output outside it", p)
	}
	return abs, nil
}

// readWorkspaceConfigFile returns the first config file that exists, in
// loadWorkspaceConfig's order, with its bytes (nil when there is none).
func readWorkspaceConfigFile(root string) (string, []byte, error) {
	for _, name := range []string{"txco.yaml", "txco.yml", "txco.json",
		filepath.Join(".txco", "target.yaml"), filepath.Join(".txco", "target.yml"), filepath.Join(".txco", "target.json")} {
		p := filepath.Join(root, name)
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return p, nil, err
		}
		return p, b, nil
	}
	return "", nil, nil
}
