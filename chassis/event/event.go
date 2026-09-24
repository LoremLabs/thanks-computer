package event

import (
	"bytes"
	"context"
	"fmt"
	"reflect"

	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Event payloads may have different types
type Type int

const (
	// Null is an empty message, an ACK
	Null Type = iota

	// JSON is a raw block of JSON
	JSON

	// Error
	ErrorStr

	// StreamHead carries the response status + headers snapshot for a
	// streamed HTTP response. Sent once, before any StreamChunk; the
	// response writer writes the head and switches into incremental-flush mode.
	StreamHead

	// StreamChunk carries a block of raw (already-decoded) response body
	// bytes in Raw, to be written and flushed immediately.
	StreamChunk

	// StreamEnd terminates a streamed response. No body; the response writer
	// returns once it receives this.
	StreamEnd

	// PBUF?
)

type RawJSON string

type Payload struct {
	Raw  string `json:"raw,omitempty"`
	Type Type   `json:"type,omitempty"`
	Meta string `json:"meta,omitempty"`
}

type Envelope struct {
	Ctx     context.Context
	Payload *Payload
	ResCh   chan Payload
	// ResultCh, when set, replaces ResCh: the bus loop answers once with
	// the run's DispatchResult (final payload + trusted tenant/stack)
	// after the pipeline returns. Streaming responses need ResCh.
	ResultCh chan DispatchResult
	// Accepted, when set, is told once — before the tenant's stack runs
	// — that the chassis has accepted this event for execution: routed
	// out of _sys/boot into a concrete tenant and admitted. An inlet that
	// delivers from a durable outbox closes its claim on this signal
	// rather than at completion, so a long run never holds a delivery
	// claim open. The send is non-blocking: buffer the channel at 1. It
	// is never sent for a run that stays in _sys, or for one admission
	// denies (those answer on ResCh/ResultCh as before).
	Accepted chan Acceptance
	Rid      string
	Src      string
}

// Acceptance is the trusted routing outcome at the moment the chassis
// commits to running a tenant's stack: the pinned tenant and the stack
// the boot handoff routed into, read from immutable pipeline state
// (processor.TenantObserver), never from the envelope.
type Acceptance struct {
	Tenant string
	Stack  string
}

// DispatchResult is the bus loop's trusted summary of one run: the final
// payload plus the tenant and stack the chassis pinned — read from
// immutable pipeline state (processor.TenantObserver), never parsed out
// of the author-visible envelope, whose `_txc.tenant` a stack could
// rewrite. An inlet that needs the routing outcome (the TCP head deciding
// whether a connection stays open) sets Envelope.ResultCh and leaves
// ResCh nil.
type DispatchResult struct {
	Payload Payload
	Tenant  string // "" or "_sys": the run never left the system tenant
	Stack   string // the routed stack; "" when unrouted
	// Ingress and HostnameVerified are the rest of the route the boot
	// handoff promoted (`_txc.ingress`, `_txc.hostname_verified`), so an
	// inlet that pins a connection can pre-stamp the same route on the
	// connection's later events.
	Ingress          string
	HostnameVerified bool
	Err              error // pipeline error, if any
}

type OpsHandler interface {
	Route(ctx context.Context, opName string, in []byte, out []byte) (Payload, error)
}

// OpsHandlerFunc adapts a plain function to the OpsHandler interface,
// in the same shape as http.HandlerFunc. Built-in core ops (e.g.
// txco://noop) are registered as OpsHandlerFunc values.
type OpsHandlerFunc func(ctx context.Context, opName string, in []byte, out []byte) (Payload, error)

// Route satisfies OpsHandler.
func (f OpsHandlerFunc) Route(ctx context.Context, opName string, in []byte, out []byte) (Payload, error) {
	return f(ctx, opName, in, out)
}

// String returns a string representation of the type.
func (t Type) String() string {
	switch t {
	default:
		return ""
	case Null:
		return "Null"
	case JSON:
		return "JSON"
	case ErrorStr:
		return "ErrorStr"
	case StreamHead:
		return "StreamHead"
	case StreamChunk:
		return "StreamChunk"
	case StreamEnd:
		return "StreamEnd"
	}
}

func PackageJSON(ctx context.Context, raw string, res chan Payload, src string) *Envelope {

	// rid setting, doesn't belong here. does rid even?
	rid, ok := ctx.Value(config.CtxKeyRid).(string)
	if (!ok) || (rid == "") {
		rid = hxid.NewTimeSort().String()
	}

	envelope := &Envelope{
		Ctx: ctx,
		Payload: &Payload{
			Raw:  raw,
			Type: JSON,
		},
		ResCh: res,
		Rid:   rid,
		Src:   src,
	}

	return envelope
}

func NewJSON(raw string) RawJSON {
	return RawJSON(raw)
}

func (raw RawJSON) Set(insertPoint string, val interface{}) RawJSON {
	rawString, _ := sjson.Set(string(raw), insertPoint, val)
	return RawJSON(rawString)
}

func (raw RawJSON) String() string {
	return string(raw)
}

func (raw RawJSON) CreateJSONPayload() (Payload, error) {
	return Payload{
		Raw:  string(raw),
		Type: JSON,
	}, nil
}

func CreateJSONPayload(raw string) Payload {
	return Payload{
		Raw:  raw,
		Type: JSON,
	}
}

func ErrResponse(errMsg string, raw string, err error) (Payload, error) {
	raw, _ = sjson.Set(raw, "actions.-1", errMsg)
	payload := Payload{
		Raw:  raw,
		Type: JSON,
	}
	return payload, err
}

func (payload Payload) String() string {
	return printStruct(payload, true)
}

func printStruct(s interface{}, names bool) string {
	// for debugging ht: https://stackoverflow.com/questions/33142594/how-to-print-struct-with-string-of-fields
	v := reflect.ValueOf(s)
	t := v.Type()
	// To avoid panic if s is not a struct:
	if t.Kind() != reflect.Struct {
		return fmt.Sprint(s)
	}

	b := &bytes.Buffer{}
	b.WriteString("{")
	for i := 0; i < v.NumField(); i++ {
		if i > 0 {
			b.WriteString(" ")
		}
		v2 := v.Field(i)
		if names {
			b.WriteString(t.Field(i).Name)
			b.WriteString(":")
		}
		if v2.CanInterface() {
			if st, ok := v2.Interface().(fmt.Stringer); ok {
				b.WriteString(st.String())
				continue
			}
		}
		fmt.Fprint(b, v2)
	}
	b.WriteString("}")
	return b.String()
}
