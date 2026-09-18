package tcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/processor"
)

func init() {
	RegisterHandler(LineHandler, func(pu *processor.Unit) Handler {
		return &lineHandler{pu: pu, lim: parseLimits(pu)}
	})
}

// lineHandler is the head's original protocol: each newline-terminated
// line is one bounded event, and the stack answers on `@tcp.res.*`.
type lineHandler struct {
	pu  *processor.Unit
	lim limits
}

func (h *lineHandler) ServeConn(ctx context.Context, rc RoutedConn) error {
	v := h.verdicts(rc)
	// One reader for the life of the connection (a fresh one per line
	// would drop whatever it had buffered past the newline).
	r := bufio.NewReader(rc.Conn)
	for {
		if err := rc.Conn.SetReadDeadline(time.Now().Add(h.lim.idleTimeout)); err != nil {
			return err
		}
		line, over, err := readLine(r, h.lim.maxLine)
		if err != nil {
			var ne net.Error
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case errors.As(err, &ne) && ne.Timeout():
				return errors.New("idle timeout")
			}
			return err
		}
		if over {
			h.pu.Logger.Warn("tcp line over limit; dropped",
				zap.String("rid", rc.RID), zap.Int("max_bytes", h.lim.maxLine))
			continue
		}
		h.pu.Logger.Debug("tcp read message", zap.String("rid", rc.RID))

		res, err := rc.Emit(ctx, Event{Body: line})
		if err != nil {
			// Shared admission gate denial: TCP has no standard rejection,
			// so write a short "<status> <reason>" line and close.
			var denied *DeniedError
			if errors.As(err, &denied) {
				v.write([]byte(strconv.Itoa(denied.Status) + " " + denied.Reason + "\n"))
			}
			return err
		}
		if why := v.apply(res.Payload.Raw, true); why != "" {
			return errors.New(why)
		}
	}
}

func (h *lineHandler) verdicts(rc RoutedConn) verdictWriter {
	return verdictWriter{
		conn:        rc.Conn,
		rid:         rc.RID,
		logger:      h.pu.Logger,
		timeout:     h.lim.respTimeout,
		hidePrivate: !strings.Contains(h.pu.Conf.WebDebug, "SHOW_PRIVATE_VARS"),
	}
}

// readLine returns the next newline-terminated line INCLUDING its
// terminator, byte for byte as sent (a body of "asdf\r\n" stays that
// way). A line longer than max is consumed to its newline and reported
// as over=true with no bytes kept, so one oversized line costs a
// bounded buffer, not the whole line in memory.
func readLine(r *bufio.Reader, max int) (line []byte, over bool, err error) {
	for {
		frag, e := r.ReadSlice('\n')
		if !over {
			if len(line)+len(frag) > max {
				over, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		switch {
		case e == nil:
			return line, over, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		default:
			return nil, over, e
		}
	}
}

// verdictWriter renders a run's `@tcp.res.*` verdict onto the socket —
// for the head's connect run on every listener, and for each line run of
// the line handler.
type verdictWriter struct {
	conn        net.Conn
	rid         string
	logger      *zap.Logger
	timeout     time.Duration
	hidePrivate bool
}

// apply renders the stack's verdict for one run. The verdict lives in the
// author-writable `_txc.tcp.res.*` subtree:
//
//	@tcp.res.write   base64 bytes to write, as-is
//	@tcp.res.action  "close" hangs up after any write; anything else keeps going
//
// echo says whether a run with no explicit write gets the default JSON
// projection of the envelope — line runs do, the connect run does not.
// Returns "" to keep going, else why the connection closes.
func (v verdictWriter) apply(out string, echo bool) string {
	if echo || gjson.Get(out, "_txc.tcp.res.write").String() != "" {
		b, err := getOutput(out, v.hidePrivate)
		if err != nil {
			v.logger.Warn("error getting output", zap.String("rid", v.rid), zap.String("err", err.Error()))
			return "bad_output"
		}
		if len(b) > 0 && !v.write(b) {
			return "write_failed"
		}
	}
	if gjson.Get(out, "_txc.tcp.res.action").String() == "close" {
		return "closed_by_stack"
	}
	return ""
}

func (v verdictWriter) write(b []byte) bool {
	_ = v.conn.SetWriteDeadline(time.Now().Add(v.timeout))
	if _, err := v.conn.Write(b); err != nil {
		v.logger.Error("write error", zap.String("rid", v.rid), zap.String("err", err.Error()))
		return false
	}
	return true
}
