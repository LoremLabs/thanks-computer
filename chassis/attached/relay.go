package attached

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// Relay is the hop between nodes: it carries a header from the node that
// runs an op to the node that holds the machine's link, and that node's
// reply back. The bundled chassis defines the seam and ships no backend; a
// build with a message bus registers one. On a single node there is none,
// and a link held elsewhere cannot exist.
//
// It mirrors the WebSocket relay (personality/websocket/relay.go): the
// interface and the registry here, a backend self-registered by init() and
// a blank import, its connection details in its own environment. Which node
// holds a link is not the relay's question: the claim in the shared KV says
// so. The relay addresses a node and waits for its one answer.
type Relay interface {
	// Forward delivers env to node and returns the reply of the link there.
	// Its error says what the caller may conclude, as Hub.Send's does:
	//
	//   - CodeAway: nothing was delivered. Nothing answers for that node, or
	//     the node refused before anything reached the machine.
	//   - CodeUnknown: the request left this node and no answer came back
	//     in time. The machine may have it.
	Forward(ctx context.Context, node string, env Envelope) (Reply, error)
	// Notify tells node that a link it holds is over. Best effort, and
	// nothing depends on it: a link that was replaced or revoked is refused
	// every request whether or not its node has heard.
	Notify(ctx context.Context, node string, n Notice) error
	// Shutdown stops answering for this node and releases the bus, bounded
	// by ctx.
	Shutdown(ctx context.Context) error
}

// Envelope is one header on its way to a link on another node.
type Envelope struct {
	WorkspaceID string `json:"workspace_id"`
	// Conn and Gen are the link the sender read in the claim. The holding
	// node forwards only to exactly that link.
	Conn   string `json:"conn"`
	Gen    int64  `json:"gen"`
	Header Header `json:"header"`
	// WaitMS is how long the holding node may wait for the machine: the
	// sender's own patience, less the hop back. A duration, so two nodes'
	// clocks need not agree.
	WaitMS int64 `json:"wait_ms"`
}

// Notice says a link is over, and why.
type Notice struct {
	WorkspaceID string `json:"workspace_id"`
	Conn        string `json:"conn"`
	Reason      string `json:"reason"`
}

// LocalHandler is what a Relay backend calls for what arrives at THIS
// node. It reaches only links this node holds, never the relay, so a
// request cannot bounce between nodes.
type LocalHandler interface {
	Forward(ctx context.Context, env Envelope) (Reply, error)
	Notify(n Notice)
}

// RelayConfig carries what a backend cannot find out for itself.
type RelayConfig struct {
	// NodeID names this chassis: the name its claims record, and so the
	// name other nodes address it by.
	NodeID  string
	Logger  *zap.Logger
	Handler LocalHandler
}

// RelayConstructor builds a Relay: it connects, subscribes this node's
// inbox, and wires what arrives to cfg.Handler.
type RelayConstructor func(cfg RelayConfig) (Relay, error)

var relayRegistry = map[string]RelayConstructor{}

// RegisterRelay adds a named backend. Called from a backend package's init().
func RegisterRelay(name string, c RelayConstructor) { relayRegistry[name] = c }

// OpenRelay constructs the named backend. An unknown name is an error that
// lists what there is.
func OpenRelay(name string, cfg RelayConfig) (Relay, error) {
	c, ok := relayRegistry[name]
	if !ok {
		avail := make([]string, 0, len(relayRegistry))
		for k := range relayRegistry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("attach: unknown relay %q (available: %v)", name, avail)
	}
	return c(cfg)
}

// hopSlack is what the sender keeps back from its own deadline for the
// answer's way home.
const hopSlack = 250 * time.Millisecond

