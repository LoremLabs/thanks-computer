// Package javyplugin vendors the Javy QuickJS plugins used to run JS/TS
// nano-ops as *dynamically linked* wasm modules.
//
// Background: a self-contained `javy build` module embeds the whole QuickJS
// engine (~1.25 MB) per op. With dynamic linking the engine lives in ONE shared
// plugin module and each op compiles to just its own bytecode (~1 KB), importing
// `invoke`/`cabi_realloc`/`memory` from the plugin's namespace. The op author's
// build (chassis/cli/op) links against the current plugin via `-C plugin=`, and
// the runtime (chassis/compute/wazero) links each module against the plugin
// whose namespace it imports — so the vendored files here are the single source
// of truth for both sides. A dynamic module's embedded bytecode is specific to
// the plugin's QuickJS build, and its imports are pinned to that namespace.
//
// Two plugins are vendored:
//
//   - txco-javy-plugin-v1.wasm — the current plugin, built from ./plugin: Javy's
//     default plugin plus the txco natives (`__txco_native.sha256`, `.hmacSha256`)
//     the @txco/op/crypto SDK uses. Every op `txco` builds links against it.
//   - javy-default-plugin-v3.wasm — the plugin earlier chassis versions built
//     against (`javy emit-plugin`, unmodified). Kept so ops already in a store
//     and prebuilt packages keep running until they are rebuilt; a rebuild
//     happens on the next `txco apply` because the build cache key includes
//     the namespace.
//
// Regenerate the current plugin with `make plugin` (see plugin/README.md).
// Regenerate the legacy one only when bumping Javy: `javy emit-plugin`. Do not
// hand-edit either.
package javyplugin

import _ "embed"

// JavyVersion is the Javy toolchain the vendored plugins were emitted from and
// the op-build path requires (chassis/cli/op/javybin downloads it). The
// current plugin's crate pins the matching javy-plugin-api; mismatches can
// produce modules whose bytecode the plugin can't decode.
const JavyVersion = "8.1.1"

// Namespace is the wasm import module name a module built today imports
// (`<dynmod>` imports `Namespace.invoke`, `.cabi_realloc`, and `.memory`). It
// is set by `import_namespace!` in plugin/src/lib.rs; bump it there and here
// together, and keep the previous plugin vendored.
const Namespace = "txco-javy-plugin-v1"

// legacyNamespace is the namespace of the stock Javy 8.1.1 plugin.
const legacyNamespace = "javy-default-plugin-v3"

// ConfigJSON is the SharedConfig a plugin reads from stdin at
// `initialize-runtime`. Both plugins' base config already enables
// text-encoding, javy-stream-io, and simd-json-builtins; only the event loop
// is off by default, and our SDK runtime relies on it (top-level `await`).
// Kebab-case keys per the plugin's config schema.
const ConfigJSON = `{"event-loop":true}`

// Plugin is one vendored plugin: the engine a dynamically linked op imports by
// Namespace. Wasm is shared and read-only — callers must not mutate it.
type Plugin struct {
	Namespace string
	Wasm      []byte
	// Native reports whether the plugin carries the txco natives
	// (`__txco_native`), i.e. whether the SDK's crypto helpers run natively.
	Native bool
}

//go:embed txco-javy-plugin-v1.wasm
var currentWasm []byte

//go:embed javy-default-plugin-v3.wasm
var legacyWasm []byte

var plugins = []Plugin{
	{Namespace: Namespace, Wasm: currentWasm, Native: true},
	{Namespace: legacyNamespace, Wasm: legacyWasm},
}

// Current returns the plugin ops are built against today.
func Current() Plugin { return plugins[0] }

// Bytes returns the current plugin module (Current().Wasm).
func Bytes() []byte { return currentWasm }

// All returns every vendored plugin, the current one first.
func All() []Plugin { return append([]Plugin(nil), plugins...) }

// Lookup returns the vendored plugin a module importing namespace ns links
// against, or false when no vendored plugin has that namespace (a module built
// by a newer txco, or against a plugin this chassis never shipped).
func Lookup(ns string) (Plugin, bool) {
	for _, p := range plugins {
		if p.Namespace == ns {
			return p, true
		}
	}
	return Plugin{}, false
}
