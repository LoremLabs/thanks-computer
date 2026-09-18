package tcp

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// EchoHandler is the example protocol handler: `;handler=echo`.
const EchoHandler = "echo"

// echoLimit is how many bytes an echo connection gets back before the
// handler hangs up.
const echoLimit = 500

// lingerClose says goodbye before the head closes the socket: half-close
// our side so the peer reads a clean EOF after the last echoed byte, then
// discard — briefly, and not much — whatever it was still sending.
// Closing with its bytes unread would reset the connection instead, and a
// reset can cost the peer the end of the echo.
func lingerClose(c net.Conn) {
	cw, ok := c.(interface{ CloseWrite() error })
	if !ok || cw.CloseWrite() != nil {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.CopyN(io.Discard, c, 64<<10)
}

func init() {
	RegisterHandler(EchoHandler, func(pu *processor.Unit) Handler {
		return &echoHandler{lim: parseLimits(pu)}
	})
}

// echoHandler is the smallest protocol that is not the line protocol: a
// raw byte stream with no framing, written back as it arrives, closed
// after echoLimit bytes. It is here as the worked example of the Handler
// seam, and as something to point a client at when checking a listener,
// an edge or a hostname end to end.
//
// It shows what a real handler does: it serves only connections the head
// routed (so a stack opts in with an `_echo` inlet, whose connect run may
// greet or refuse like any other), it owns the bytes between accept and
// close without a pipeline run per read, it hangs up politely, and it
// reports to the stack under its own source — one `@src == "echo"` event
// when the connection ends, carrying `@echo.bytes` and `@echo.reason`.
type echoHandler struct {
	lim limits
}

func (h *echoHandler) ServeConn(ctx context.Context, rc RoutedConn) error {
	buf := make([]byte, echoLimit)
	sent, reason := 0, "limit"
	var failed error
	for sent < echoLimit {
		_ = rc.Conn.SetReadDeadline(time.Now().Add(h.lim.idleTimeout))
		n, err := rc.Conn.Read(buf[:echoLimit-sent])
		if n > 0 {
			_ = rc.Conn.SetWriteDeadline(time.Now().Add(h.lim.respTimeout))
			if _, werr := rc.Conn.Write(buf[:n]); werr != nil {
				reason, failed = "error", werr
				break
			}
			sent += n
		}
		if err != nil {
			var ne net.Error
			switch {
			case errors.Is(err, io.EOF):
				reason = "eof"
			case errors.As(err, &ne) && ne.Timeout():
				reason, failed = "idle", errors.New("idle timeout")
			default:
				reason, failed = "error", err
			}
			break
		}
	}
	if reason == "limit" {
		lingerClose(rc.Conn)
	}
	// The stack hears about the connection once, after the fact. Nothing
	// it answers can matter any more, and an error here (shutdown, the
	// inlet withdrawn meanwhile) changes nothing: the socket closes next.
	_, _ = rc.Emit(ctx, Event{Facts: map[string]any{
		"phase":  "close",
		"bytes":  sent,
		"reason": reason,
	}})
	return failed
}
