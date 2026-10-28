// Package grantwire is what a command and the chassis agree on when the
// command exercises a run grant: the variables the command is handed, and
// the few lines of protocol the launcher (`txco sandbox`) speaks to the
// chassis over a Unix socket.
//
// It is a leaf: the launcher is a small program that must start fast and
// import nothing of the server.
//
// The protocol is one request and one response per connection, each a line
// of JSON. A request opens ONE sandbox, by name; the program never asks for
// a secret by name:
//
//	→ {"v":2,"token":"rg1.…","sandbox":"github"}
//	← {"ok":true,"env":{"GH_TOKEN":"<base64>"}}
//	← {"ok":false,"error":"refused"}
//
// A refusal says nothing of why. The reason is in the trace of the tenant's
// `_grant` run, for whoever may read that; the program that asked may not.
package grantwire

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// The variables a command is handed with its run grant.
const (
	EnvRun    = "TXCO_RUN"        // the run's name
	EnvToken  = "TXCO_RUN_GRANT"  // the token; the launcher presents it
	EnvSocket = "TXCO_GRANT_SOCK" // where the launcher reaches the chassis
	EnvBin    = "TXCO_BIN"        // the txco binary, for `"$TXCO_BIN" sandbox …`
)

// Env is every one of them. The launcher removes them all from the
// environment of the program it starts.
var Env = []string{EnvRun, EnvToken, EnvSocket, EnvBin}

const (
	// Version is the protocol's. A chassis refuses one it does not speak.
	Version = 2
	// MaxRequest bounds one request line; a token is about 350 bytes.
	MaxRequest = 8 << 10
	// MaxResponse bounds one response line: a sandbox's values, base64, and
	// their frame.
	MaxResponse = 16 << 20
	// MaxSocketPath is the longest path the chassis will listen on. A Unix
	// socket's address is a fixed field of the kernel's: 104 bytes on macOS,
	// 108 on Linux, the terminator included.
	MaxSocketPath = 100
)

// The errors a response may carry.
const (
	ErrorRefused     = "refused"     // the request was decided, and the answer is no
	ErrorUnavailable = "unavailable" // the chassis could not decide
	ErrorBadRequest  = "bad_request" // not a request this chassis understands
)

// Request is one ask: open a sandbox. Entry is what of it: "" its
// variables (to start a program); `file:<path>` one file it serves;
// `dotenv:<path>` its variables for its dotenv file (chassis/sandbox
// ParseEntry).
type Request struct {
	V       int    `json:"v"`
	Token   string `json:"token"`
	Sandbox string `json:"sandbox"`
	Entry   string `json:"entry,omitempty"`
}

// Response is the answer.
type Response struct {
	OK    bool              `json:"ok"`
	Env   map[string]string `json:"env,omitempty"` // variable (or file:<path>) → base64 (standard, padded)
	Serve []string          `json:"serve,omitempty"` // file:<path> and dotenv:<path>: what the sandbox serves as files
	Error string            `json:"error,omitempty"`
}

// EntryList is the Request.Entry that asks only which files a sandbox
// serves: nothing is released.
const EntryList = "list"

// Opened is what an open hands over: the values, and the files the
// sandbox serves (paths only).
type Opened struct {
	Env   map[string][]byte
	Serve []string
}

var (
	// ErrRefused: the chassis decided, and the answer is no.
	ErrRefused = errors.New("refused")
	// ErrUnavailable: the chassis could not be asked, or could not decide.
	ErrUnavailable = errors.New("unavailable")
)

// Open asks the chassis listening on socket to open one sandbox and returns
// what it hands over: variable (or file:<path>) → value, and the files it
// serves. The error is ErrRefused, or wraps
// ErrUnavailable with what went wrong between here and the chassis. The
// caller owns the values and should overwrite them once they are handed on.
func Open(ctx context.Context, socket string, req Request) (Opened, error) {
	if socket == "" {
		return Opened{}, fmt.Errorf("%w: no socket", ErrUnavailable)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	req.V = Version
	line, err := json.Marshal(req)
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resp, err := ReadLine(bufio.NewReaderSize(conn, 64<<10), MaxResponse)
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return decodeResponse(resp)
}

// decodeResponse reads a Response: the values, ErrRefused, or
// ErrUnavailable with why.
func decodeResponse(resp []byte) (Opened, error) {
	var out Response
	if err := json.Unmarshal(resp, &out); err != nil {
		return Opened{}, fmt.Errorf("%w: the chassis answered with something that is not a response", ErrUnavailable)
	}
	switch {
	case out.OK:
		env := make(map[string][]byte, len(out.Env))
		for v, enc := range out.Env {
			value, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				return Opened{}, fmt.Errorf("%w: the chassis answered with a value that does not decode", ErrUnavailable)
			}
			env[v] = value
		}
		return Opened{Env: env, Serve: out.Serve}, nil
	case out.Error == ErrorRefused:
		return Opened{}, ErrRefused
	}
	return Opened{}, fmt.Errorf("%w: %s", ErrUnavailable, strings.TrimSpace(out.Error))
}

// ReadLine reads one line of at most max bytes, without its newline. A line
// that is longer is an error, not a truncation: half a request must never
// be read as a whole one.
func ReadLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max+1 {
			return nil, errors.New("line too long")
		}
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
