package trace

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// A trace is one document, and a run's trace can hold thousands of step
// bodies. A sink that buffers until End, and a reader that loads the whole
// document, would hold them all in memory twice. The spool moves bodies out:
// past a threshold each one is appended to a temp file on this machine, the
// file is uploaded as it stands when the trace ends, and a body is read back
// alone when someone opens its step.
//
// The format is what makes that work, and it is the whole contract:
//
//   - A spool is a run of concatenated gzip members, one per step, each
//     holding {"in":…,"out":…}. Each has its own writer, so any member
//     decodes alone, and the whole file is still a valid gzip stream.
//   - An extent (offset, length) is a position in exactly the bytes uploaded.
//     The spool's counter moves only after a complete write, and a store must
//     keep the bytes as given: no Content-Encoding on the object.
//   - gzip's CRC32 and length trailer mean a short or misaligned range is an
//     error, never wrong JSON.
//
// A spool file is deliberately disposable. It lives as long as its trace, it
// is never fsynced, and what a crash leaves is swept on the next start: the
// disk it sits on may not outlive the machine, so it could not be a record
// anyway. The bytes in it have been through redaction already.
//
// The file sink does not use this: it writes one file per body and needs no
// spool. Any sink that buffers a trace to its end can.

// ErrSpoolFull means the process-wide cap would be exceeded.
var ErrSpoolFull = errors.New("trace: spool is full")

// maxBodyMember bounds one decoded step, so a bad extent cannot expand into
// an unbounded read.
const maxBodyMember = 256 << 20

// BodyExtent is where one step's member sits in a spool's bytes.
type BodyExtent struct {
	Off, Len int64
}

// StepBody is one step's bodies. A body that was not valid JSON is null.
type StepBody struct {
	In  json.RawMessage `json:"in"`
	Out json.RawMessage `json:"out"`
}

// BodyStore is what a backend supplies to hold spools once a trace ends.
// PutBodies stores exactly size bytes from r, unmodified; GetBodies returns
// exactly the length bytes at off, or an error. Between them they must not
// transform a byte: tracetest.BodyStoreConformance is the check.
type BodyStore interface {
	PutBodies(ctx context.Context, key string, r io.ReadSeeker, size int64) error
	GetBodies(ctx context.Context, key string, off, length int64) ([]byte, error)
}

// StepBodyReader is what a Reader offers when a step's bodies are not in its
// document. It is optional: a backend without it serves bodies inline. tenant
// non-empty confines the read to that tenant's traces.
type StepBodyReader interface {
	StepBody(ctx context.Context, rid, ref, tenant string) (StepBody, error)
}

// ReadStepBody fetches and decodes one member named by a ref.
func ReadStepBody(ctx context.Context, store BodyStore, ref string) (StepBody, error) {
	key, ext, ok := ParseBodyRef(ref)
	if !ok {
		return StepBody{}, fmt.Errorf("trace: bad body ref %q", ref)
	}
	member, err := store.GetBodies(ctx, key, ext.Off, ext.Len)
	if err != nil {
		return StepBody{}, err
	}
	return DecodeBody(member)
}

// FormatBodyRef names one member of a stored spool.
func FormatBodyRef(key string, e BodyExtent) string {
	return key + "#" + strconv.FormatInt(e.Off, 10) + "-" + strconv.FormatInt(e.Len, 10)
}

// ParseBodyRef is FormatBodyRef's inverse. It checks the form only: a reader
// must still check that the key is one this caller may read.
func ParseBodyRef(ref string) (key string, e BodyExtent, ok bool) {
	i := strings.LastIndexByte(ref, '#')
	if i <= 0 {
		return "", BodyExtent{}, false
	}
	offs, lens, found := strings.Cut(ref[i+1:], "-")
	if !found {
		return "", BodyExtent{}, false
	}
	off, err1 := strconv.ParseInt(offs, 10, 64)
	length, err2 := strconv.ParseInt(lens, 10, 64)
	if err1 != nil || err2 != nil || off < 0 || length <= 0 {
		return "", BodyExtent{}, false
	}
	return ref[:i], BodyExtent{Off: off, Len: length}, true
}

// gzipWriters are reused: a new one allocates over a megabyte, and a member is
// made for every step of a long trace.
var gzipWriters = sync.Pool{New: func() any {
	zw, err := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	if err != nil {
		panic(err) // BestSpeed is a valid level
	}
	return zw
}}

