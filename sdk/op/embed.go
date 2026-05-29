// Package op embeds the @txco/op TypeScript SDK sources so the `txco op` build
// pipeline can resolve `import { op } from "@txco/op"` (and its subpaths) with
// no npm install. The same sources are published to npm for editor types; the
// embedded copy is the build-time source of truth.
package op

import "embed"

// SDK holds the @txco/op source tree (src/*.ts). esbuild's resolver plugin
// maps the `@txco/op` and `@txco/op/<subpath>` specifiers to these files.
//
//go:embed src
var SDK embed.FS
