package webabi

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

// maxStackPath is the longest stack-relative path the admin API accepts.
const maxStackPath = 1024

// Dir is a loaded ABI directory.
type Dir struct {
	// Path is the directory, absolute.
	Path     string
	Manifest *Manifest
	// Public is every file under public/, as a FILES/-relative path (the
	// stack path is "FILES/" + it), sorted. Dot paths and symlinks are not
	// in it: they never deploy.
	Public []string
	// HasServer: the manifest names a server entry.
	HasServer bool
	// HasOps: an ops/ directory exists.
	HasOps bool
	// Warnings are worth telling the author but don't stop an install.
	Warnings []string
}

// Load reads and checks an ABI directory. It reads paths only, never the
// bytes of public/ (collecting them is the installer's job).
func Load(dir string) (*Dir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m, err := ReadManifest(abs)
	if err != nil {
		return nil, err
	}
	d := &Dir{Path: abs, Manifest: m, HasServer: m.Server != nil}

	pub := filepath.Join(abs, "public")
	if fi, err := os.Stat(pub); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s: no public/ directory (an empty one is fine)", abs)
	}
	var dropped []string
	err = filepath.WalkDir(pub, func(p string, e fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(pub, p)
		if rerr != nil || rel == "." {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(e.Name(), ".") {
			dropped = append(dropped, "public/"+rel)
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			if rel == static.MarkerDir {
				return fmt.Errorf("public/%s/ is reserved for the installer", static.MarkerDir)
			}
			return nil
		}
		if !e.Type().IsRegular() {
			d.Warnings = append(d.Warnings, "public/"+rel+": not a regular file (a symlink?), skipped")
			return nil
		}
		if err := checkStackPathLen(rel); err != nil {
			return err
		}
		d.Public = append(d.Public, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(d.Public)
	if len(dropped) > 0 {
		d.Warnings = append(d.Warnings, fmt.Sprintf("%d dot path(s) under public/ never deploy (%s)", len(dropped), strings.Join(firstN(dropped, 3), ", ")))
	}

	srv := filepath.Join(abs, "server")
	srvExists := isDir(srv)
	switch {
	case m.Server != nil:
		entry := filepath.Join(abs, filepath.FromSlash(m.Server.Entry))
		if fi, err := os.Stat(entry); err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s names server entry %s, which isn't a file in %s", ManifestName, m.Server.Entry, abs)
		}
	case srvExists:
		return nil, fmt.Errorf("%s: a server/ directory, but %s names no server.entry", abs, ManifestName)
	}
	d.HasOps = isDir(filepath.Join(abs, "ops"))

	if entries, err := os.ReadDir(abs); err == nil {
		for _, e := range entries {
			switch name := e.Name(); {
			case name == ManifestName, name == "public", name == "server", name == "ops", strings.HasPrefix(name, "."):
			case e.IsDir() && (name == "FILES" || isNumber(name)):
				// The layout a pre-ABI adapter (SvelteKit adapter 0.2) writes into
				// its out dir: public/ and ops/ here would be stale.
				return nil, fmt.Errorf("%s: %s/ is the layout an older adapter writes (FILES/, NNNN/), so this build is stale or mixed — rebuild with a Web ABI producer (SvelteKit adapter ≥ 0.3)", abs, name)
			default:
				d.Warnings = append(d.Warnings, name+": not part of a Web ABI directory, ignored")
			}
		}
	}
	for _, p := range m.ImmutablePrefixes() {
		matched := false
		for _, f := range d.Public {
			if strings.HasPrefix(f, p) {
				matched = true
				break
			}
		}
		if !matched {
			d.Warnings = append(d.Warnings, "immutable prefix "+p+" matches no file in public/")
		}
	}
	return d, nil
}

// PublicRoots are the private roots (paths cut at their first "_" segment)
// public/ installs, sorted: each gets a public marker.
func (d *Dir) PublicRoots() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range d.Public {
		if r := static.PrivateRoot(f); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// MarkerContent is every marker's body. Static reads markers by path only;
// the body just keeps the file non-empty.
const MarkerContent = "txco web abi marker\n"

// Markers returns the stack paths (FILES/…) of the markers an install
// writes, sorted: one public marker per private root public/ installs, one
// immutable marker per manifest prefix.
func (d *Dir) Markers() []string {
	var out []string
	for _, r := range d.PublicRoots() {
		out = append(out, "FILES/"+static.PublicMarker(r))
	}
	for _, p := range d.Manifest.ImmutablePrefixes() {
		out = append(out, "FILES/"+static.ImmutableMarker(p))
	}
	sort.Strings(out)
	return out
}

// ProvenancePath is the stack path of the file that lists every path an
// install owns, so `txco pull` can leave them out of the author's tree. It
// lives under FILES/_txco/, private like any "_" path.
const ProvenancePath = "FILES/" + static.MarkerDir + "/web-abi.json"

// Provenance renders the provenance file for an install that owns paths
// (stack paths; the file itself is added).
func Provenance(owned []string) []byte {
	all := append([]string{ProvenancePath}, owned...)
	sort.Strings(all)
	b, _ := json.MarshalIndent(struct {
		ABI   int      `json:"abi"`
		Paths []string `json:"paths"`
	}{ABIVersion, all}, "", "  ")
	return append(b, '\n')
}

// ParseProvenance reads a provenance file's owned paths.
func ParseProvenance(data []byte) ([]string, error) {
	var p struct {
		ABI   int      `json:"abi"`
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return p.Paths, nil
}

// checkStackPathLen refuses a public/ file whose stack path, or whose public
// marker's, is longer than the admin API accepts.
func checkStackPathLen(rel string) error {
	if len("FILES/"+rel) > maxStackPath || len("FILES/"+static.PublicMarker(static.PrivateRoot(rel))) > maxStackPath {
		return fmt.Errorf("public/%s: path too long (the limit is %d bytes as a stack path)", rel, maxStackPath)
	}
	return nil
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string(nil), s[:n]...), "…")
}