// EncodeBody is one step's member: an independent gzip stream of
// {"in":…,"out":…}, cheap to make, since it runs on the op's goroutine.
func EncodeBody(in, out []byte) ([]byte, error) {
	var raw bytes.Buffer
	raw.Grow(len(in) + len(out) + 16)
	raw.WriteString(`{"in":`)
	raw.Write(jsonOrNull(in))
	raw.WriteString(`,"out":`)
	raw.Write(jsonOrNull(out))
	raw.WriteByte('}')

	var buf bytes.Buffer
	buf.Grow(raw.Len()/4 + 64)
	zw := gzipWriters.Get().(*gzip.Writer)
	defer gzipWriters.Put(zw)
	zw.Reset(&buf) // a whole new stream: nothing carries over from the last
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jsonOrNull(b []byte) []byte {
	if len(b) == 0 || !json.Valid(b) {
		return []byte("null")
	}
	return b
}

// DecodeBody reads exactly one member. Anything else is an error: a range one
// byte long has trailing bytes, one byte short fails the trailer, and a
// misaligned one has no gzip header.
func DecodeBody(member []byte) (StepBody, error) {
	br := bytes.NewReader(member)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return StepBody{}, fmt.Errorf("trace: body member: %w", err)
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, maxBodyMember+1))
	if err != nil {
		return StepBody{}, fmt.Errorf("trace: body member: %w", err)
	}
	if len(raw) > maxBodyMember {
		return StepBody{}, errors.New("trace: body member too large")
	}
	if br.Len() != 0 {
		return StepBody{}, errors.New("trace: body member has trailing bytes")
	}
	var b StepBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return StepBody{}, fmt.Errorf("trace: body member: %w", err)
	}
	return b, nil
}

// SpoolDir is a sink's spool directory. Its cap is process-wide: a run of
// long traces fills a disk, not one of them. 0 means never spill.
type SpoolDir struct {
	dir     string
	capB    int64
	used    atomic.Int64
	counter atomic.Uint64
}

// NewSpoolDir makes dir and clears what an earlier process left in it.
func NewSpoolDir(dir string, capBytes int64) (*SpoolDir, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	old, err := filepath.Glob(filepath.Join(dir, "*"+spoolSuffix))
	if err != nil {
		return nil, err
	}
	for _, f := range old {
		_ = os.Remove(f)
	}
	return &SpoolDir{dir: dir, capB: capBytes}, nil
}

const spoolSuffix = ".spool"

// Used is the bytes held across every open spool.
func (d *SpoolDir) Used() int64 { return d.used.Load() }

func (d *SpoolDir) open() (*Spool, error) {
	name := fmt.Sprintf("%d-%d%s", os.Getpid(), d.counter.Add(1), spoolSuffix)
	path := filepath.Join(d.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &Spool{dir: d, path: path, f: f, writeAt: f.WriteAt}, nil
}

// Spool is one trace's file. Append is safe for concurrent use.
type Spool struct {
	dir  *SpoolDir
	path string

	mu      sync.Mutex
	f       *os.File
	off     int64 // advances only after a complete write
	removed bool
	writeAt func(b []byte, off int64) (int, error)
}

// appendMember writes one member and answers where it sits. A failed or short
// write is rolled back, so no extent points at bytes that are not there.
func (s *Spool) appendMember(member []byte) (BodyExtent, error) {
	n := int64(len(member))
	if n == 0 {
		return BodyExtent{}, errors.New("trace: empty body member")
	}
	if s.dir.used.Add(n) > s.dir.capB {
		s.dir.used.Add(-n)
		return BodyExtent{}, ErrSpoolFull
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		s.dir.used.Add(-n)
		return BodyExtent{}, os.ErrClosed
	}
	wrote, err := s.writeAt(member, s.off)
	if err == nil && int64(wrote) != n {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.dir.used.Add(-n)
		_ = s.f.Truncate(s.off)
		return BodyExtent{}, err
	}
	e := BodyExtent{Off: s.off, Len: n}
	s.off += n
	return e, nil
}

// File opens what was written for reading, from its first byte, and answers
// its size. It refuses a file that is not the length the extents describe:
// an upload of those bytes would make every ref a lie. The caller closes it.
func (s *Spool) File() (*os.File, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return nil, 0, os.ErrClosed
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if st.Size() != s.off {
		_ = f.Close()
		return nil, 0, fmt.Errorf("trace: spool is %d bytes, its extents describe %d", st.Size(), s.off)
	}
	return f, s.off, nil
}

// Size is the bytes held.
func (s *Spool) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.off
}

// Remove deletes the file and gives its bytes back to the cap. Safe to call
// more than once, on every path out of a trace.
func (s *Spool) Remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return
	}
	s.removed = true
	s.dir.used.Add(-s.off)
	_ = s.f.Close()
	_ = os.Remove(s.path)
}

// bodySlot is one step's bodies, wherever they ended up.
type bodySlot struct {
	in, out []byte
	inline  bool
	ext     BodyExtent
	spilled bool
}

