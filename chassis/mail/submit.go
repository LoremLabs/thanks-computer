package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	smtp "github.com/emersion/go-smtp"
)

// ErrNoRelay is returned by the op when no relay is configured.
var ErrNoRelay = errors.New("sendmail: no mail relay configured (set --mail-relay-addr / TXCO_MAIL_RELAY_ADDR)")

// SubmitFunc hands one rendered RFC 5322 message to the relay for `from` →
// `rcpts` (the envelope recipients: the To recipient plus any Cc/Bcc).
// Injectable so tests can substitute a fake.
type SubmitFunc func(ctx context.Context, from string, rcpts []string, msg []byte) error

// makeSMTPSubmit builds the production submit. The relay is trusted infra on
// the private net (no auth — restricted by source network, same posture as
// the LMTP inlet). The default ("none") path dials with a tight timeout and
// bounds the whole conversation with a deadline. STARTTLS uses dial-time TLS
// (this go-smtp Client has no post-connect StartTLS); InsecureSkipVerify
// because the private relay's cert is self-signed and the point is just
// encryption, not auth.
func makeSMTPSubmit(cfg Config) SubmitFunc {
	if cfg.RelayAddr == "" {
		return func(context.Context, string, []string, []byte) error { return ErrNoRelay }
	}
	return func(ctx context.Context, from string, rcpts []string, msg []byte) error {
		host, _, _ := net.SplitHostPort(cfg.RelayAddr)
		var c *smtp.Client
		if cfg.RelayTLS == "starttls" {
			cl, err := smtp.DialStartTLS(cfg.RelayAddr, &tls.Config{ServerName: host, InsecureSkipVerify: true})
			if err != nil {
				return fmt.Errorf("dial+starttls relay %s: %w", cfg.RelayAddr, err)
			}
			c = cl
		} else {
			d := net.Dialer{Timeout: cfg.DialTimeout}
			conn, err := d.DialContext(ctx, "tcp", cfg.RelayAddr)
			if err != nil {
				return fmt.Errorf("dial relay %s: %w", cfg.RelayAddr, err)
			}
			if cfg.DialTimeout > 0 {
				_ = conn.SetDeadline(time.Now().Add(cfg.DialTimeout))
			}
			c = smtp.NewClient(conn)
		}
		defer c.Close()
		// SendMail auto-greets (Mail → hello), so no explicit EHLO needed.
		if err := c.SendMail(from, rcpts, bytes.NewReader(msg)); err != nil {
			return fmt.Errorf("submit to %s: %w", cfg.RelayAddr, err)
		}
		return c.Quit()
	}
}
