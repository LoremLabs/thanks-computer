package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codeloadHost is the GitHub host that serves repo tarballs without auth for
// public repos. Overridable in tests via SetCodeloadBaseURL.
var codeloadHost = "https://codeload.github.com"

// SetCodeloadBaseURL lets tests point the GitHub fetcher at an httptest
// server. Returns the previous value so callers can restore in t.Cleanup.
func SetCodeloadBaseURL(u string) string {
	prev := codeloadHost
	codeloadHost = u
	return prev
}

// maxFileBytes guards against runaway tar entries. 10 MB per file is plenty
// for resonator.txcl + mock JSON; sources that need more belong in their
// own repo with custom tooling.
const maxFileBytes = 10 * 1024 * 1024

// maxTotalBytes caps the whole extracted archive. Same rationale.
const maxTotalBytes = 200 * 1024 * 1024

type githubSource struct {
	owner   string
	repo    string
	ref     string // branch / tag / commit; empty means default ("main", with master fallback)
	subpath string // forward-slash path inside the repo; "" = repo root
	spec    string // original spec, kept for error messages
}

func (g *githubSource) Spec() string { return g.spec }

// parseGitHub decodes the part after "github:". Forms accepted:
//
//	owner/repo
//	owner/repo/sub/path
//	owner/repo@ref
//	owner/repo@ref/sub/path
//
// owner and repo are mandatory; @ref and /subpath are optional. Whitespace
// and trailing slashes are stripped.
func parseGitHub(rest string) (*githubSource, error) {
	full := "github:" + rest
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return nil, fmt.Errorf("github source missing owner/repo: %s", full)
	}

	// Split off the @ref first, since the ref might itself contain slashes
	// (refs/heads/foo) or be a tag.
	var ref string
	if at := strings.Index(rest, "@"); at >= 0 {
		head := rest[:at]
		tail := rest[at+1:]
		// `owner/repo@ref/subpath`: split tail on the FIRST slash.
		if slash := strings.Index(tail, "/"); slash >= 0 {
			ref = tail[:slash]
			rest = head + "/" + tail[slash+1:]
		} else {
			ref = tail
			rest = head
		}
		if ref == "" {
			return nil, fmt.Errorf("github source has empty @ref: %s", full)
		}
	}

	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("github source needs at least owner/repo: %s", full)
	}
	owner, repo := parts[0], parts[1]
	subpath := ""
	if len(parts) == 3 {
		subpath = strings.Trim(parts[2], "/")
	}
	if strings.Contains(subpath, "..") {
		return nil, fmt.Errorf("github source subpath may not contain '..': %s", full)
	}

	return &githubSource{
		owner:   owner,
		repo:    repo,
		ref:     ref,
		subpath: subpath,
		spec:    full,
	}, nil
}

// Fetch downloads the repo tarball, extracts the requested subtree into
// destDir, and returns the number of regular files written.
func (g *githubSource) Fetch(ctx context.Context, destDir string) (int, error) {
	candidates := []string{g.ref}
	if g.ref == "" {
		candidates = []string{"main", "master"}
	}

	var lastErr error
	for _, ref := range candidates {
		n, err := g.fetchRef(ctx, ref, destDir)
		if err == nil {
			return n, nil
		}
		// Only fall through on 404 when no explicit ref was given. Other
		// errors (network, malformed tar) are real failures.
		if g.ref == "" && isNotFound(err) {
			lastErr = err
			continue
		}
		return 0, err
	}
	return 0, fmt.Errorf("could not locate default branch (tried main, master): %w", lastErr)
}

func (g *githubSource) fetchRef(ctx context.Context, ref, destDir string) (int, error) {
	u := fmt.Sprintf("%s/%s/%s/tar.gz/%s",
		strings.TrimRight(codeloadHost, "/"),
		url.PathEscape(g.owner),
		url.PathEscape(g.repo),
		url.PathEscape(ref))

	httpClient := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "txco-cli/1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, &httpError{
			Status: resp.StatusCode,
			URL:    u,
			Body:   strings.TrimSpace(string(body)),
		}
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	// stripTopDir=true: codeload prepends a synthetic `<repo>-<sha>/` top dir
	// to every entry; detect and strip it.
	return extractTar(tar.NewReader(gz), true, g.subpath, destDir)
}