// BodyBuffer is the threshold policy a buffering sink needs. Bodies stay in
// memory, as they arrive, until a trace has held more than `after` bytes of
// them. Then they move to a spool and every later one follows, so an ordinary
// request never touches the disk. Past the spool's cap, on any write error, or
// with no spool directory at all, a body is dropped and its step keeps its
// sizes: tracing never hurts a request.
//
// Put is safe for concurrent use. Seal and Apply run once, as the trace ends.
type BodyBuffer struct {
	dir   *SpoolDir
	after int64

	mu     sync.Mutex
	held   int64
	slots  map[int]*bodySlot
	spool  *Spool
	sealed bool
	failed error // the first reason bodies were dropped
}

// NewBodyBuffer starts a buffer. A nil dir, or a cap of 0, never spills.
func NewBodyBuffer(dir *SpoolDir, after int64) *BodyBuffer {
	return &BodyBuffer{dir: dir, after: after, slots: map[int]*bodySlot{}}
}

// Put takes one step's bodies; i is the step's index in the trace.
func (b *BodyBuffer) Put(i int, in, out []byte) {
	n := int64(len(in) + len(out))
	if n == 0 {
		return
	}
	b.mu.Lock()
	if b.sealed {
		b.mu.Unlock()
		return
	}
	slot := &bodySlot{}
	b.slots[i] = slot
	b.held += n
	if b.held <= b.after {
		slot.in, slot.out, slot.inline = in, out, true
		b.mu.Unlock()
		return
	}
	spool := b.crossLocked()
	b.mu.Unlock()
	if spool == nil {
		return // dropped: an empty slot is a step that keeps only its sizes
	}

	// Compress outside the lock: it is the slow part, and parallel ops at one
	// scope should not queue behind it.
	member, err := EncodeBody(in, out)
	var ext BodyExtent
	if err == nil {
		ext, err = spool.appendMember(member)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		if b.failed == nil {
			b.failed = err
		}
		return
	}
	slot.ext, slot.spilled = ext, true
}

// crossLocked opens the spool on the first body past the threshold and moves
// what is held inline into it. It runs once, and is bounded by the threshold.
func (b *BodyBuffer) crossLocked() *Spool {
	if b.spool != nil || b.failed != nil {
		return b.spool
	}
	if b.dir == nil || b.dir.capB <= 0 {
		b.failed = ErrSpoolFull
		b.dropInlineLocked()
		return nil
	}
	s, err := b.dir.open()
	if err != nil {
		b.failed = err
		b.dropInlineLocked()
		return nil
	}
	b.spool = s
	for _, slot := range b.slots {
		if !slot.inline {
			continue
		}
		member, err := EncodeBody(slot.in, slot.out)
		var ext BodyExtent
		if err == nil {
			ext, err = s.appendMember(member)
		}
		slot.in, slot.out, slot.inline = nil, nil, false
		if err != nil {
			if b.failed == nil {
				b.failed = err
			}
			continue
		}
		slot.ext, slot.spilled = ext, true
	}
	return s
}

// dropInlineLocked lets go of what is held. With no spool to move to, keeping
// it would leave the trace over the threshold in memory for the rest of a run.
func (b *BodyBuffer) dropInlineLocked() {
	for _, slot := range b.slots {
		slot.in, slot.out, slot.inline = nil, nil, false
	}
}

// Err is the first reason bodies were dropped, if any.
func (b *BodyBuffer) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failed
}

// Seal refuses further bodies and answers the spool, or nil if the trace
// never spilled. The caller uploads it and calls Remove on every path.
func (b *BodyBuffer) Seal() *Spool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sealed = true
	return b.spool
}

// Apply puts the bodies onto a trace's steps. A body held inline is set on
// In and Out. A spilled one gets a BodyRef under key. With key "" nothing was
// stored, so a spilled body counts as dropped, and a dropped body keeps its
// step's sizes and reads as truncated.
func (b *BodyBuffer) Apply(steps []Step, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range steps {
		slot, ok := b.slots[i]
		if !ok {
			continue
		}
		switch {
		case slot.inline:
			steps[i].In, steps[i].Out = rawOrNil(slot.in), rawOrNil(slot.out)
		case slot.spilled && key != "":
			steps[i].BodyRef = FormatBodyRef(key, slot.ext)
		default:
			steps[i].InputTruncated = steps[i].InputBytes > 0
			steps[i].OutputTruncated = steps[i].OutputBytes > 0
		}
	}
}

// rawOrNil keeps a body as the bytes it arrived in. Decoded into maps it costs
// several times that. Not JSON: nil, which the wire shape omits.
func rawOrNil(b []byte) any {
	if len(b) == 0 || !json.Valid(b) {
		return nil
	}
	return json.RawMessage(b)
}
