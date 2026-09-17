package drive

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeObjects is an in-memory ObjectStore whose Get returns either a
// seeker or a bare stream — the two shapes a real backend produces.
type fakeObjects struct {
	data   map[string][]byte
	seeker bool
	gets   int
}

type seekCloser struct{ *bytes.Reader }

func (seekCloser) Close() error { return nil }

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	b, ok := f.data[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	f.gets++
	info := ObjectInfo{Size: int64(len(b)), ModTime: time.Now()}
	if f.seeker {
		return seekCloser{bytes.NewReader(b)}, info, nil
	}
	return io.NopCloser(bytes.NewReader(b)), info, nil // NopCloser hides Seek
}
func (f *fakeObjects) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (f *fakeObjects) Stat(context.Context, string) (ObjectInfo, error)            { return ObjectInfo{}, nil }
func (f *fakeObjects) List(context.Context, string, func(string, ObjectInfo) error) error {
	return nil
}
func (f *fakeObjects) Delete(context.Context, string) error { return nil }
func (f *fakeObjects) Name() string                         { return "fake" }

// rangedObjects is fakeObjects that can open a range, recording each ask.
type rangedObjects struct {
	*fakeObjects
	ranges []int64
}

func (r *rangedObjects) GetRange(_ context.Context, key string, off, length int64) (io.ReadCloser, error) {
	b, ok := r.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	r.ranges = append(r.ranges, off)
	end := int64(len(b))
	if length >= 0 && off+length < end {
		end = off + length
	}
	return io.NopCloser(bytes.NewReader(b[off:end])), nil
}

func TestObjectReader(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 100)) // 1000 bytes
	for _, tc := range []struct {
		name string
		mk   func() (ObjectStore, func() (gets int, ranges []int64))
	}{
		{"seeker", func() (ObjectStore, func() (int, []int64)) {
			f := &fakeObjects{data: map[string][]byte{"k": content}, seeker: true}
			return f, func() (int, []int64) { return f.gets, nil }
		}},
		{"stream-only", func() (ObjectStore, func() (int, []int64)) {
			f := &fakeObjects{data: map[string][]byte{"k": content}}
			return f, func() (int, []int64) { return f.gets, nil }
		}},
		{"ranged", func() (ObjectStore, func() (int, []int64)) {
			r := &rangedObjects{fakeObjects: &fakeObjects{data: map[string][]byte{"k": content}}}
			return r, func() (int, []int64) { return r.gets, r.ranges }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects, stats := tc.mk()
			rd := &objectReader{ctx: context.Background(), objects: objects, key: "k", size: int64(len(content))}

			// Seeking opens nothing: the size is the index's.
			if n, err := rd.Seek(0, io.SeekEnd); err != nil || n != 1000 {
				t.Fatalf("SeekEnd: %d, %v", n, err)
			}
			if n, err := rd.Seek(-10, io.SeekCurrent); err != nil || n != 990 {
				t.Fatalf("SeekCurrent: %d, %v", n, err)
			}
			if _, err := rd.Seek(-1, io.SeekStart); err == nil {
				t.Fatal("a negative seek was accepted")
			}
			if gets, _ := stats(); gets != 0 {
				t.Fatalf("seeking opened the object %d times", gets)
			}

			// The ServeContent shape: seek to the start of a range, read it.
			if _, err := rd.Seek(500, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 10)
			if _, err := io.ReadFull(rd, buf); err != nil || string(buf) != "0123456789" {
				t.Fatalf("read at 500: %q, %v", buf, err)
			}
			if gets, ranges := stats(); tc.name == "ranged" {
				if gets != 0 || len(ranges) != 1 || ranges[0] != 500 {
					t.Fatalf("ranged backend: gets=%d ranges=%v, want one range at 500", gets, ranges)
				}
			} else if gets != 1 {
				t.Fatalf("gets=%d, want 1", gets)
			}

			// Reading on is the same stream: no reopen.
			if _, err := io.ReadFull(rd, buf); err != nil || string(buf) != "0123456789" {
				t.Fatalf("read at 510: %q, %v", buf, err)
			}
			if gets, ranges := stats(); gets+len(ranges) != 1 {
				t.Fatalf("a sequential read reopened the object (gets=%d ranges=%v)", gets, ranges)
			}

			// A seek backwards reopens at the new offset.
			if _, err := rd.Seek(3, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(rd, buf[:4]); err != nil || string(buf[:4]) != "3456" {
				t.Fatalf("read at 3: %q, %v", buf[:4], err)
			}
			if gets, ranges := stats(); gets+len(ranges) != 2 {
				t.Fatalf("a backward seek did not reopen (gets=%d ranges=%v)", gets, ranges)
			}

			// The tail, straddling the end: exactly the bytes left, then EOF.
			if _, err := rd.Seek(-7, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			rest, err := io.ReadAll(rd)
			if err != nil || string(rest) != "3456789" {
				t.Fatalf("tail: %q, %v", rest, err)
			}
			// At or past the end: EOF without touching the backend.
			before, beforeR := stats()
			if _, err := rd.Seek(5000, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if n, err := rd.Read(buf); n != 0 || err != io.EOF {
				t.Fatalf("read past end: %d, %v", n, err)
			}
			if after, afterR := stats(); after != before || len(afterR) != len(beforeR) {
				t.Fatal("a read past the end opened the object")
			}
			// A read from the start never asks for a range.
			if _, err := rd.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			all, err := io.ReadAll(rd)
			if err != nil || !bytes.Equal(all, content) {
				t.Fatalf("whole read: %d bytes, %v", len(all), err)
			}
			if gets, ranges := stats(); tc.name == "ranged" {
				for _, off := range ranges {
					if off == 0 {
						t.Fatalf("a read from 0 went through GetRange: %v", ranges)
					}
				}
				if gets != 1 {
					t.Fatalf("a read from 0 should be one plain Get, got %d", gets)
				}
			}
			if err := rd.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
