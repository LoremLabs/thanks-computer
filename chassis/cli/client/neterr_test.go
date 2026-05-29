package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// wrapDialErr mirrors the shape Go's net/http surfaces for a refused
// connection: *url.Error → *net.OpError → *os.SyscallError → ECONNREFUSED.
// Building one by hand lets us test prettifyNetworkError without
// actually opening a socket.
func wrapDialErr(method, urlStr string, syscallErr syscall.Errno) error {
	opErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &net.AddrError{Err: "syscall", Addr: ""},
	}
	// Replace the inner err with the real syscall error so errors.Is
	// works the way the production stack does.
	opErr.Err = fmt.Errorf("%w", syscallErr)
	return &url.Error{Op: method, URL: urlStr, Err: opErr}
}

func TestPrettifyConnectionRefused(t *testing.T) {
	err := wrapDialErr("Post",
		"http://localhost:8081/v1/tenants/default/auth/browser/bootstrap",
		syscall.ECONNREFUSED)
	got := prettifyNetworkError(err, "http://localhost:8081")
	if !strings.Contains(got.Error(), "localhost:8081") {
		t.Errorf("missing host:port in message: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "connection refused") {
		t.Errorf("missing 'connection refused' hint: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "is the chassis running") {
		t.Errorf("missing the actionable nudge: %q", got.Error())
	}
	// Underlying error must still be reachable via errors.Is so
	// programmatic callers (and future retry logic) can branch on it.
	if !errors.Is(got, syscall.ECONNREFUSED) {
		t.Errorf("errors.Is should still find ECONNREFUSED")
	}
}

func TestPrettifyDNSError(t *testing.T) {
	urlErr := &url.Error{
		Op:  "Post",
		URL: "http://typo.invalid/anything",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &net.DNSError{Name: "typo.invalid", Err: "no such host"},
		},
	}
	got := prettifyNetworkError(urlErr, "http://typo.invalid")
	if !strings.Contains(got.Error(), "typo.invalid") {
		t.Errorf("missing host name: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "resolve") {
		t.Errorf("missing 'resolve' wording: %q", got.Error())
	}
}

// timeoutErr satisfies net.Error so url.Error.Timeout() reports true
// — the production timeout path that fires when http.Client.Timeout
// elapses.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestPrettifyTimeout(t *testing.T) {
	urlErr := &url.Error{Op: "Get", URL: "http://slow.example/", Err: timeoutErr{}}
	got := prettifyNetworkError(urlErr, "http://slow.example:8081")
	if !strings.Contains(got.Error(), "timed out") {
		t.Errorf("missing 'timed out' wording: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "slow.example:8081") {
		t.Errorf("missing host:port in message: %q", got.Error())
	}
}

func TestPrettifyPassthroughUnknown(t *testing.T) {
	// An error we don't have a friendlier translation for must pass
	// through verbatim — pretending to recognise everything hides
	// genuinely novel failure modes from operators.
	orig := errors.New("something weird")
	got := prettifyNetworkError(orig, "http://localhost:8081")
	if got != orig {
		t.Errorf("expected passthrough, got %v", got)
	}
}

func TestChassisHostFromAddr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://localhost:8081", "localhost:8081"},
		{"https://chassis.example.com", "chassis.example.com"},
		{"http://[::1]:8081", "[::1]:8081"},
		{"", "(unknown chassis)"},
	}
	for _, tc := range cases {
		got := chassisHostFromAddr(tc.in)
		if got != tc.want {
			t.Errorf("chassisHostFromAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
