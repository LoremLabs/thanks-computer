package trace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// fileReader is the filesystem-backed Reader. It is a verbatim move of
// the legacy admin trace read path (newest-first by dir mtime, full
// file-walk grep, fs-stat ETag, per-rid aggregation from
// in.json/timeline.jsonl/steps/*/meta.json), so single-box/colocated
// deploys behave byte-for-byte as before.
type fileReader struct {
	dir string
}

func init() {
	RegisterReader("file", func(cfg StoreConfig) (Reader, error) {
		return &fileReader{dir: cfg.Dir}, nil
	})
}

const (
	fileListDefaultLimit = 50
	fileListMaxLimit     = 500
	fileListGrepScanMax  = 5000
	fileListGrepFileCap  = 1 << 20
)

// validRID matches the character class hxid emits (alphanumerics) plus
// the legacy `_-`. Anything else is rejected before it touches a path.
var validRID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (fr *fileReader) RawFS() (http.FileSystem, bool) {
	return http.Dir(fr.dir), true
}

// IndexNames: newest-first by dir name (legacy behavior of the HTML
// index — hxid is time-sortable; reverse-name-sort = newest-first
// without per-entry Stat).
func (fr *fileReader) IndexNames(_ context.Context, max int) ([]string, int, error) {
	dir := filepath.Join(fr.dir, "requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	total := len(names)
	if max > 0 && len(names) > max {
		names = names[:max]
	}
	return names, total, nil
}

func (fr *fileReader) Get(_ context.Context, rid string, full bool) (RequestDetail, error) {
	if !validRID.MatchString(rid) {
		return RequestDetail{}, ErrNotFound
	}
	reqDir := filepath.Join(fr.dir, "requests", rid)
	if _, err := os.Stat(reqDir); err != nil {
		if os.IsNotExist(err) {
			return RequestDetail{}, ErrNotFound
		}
		return RequestDetail{}, err
	}
	d := RequestDetail{RID: rid, Status: "in-flight", Steps: []Step{}}
	readHeader(reqDir, &d, full)
	readTimeline(reqDir, &d)
	readSteps(reqDir, &d, full)
	if full {
		readOut(reqDir, &d)
	}
	return d, nil
}

func readHeader(reqDir string, d *RequestDetail, full bool) {
	data, err := os.ReadFile(filepath.Join(reqDir, "in.json"))
	if err != nil {
		return
	}
	var in map[string]any
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	if v, ok := in["src"].(string); ok {
		d.Src = v
	}
	if v, ok := in["tenant"].(string); ok {
		d.Tenant = v
	}
	if v, ok := in["stack"].(string); ok {
		d.Stack = v
	}
	if v, ok := in["started_at"].(string); ok {
		d.StartedAt = v
	}
	switch v := in["payload_bytes"].(type) {
	case float64:
		d.PayloadBytes = int64(v)
	case int64:
		d.PayloadBytes = v
	}
	if v, ok := in["payload_truncated"].(bool); ok {
		d.PayloadTruncated = v
	}
	if full {
		d.In = in
	}
}

func readTimeline(reqDir string, d *RequestDetail) {
	f, err := os.Open(filepath.Join(reqDir, "timeline.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["event"] {
		case "stage.jump":
			if d.Route == "" {
				if v, ok := ev["to"].(string); ok {
					d.Route = v
				}
			}
		case "request.end":
			if v, ok := ev["ts"].(string); ok {
				d.FinishedAt = v
			}
			if v, ok := ev["status"].(string); ok && v != "" {
				d.Status = v
			}
			// The request-level reason a non-ok status occurred. Written
			// only when non-empty (file.go End), so absent on ok requests.
			if v, ok := ev["reason"].(string); ok && v != "" {
				d.Error = v
			}
			if v, ok := ev["duration_ms"].(float64); ok {
				n := int64(v)
				d.DurationMs = &n
			}
		case "request.usage":
			if v, ok := ev["fuel"].(float64); ok {
				d.Fuel = int64(v)
			}
			if v, ok := ev["bytes_out"].(float64); ok {
				d.BytesOut = int64(v)
			}
			// Resolved tenant (the one the pipeline routed to), which
			// overrides the `_sys` entry tenant in in.json — every request
			// enters pinned to _sys, so in.json is always _sys. This is the
			// value admin tenant-scoping filters on.
			if v, ok := ev["tenant"].(string); ok && v != "" {
				d.Tenant = v
			}
		}
	}
}

func readSteps(reqDir string, d *RequestDetail, full bool) {
	entries, err := os.ReadDir(filepath.Join(reqDir, "steps"))
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stepDir := filepath.Join(reqDir, "steps", e.Name())
		data, err := os.ReadFile(filepath.Join(stepDir, "meta.json"))
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		st := Step{}
		if v, ok := m["name"].(string); ok {
			st.Name = v
		}
		if v, ok := m["operation"].(string); ok {
			st.Operation = v
		}
		if v, ok := m["transport"].(string); ok {
			st.Transport = v
		}
		if v, ok := m["stack"].(string); ok {
			st.Stack = v
		}
		if v, ok := m["scope"].(float64); ok {
			st.Scope = int(v)
		}
		if v, ok := m["started_at"].(string); ok {
			st.StartedAt = v
		}
		if v, ok := m["finished_at"].(string); ok {
			st.FinishedAt = v
		}
		if v, ok := m["duration_ms"].(float64); ok {
			st.DurationMs = int64(v)
		}
		if v, ok := m["status"].(string); ok {
			st.Status = v
		}
		if v, ok := m["input_bytes"].(float64); ok {
			st.InputBytes = int64(v)
		}
		if v, ok := m["output_bytes"].(float64); ok {
			st.OutputBytes = int64(v)
		}
		if v, ok := m["input_truncated"].(bool); ok {
			st.InputTruncated = v
		}
		if v, ok := m["output_truncated"].(bool); ok {
			st.OutputTruncated = v
		}
		if v, ok := m["error"].(string); ok {
			st.Error = v
		}
		if full {
			if b, err := os.ReadFile(filepath.Join(stepDir, "in.json")); err == nil {
				var v any
				if json.Unmarshal(b, &v) == nil {
					st.In = v
				}
			}
			if b, err := os.ReadFile(filepath.Join(stepDir, "out.json")); err == nil {
				var v any
				if json.Unmarshal(b, &v) == nil {
					st.Out = v
				}
			}
		}
		d.Steps = append(d.Steps, st)
	}
}

