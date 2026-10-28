package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// A sandbox's FILES (SANDBOXES/<name>.yaml `files:` and `dotenv:`) are
// served as named pipes (FIFOs, mode 0600). Each time something opens one,
// its value is asked of the chassis at that moment — decided by the
// tenant's `_grant` stack and logged there, the store's current version, a
// connection's token renewed when due — written once, and the pipe is
// re-armed for the next reader. Nothing is written to disk, and nothing is
// held here between reads.
//
// Re-arming: before writing, a fresh FIFO is made beside the path; once
// the value is written it is renamed over the path. The reader that is
// still draining keeps the pipe it opened, and the next open finds the
// fresh one — a reader never gets a value twice.

// sandboxPoll is how often a pipe with no reader is tried again.
var sandboxPoll = 50 * time.Millisecond

// sandboxWriteTimeout bounds one write: a reader that opens a file and
// never reads does not stall the pipe for the next.
const sandboxWriteTimeout = 10 * time.Second

// servedFile is one file a sandbox serves, as this program serves it.
type servedFile struct {
	sandbox string
	key     string // file:<path> or dotenv:<path>, as the grant names it
	kind    string // sandbox.EntryFile or sandbox.EntryDotenv
	shown   string // the declared path, for the log
	path    string // where it is served
}

// fileServer serves the files of the sandboxes this program opened.
type fileServer struct {
	open  func(ctx context.Context, req grantwire.Request) (grantwire.Opened, error)
	token string
	logw  io.Writer
	files []servedFile

	mu   sync.Mutex
	gone chan struct{} // closed when the chassis no longer answers for the grant
	once sync.Once
	wg   sync.WaitGroup
}

// resolvePath is where a declared path is served: under the home
// directory for `~/…`, else under dir.
func resolvePath(dir, home, declared string) (string, error) {
	if !sandbox.ValidPath(declared) {
		return "", fmt.Errorf("%q is not a path a sandbox may serve", declared)
	}
	if rest, ok := strings.CutPrefix(declared, "~/"); ok {
		if home == "" {
			return "", fmt.Errorf("%s: no home directory to serve it under", declared)
		}
		return filepath.Join(home, rest), nil
	}
	return filepath.Join(dir, declared), nil
}

// newFileServer lays out what to serve: the keys each sandbox serves
// (grantwire Opened.Serve), resolved under dir and home. Two sandboxes
// may not serve one path.
func newFileServer(open func(context.Context, grantwire.Request) (grantwire.Opened, error), token string, logw io.Writer,
	dir, home string, serve map[string][]string) (*fileServer, error) {
	s := &fileServer{open: open, token: token, logw: logw, gone: make(chan struct{})}
	byPath := map[string]string{}
	names := make([]string, 0, len(serve))
	for n := range serve {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, key := range serve[name] {
			kind, declared, ok := sandbox.ParseEntry(key)
			if !ok || kind == sandbox.EntryVar {
				return nil, fmt.Errorf("%s: the chassis named %q to serve, which is not a file", name, key)
			}
			p, err := resolvePath(dir, home, declared)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", name, err)
			}
			if other, dup := byPath[p]; dup {
				return nil, fmt.Errorf("sandboxes %s and %s both serve %s", other, name, p)
			}
			byPath[p] = name
			s.files = append(s.files, servedFile{sandbox: name, key: key, kind: kind, shown: declared, path: p})
		}
	}
	return s, nil
}

// prepare makes each pipe: its directory (0700 when made here), and the
// FIFO itself, mode 0600. A FIFO already at the path — one a server that
// died left — is replaced; anything else there is never touched.
func (s *fileServer) prepare() error {
	for _, f := range s.files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
			return fmt.Errorf("%s: %v", f.path, err)
		}
		switch fi, err := os.Lstat(f.path); {
		case err == nil && fi.Mode()&os.ModeNamedPipe != 0:
			_ = os.Remove(f.path)
		case err == nil:
			return fmt.Errorf("%s exists and is not a pipe this command made: move it away to serve it", f.path)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("%s: %v", f.path, err)
		}
		if err := mkfifo(f.path); err != nil {
			s.remove()
			return err
		}
	}
	return nil
}

func mkfifo(path string) error {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return fmt.Errorf("%s: making the pipe: %v", path, err)
	}
	// The umask may have taken bits away, never added them; set exactly.
	return os.Chmod(path, 0o600)
}

// serve starts one loop per file; each runs until ctx ends, or until the
// chassis no longer answers for the grant (Gone).
func (s *fileServer) serve(ctx context.Context) {
	for _, f := range s.files {
		s.wg.Add(1)
		go func(f servedFile) {
			defer s.wg.Done()
			s.loop(ctx, f)
		}(f)
	}
}

