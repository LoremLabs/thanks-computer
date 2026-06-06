package mail

import (
	"bytes"
	"net/mail"

	"github.com/jhillyerd/enmime"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// composeMIME builds a multipart (text + html) RFC 5322 message and returns
// the encoded bytes plus a generated Message-ID. from/to are parsed
// addresses (display name + bare address). enmime does not auto-add a
// Message-ID, so the one set here is the only one; Postfix stamps Date and
// (if absent) a Received-derived id. msgIDDomain is the From domain.
func composeMIME(from, to mail.Address, subject, htmlBody, textBody, msgIDDomain string) (msg []byte, messageID string, err error) {
	messageID = "<" + hxid.New().String() + "@" + msgIDDomain + ">"
	b := enmime.Builder().
		From(from.Name, from.Address).
		To(to.Name, to.Address).
		Subject(subject).
		Text([]byte(textBody)).
		HTML([]byte(htmlBody)).
		Header("Message-ID", messageID).
		Header("Auto-Submitted", "auto-generated").
		Header("X-Auto-Response-Suppress", "OOF, AutoReply")
	part, berr := b.Build()
	if berr != nil {
		return nil, "", berr
	}
	var buf bytes.Buffer
	if eerr := part.Encode(&buf); eerr != nil {
		return nil, "", eerr
	}
	return buf.Bytes(), messageID, nil
}