func readOut(reqDir string, d *RequestDetail) {
	b, err := os.ReadFile(filepath.Join(reqDir, "out.json"))
	if err != nil {
		return
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		d.Out = v
	}
}

func (fr *fileReader) List(_ context.Context, q ListQuery) (ListResult, error) {
	dir := filepath.Join(fr.dir, "requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ListResult{Traces: []Summary{}}, nil
		}
		return ListResult{}, err
	}

	// Trace dirs, newest-first by mtime (scheme-agnostic; resume-<...>
	// RIDs aren't name-time-sortable).
	dirs := entries[:0]
	mtime := make(map[string]int64)
	for _, e := range entries {
		if e.IsDir() && validRID.MatchString(e.Name()) {
			dirs = append(dirs, e)
			if fi, ierr := e.Info(); ierr == nil {
				mtime[e.Name()] = fi.ModTime().UnixNano()
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		mi, mj := mtime[dirs[i].Name()], mtime[dirs[j].Name()]
		if mi != mj {
			return mi > mj
		}
		return dirs[i].Name() > dirs[j].Name()
	})

	limit := fileListDefaultLimit
	if q.Limit > 0 {
		limit = q.Limit
		if limit > fileListMaxLimit {
			limit = fileListMaxLimit
		}
	}
	grep := strings.ToLower(q.Grep)

	etag := computeListETag(dir, dirs, limit, grep, q.Tenant)
	res := ListResult{ETag: etag}
	if etag != "" && q.IfNoneMatch != "" && q.IfNoneMatch == etag {
		res.NotModified = true
		return res, nil
	}

	res.Traces = make([]Summary, 0, limit)
	if grep == "" && q.Tenant == "" {
		res.Total = len(dirs)
		if len(dirs) > limit {
			dirs = dirs[:limit]
		}
		for _, e := range dirs {
			res.Traces = append(res.Traces, readListEntry(filepath.Join(dir, e.Name()), e.Name()))
		}
	} else {
		// Scan path: grep and/or tenant filtering. Tenant scoping needs the
		// entry's RESOLVED tenant (from its request.usage event), so read the
		// entry before the count/limit decision when filtering by tenant.
		scanCap := fileListGrepScanMax
		if scanCap > len(dirs) {
			scanCap = len(dirs)
		}
		for _, e := range dirs[:scanCap] {
			reqDir := filepath.Join(dir, e.Name())
			if grep != "" && !matchesGrep(reqDir, grep) {
				continue
			}
			var entry Summary
			read := false
			if q.Tenant != "" {
				entry = readListEntry(reqDir, e.Name())
				read = true
				if entry.Tenant != q.Tenant {
					continue
				}
			}
			res.Total++
			if len(res.Traces) < limit {
				if !read {
					entry = readListEntry(reqDir, e.Name())
				}
				res.Traces = append(res.Traces, entry)
			}
		}
	}
	return res, nil
}

