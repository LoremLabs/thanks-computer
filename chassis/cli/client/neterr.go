package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// NetworkError is the user-facing wrapping prettifyNetworkError uses
// when it recognises a transport-layer failure. Error() returns just
// the friendly sentence — the raw `Post "http://…": dial tcp …` text
// is hidden from `%v` output but still reachable via errors.Unwrap
// (and therefore errors.Is on syscall.ECONNREFUSED, net.DNSError,
// etc.). Debuggers grep the wrap chain; operators read the message.
type NetworkError struct {
	Msg   string
	Cause error
}

func (e *NetworkError) Error() string { return e.Msg }
func (e *NetworkError) Unwrap() error { return e.Cause }

// prettifyNetworkError translates the Go stdlib's transport-layer
// errors (connection refused, DNS lookup miss, timeout) into short
// human messages. The defaults — `Post "http://localhost:8081/...":
// dial tcp [::1]:8081: connect: connection refused` — answer the
// "what happened?" question with debugger detail; we'd rather answer
// "what should I do?" with one sentence.
//
// chassisAddr is the configured chassis URL (Target.Addr), used to
// derive a short host:port for the user-facing message.
func prettifyNetworkError(err error, chassisAddr string) error {
	if err == nil {
		return nil
	}
	host := chassisHostFromAddr(chassisAddr)

	// `connection refused` is the most common — chassis not running,
	// wrong port, or firewalled.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return &NetworkError{
			Msg:   fmt.Sprintf("can't connect to chassis at %s: connection refused — is the chassis running?", host),
			Cause: err,
		}
	}
	// `no such host` / DNS miss — typo in --url, or chassis hostname
	// not resolvable from this machine.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &NetworkError{
			Msg:   fmt.Sprintf("can't resolve chassis host %q — check --url for a typo or DNS issue", dnsErr.Name),
			Cause: err,
		}
	}
	// Timeout — chassis is reachable but didn't answer in time.
	// `errors.Is(err, context.DeadlineExceeded)` covers explicit
	// context timeouts; the url.Error.Timeout() check catches the
	// http.Client.Timeout case.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return &NetworkError{
			Msg:   fmt.Sprintf("timed out connecting to chassis at %s", host),
			Cause: err,
		}
	}
	// Other syscalls worth calling out by friendly name. EHOSTUNREACH
	// shows up on a wrong-network host; ENETUNREACH on a broken
	// route table.
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return &NetworkError{
			Msg:   fmt.Sprintf("can't reach chassis host %s — no route (check the network)", host),
			Cause: err,
		}
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return &NetworkError{
			Msg:   fmt.Sprintf("can't reach chassis host %s — network unreachable", host),
			Cause: err,
		}
	}
	// "EOF" or "connection reset by peer" usually means the chassis
	// closed the socket mid-response — frequently a TLS mismatch
	// (http:// against an HTTPS listener) or a crashed handler.
	msg := err.Error()
	if strings.Contains(msg, "EOF") || strings.Contains(msg, "connection reset") {
		return &NetworkError{
			Msg:   fmt.Sprintf("chassis at %s closed the connection unexpectedly — wrong scheme (http vs https)?", host),
			Cause: err,
		}
	}
	return err
}

// chassisHostFromAddr extracts the host:port portion of a chassis
// URL for use in user-facing messages. Falls back to the raw value
// when parsing fails — we'd rather show "http://chassis.example"
// than nothing.
func chassisHostFromAddr(addr string) string {
	if addr == "" {
		return "(unknown chassis)"
	}
	u, err := url.Parse(addr)
	if err != nil {
		return addr
	}
	if u.Host != "" {
		return u.Host
	}
	// Some callers pass a bare host:port without scheme; url.Parse
	// stuffs that into u.Path.
	return strings.TrimPrefix(addr, "/")
}
