package webabi

import (
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

// CheckOverlay reports every way an ABI install would collide with the
// author's own stack tree, as messages naming both sides. author and abi are
// the stack paths each side contributes ("100/a.txcl", "100/mock-request.json",
// "FILES/x.js", the markers, the provenance file). An empty result means the
// two lay over each other cleanly.
//
//   - the same path from both sides (an op, a scope's mock, a file);
//   - an author file under a "_" root public/ makes public: it would be
//     served too;
//   - an author file under an immutable prefix: it would be cached for a year;
//   - any author FILES/_txco/: the installer owns it in a bound stack.
func CheckOverlay(author, abi []string, d *Dir) []string {
	abiSet := make(map[string]bool, len(abi))
	for _, p := range abi {
		abiSet[p] = true
	}
	roots := d.PublicRoots()
	prefixes := d.Manifest.ImmutablePrefixes()

	var out []string
	for _, p := range author {
		if abiSet[p] {
			out = append(out, p+": the author's stack and the Web ABI build both have it")
			continue
		}
		rel, isFile := strings.CutPrefix(p, "FILES/")
		if !isFile {
			continue
		}
		if firstSegment(rel) == static.MarkerDir {
			out = append(out, p+": FILES/"+static.MarkerDir+"/ belongs to the Web ABI installer in a stack bound to a build")
			continue
		}
		if r := static.PrivateRoot(rel); r != "" {
			for _, root := range roots {
				if r == root {
					out = append(out, p+": public/ makes "+root+" public, so this private file would be served too")
					break
				}
			}
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(rel, pre) {
				out = append(out, p+": under the build's immutable prefix "+pre+", so it would be cached for a year")
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