func computeListETag(reqRoot string, dirs []os.DirEntry, limit int, grep, tenant string) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "v=1 limit=%d grep=%s tenant=%s total=%d\n", limit, grep, tenant, len(dirs))
	top := limit
	if grep != "" || tenant != "" {
		top = fileListGrepScanMax
	}
	if top > len(dirs) {
		top = len(dirs)
	}
	for _, e := range dirs[:top] {
		reqDir := filepath.Join(reqRoot, e.Name())
		fmt.Fprintf(h, "rid=%s\n", e.Name())
		if grep == "" {
			fingerprintFile(h, filepath.Join(reqDir, "in.json"))
			fingerprintFile(h, filepath.Join(reqDir, "timeline.jsonl"))
		} else {
			_ = filepath.WalkDir(reqDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				rel, _ := filepath.Rel(reqDir, path)
				fmt.Fprintf(h, "%s %d %d\n", rel, info.Size(), info.ModTime().UnixNano())
				return nil
			})
		}
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

func fingerprintFile(h io.Writer, path string) {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(h, "%s missing\n", filepath.Base(path))
		return
	}
	fmt.Fprintf(h, "%s %d %d\n", filepath.Base(path), info.Size(), info.ModTime().UnixNano())
}

func matchesGrep(reqDir, needle string) bool {
	if needle == "" {
		return true
	}
	needleBytes := []byte(needle)
	found := false
	_ = filepath.WalkDir(reqDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if found {
			return filepath.SkipAll
		}
		if fileContainsCI(path, needleBytes) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func fileContainsCI(path string, needle []byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, fileListGrepFileCap))
	if err != nil || len(buf) == 0 {
		return false
	}
	return bytes.Contains(bytes.ToLower(buf), needle)
}

func readListEntry(reqDir, rid string) Summary {
	s := Summary{RID: rid, Status: "in-flight"}
	if data, err := os.ReadFile(filepath.Join(reqDir, "in.json")); err == nil {
		var in map[string]any
		if json.Unmarshal(data, &in) == nil {
			if v, ok := in["src"].(string); ok {
				s.Src = v
			}
			if v, ok := in["tenant"].(string); ok {
				s.Tenant = v
			}
			if v, ok := in["stack"].(string); ok {
				s.Stack = v
			}
			if v, ok := in["started_at"].(string); ok {
				s.StartedAt = v
			}
		}
	}
	if f, err := os.Open(filepath.Join(reqDir, "timeline.jsonl")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			switch ev["event"] {
			case "stage.jump":
				if s.Route == "" {
					if v, ok := ev["to"].(string); ok {
						s.Route = v
					}
				}
			case "request.end":
				if v, ok := ev["ts"].(string); ok {
					s.FinishedAt = v
				}
				if v, ok := ev["status"].(string); ok && v != "" {
					s.Status = v
				}
				if v, ok := ev["reason"].(string); ok && v != "" {
					s.Error = v
				}
				if v, ok := ev["duration_ms"].(float64); ok {
					n := int64(v)
					s.DurationMs = &n
				}
			case "request.usage":
				// Resolved tenant overrides the `_sys` entry tenant in
				// in.json — what admin tenant-scoping filters on.
				if v, ok := ev["tenant"].(string); ok && v != "" {
					s.Tenant = v
				}
			}
		}
	}
	return s
}
