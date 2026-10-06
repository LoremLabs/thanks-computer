package static

import (
	"path"
	"strings"
)

// Markers are files whose PATH is the whole message; their bytes are never
// read, so a marker works the same on the fleet, where a tenant FILES/ row
// carries only its content hash. They live under FILES/_txco/, which is
// private like any "_" path, and never enter a layer's file set, so neither
// HTTP nor read-file (Asset) sees them.
//
//	_txco/public/<root>/_txco_mark       <root> may be served over HTTP
//	_txco/immutable/<prefix>/_txco_mark  files under <prefix>/ are content-
//	                                     hashed: cache them for a year
//
// A Web ABI install writes them: a public marker for each "_" root its
// public/ tree contains, an immutable marker for each prefix its manifest
// names.
const (
	MarkerDir  = "_txco"
	markerLeaf = "_txco_mark"
)

// PublicMarker is the FILES/-relative path of the marker that makes root
// (a path cut at its first "_" segment, see PrivateRoot) servable.
func PublicMarker(root string) string {
	return MarkerDir + "/public/" + root + "/" + markerLeaf
}

// ImmutableMarker is the FILES/-relative path of the marker that marks
// every file under prefix immutable.
func ImmutableMarker(prefix string) string {
	return MarkerDir + "/immutable/" + strings.TrimSuffix(prefix, "/") + "/" + markerLeaf
}

// PrivateRoot returns rel cut at the end of its first "_"-prefixed segment
// ("" when it has none): _app/x.js → _app, assets/_Dk3.js → assets/_Dk3.js,
// a/_b/_c/d → a/_b. It is the unit a public marker names.
func PrivateRoot(rel string) string {
	for i := 0; i < len(rel); {
		seg := rel[i:]
		if j := strings.IndexByte(seg, '/'); j >= 0 {
			seg = seg[:j]
		}
		if strings.HasPrefix(seg, "_") {
			return rel[:i+len(seg)]
		}
		i += len(seg) + 1
	}
	return ""
}

func privateRoot(rel string) string { return PrivateRoot(rel) }

// marks is a layer's marker set.
type marks struct {
	public    map[string]struct{} // private roots that may be served
	immutable []string            // prefixes, each ending in "/"
}

func newMarks() marks { return marks{public: map[string]struct{}{}} }

func (m *marks) add(rel string) {
	kind, target, ok := parseMarker(rel)
	if !ok {
		return
	}
	switch kind {
	case "public":
		m.public[target] = struct{}{}
	case "immutable":
		m.immutable = append(m.immutable, target+"/")
	}
}

func (m marks) isPublic(root string) bool {
	_, ok := m.public[root]
	return ok
}

func (m marks) isImmutable(rel string) bool {
	for _, p := range m.immutable {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// parseMarker reads a marker path. _txco itself can never be made public
// or immutable: its own files (a marker, an install's provenance) stay
// private whatever a tree contains.
func parseMarker(rel string) (kind, target string, ok bool) {
	rest, ok := strings.CutPrefix(rel, MarkerDir+"/")
	if !ok {
		return "", "", false
	}
	if rest, ok = strings.CutSuffix(rest, "/"+markerLeaf); !ok {
		return "", "", false
	}
	kind, target, ok = strings.Cut(rest, "/")
	if !ok || target == "" || (kind != "public" && kind != "immutable") {
		return "", "", false
	}
	if first, _, _ := strings.Cut(target, "/"); first == MarkerDir {
		return "", "", false
	}
	return kind, target, true
}

func isMarker(rel string) bool {
	_, _, ok := parseMarker(rel)
	return ok
}

// isRootPath reports whether a request path names the root ("/", "",
// "/./"), as opposed to one safeRel refused (a dot segment).
func isRootPath(reqPath string) bool {
	return strings.Trim(path.Clean("/"+reqPath), "/") == ""
}
