package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// FileSink writes the per-request artifact tree under Dir.
// Concurrent calls to Begin are safe: each request gets its own
// subdirectory keyed by RID, so there's no cross-request contention.
//
// Writes are STREAMING — each Step/Event lands on disk as it arrives.
// We tried deferring all writes to End in a single burst (in case
// batching helped) but benched slower across the board: the worker
// got blocked through each request's flush burst, the channel filled,
// and overall throughput dropped. Streaming smears the small writes
// out so the worker stays busy without stalling.
type FileSink struct {
	Dir  string
	Mode Mode
}

func init() {
	Register("file", func(cfg StoreConfig) (Sink, error) {
		return NewFileSink(cfg.Dir, cfg.Mode)
	})
}

// NewFileSink returns a sink that writes to dir in the given mode.
// dir is created (with parents) if it doesn't exist; an error from
// MkdirAll fails fast at startup rather than on the first request.
func NewFileSink(dir string, mode Mode) (*FileSink, error) {
	if err := os.MkdirAll(filepath.Join(dir, "requests"), 0o755); err != nil {
		return nil, fmt.Errorf("trace: create dir %q: %w", dir, err)
	}
	return &FileSink{Dir: dir, Mode: mode}, nil
}

// Close is a no-op for FileSink — sink-level resources aren't held;
// each request tracer closes its own timeline file in End.
func (s *FileSink) Close(context.Context) error { return nil }

// Begin makes the per-request directory, writes in.json, and opens
// timeline.jsonl. Returns a tracer that's safe for concurrent
// Step/Event calls; End is expected exactly once.
func (s *FileSink) Begin(info RequestInfo) RequestTracer {
	reqDir := filepath.Join(s.Dir, "requests", info.RID)
	stepsDir := filepath.Join(reqDir, "steps")
	if err := os.MkdirAll(stepsDir, 0o755); err != nil {
		// On filesystem failure return a Noop so the request still
		// completes — tracing is observability, not correctness.
		return NoopTracer{}
	}

	// in.json snapshots the inbound envelope, plus a small header
	// with the chassis-side identity (rid, src, tenant, stack, ts).
	// Parse the payload into `any` so MarshalIndent recursively indents
	// it as part of the outer doc — otherwise compact upstream JSON
	// would appear as one long string at the `payload` field.
	payloadBytes := info.PayloadBytes
	if payloadBytes == 0 {
		payloadBytes = len(info.Payload)
	}
	var payloadField any = json.RawMessage(info.Payload)
	var parsed any
	if err := json.Unmarshal(info.Payload, &parsed); err == nil {
		payloadField = parsed
	}
	inDoc := map[string]any{
		"rid":           info.RID,
		"src":           info.Src,
		"tenant":        info.Tenant,
		"stack":         info.Stack,
		"started_at":    info.StartedAt.UTC().Format(time.RFC3339Nano),
		"payload_bytes": payloadBytes,
		"payload":       payloadField,
	}
	if payloadBytes > len(info.Payload) {
		inDoc["payload_truncated"] = true
	}
	if data, err := json.MarshalIndent(inDoc, "", "  "); err == nil {
		_ = writeFile(filepath.Join(reqDir, "in.json"), data)
	}

	timeline, err := os.OpenFile(
		filepath.Join(reqDir, "timeline.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o644,
	)
	if err != nil {
		return NoopTracer{}
	}

	t := &fileTracer{
		dir:       reqDir,
		stepsDir:  stepsDir,
		mode:      s.Mode,
		timeline:  timeline,
		startedAt: info.StartedAt,
	}
	t.Event(TimelineEvent{
		Ts:    info.StartedAt,
		Event: "request.start",
		Fields: map[string]any{
			"rid":    info.RID,
			"src":    info.Src,
			"tenant": info.Tenant,
			"stack":  info.Stack,
		},
	})
	return t
}

// fileTracer is the per-request handle backed by Dir.
type fileTracer struct {
	dir       string
	stepsDir  string
	mode      Mode
	timeline  *os.File
	startedAt time.Time

	mu sync.Mutex // protects timeline writes (multiple goroutines may emit events)
}

// Step records one op execution. In ModeSummary, only meta.json is
// written (no payload bytes). In ModeFull, op.json + in.json + out.json
// + meta.json are written. Always emits step.start/step.end timeline
// events so the JSONL is a complete ordered log.
//
// Folder name is "<scope>-<name>" with the scope zero-padded to four
// digits so the directory listing sorts in scope order. The number
// reflects the rule's on-disk path (OPS/<stack>/<scope>/<name>.txcl)
// so the trace tree reads exactly like the source tree. Two ops at the
// same scope produce siblings with the same prefix (e.g. 0100-hello /
// 0100-world), which is the trace's signal for "ran in parallel."
func (t *fileTracer) Step(info StepInfo) {
	base := fmt.Sprintf("%04d-%s", info.Scope, sanitizeName(info.Name))
	folder, err := createStepDir(t.stepsDir, base)
	if err != nil {
		return
	}
	stepDir := filepath.Join(t.stepsDir, folder)

	durationMs := info.FinishedAt.Sub(info.StartedAt).Milliseconds()

	inputBytes := info.InputBytes
	if inputBytes == 0 {
		inputBytes = len(info.Input)
	}
	outputBytes := info.OutputBytes
	if outputBytes == 0 {
		outputBytes = len(info.Output)
	}
	meta := map[string]any{
		"trace_id":     filepath.Base(t.dir),
		"stack":        info.Stack,
		"scope":        info.Scope,
		"name":         info.Name,
		"operation":    info.Operation,
		"transport":    info.Transport,
		"started_at":   info.StartedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":  info.FinishedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms":  durationMs,
		"status":       info.Status,
		"input_bytes":  inputBytes,
		"output_bytes": outputBytes,
	}
	if inputBytes > len(info.Input) {
		meta["input_truncated"] = true
	}
	if outputBytes > len(info.Output) {
		meta["output_truncated"] = true
	}
	if info.Error != "" {
		meta["error"] = info.Error
	}
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = writeFile(filepath.Join(stepDir, "meta.json"), data)
	}

	if t.mode == ModeFull {
		op := map[string]any{
			"stack":     info.Stack,
			"scope":     info.Scope,
			"name":      info.Name,
			"operation": info.Operation,
			"transport": info.Transport,
			"txcl":      info.Txcl,
		}
		if data, err := json.MarshalIndent(op, "", "  "); err == nil {
			_ = writeFile(filepath.Join(stepDir, "op.json"), data)
		}
		_ = writeFile(filepath.Join(stepDir, "in.json"), prettifyJSON(info.Input))
		_ = writeFile(filepath.Join(stepDir, "out.json"), prettifyJSON(info.Output))
	}

	t.Event(TimelineEvent{
		Ts:    info.StartedAt,
		Event: "step.start",
		Fields: map[string]any{
			"stack":     info.Stack,
			"scope":     info.Scope,
			"name":      info.Name,
			"operation": info.Operation,
			"transport": info.Transport,
		},
	})
	t.Event(TimelineEvent{
		Ts:    info.FinishedAt,
		Event: "step.end",
		Fields: map[string]any{
			"scope":        info.Scope,
			"name":         info.Name,
			"duration_ms":  durationMs,
			"status":       info.Status,
			"output_bytes": outputBytes,
		},
	})
}

