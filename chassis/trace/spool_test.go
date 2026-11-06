package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func spoolDir(t *testing.T, capBytes int64) *SpoolDir {
	t.Helper()
	d, err := NewSpoolDir(t.TempDir(), capBytes)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func spoolFiles(t *testing.T, d *SpoolDir) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(d.dir, "*"+spoolSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonBody(n int) []byte {
	return []byte(fmt.Sprintf(`{"body":"%s"}`, strings.Repeat("x", n)))
}

// Most traces are ordinary web requests: below the threshold the disk is not
// touched, and bodies come back as the bytes they arrived in.
func TestBodyBufferBelowThresholdStaysInMemory(t *testing.T) {
	d := spoolDir(t, 1<<20)
	b := NewBodyBuffer(d, 1000)
	in, out := jsonBody(100), jsonBody(200)
	b.Put(0, in, out)

	if s := b.Seal(); s != nil {
		t.Fatalf("spilled below the threshold: %+v", s)
	}
	if len(spoolFiles(t, d)) != 0 {
		t.Fatal("a file exists below the threshold")
	}
	steps := make([]Step, 1)
	b.Apply(steps, "key")
	if steps[0].BodyRef != "" {
		t.Errorf("ref = %q", steps[0].BodyRef)
	}
	got, ok := steps[0].In.(json.RawMessage)
	if !ok || !bytes.Equal(got, in) {
		t.Errorf("In = %T %s", steps[0].In, steps[0].In)
	}
}

// Crossing the threshold takes what is held with it, so every body of the
// trace ends up in one place.
func TestBodyBufferCrossingMovesEarlierBodies(t *testing.T) {
	d := spoolDir(t, 1<<20)
	b := NewBodyBuffer(d, 500)
	b.Put(0, jsonBody(100), nil)
	b.Put(1, jsonBody(100), nil)
	b.Put(2, jsonBody(400), jsonBody(10))
	b.Put(3, jsonBody(10), nil)

	s := b.Seal()
	if s == nil {
		t.Fatalf("did not spill: %v", b.Err())
	}
	defer s.Remove()
	store := memStore{}
	f, size, err := s.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := store.PutBodies(context.Background(), "k", f, size); err != nil {
		t.Fatal(err)
	}

	steps := make([]Step, 4)
	b.Apply(steps, "k")
	for i, s := range steps {
		if s.BodyRef == "" || s.In != nil {
			t.Fatalf("step %d: %+v", i, s)
		}
		got, err := ReadStepBody(context.Background(), store, s.BodyRef)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if len(got.In) == 0 {
			t.Errorf("step %d lost its input", i)
		}
	}
}

// memStore keeps the package's own tests free of tracetest, which imports it.
type memStore map[string][]byte

func (m memStore) PutBodies(_ context.Context, key string, r io.ReadSeeker, size int64) error {
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	m[key] = b
	return nil
}

func (m memStore) GetBodies(_ context.Context, key string, off, length int64) ([]byte, error) {
	return m[key][off : off+length], nil
}

// Every way a spill can fail drops the body and keeps the step's sizes: a full
// disk must not be the reason a request failed.
func TestBodyBufferDropsWithoutFailing(t *testing.T) {
	for name, dir := range map[string]*SpoolDir{
		"no directory": nil,
		"cap of zero":  spoolDir(t, 0),
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBodyBuffer(dir, 100)
			b.Put(0, jsonBody(50), nil)
			b.Put(1, jsonBody(500), nil)
			if s := b.Seal(); s != nil {
				t.Fatalf("spooled: %+v", s)
			}
			if b.Err() == nil {
				t.Error("no reason recorded")
			}
			steps := []Step{{InputBytes: 60}, {InputBytes: 510}}
			b.Apply(steps, "")
			for i, s := range steps {
				if s.In != nil || s.BodyRef != "" || !s.InputTruncated {
					t.Errorf("step %d: %+v", i, s)
				}
			}
		})
	}

	t.Run("over the cap", func(t *testing.T) {
		d := spoolDir(t, 200)
		b := NewBodyBuffer(d, 0)
		b.Put(0, jsonBody(10), nil)         // fits
		b.Put(1, incompressible(4000), nil) // does not
		s := b.Seal()
		if s == nil {
			t.Fatal("nothing spilled")
		}
		defer s.Remove()
		if !errors.Is(b.Err(), ErrSpoolFull) {
			t.Errorf("err = %v", b.Err())
		}
		steps := []Step{{InputBytes: 20}, {InputBytes: 4000}}
		b.Apply(steps, "k")
		if steps[0].BodyRef == "" {
			t.Errorf("the body that fit has no ref: %+v", steps[0])
		}
		if steps[1].BodyRef != "" || !steps[1].InputTruncated {
			t.Errorf("the body over the cap: %+v", steps[1])
		}
	})
}

