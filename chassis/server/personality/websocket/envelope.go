package websocket

import (
	"encoding/base64"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

const (
	phaseMessage = "message"
	phaseClose   = "close"
)

// envelopeInput is everything buildEnvelope needs, so the shape is a pure
// function of its inputs (unit-testable without a socket).
type envelopeInput struct {
	rid string
	now time.Time

	tenant           string
	stack            string // the web stack that accepted
	ingress          string
	hostnameVerified bool
	private          bool

	sessionID   string
	subprotocol string
	connectedAt time.Time
	seq         int64
	state       string // raw JSON object or ""

	host, path, origin, userAgent, clientIP string

	phase string
	// phase=message
	msgType MessageType
	msg     []byte
	// phase=close
	closeCode      int
	closeReason    string
	closeInitiator string
}

// buildEnvelope renders one session run's envelope. Every `_txc.websocket.*`
// fact is chassis-stamped; the route is pre-stamped the LMTP way so
// detectTenantBody short-circuits and boot/100 promotes it.
func buildEnvelope(in envelopeInput) string {
	b := jsonx.New()
	b.Set("_txc.src", Source)
	b.Set("_txc.rid", in.rid)
	b.Set("_ts", in.now.UTC().Format(time.RFC3339))
	if in.clientIP != "" {
		b.Set("_txc.client.ip", in.clientIP)
	}
	if in.private {
		b.Set("_txc.flag_private", true)
	}

	sub := in.stack + "/" + SubStack
	b.Set("_txc.route.tenant", in.tenant)
	b.Set("_txc.route.stack", sub)
	b.Set("_txc.route.ingress", in.ingress)
	b.Set("_txc.route.hostname_verified", in.hostnameVerified)
	b.Set("_txc.route.to", sub+"/0")

	b.Set("_txc.websocket.tenant", in.tenant)
	b.Set("_txc.websocket.phase", in.phase)
	b.Set("_txc.websocket.session.id", in.sessionID)
	b.Set("_txc.websocket.session.stack", in.stack)
	b.Set("_txc.websocket.session.subprotocol", in.subprotocol)
	b.Set("_txc.websocket.session.connected_at", in.connectedAt.UTC().Format(time.RFC3339))
	b.Set("_txc.websocket.session.seq", in.seq)
	if in.state != "" {
		b.SetRaw("_txc.websocket.session.state", in.state)
	}
	b.Set("_txc.websocket.req.host", in.host)
	b.Set("_txc.websocket.req.path", in.path)
	b.Set("_txc.websocket.req.origin", in.origin)
	b.Set("_txc.websocket.req.user_agent", in.userAgent)

	switch in.phase {
	case phaseMessage:
		b.Set("_txc.websocket.msg.type", in.msgType.String())
		b.Set("_txc.websocket.msg.bytes", len(in.msg))
		if in.msgType == MessageBinary {
			b.Set("_txc.websocket.msg.data", base64.StdEncoding.EncodeToString(in.msg))
		} else {
			b.Set("_txc.websocket.msg.text", string(in.msg))
		}
	case phaseClose:
		b.Set("_txc.websocket.close.code", in.closeCode)
		b.Set("_txc.websocket.close.reason", in.closeReason)
		b.Set("_txc.websocket.close.initiated_by", in.closeInitiator)
	}
	return b.String()
}
