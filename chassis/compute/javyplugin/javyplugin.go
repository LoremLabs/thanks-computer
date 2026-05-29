// Package javyplugin vendors the Javy QuickJS plugin used to run JS/TS
// nano-ops as *dynamically linked* wasm modules.
//
// Background: a self-contained `javy build` module embeds the whole QuickJS
// engine (~1.25 MB) per op. With dynamic linking the engine lives in ONE shared
// plugin module and each op compiles to just its own bytecode (~1 KB), importing
// `invoke`/`cabi_realloc`/`memory` from this plugin's namespace. The op author's
// build (chassis/cli/op) links against this exact plugin via `-C plugin=`, and
// the runtime (chassis/compute/wazero) links against the SAME bytes — so the
// vendored file here is the single source of truth for both sides. They MUST
// match: a dynamic module's embedded bytecode is specific to the plugin's QuickJS
// build, and its imports are pinned to the namespace version below.
//
// Regenerate (only when bumping Javy): `javy emit-plugin -o plugin.wasm` with the
// pinned Javy version, then re-run the op-build tests. Do not hand-edit.
package javyplugin

import _ "embed"

// JavyVersion is the Javy toolchain the vendored plugin was emitted from. The
// op-build path requires a matching `javy` on PATH; mismatches can produce
// modules whose bytecode the plugin can't decode.
const JavyVersion = "8.1.1"

// Namespace is the wasm import module name a dynamically linked module uses to
// reach this plugin (`<dynmod>` imports `Namespace.invoke`, `.cabi_realloc`,
// and `.memory`). It is version-pinned by Javy; emit-plugin for 8.1.1 yields v3.
const Namespace = "javy-default-plugin-v3"

// ConfigJSON is the SharedConfig the plugin reads from stdin at
// `initialize-runtime`. The plugin's base config already enables text-encoding,
// javy-stream-io, and simd-json-builtins; only the event loop is off by default,
// and our SDK runtime relies on it (top-level `await`). Kebab-case keys per the
// plugin's config schema.
const ConfigJSON = `{"event-loop":true}`

//go:embed plugin.wasm
var pluginWasm []byte

// Bytes returns the vendored plugin module. The slice is shared and read-only —
// callers must not mutate it.
func Bytes() []byte { return pluginWasm }
