// Package tracetest holds what a trace backend proves itself with.
package tracetest

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// BodyStoreConformance is the one test every BodyStore runs: an in-memory
// store here, an S3 stub in a backend's tests, and the real object store when
// its credentials are present. It is the proof that the bytes a spool wrote
// are the bytes a range reads back, and that each member stands alone.
//
// key is where the test object goes; the caller cleans up.
func BodyStoreConformance(t *testing.T, store trace.BodyStore, key string) {
	t.Helper()
	ctx := context.Background()

	dir, err := trace.NewSpoolDir(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("NewSpoolDir: %v", err)
	}
	buf := trace.NewBodyBuffer(dir, 0) // everything spills
	want := conformanceBodies(t)
	for i, b := range want {
		buf.Put(i, b.In, b.Out)
	}
	spool := buf.Seal()
	if spool == nil {
		t.Fatalf("nothing spilled: %v", buf.Err())
	}
	defer spool.Remove()
	if err := buf.Err(); err != nil {
		t.Fatalf("spill: %v", err)
	}

	f, size, err := spool.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer f.Close()
	if err := store.PutBodies(ctx, key, f, size); err != nil {
		t.Fatalf("PutBodies: %v", err)
	}

	steps := make([]trace.Step, len(want))
	buf.Apply(steps, key)

	// Each member, fetched alone and out of order, is exactly what went in.
	order := shuffled(t, len(want))
	for _, i := range order {
		if steps[i].BodyRef == "" {
			t.Fatalf("step %d has no ref", i)
		}
		got, err := trace.ReadStepBody(ctx, store, steps[i].BodyRef)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !sameJSON(got.In, want[i].In) || !sameJSON(got.Out, want[i].Out) {
			t.Fatalf("step %d came back different: in %d bytes, out %d bytes", i, len(got.In), len(got.Out))
		}
	}

	// The object is the file: its whole bytes are the gzip stream of every
	// member, in the order they landed.
	whole, err := store.GetBodies(ctx, key, 0, size)
	if err != nil {
		t.Fatalf("whole object: %v", err)
	}
	if int64(len(whole)) != size {
		t.Fatalf("whole object is %d bytes, wrote %d", len(whole), size)
	}
	zr, err := gzip.NewReader(bytes.NewReader(whole))
	if err != nil {
		t.Fatalf("whole object is not gzip: %v", err)
	}
	all, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("whole object does not read through: %v", err)
	}
	for i := range want {
		if !bytes.Contains(all, []byte(fmt.Sprintf(`"marker":%d`, i))) {
			t.Fatalf("member %d is missing from the whole object", i)
		}
	}

	// A wrong range is an error, never wrong JSON. Use an extent with a
	// neighbour after it, so the long range has a byte to read.
	_, ext, ok := trace.ParseBodyRef(steps[0].BodyRef)
	if !ok {
		t.Fatalf("bad ref %q", steps[0].BodyRef)
	}
	for _, s := range steps {
		if _, e, ok := trace.ParseBodyRef(s.BodyRef); ok && e.Off+e.Len < size {
			ext = e
			break
		}
	}
	for name, r := range map[string]trace.BodyExtent{
		"one byte short": {Off: ext.Off, Len: ext.Len - 1},
		"one byte long":  {Off: ext.Off, Len: ext.Len + 1},
		"misaligned":     {Off: ext.Off + 1, Len: ext.Len},
	} {
		if r.Off+r.Len > size {
			continue
		}
		b, err := store.GetBodies(ctx, key, r.Off, r.Len)
		if err != nil {
			continue // a store may refuse; it may not return the wrong bytes as right ones
		}
		if _, err := trace.DecodeBody(b); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
}

// conformanceBodies vary in every way a store or a client could get wrong:
// empty sides, non-ASCII, bodies gzip cannot shrink, and bodies that shrink a
// hundredfold, the two that show an offset drifting from the stored bytes.
func conformanceBodies(t *testing.T) []trace.StepBody {
	t.Helper()
	incompressible := make([]byte, 200<<10)
	if _, err := rand.Read(incompressible); err != nil {
		t.Fatal(err)
	}
	body := func(i int, s string) json.RawMessage {
		b, err := json.Marshal(map[string]any{"marker": i, "s": s})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var out []trace.StepBody
	for i, s := range []string{
		"small",
		"non-ascii ✓ é 日本語 \" \\ \n",
		strings.Repeat("a", 1<<20),
		base64.StdEncoding.EncodeToString(incompressible),
		strings.Repeat(`{"a":[1,2,3]},`, 20000),
	} {
		out = append(out, trace.StepBody{In: body(i, s), Out: body(100+i, s)})
	}
	out = append(out, trace.StepBody{In: body(5, "only in")})
	out = append(out, trace.StepBody{Out: body(6, "only out")})
	return out
}

func shuffled(t *testing.T, n int) []int {
	t.Helper()
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		out[i], out[int(j.Int64())] = out[int(j.Int64())], out[i]
	}
	return out
}

func sameJSON(got, want json.RawMessage) bool {
	if len(want) == 0 {
		return len(got) == 0 || string(got) == "null"
	}
	return bytes.Equal(got, want)
}

// MemBodyStore is a BodyStore in memory, for tests.
type MemBodyStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// PutBodies keeps a copy of the bytes.
func (m *MemBodyStore) PutBodies(_ context.Context, key string, r io.ReadSeeker, size int64) error {
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[key] = b
	return nil
}

// GetBodies answers a range of a stored object.
func (m *MemBodyStore) GetBodies(_ context.Context, key string, off, length int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if off < 0 || off+length > int64(len(b)) {
		return nil, io.ErrUnexpectedEOF
	}
	return bytes.Clone(b[off : off+length]), nil
}
