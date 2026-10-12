package compute

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// DefaultArtifactCacheBytes bounds the module bytes a Manager keeps after
// resolving them. A dynamically linked JS op is 1–10 KB and a self-contained
// module up to a few MB, so this holds every op a node runs in practice.
const DefaultArtifactCacheBytes = 64 << 20

// artifactCache keeps resolved artifacts by ref, least recently used out
// first, within a byte budget. A ref names its content, so an entry never goes
// stale and there is nothing to invalidate. Cached Wasm slices are shared by
// every caller and read-only.
type artifactCache struct {
	max int

	mu    sync.Mutex
	size  int
	ll    *list.List // front = most recently used; values are *cachedArtifact
	byRef map[Ref]*list.Element
}

type cachedArtifact struct {
	ref Ref
	art Artifact
}

func newArtifactCache(max int) *artifactCache {
	return &artifactCache{max: max, ll: list.New(), byRef: map[Ref]*list.Element{}}
}

func (c *artifactCache) get(ref Ref) (Artifact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byRef[ref]
	if !ok {
		return Artifact{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cachedArtifact).art, true
}

// put adds art, evicting the least recently used entries until it fits. An
// artifact bigger than the whole budget isn't kept.
func (c *artifactCache) put(ref Ref, art Artifact) {
	n := len(art.Wasm)
	if n > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byRef[ref]; ok {
		return
	}
	for c.size+n > c.max {
		old := c.ll.Back()
		e := old.Value.(*cachedArtifact)
		c.ll.Remove(old)
		delete(c.byRef, e.ref)
		c.size -= len(e.art.Wasm)
	}
	c.byRef[ref] = c.ll.PushFront(&cachedArtifact{ref: ref, art: art})
	c.size += n
}

// matchesDigest reports whether b is the content ref names. Only sha256 refs
// can be checked (the only algorithm the upload endpoint accepts).
func matchesDigest(ref Ref, b []byte) bool {
	if ref.Alg != "sha256" {
		return false
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) == ref.Digest
}
