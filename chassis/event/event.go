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
	// outlet writes the head and switches into incremental-flush mode.
	StreamHead

	// StreamChunk carries a block of raw (already-decoded) response body
	// bytes in Raw, to be written and flushed immediately.
	StreamChunk

	// StreamEnd terminates a streamed response. No body; the outlet
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
	Rid     string
	Src     string
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
