package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	computeapi "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/computesrc"
)

// Compute source (chassis/computesrc): every compute `apply` builds from a
// colocated NAME.js/.ts also keeps its source, so the admin can show what a
// `compute://sha256/<digest>` does. A stack's version gets one fingerprint-
// only COMPUTES/<digest>.json row per compute its ops EXEC; the bundle bytes
// go to the chassis file store first, once per distinct source.

var computeRefRe = regexp.MustCompile(`compute://sha256/([0-9a-f]{64})`)

// computeSourceRows returns one COMPUTES/<digest>.json row per compute the
// stack's resolved ops EXEC whose source was built here — a prebuilt .wasm or
// a remote worker has none — plus the bundles to make resident before the
// draft references them. Rows are sorted by path.
func computeSourceRows(ops []bundle.Op, built []computeapi.Built) ([]client.StackFile, []casUpload) {
	byDigest := make(map[string]computeapi.Built, len(built))
	for _, b := range built {
		if b.Source != nil && b.SourceHash != "" && b.SourceFile != "" {
			byDigest[b.Digest] = b
		}
	}
	var rows []client.StackFile
	var uploads []casUpload
	seen := map[string]bool{}
	for _, op := range ops {
		for _, m := range computeRefRe.FindAllStringSubmatch(op.Txcl, -1) {
			d := m[1]
			b, ok := byDigest[d]
			if !ok || seen[d] {
				continue
			}
			st, err := os.Stat(b.SourceFile)
			if err != nil {
				continue // the local copy went missing; the admin just shows no source
			}
			seen[d] = true
			p := computesrc.Path(d)
			rows = append(rows, client.StackFile{Path: p, ContentHash: b.SourceHash, Encoding: "cas"})
			uploads = append(uploads, casUpload{Path: p, LocalPath: b.SourceFile, Hash: b.SourceHash, Size: st.Size()})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows, uploads
}

// chassisKeepsComputeSource reports whether the chassis accepts COMPUTES/
// rows. They arrived in the same release as POST /blobs/missing, so that
// endpoint's absence means an older chassis, which would reject the draft:
// apply then deploys without the rows and says so once. Any other probe
// error is left for the deploy itself to surface.
func chassisKeepsComputeSource(ctx context.Context, c *client.Client, built []computeapi.Built, stderr io.Writer, cmd string) bool {
	hasSource := false
	for _, b := range built {
		if b.Source != nil {
			hasSource = true
			break
		}
	}
	if !hasSource {
		return false
	}
	if _, err := c.MissingBlobs(ctx, nil); errors.Is(err, client.ErrNoMissingBlobs) {
		_, _ = io.WriteString(stderr, cmd+": this chassis doesn't keep compute source; upgrade it to see each compute's source in the admin\n")
		return false
	}
	return true
}
