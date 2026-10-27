package cli

// Provider and connection collection and checking for `txco apply`, `txco
// dev` and `txco lint`: the PROVIDERS/ subtree holds one YAML declaration
// per outside service a stack answers for, CONNECTIONS/ one per provider
// the stack uses (chassis/connection). Declarations are CODE — they deploy
// with the rules that use them, inline in the draft like CAPS/ — and every
// one is parsed here, before the server does the same on validate and
// activate. Whether another stack of the tenant already declares a
// provider is the server's check: only it knows what is active.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/connection"
)

// collectDeclFiles walks <stackDir>/<dir>/ and returns the draft rows
// (declarations inline). An absent directory yields nil, nil.
func collectDeclFiles(stackDir, dir string, name func(string) string, what string) ([]client.StackFile, error) {
	full := filepath.Join(stackDir, dir)
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	var files []client.StackFile
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		rel := dir + "/" + n
		if e.IsDir() {
			return nil, fmt.Errorf("%s: nested directories are not allowed under %s/ (declarations are single <name>%s files)", rel, dir, connection.DeclExt)
		}
		if name(rel) == "" {
			return nil, fmt.Errorf("%s: %s declarations are <name>%s with a name matching [a-z][a-z0-9_-]*", rel, what, connection.DeclExt)
		}
		content, rerr := os.ReadFile(filepath.Join(full, n))
		if rerr != nil {
			return nil, rerr
		}
		ch := sha256.Sum256(content)
		files = append(files, client.StackFile{Path: rel, Content: string(content), ContentHash: hex.EncodeToString(ch[:])})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// collectProviderFiles walks <stackDir>/PROVIDERS/.
func collectProviderFiles(stackDir string) ([]client.StackFile, error) {
	return collectDeclFiles(stackDir, connection.ProviderDir, connection.ProviderName, "provider")
}

// collectConnectionFiles walks <stackDir>/CONNECTIONS/.
func collectConnectionFiles(stackDir string) ([]client.StackFile, error) {
	return collectDeclFiles(stackDir, connection.ConsumerDir, connection.ConsumerName, "connection")
}

// checkConnectionDecls parses every provider and connection declaration of
// every stack the walked ops belong to. It returns one message per broken
// declaration. A connection's origins are checked against its provider
// when that provider is declared in the same tree.
func checkConnectionDecls(ops []bundle.Op, dir string) []string {
	var msgs []string
	seen := map[string]bool{}
	providers := map[string]*connection.Provider{}
	type consumer struct {
		path string
		decl *connection.Consumer
	}
	var consumers []consumer
	for _, op := range ops {
		if seen[op.Stack] {
			continue
		}
		seen[op.Stack] = true
		stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(op.Stack))
		pfiles, err := collectProviderFiles(stackDir)
		if err != nil {
			msgs = append(msgs, err.Error())
		}
		for _, f := range pfiles {
			p, perr := connection.ParseProvider([]byte(f.Content))
			where := filepath.Join(stackDir, filepath.FromSlash(f.Path))
			if perr != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", where, perr))
				continue
			}
			providers[connection.ProviderName(f.Path)] = p
		}
		cfiles, err := collectConnectionFiles(stackDir)
		if err != nil {
			msgs = append(msgs, err.Error())
		}
		for _, f := range cfiles {
			where := filepath.Join(stackDir, filepath.FromSlash(f.Path))
			c, perr := connection.ParseConsumer([]byte(f.Content))
			if perr != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", where, perr))
				continue
			}
			if name := connection.ConsumerName(f.Path); c.Provider != name {
				msgs = append(msgs, fmt.Sprintf("%s: the file is for provider %q but `provider` says %q", where, name, c.Provider))
				continue
			}
			consumers = append(consumers, consumer{path: where, decl: c})
		}
	}
	for _, c := range consumers {
		p, ok := providers[c.decl.Provider]
		if !ok {
			continue // another tenant stack may declare it; the server checks at use
		}
		for _, o := range c.decl.OriginList() {
			if !p.APIOrigin(o) {
				msgs = append(msgs, fmt.Sprintf("%s: origin %s is not one of provider %q's api destinations", c.path, o, c.decl.Provider))
			}
		}
	}
	sort.Strings(msgs)
	return msgs
}