// createStepDir attempts to create stepsDir/base; if it already exists,
// tries base + "-2", "-3", ... until a fresh name is created. Uses
// os.Mkdir (not MkdirAll) so the create/already-exists check is atomic
// at the filesystem level — necessary because Step() runs concurrently
// across goroutines and same-scope siblings produce the same `base`.
// stepsDir itself is created in Begin(); Mkdir of just the leaf is safe.
// Bounded at 1000 attempts; collisions in practice come from retries or
// goto-loops re-entering the same op within one request.
func createStepDir(stepsDir, base string) (string, error) {
	if err := os.Mkdir(filepath.Join(stepsDir, base), 0o755); err == nil {
		return base, nil
	} else if !os.IsExist(err) {
		return "", err
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if err := os.Mkdir(filepath.Join(stepsDir, candidate), 0o755); err == nil {
			return candidate, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("trace: step dir collision overflow for %q", base)
}

// Event appends a single JSON line to timeline.jsonl under the
// tracer's mutex. Each line is a complete JSON object; readers that
// catch the file mid-stream may miss a trailing newline but never
// see a corrupted line.
func (t *fileTracer) Event(ev TimelineEvent) {
	doc := map[string]any{
		"ts":    ev.Ts.UTC().Format(time.RFC3339Nano),
		"event": ev.Event,
	}
	for k, v := range ev.Fields {
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.timeline.Write(append(data, '\n'))
}

// End writes out.json at the request root (the final response after
// all merges), appends request.end to the timeline, and closes the
// timeline file. After End the tracer must not be used.
func (t *fileTracer) End(status, reason string, final []byte) {
	finishedAt := time.Now()
	if len(final) > 0 {
		_ = writeFile(filepath.Join(t.dir, "out.json"), prettifyJSON(final))
	}
	fields := map[string]any{
		"status":      status,
		"duration_ms": finishedAt.Sub(t.startedAt).Milliseconds(),
	}
	// Only stamp a reason when there is one (mirrors `if info.Error != ""`
	// on the step path) so the common ok request.end event stays unchanged.
	if reason != "" {
		fields["reason"] = reason
	}
	t.Event(TimelineEvent{
		Ts:     finishedAt,
		Event:  "request.end",
		Fields: fields,
	})
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.timeline.Close()
}

// writeFile writes data to path in one syscall (Open(CREATE|TRUNC) +
// Write + Close = 3 syscalls). We dropped the temp+rename atomicity:
// trace artifacts are short-lived per file (one write, never
// rewritten) and the reader-during-write window is microseconds.
// In dev nobody's browsing /traces/ in the middle of a request;
// catching a partial file in production is a refresh-and-retry, not a
// data loss. The savings: ~25% fewer syscalls per file × 22 files per
// request adds up across thousands of req/s.
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// prettifyJSON returns a 2-space-indented form of `data` when it
// parses as JSON; otherwise returns `data` unchanged. Uses
// json.Indent which streams over the bytes (no Unmarshal-into-`any`
// allocation) — much cheaper than the round-trip MarshalIndent
// version that allocates intermediate maps and slices per call.
func prettifyJSON(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return data
	}
	return buf.Bytes()
}

// sanitizeName makes an op name safe for use as a path component.
var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeName(name string) string {
	if name == "" {
		return "unnamed"
	}
	s := unsafeNameChars.ReplaceAllString(name, "_")
	// A name that is only dots ('.', '..', ...) is a filesystem
	// traversal primitive the char allowlist does NOT catch — '.' is a
	// permitted name char. Prefix it so a name like ".." can never be
	// the special parent-dir entry when used as a path component.
	if strings.Trim(s, ".") == "" {
		return "_" + s
	}
	return s
}