// extractTar writes regular files from a tar stream into destDir. It is the
// shared, transport-agnostic extractor used by BOTH the GitHub source and the
// OCI source (the package layer is a tar.gz unpacked the same way).
//
// stripTopDir handles the GitHub codeload convention where every entry is
// prefixed with a synthetic `<repo>-<sha>/` top directory: when true, the first
// real entry's top segment is detected and stripped from all entries. OCI
// layers carry no such prefix, so they pass false. subpath (relative to the
// post-strip root), when non-empty, selects a subtree and re-roots it.
//
// Safety (identical for both callers):
//   - rejects entries with `..` segments (zip-slip) via safeRelPath
//   - skips symlinks, device files, hardlinks
//   - caps individual file size at maxFileBytes
//   - caps cumulative size at maxTotalBytes
func extractTar(tr *tar.Reader, stripTopDir bool, subpath, destDir string) (int, error) {
	var prefix string
	var written int
	var totalBytes int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("tar read: %w", err)
		}

		// Skip pax/extended/global headers — codeload puts a
		// `pax_global_header` as the first entry, and we don't want it
		// captured as the repo prefix below.
		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		clean, ok := safeRelPath(hdr.Name)
		if !ok {
			// "."/"/"/".." or hostile path; skip
			continue
		}

		rel := clean
		if stripTopDir {
			// First real entry establishes the `<repo>-<sha>` prefix that
			// codeload prepends to every entry.
			if prefix == "" {
				parts := strings.SplitN(clean, "/", 2)
				prefix = parts[0]
			}
			rel = strings.TrimPrefix(clean, prefix+"/")
			if rel == "" || rel == clean {
				// either the bare prefix dir entry, or the entry didn't start
				// with the expected prefix — skip
				continue
			}
		}

		if subpath != "" {
			if rel != subpath && !strings.HasPrefix(rel, subpath+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, subpath+"/")
			if rel == "" {
				continue
			}
		}

		// Only touch regular files and dirs. Symlinks would be the obvious
		// extension but they raise their own zip-slip-style concerns;
		// sources that need them can wait.
		switch hdr.Typeflag {
		case tar.TypeDir:
			out := filepath.Join(destDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(out, 0o755); err != nil {
				return written, fmt.Errorf("mkdir %s: %w", out, err)
			}
		case tar.TypeReg:
			if hdr.Size > maxFileBytes {
				return written, fmt.Errorf("source file %s exceeds %d-byte limit", rel, maxFileBytes)
			}
			out := filepath.Join(destDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return written, fmt.Errorf("mkdir %s: %w", filepath.Dir(out), err)
			}
			f, err := os.Create(out)
			if err != nil {
				return written, fmt.Errorf("create %s: %w", out, err)
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxFileBytes))
			closeErr := f.Close()
			if err != nil {
				return written, fmt.Errorf("write %s: %w", out, err)
			}
			if closeErr != nil {
				return written, fmt.Errorf("close %s: %w", out, closeErr)
			}
			totalBytes += n
			if totalBytes > maxTotalBytes {
				return written, fmt.Errorf("source archive exceeds %d-byte total limit", maxTotalBytes)
			}
			written++
		default:
			// skip symlinks, char/block devices, fifos, etc.
		}
	}
	return written, nil
}

// httpError wraps a non-2xx codeload response so callers can introspect.
type httpError struct {
	Status int
	URL    string
	Body   string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("GET %s: %d", e.URL, e.Status)
	}
	return fmt.Sprintf("GET %s: %d: %s", e.URL, e.Status, e.Body)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := err.(*httpError); ok {
		return he.Status == http.StatusNotFound
	}
	return false
}