// SetRelay gives the service its hop between nodes. Call it before the
// service takes requests.
func (s *Service) SetRelay(r Relay) {
	s.relay = r
	s.Forward = func(ctx context.Context, node, workspaceID, conn string, gen int64, hdr Header) (Reply, error) {
		wait := 30 * time.Second
		if d, ok := ctx.Deadline(); ok {
			wait = time.Until(d) - hopSlack
		}
		if wait <= 0 {
			return Reply{}, away("no time left to reach the machine's node")
		}
		return r.Forward(ctx, node, Envelope{WorkspaceID: workspaceID, Conn: conn, Gen: gen, Header: hdr, WaitMS: wait.Milliseconds()})
	}
	s.Hub.Notify = func(ctx context.Context, node, workspaceID, conn, reason string) {
		nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = r.Notify(nctx, node, Notice{WorkspaceID: workspaceID, Conn: conn, Reason: reason})
	}
}

// Local is this node's side of the relay: what a backend hands inbound
// requests to.
func (s *Service) Local() LocalHandler { return localHandler{s} }

type localHandler struct{ s *Service }

func (l localHandler) Forward(ctx context.Context, env Envelope) (Reply, error) {
	if env.WaitMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(env.WaitMS)*time.Millisecond)
		defer cancel()
	}
	return l.s.Hub.Send(ctx, env.WorkspaceID, env.Conn, env.Gen, env.Header)
}

func (l localHandler) Notify(n Notice) {
	// Only the reasons a chassis gives; never one a peer made up.
	if n.Reason != ReasonReplaced && n.Reason != ReasonRevoked {
		return
	}
	l.s.Hub.Close(n.WorkspaceID, n.Conn, n.Reason)
}

// Shutdown releases the relay, if there is one.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.relay == nil {
		return nil
	}
	return s.relay.Shutdown(ctx)
}

// RelayAnswer is what a backend sends back for a Forward: the link's reply, or
// the coded error the holding node returned. One of the two.
type RelayAnswer struct {
	Reply Reply      `json:"reply"`
	Error *WireError `json:"error,omitempty"`
}

// RelayAnswerOf packs a local Forward's outcome for the wire.
func RelayAnswerOf(rep Reply, err error) RelayAnswer {
	if err == nil {
		return RelayAnswer{Reply: rep}
	}
	var we *workspace.Error
	if errors.As(err, &we) {
		return RelayAnswer{Error: &WireError{Code: we.Code, Message: we.Message}}
	}
	return RelayAnswer{Error: &WireError{Code: "provider", Message: err.Error()}}
}

// Result unpacks a RelayAnswer on the sending node.
func (a RelayAnswer) Result() (Reply, error) {
	if a.Error != nil {
		return Reply{}, a.Error.err()
	}
	return a.Reply, nil
}

// Fit shrinks an answer to at most max bytes of JSON, for a bus with a
// payload limit. Only an exec's two outputs can be large, so those are what
// is cut, and the cut is flagged as an exec's own caps flag theirs. An
// answer that still does not fit is replaced by an error saying so.
func (a RelayAnswer) Fit(max int) RelayAnswer {
	size := func(v RelayAnswer) int { b, _ := json.Marshal(v); return len(b) }
	if max <= 0 || size(a) <= max {
		return a
	}
	if a.Error != nil {
		// An error is a code and a sentence: clip the sentence, keep the code.
		if len(a.Error.Message) > 512 {
			a.Error = &WireError{Code: a.Error.Code, Message: a.Error.Message[:512]}
		}
		return a
	}
	var er ExecReply
	if !a.Reply.OK || json.Unmarshal(a.Reply.Payload, &er) != nil {
		return RelayAnswer{Error: &WireError{Code: "provider", Message: "the machine's answer is too large to carry between nodes"}}
	}
	for i := 0; i < 24 && size(a) > max; i++ {
		// Halve the larger output. JSON escaping makes the size hard to
		// predict, so cut and measure.
		if len(er.Stdout) >= len(er.Stderr) {
			er.Stdout, er.StdoutTruncated = er.Stdout[:len(er.Stdout)/2], true
		} else {
			er.Stderr, er.StderrTruncated = er.Stderr[:len(er.Stderr)/2], true
		}
		a.Reply.Payload, _ = json.Marshal(er)
	}
	if size(a) > max {
		return RelayAnswer{Error: &WireError{Code: "provider", Message: "the machine's answer is too large to carry between nodes"}}
	}
	return a
}
