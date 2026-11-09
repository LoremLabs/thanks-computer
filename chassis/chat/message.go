package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// UnmarshalJSON decodes a message as the struct's tags say, with two
// allowances for what a transcript carries:
//
//   - `"content": null` decodes as "" for every role, as it did before
//     (an assistant turn that only calls tools has null content).
//   - On a `tool` message, `content` may be any JSON value: an executor
//     hands a capability's answer back as it came. An object or array is
//     stored compacted, so Content stays a string.
//
// On any other role a non-string content is still an error, so a user
// message of content parts is refused as it was.
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message // no methods: decodes with the plain tags
	var aux struct {
		alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*m = Message(aux.alias)
	raw := bytes.TrimSpace(aux.Content)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
		m.Content = ""
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		m.Content = s
	case strings.EqualFold(m.Role, "tool"):
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return err
		}
		m.Content = buf.String()
	default:
		return errors.New("chat: message content must be a string (an object or array is allowed on a tool message only)")
	}
	return nil
}