func incompressible(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"body":"`)
	x := uint32(2463534242)
	for b.Len() < n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		fmt.Fprintf(&b, "%08x", x)
	}
	b.WriteString(`"}`)
	return []byte(b.String())
}

// With nothing stored, a spilled body is a dropped one: no ref may point at
// an object that does not exist.
func TestBodyBufferApplyWithoutKey(t *testing.T) {
	d := spoolDir(t, 1<<20)
	b := NewBodyBuffer(d, 0)
	b.Put(0, jsonBody(10), nil)
	s := b.Seal()
	defer s.Remove()
	steps := []Step{{InputBytes: 20}}
	b.Apply(steps, "")
	if steps[0].BodyRef != "" || !steps[0].InputTruncated {
		t.Errorf("step = %+v", steps[0])
	}
}

// A short write must not leave an extent past the good bytes, or a counter
// that has moved: every later member would then point at the wrong place.
func TestSpoolShortWriteRollsBack(t *testing.T) {
	for name, fail := range map[string]func(f *os.File) func([]byte, int64) (int, error){
		"short": func(f *os.File) func([]byte, int64) (int, error) {
			return func(b []byte, off int64) (int, error) { return f.WriteAt(b[:len(b)/2], off) }
		},
		"error": func(f *os.File) func([]byte, int64) (int, error) {
			return func(b []byte, off int64) (int, error) {
				_, _ = f.WriteAt(b[:len(b)/2], off)
				return len(b) / 2, errors.New("disk full")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := spoolDir(t, 1<<20)
			s, err := d.open()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Remove()
			good, _ := EncodeBody(jsonBody(10), nil)
			first, err := s.appendMember(good)
			if err != nil {
				t.Fatal(err)
			}

			real := s.writeAt
			s.writeAt = fail(s.f)
			if _, err := s.appendMember(good); err == nil {
				t.Fatal("a failed write reported success")
			}
			s.writeAt = real
			if got := d.Used(); got != first.Len {
				t.Errorf("used = %d, want %d: a failed write stayed counted", got, first.Len)
			}

			second, err := s.appendMember(good)
			if err != nil {
				t.Fatal(err)
			}
			if second.Off != first.Off+first.Len {
				t.Errorf("second member at %d, want %d", second.Off, first.Off+first.Len)
			}
			f, size, err := s.File()
			if err != nil {
				t.Fatalf("File after a rollback: %v", err)
			}
			defer f.Close()
			raw, _ := io.ReadAll(f)
			if int64(len(raw)) != size {
				t.Fatalf("read %d bytes, size %d", len(raw), size)
			}
			if _, err := DecodeBody(raw[second.Off : second.Off+second.Len]); err != nil {
				t.Errorf("second member: %v", err)
			}
		})
	}
}