// Gone is closed when the chassis has refused even to name the files: the
// run's grant has ended (or no longer names the sandbox).
func (s *fileServer) Gone() <-chan struct{} { return s.gone }

// stop waits for the loops (ctx must be done) and removes every pipe.
func (s *fileServer) stop() {
	s.wg.Wait()
	s.remove()
}

func (s *fileServer) remove() {
	for _, f := range s.files {
		if fi, err := os.Lstat(f.path); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
			_ = os.Remove(f.path)
		}
		_ = os.Remove(f.path + ".next")
	}
}

func (s *fileServer) loop(ctx context.Context, f servedFile) {
	for ctx.Err() == nil {
		// A reader? Opening a FIFO to write without blocking fails with
		// ENXIO while nobody has it open to read.
		w, err := os.OpenFile(f.path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if !errors.Is(err, syscall.ENXIO) {
				s.logf(f, "cannot open the pipe: %v", err)
				if !sleepCtx(ctx, time.Second) {
					return
				}
				continue
			}
			if !sleepCtx(ctx, sandboxPoll) {
				return
			}
			continue
		}
		// The next pipe, before anything is written to this one.
		next := f.path + ".next"
		_ = os.Remove(next)
		nerr := mkfifo(next)

		value, status, refused := s.fetch(ctx, f)
		if value != nil {
			_ = w.SetWriteDeadline(time.Now().Add(sandboxWriteTimeout))
			if _, err := w.Write(value); err != nil {
				status = fmt.Sprintf("the reader went away before it read it all (%v)", err)
			}
			for i := range value {
				value[i] = 0
			}
		}
		if nerr == nil {
			// The reader keeps the pipe it opened; the next open finds the
			// fresh one.
			if err := os.Rename(next, f.path); err != nil {
				s.logf(f, "re-arming the pipe: %v", err)
			}
		}
		_ = w.Close()
		s.logf(f, "%s", status)

		if refused && s.ended(ctx, f.sandbox) {
			s.markGone()
			return
		}
	}
}

// fetch asks the chassis for one file's value now: a file's own value, or
// the variables as dotenv lines. refused: the chassis decided against it.
func (s *fileServer) fetch(ctx context.Context, f servedFile) (value []byte, status string, refused bool) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	got, err := s.open(cctx, grantwire.Request{Token: s.token, Sandbox: f.sandbox, Entry: f.key})
	switch {
	case errors.Is(err, grantwire.ErrRefused):
		return nil, "refused", true
	case err != nil:
		return nil, fmt.Sprintf("unavailable, served empty: %v", err), false
	}
	defer func() {
		for _, v := range got.Env {
			for i := range v {
				v[i] = 0
			}
		}
	}()
	if f.kind == sandbox.EntryFile {
		v := got.Env[f.key]
		out := append([]byte(nil), v...)
		return out, fmt.Sprintf("delivered (%d bytes)", len(out)), false
	}
	out := dotenvLines(got.Env)
	return out, fmt.Sprintf("delivered (%d variables)", len(got.Env)), false
}

// ended asks the chassis only which files the sandbox serves: refused, and
// the grant is over (or no longer names the sandbox).
func (s *fileServer) ended(ctx context.Context, name string) bool {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.open(cctx, grantwire.Request{Token: s.token, Sandbox: name, Entry: grantwire.EntryList})
	return errors.Is(err, grantwire.ErrRefused)
}

func (s *fileServer) markGone() { s.once.Do(func() { close(s.gone) }) }

func (s *fileServer) logf(f servedFile, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.logw, "%s txco sandbox: %s %s: %s\n", time.Now().UTC().Format(time.RFC3339), f.sandbox, f.shown, fmt.Sprintf(format, args...))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dotenvPlain is a value written as it is; anything else is single-quoted
// the way a shell reads it (`set -a; . ./run.env`) and dotenv libraries
// read it, a quote inside as '\''.
var dotenvPlain = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]*$`)

// dotenvLines is the variables as a .env file: NAME=value, sorted.
func dotenvLines(env map[string][]byte) []byte {
	names := make([]string, 0, len(env))
	for n := range env {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		v := env[n]
		b.WriteString(n)
		b.WriteByte('=')
		if dotenvPlain.Match(v) {
			b.Write(v)
		} else {
			b.WriteByte('\'')
			b.Write(bytes.ReplaceAll(v, []byte("'"), []byte(`'\''`)))
			b.WriteByte('\'')
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}
