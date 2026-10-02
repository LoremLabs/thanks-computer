package op

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/computesrc"
)

// sourceBundle collects the authored files a compute was built from, out of
// the sourcemap esbuild already produces: every source except the embedded
// SDK (the txco-op: namespace) and the generated entry stub, with its exact
// content (TypeScript stays TypeScript). Paths are made relative to the
// entry's directory so the bundle — and its hash — doesn't depend on where
// the workspace sits or which directory `txco` ran from.
func sourceBundle(entryPath string, sourceMap []byte) (*computesrc.Bundle, error) {
	abs, err := filepath.Abs(entryPath)
	if err != nil {
		return nil, err
	}
	dir := realPath(filepath.Dir(abs))
	stub := entryStub(filepath.Base(abs))

	var m struct {
		Sources        []string  `json:"sources"`
		SourcesContent []*string `json:"sourcesContent"`
	}
	if err := json.Unmarshal(sourceMap, &m); err != nil {
		return nil, fmt.Errorf("read sourcemap: %w", err)
	}
	b := &computesrc.Bundle{V: computesrc.Version, Entry: filepath.Base(abs)}
	for i, s := range m.Sources {
		if strings.HasPrefix(s, "txco-op:") {
			continue
		}
		if i >= len(m.SourcesContent) || m.SourcesContent[i] == nil {
			return nil, fmt.Errorf("sourcemap has no content for %s", s)
		}
		content := *m.SourcesContent[i]
		// esbuild writes sources relative to its working directory (the
		// process cwd), the same base filepath.Abs uses.
		sabs, err := filepath.Abs(filepath.FromSlash(s))
		if err != nil {
			return nil, err
		}
		real := realPath(sabs)
		if content == stub && filepath.Dir(real) == dir && filepath.Base(real) == entryStubFile {
			continue
		}
		rel, err := filepath.Rel(dir, real)
		if err != nil {
			return nil, err
		}
		b.Files = append(b.Files, computesrc.File{Path: filepath.ToSlash(rel), Content: content})
	}
	return b, nil
}

// realPath resolves symlinks in p (esbuild may report real paths, e.g.
// /private/var for /var on macOS). A path that doesn't exist — the entry
// stub — resolves through its directory.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
}

// writeSource encodes a bundle and keeps a copy under cacheDir as
// <hash>.src.json, the local file apply streams to the file store.
func writeSource(cacheDir string, b *computesrc.Bundle) (hash, file string, err error) {
	data, hash, err := b.Encode()
	if err != nil {
		return "", "", err
	}
	file = filepath.Join(cacheDir, hash+".src.json")
	if _, serr := os.Stat(file); serr == nil {
		return hash, file, nil
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return "", "", err
	}
	return hash, file, nil
}