// File refuses bytes the extents do not describe.
func TestSpoolFileRefusesASizeMismatch(t *testing.T) {
	d := spoolDir(t, 1<<20)
	s, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Remove()
	m, _ := EncodeBody(jsonBody(10), nil)
	if _, err := s.appendMember(m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.WriteAt([]byte("extra"), s.off); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.File(); err == nil {
		t.Fatal("File accepted bytes no extent describes")
	}
}

func TestSpoolRemoveIsIdempotentAndGivesBackTheCap(t *testing.T) {
	d := spoolDir(t, 1<<20)
	s, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	m, _ := EncodeBody(jsonBody(10), nil)
	if _, err := s.appendMember(m); err != nil {
		t.Fatal(err)
	}
	s.Remove()
	s.Remove()
	if len(spoolFiles(t, d)) != 0 {
		t.Error("the file survived Remove")
	}
	if d.Used() != 0 {
		t.Errorf("used = %d", d.Used())
	}
	if _, err := s.appendMember(m); err == nil {
		t.Error("appended to a removed spool")
	}
	if d.Used() != 0 {
		t.Errorf("used after a refused append = %d", d.Used())
	}
}

// What a killed process left behind is cleared at the next start, and only
// that: nothing else in the directory is touched.
func TestNewSpoolDirSweepsLeftovers(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "123-1"+spoolSuffix)
	other := filepath.Join(dir, "keep.txt")
	for _, p := range []string{left, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewSpoolDir(dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Error("the leftover survived")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("the sweep took a file that is not a spool")
	}
}

func TestSpoolFileIsPrivate(t *testing.T) {
	d := spoolDir(t, 1<<20)
	s, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Remove()
	fi, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestBodyRefRoundTrip(t *testing.T) {
	ref := FormatBodyRef("traces/bodies/acme/123-rid.bodies", BodyExtent{Off: 42, Len: 7})
	key, ext, ok := ParseBodyRef(ref)
	if !ok || key != "traces/bodies/acme/123-rid.bodies" || ext != (BodyExtent{42, 7}) {
		t.Fatalf("%q %+v %v", key, ext, ok)
	}
	for _, bad := range []string{"", "#1-2", "key", "key#", "key#1", "key#a-b", "key#-1-2", "key#1-0", "key#1--2"} {
		if _, _, ok := ParseBodyRef(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A body that is not JSON reads as null, and an empty side stays empty.
func TestEncodeBodyHandlesWhatIsNotJSON(t *testing.T) {
	m, err := EncodeBody([]byte("not json {"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBody(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b.In) != "null" || string(b.Out) != "null" {
		t.Errorf("in = %s, out = %s", b.In, b.Out)
	}
}

// Concurrent steps: every body lands in a member of its own, under -race.
func TestBodyBufferConcurrent(t *testing.T) {
	d := spoolDir(t, 1<<30)
	b := NewBodyBuffer(d, 1000)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Put(i, []byte(fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("y", 100))), nil)
		}(i)
	}
	wg.Wait()
	s := b.Seal()
	defer s.Remove()
	f, size, err := s.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	store := memStore{}
	if err := store.PutBodies(context.Background(), "k", f, size); err != nil {
		t.Fatal(err)
	}
	steps := make([]Step, n)
	b.Apply(steps, "k")
	for i, st := range steps {
		if st.BodyRef == "" {
			if st.In == nil {
				t.Fatalf("step %d has nothing", i)
			}
			continue // held inline before the crossing can't happen: it moved
		}
		got, err := ReadStepBody(context.Background(), store, st.BodyRef)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !bytes.Contains(got.In, []byte(fmt.Sprintf(`"i":%d,`, i))) {
			t.Fatalf("step %d got another step's body: %.40s", i, got.In)
		}
	}
}

// A spilled step's cost lands on the op's goroutine. Against today's cost of
// decoding the same bytes into maps, it should be about even.
func BenchmarkSpillEnvelope(b *testing.B) {
	body := jsonEnvelope(230 << 10)
	d, err := NewSpoolDir(b.TempDir(), 1<<40)
	if err != nil {
		b.Fatal(err)
	}
	s, err := d.open()
	if err != nil {
		b.Fatal(err)
	}
	defer s.Remove()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		m, err := EncodeBody(body, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.appendMember(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeEnvelope(b *testing.B) {
	body := jsonEnvelope(230 << 10)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// jsonEnvelope reads a real one when the build left it, and otherwise makes a
// nested document of the same size.
func jsonEnvelope(n int) []byte {
	if p := os.Getenv("TRACE_BENCH_ENVELOPE"); p != "" {
		if b, err := os.ReadFile(p); err == nil && json.Valid(b) {
			return b
		}
	}
	var b strings.Builder
	b.WriteString(`{"_txc":{"src":"http"},"rows":[`)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `{"id":%d,"name":"step-%d","text":"%s"},`, i, i, strings.Repeat("lorem ipsum ", 8))
	}
	b.WriteString(`{}]}`)
	return []byte(b.String())
}
