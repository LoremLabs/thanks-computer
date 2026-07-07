package mail

import (
	"bytes"
	"net/mail"

	"github.com/jhillyerd/enmime/v2"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// composeMIME builds a multipart (text + html) RFC 5322 message and returns
// the encoded bytes plus a generated Message-ID. from/to are parsed
// addresses (display name + bare address). enmime does not auto-add a
// Message-ID, so the one set here is the only one; Postfix stamps Date and
// (if absent) a Received-derived id. msgIDDomain is the From domain.
func composeMIME(from, to mail.Address, cc []mail.Address, replyTo string, extraHeaders map[string]string, subject, htmlBody, textBody, msgIDDomain string) (msg []byte, messageID string, err error) {
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
	for _, a := range cc {
		b = b.CC(a.Name, a.Address) // visible Cc header (Bcc is envelope-only, never a header)
	}
	if replyTo != "" {
		b = b.Header("Reply-To", replyTo)
	}
	for k, v := range extraHeaders { // caller pre-filters protected/structural headers
		b = b.Header(k, v)
	}
	part, berr := b.Build()
	if berr != nil {
		return nil, "", berr
	}
	// Force quoted-printable on the body parts. enmime's default picks 7bit
	// for pure-ASCII content regardless of line length, and drip HTML is one
	// long line — the MTA folds it at ~998 chars, which corrupts any attribute
	// the fold lands in and breaks the DKIM body hash. QP wraps at 76 chars
	// and decodes byte-exact. The root part gets no encoder so top-level
	// headers keep their default encoding.
	enc := enmime.NewEncoder(enmime.ForceQuotedPrintableCte(true))
	for c := part.FirstChild; c != nil; c = c.NextSibling {
		c.WithEncoder(enc)
	}
	if part.FirstChild == nil { // single-part message: the root is the body
		part.WithEncoder(enc)
	}
	var buf bytes.Buffer
	if eerr := part.Encode(&buf); eerr != nil {
		return nil, "", eerr
	}
	return buf.Bytes(), messageID, nil
}
