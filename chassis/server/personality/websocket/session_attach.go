package websocket

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/attach"
)

// This file is the attached-transport frame path (D1, D3, D6, D11). Once a
// message run binds a PTY to the session (workspace://<name>/attach), the
// session stops running the stack per frame: inbound binary is the
// terminal's stdin, inbound text is a typed control envelope, and a pump
// goroutine carries the terminal's output back out. The binding lives in
// processor.Unit.Attachments; the session holds a pointer to the one that
// is its own.

// maybeAttach is called after each message run. If that run bound a PTY to
// this session (and no pump is running yet), the session goes into attached
// mode and the pump starts. Idempotent and cheap on the common path (one
// registry lookup, no attachment → return).
func (s *session) maybeAttach() {
	if s.c.pu.Attachments == nil || s.attachment.Load() != nil {
		return
	}
	att, ok := s.c.pu.Attachments.Lookup(s.tenant, s.id)
	if !ok {
		return
	}
	s.attachment.Store(att)
	s.startPump(att)
}

// handleAttachedFrame processes one inbound frame while attached, in the
// reader goroutine — NOT through the 16-deep inbound queue, whose overflow
// closes the session (A4). One reader means ordering is preserved for free,
// and a slow PTY applies real backpressure through TCP.
func (s *session) handleAttachedFrame(att *attach.Attachment, typ MessageType, data []byte) {
	s.touch()
	if typ == MessageBinary {
		att.BytesIn.Add(int64(len(data)))
		if _, err := att.Conn.Write(data); err != nil {
			// The process is gone; the pump's read will end the session.
			s.c.pu.Logger.Debug("websocket attached write failed",
				zap.String("sid", s.id), zap.String("err", err.Error()))
		}
		return
	}
	// Text is a control envelope: {"type":"resize"|"signal"|"detach", ...}.
	var ctl struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &ctl); err != nil || ctl.Type == "" {
		_ = s.send(s.ctx, MessageText, []byte(`{"type":"error","error":"control frame must be JSON with a type"}`))
		return
	}
	switch ctl.Type {
	case attach.ControlDetach:
		s.detach("client detached")
	case attach.ControlResize, attach.ControlSignal:
		if err := att.Conn.Control(att.Context(), ctl.Type, data); err != nil {
			_ = s.send(s.ctx, MessageText, []byte(fmt.Sprintf(`{"type":"error","error":%q}`, err.Error())))
		}
	default:
		_ = s.send(s.ctx, MessageText, []byte(fmt.Sprintf(`{"type":"error","error":"unknown control type %q"}`, ctl.Type)))
	}
}

// startPump carries the resource's output to the client: read the binding,
// write a binary frame, forever — until the process ends (EOF) or the
// attachment is closed. Backpressure copies the streaming shape (D11): a
// bounded read, a blocking write whose deadline (--websocket-write-timeout)
// closes a client too slow to drain. Every write touches liveness (s.send
// does), so the idle timeout never fires on a session that is only
// receiving output — a human watching a build.
func (s *session) startPump(att *attach.Attachment) {
	s.c.wg.Add(1)
	go func() {
		defer s.c.wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := att.Conn.Read(buf)
			if n > 0 {
				att.BytesOut.Add(int64(n))
				frame := make([]byte, n)
				copy(frame, buf[:n])
				if werr := s.send(att.Context(), MessageBinary, frame); werr != nil {
					return // the client is gone; send already closed the session
				}
			}
			if rerr != nil {
				s.pumpEnd(att)
				return
			}
		}
	}()
}

// pumpEnd runs once the binding's output is exhausted. Why it ended decides
// what the client is told and whether the socket lives:
//
//   - detach: the client asked; the process is already cancelled and the
//     lease released; the socket returns to ordinary per-message runs.
//   - expiry: max_duration passed; an {"type":"expired"} frame, then the exit.
//   - process exit: the last output flushed; {"type":"exit","code":N}, close 1000.
func (s *session) pumpEnd(att *attach.Attachment) {
	// Take the binding off the session first, so the reader stops treating
	// frames as terminal input and (on detach) the socket is usable again.
	if s.attachment.CompareAndSwap(att, nil) {
		s.c.detachUnbind(s.tenant, s.id, att)
	}
	// Teardown or an explicit detach already decided the socket's fate; a
	// closing socket cannot take frames anyway.
	if s.closed.Load() || att.Reason() == "client detached" {
		return
	}
	exit, _ := att.Conn.Wait()
	if att.Reason() == "expired" {
		_ = s.send(s.ctx, MessageText, []byte(`{"type":"expired"}`))
	}
	_ = s.send(s.ctx, MessageText, []byte(fmt.Sprintf(`{"type":"exit","code":%d}`, exit)))
	s.closeWith(1000, "process exited", initiatorChassis)
}

// detach ends the current attachment without closing the socket: the
// process is cancelled and the lease released, and the socket returns to
// ordinary per-message stack runs (D6). The pump reads EOF next and runs
// pumpEnd, which clears the session's pointer.
func (s *session) detach(reason string) {
	att := s.attachment.Load()
	if att == nil {
		return
	}
	s.c.detachUnbind(s.tenant, s.id, att)
	att.Close(reason)
}

// teardownAttachment is called from finish(): end any binding so the
// process dies with the socket. Distinct reason so the pump does not try to
// write an exit frame down a closing socket.
func (s *session) teardownAttachment() {
	if att := s.attachment.Load(); att != nil {
		s.c.detachUnbind(s.tenant, s.id, att)
		att.Close("session closed")
	}
}

// detachUnbind removes the session's attachment from the registry once.
func (c *Controller) detachUnbind(tenant, sid string, att *attach.Attachment) {
	if c.pu.Attachments == nil {
		return
	}
	if cur, ok := c.pu.Attachments.Lookup(tenant, sid); ok && cur == att {
		c.pu.Attachments.Unbind(tenant, sid)
	}
}

// Attached reports whether the session currently holds a PTY (used to
// refuse txco://websocket/send racing the pump — D5).
func (s *session) Attached() bool { return s.attachment.Load() != nil }
