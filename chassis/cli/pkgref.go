package cli

import "strings"

// Baked blessed defaults for resolving bare/namespaced package refs. They live
// in the CLI layer (NOT the chassis/runtime, which stays host-agnostic) and are
// overridden by a `registry:` block in the workspace txco.yaml. This gives a
// zero-config `txco install sales@v3` out of the box.
const (
	defaultRegistry  = "registry.thanks.computer"
	defaultNamespace = "txco"
)

// workspaceRegistry returns the workspace's registry config (nil-safe — an
// absent txco.yaml yields the zero value, so the baked defaults apply).
func workspaceRegistry(dir string) registryConfig {
	if cfg := loadWorkspaceConfig(dir); cfg != nil {
		return cfg.Registry
	}
	return registryConfig{}
}

// resolvePackageRef rewrites a user-typed package source into a spec that
// source.Parse understands:
//   - explicit schemes (dir:/file:/github:/oci:) pass through unchanged;
//   - local paths (./, ../, /) get a dir: prefix;
//   - bare/namespaced registry refs (sales@v3, acme/sales@v3) expand to a
//     concrete oci:// ref using the workspace registry config, falling back to
//     the baked defaults; an alias on the first path segment is substituted.
func resolvePackageRef(spec string, reg registryConfig) string {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "":
		return spec
	case strings.HasPrefix(spec, "dir:"), strings.HasPrefix(spec, "file:"),
		strings.HasPrefix(spec, "github:"), strings.HasPrefix(spec, "oci:"):
		return spec
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"),
		strings.HasPrefix(spec, "/"):
		return "dir:" + spec
	}

	registry := reg.Default
	if registry == "" {
		registry = defaultRegistry
	}
	namespace := reg.DefaultNamespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	base, ver := splitRefVersion(spec) // base="ns/name" or "name"; ver="v3"/"0.1.0"/""

	// Alias on the first path segment; alias value is a host/namespace prefix.
	first, rest := base, ""
	if i := strings.Index(base, "/"); i >= 0 {
		first, rest = base[:i], base[i+1:]
	}
	if aliasVal, ok := reg.Aliases[first]; ok && rest != "" {
		return "oci://" + aliasVal + "/" + rest + verSuffix(ver)
	}

	if strings.Contains(base, "/") {
		// namespaced: ns/name → <registry>/ns/name
		return "oci://" + registry + "/" + base + verSuffix(ver)
	}
	// bare name → <registry>/<namespace>/name
	return "oci://" + registry + "/" + namespace + "/" + base + verSuffix(ver)
}

// splitRefVersion splits "name@ver" or "name:tag" into (base, version). A colon
// counts as a version separator only in the final segment.
func splitRefVersion(s string) (base, ver string) {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[:at], s[at+1:]
	}
	if colon := strings.LastIndex(s, ":"); colon >= 0 && !strings.Contains(s[colon+1:], "/") {
		return s[:colon], s[colon+1:]
	}
	return s, ""
}

func verSuffix(ver string) string {
	if ver == "" {
		return "" // ParseRef defaults to :latest
	}
	return ":" + ver
}
