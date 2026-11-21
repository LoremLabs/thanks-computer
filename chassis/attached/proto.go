package attached

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// The link is one connection, dialed out by the machine, carrying yamux
// streams. Every stream is one exchange: a JSON header line that names its
// kind, then one JSON line back. The header carries a payload the transport
// does not look into, so a new kind of work needs no new transport.
//
//	the machine opens:  hello     -> challenge -> answer -> welcome
//	                    complete  -> ack                    (a detached job ended)
//	the chassis opens:  request   -> reply
//	                    cancel    -> ack
//	                    stop      -> ack
//	                    bye                                 (why the link is closing)

// ProtocolVersion is signed into every challenge, so an answer made for one
// version of this protocol is never an answer for another.
const ProtocolVersion = "txco-attach-v1"

// Stream kinds.
const (
	KindHello    = "hello"
	KindRequest  = "request"
	KindCancel   = "cancel"
	KindStop     = "stop"
	KindComplete = "complete"
	KindBye      = "bye"
)

// Reasons a link is closed by the chassis. A machine told `replaced` or
// `revoked` stops serving that credential and does not reconnect.
const (
	ReasonReplaced = "replaced"
	ReasonRevoked  = "revoked"
	ReasonRefused  = "refused"
	ReasonShutdown = "shutdown"
)

// maxMessage bounds one JSON line on a stream. An exec's two outputs are
// capped at 1 MiB each before JSON escaping.
const maxMessage = 16 << 20

// handshakeTimeout bounds the hello exchange, and with it the life of a
// challenge: an answer that arrives later is refused.
const handshakeTimeout = 10 * time.Second

// Header opens a stream.
type Header struct {
	Kind string `json:"kind"`

	// hello
	Credential string `json:"credential,omitempty"`
	Machine    string `json:"machine,omitempty"`
	Root       string `json:"root,omitempty"`
	OS         string `json:"os,omitempty"`
	Version    string `json:"version,omitempty"`

	// request, cancel
	ID string `json:"id,omitempty"`
	// WithinMS is how long the machine has to answer, counted from when it
	// reads the request. A duration, never a time of day: the machine's
	// clock is its owner's, and may be minutes off.
	WithinMS int64 `json:"within_ms,omitempty"`
	// A request made by an async op carries that op's continuation. The
	// machine sends both back, unchanged, with the job's end (complete).
	OPC   string `json:"opc,omitempty"`
	Token string `json:"token,omitempty"`

	// stop, and the job a completion is for
	Job string `json:"job,omitempty"`

	// bye
	Reason string `json:"reason,omitempty"`

	// request: the work. complete: the result.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Challenge is the chassis's answer to a hello.
type Challenge struct {
	Nonce []byte `json:"nonce"`
	Conn  string `json:"conn"`
	// Refused is set instead of a nonce when the hello itself is refused.
	Refused string `json:"refused,omitempty"`
}

// Answer is the machine's signature over ChallengeBytes.
type Answer struct {
	Signature []byte `json:"signature"`
}

// Welcome ends the handshake.
type Welcome struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Gen    int64  `json:"gen,omitempty"`
	// What the credential is for, as the chassis recorded it. Shown to the
	// owner; decides nothing.
	Label string `json:"label,omitempty"`
	Name  string `json:"name,omitempty"`
}

// WireError is a coded failure on the wire: a workspace.Error's two fields.
type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *WireError) err() error {
	if e == nil {
		return nil
	}
	return &workspace.Error{Code: e.Code, Message: e.Message}
}

// Reply closes a stream the chassis opened, or acknowledges one the machine
// opened.
type Reply struct {
	OK      bool            `json:"ok"`
	Error   *WireError      `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ExecPayload is a request's payload today: one workspace exec.
type ExecPayload struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Stdin   []byte            `json:"stdin,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// TimeoutMS is the command's life: the whole of a held exec, or a
	// detached job's own deadline.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
	Detach    bool  `json:"detach,omitempty"`
}

// ExecReply is what the machine's `_attach` stack answers: an exec's result,
// the job a detached exec left running, or a coded error.
type ExecReply struct {
	Exit            int    `json:"exit"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	WallMS          int64  `json:"wall_ms,omitempty"`
	Job             string `json:"job,omitempty"`
	// Stopped: a detached job ended because it was asked to.
	Stopped bool       `json:"stopped,omitempty"`
	Error   *WireError `json:"error,omitempty"`
}

// ChallengeBytes is what a machine signs to prove it holds a credential's
// key ON THIS CONNECTION. Each part is length-prefixed, so no two different
// lists of parts make the same bytes.
//
// The parts bind the answer to: this protocol; the host the machine dialed
// (an answer relayed from another host fails); the one credential; the one
// connection; and a nonce the chassis made for it and uses once.
func ChallengeBytes(host, credential, conn string, nonce []byte) []byte {
	var b bytes.Buffer
	for _, part := range [][]byte{[]byte(ProtocolVersion), []byte(host), []byte(credential), []byte(conn), nonce} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		b.Write(n[:])
		b.Write(part)
	}
	return b.Bytes()
}

// writeMsg writes v as one JSON line.
func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxMessage {
		return fmt.Errorf("attached: message of %d bytes exceeds the link's limit", len(b))
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// msgReader reads JSON lines from one stream, each within maxMessage.
type msgReader struct{ dec *json.Decoder }

func newMsgReader(r io.Reader) *msgReader {
	return &msgReader{dec: json.NewDecoder(&capReader{r: r, left: maxMessage})}
}

func (m *msgReader) read(v any) error {
	if err := m.dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// capReader fails once more than its budget has been read: a stream carries
// a few small lines, and a peer that sends more is not speaking the protocol.
type capReader struct {
	r    io.Reader
	left int64
}

var errTooLarge = errors.New("attached: message exceeds the link's limit")

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func away(msg string) error    { return &workspace.Error{Code: workspace.CodeAway, Message: msg} }
func unknown(msg string) error { return &workspace.Error{Code: workspace.CodeUnknown, Message: msg} }
