package op

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	esbuild "github.com/evanw/esbuild/pkg/api"

	sdkop "github.com/loremlabs/thanks-computer/sdk/op"
)

// sdkModulePath maps a `@txco/op` specifier to its embedded source file.
// Returns false for an unknown subpath so the resolver can error clearly.
func sdkModulePath(spec string) (string, bool) {
	switch spec {
	case "@txco/op":
		return "src/index.ts", true
	case "@txco/op/runtime":
		return "src/runtime.ts", true
	case "@txco/op/envelope":
		return "src/envelope.ts", true
	case "@txco/op/schema":
		return "src/schema.ts", true
	case "@txco/op/crypto":
		return "src/crypto.ts", true
	case "@txco/op/codec":
		return "src/codec.ts", true
	}
	return "", false
}

// txcoOpPlugin resolves `@txco/op[/<subpath>]` imports to the embedded SDK
// sources (no npm install required). All SDK files are loaded as TypeScript.
func txcoOpPlugin() esbuild.Plugin {
	return esbuild.Plugin{
		Name: "txco-op",
		Setup: func(b esbuild.PluginBuild) {
			b.OnResolve(esbuild.OnResolveOptions{Filter: `^@txco/op`}, func(args esbuild.OnResolveArgs) (esbuild.OnResolveResult, error) {
				if _, ok := sdkModulePath(args.Path); !ok {
					return esbuild.OnResolveResult{}, fmt.Errorf("unknown @txco/op subpath %q (valid: @txco/op, /envelope, /schema, /crypto, /codec)", args.Path)
				}
				return esbuild.OnResolveResult{Path: args.Path, Namespace: "txco-op"}, nil
			})
			b.OnLoad(esbuild.OnLoadOptions{Filter: `.*`, Namespace: "txco-op"}, func(args esbuild.OnLoadArgs) (esbuild.OnLoadResult, error) {
				rel, _ := sdkModulePath(args.Path)
				data, err := fs.ReadFile(sdkop.SDK, rel)
				if err != nil {
					return esbuild.OnLoadResult{}, fmt.Errorf("read embedded %s: %w", rel, err)
				}
				contents := string(data)
				loader := esbuild.LoaderTS
				return esbuild.OnLoadResult{Contents: &contents, Loader: loader}, nil
			})
		},
	}
}

// bundle compiles an author's compute entry (.js/.ts) into a single
// self-contained script ready for javy. It injects an entry stub that imports
// the author's `export default op(...)` plus the SDK runtime, resolves
// `@txco/op` from the embedded sources, and tree-shakes unused helpers.
//
// Returns the bundled JS and its sourcemap (for remapping later errors).
func bundle(entryPath string) (js []byte, sourceMap []byte, err error) {
	abs, err := filepath.Abs(entryPath)
	if err != nil {
		return nil, nil, err
	}
	// Top-level await is deliberate: javy surfaces an unhandled async rejection
	// as a silent exit-0, but a rejecting top-level await propagates as a guest
	// error (nonzero exit). So a throwing/rejecting handler HALTS rather than
	// silently emitting {}.
	stub := fmt.Sprintf(`import handler from %q;
import { __run } from "@txco/op/runtime";
await __run(handler);
`, "./"+filepath.Base(abs))

	result := esbuild.Build(esbuild.BuildOptions{
		Stdin: &esbuild.StdinOptions{
			Contents:   stub,
			ResolveDir: filepath.Dir(abs),
			Sourcefile: "entry.js",
			Loader:     esbuild.LoaderJS,
		},
		Bundle:   true,
		Outfile:  "entry.js",
		Format:   esbuild.FormatESModule,
		Platform: esbuild.PlatformNeutral,
		Target:   esbuild.ES2022,
		// Tree-shaking is on (drops unused SDK helpers). Minification is off: it
		// would garble error locations, and under dynamic linking the module is
		// just this bundle's bytecode (~1 KB) — the QuickJS engine lives in the
		// shared plugin — so the size win wouldn't justify the legibility loss.
		TreeShaking: esbuild.TreeShakingTrue,
		Sourcemap:   esbuild.SourceMapExternal,
		Write:       false,
		LogLevel:    esbuild.LogLevelSilent,
		Plugins:     []esbuild.Plugin{txcoOpPlugin()},
	})

	if len(result.Errors) > 0 {
		return nil, nil, fmt.Errorf("%s", formatEsbuildErrors(result.Errors))
	}

	for _, f := range result.OutputFiles {
		if strings.HasSuffix(f.Path, ".map") {
			sourceMap = f.Contents
		} else {
			js = f.Contents
		}
	}
	if js == nil {
		return nil, nil, fmt.Errorf("esbuild produced no output for %s", entryPath)
	}
	return js, sourceMap, nil
}

func formatEsbuildErrors(errs []esbuild.Message) string {
	var b strings.Builder
	for i, m := range errs {
		if i > 0 {
			b.WriteString("\n")
		}
		if m.Location != nil {
			fmt.Fprintf(&b, "%s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text)
		} else {
			b.WriteString(m.Text)
		}
	}
	return b.String()
}
