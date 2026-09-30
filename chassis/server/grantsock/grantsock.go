// Package grantsock is the Unix socket the launcher (`txco sandbox`) reaches
// the chassis on: the `grant` personality. It carries a program's request to
// open a sandbox to the grant gateway and the gateway's answer back
// (chassis/grantwire has the protocol).
//
// It is a socket and not a route on the web head because only processes on
// this machine have any business asking: the socket lives in a directory
// only the chassis's user may enter, so nothing on the network can reach it
// and nothing else on the machine can either.
package grantsock

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/server/grantgw"
)

// Gateway is what the socket needs of the grant gateway.
type Gateway interface {
	OpenSandbox(ctx context.Context, o grantgw.Open) grantgw.Answer
	ServeSocket(path string)
}

const (
	// maxConns bounds the requests in flight. Past it a connection waits
	// its turn; the launcher asks once per sandbox, at a program's start.
	maxConns = 64
	// readTimeout bounds how long a connection may take to send its one
	// request line.
	readTimeout = 5 * time.Second
	// askTimeout bounds one request end to end. The gateway bounds the
	// tenant's rules itself; this covers the stores around them.
	askTimeout = 30 * time.Second
)

// DefaultPath is where the socket lives when --grant-socket is not given: a
// directory of its own under the system's temp dir, named for the chassis's
// data directory so two chassis on one machine do not share it and a
// restart finds the same place.
func DefaultPath(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "txco-"+hex.EncodeToString(sum[:4]), "g.sock")
}

// Controller listens on the socket.
type Controller struct {
	ctx  context.Context
	log  *zap.Logger
	gw   Gateway
	path string

	mu   sync.Mutex
	ln   net.Listener
	wg   sync.WaitGroup
	slot chan struct{}
}

// New prepares the socket at path and refuses what cannot work: a path too
// long for the kernel, a directory others may enter, a socket another
// chassis is listening on.
func New(ctx context.Context, log *zap.Logger, gw Gateway, path string) (*Controller, error) {
	if log == nil {
		log = zap.NewNop()
	}
	if gw == nil {
		return nil, errors.New("grant socket: no grant gateway on this node (it needs the identity store and the secret store)")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("grant socket: %w", err)
	}
	if len(abs) > grantwire.MaxSocketPath {
		return nil, fmt.Errorf("grant socket: the path is %d bytes and a Unix socket's may be at most %d: %s (set --grant-socket to a shorter one)",
			len(abs), grantwire.MaxSocketPath, abs)
	}
	return &Controller{ctx: ctx, log: log, gw: gw, path: abs, slot: make(chan struct{}, maxConns)}, nil
}

// Path is where the socket is.
func (c *Controller) Path() string { return c.path }

// Listen makes the socket. It is apart from Start so that a chassis that
// cannot listen fails at boot, not in a goroutine.
func (c *Controller) Listen() error {
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("grant socket: %w", err)
	}
	// The directory is the wall: only this user may enter it. MkdirAll
	// leaves an existing directory as it was, so say what it must be.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("grant socket: %s must be this user's own: %w", dir, err)
	}
	if fi, err := os.Lstat(c.path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("grant socket: %s exists and is not a socket", c.path)
		}
		// A socket file outlives the process that made it. One that answers
		// is another chassis; one that does not is left over.
		if conn, err := net.DialTimeout("unix", c.path, time.Second); err == nil {
			_ = conn.Close()
			return fmt.Errorf("grant socket: another process is listening on %s", c.path)
		}
		if err := os.Remove(c.path); err != nil {
			return fmt.Errorf("grant socket: removing the stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", c.path)
	if err != nil {
		return fmt.Errorf("grant socket: %w", err)
	}
	if err := os.Chmod(c.path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("grant socket: %w", err)
	}
	c.mu.Lock()
	c.ln = ln
	c.mu.Unlock()
	return nil
}

// Start serves the socket until Stop, and returns at once, as every
// personality's Start does. Listen must have succeeded; without it Start
// does nothing.
func (c *Controller) Start() {
	c.mu.Lock()
	ln := c.ln
	c.mu.Unlock()
	if ln == nil {
		return
	}
	c.gw.ServeSocket(c.path)
	c.log.Info("grant socket listening", zap.String("path", c.path))
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.accept(ln)
	}()
}

func (c *Controller) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			c.log.Warn("grant socket: accept", zap.Error(err))
			select {
			case <-time.After(100 * time.Millisecond):
				continue
			case <-c.ctx.Done():
				return
			}
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer conn.Close()
			select {
			case c.slot <- struct{}{}:
				defer func() { <-c.slot }()
			case <-c.ctx.Done():
				return
			}
			c.serve(conn)
		}()
	}
}

// Stop closes the socket, waits for the requests in flight, and removes it.
func (c *Controller) Stop() {
	c.gw.ServeSocket("")
	c.mu.Lock()
	ln := c.ln
	c.ln = nil
	c.mu.Unlock()
	if ln == nil {
		return
	}
	_ = ln.Close()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	_ = os.Remove(c.path)
}

func reply(conn net.Conn, r grantwire.Response) {
	line, _ := json.Marshal(r)
	_ = conn.SetWriteDeadline(time.Now().Add(readTimeout))
	_, _ = conn.Write(append(line, '\n'))
}

// serve answers the one request of a connection.
func (c *Controller) serve(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	line, err := grantwire.ReadLine(bufio.NewReaderSize(conn, grantwire.MaxRequest), grantwire.MaxRequest)
	if err != nil {
		reply(conn, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	// A request names a sandbox and nothing else: there is no way to ask
	// for a secret by name, and an older launcher's request (v1, `kind` and
	// `name`) is not one this chassis understands.
	var req grantwire.Request
	if err := json.Unmarshal(line, &req); err != nil || req.V != grantwire.Version || req.Token == "" || !sandbox.ValidName(req.Sandbox) {
		reply(conn, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, askTimeout)
	defer cancel()
	ans := c.gw.OpenSandbox(ctx, grantgw.Open{Token: req.Token, Sandbox: req.Sandbox, Via: grantgw.ViaLauncher})
	switch {
	case ans.Allowed:
		env := make(map[string]string, len(ans.Env))
		for v, value := range ans.Env {
			env[v] = base64.StdEncoding.EncodeToString(value)
		}
		reply(conn, grantwire.Response{OK: true, Env: env})
		ans.Zero()
	case ans.Unavailable:
		reply(conn, grantwire.Response{Error: grantwire.ErrorUnavailable})
	default:
		// One answer for every refusal: the program is not told why.
		reply(conn, grantwire.Response{Error: grantwire.ErrorRefused})
	}
}
