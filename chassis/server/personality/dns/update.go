package dns

import (
	"strings"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// RFC2136 dynamic-update receiver, scoped HARD to ACME DNS-01.
//
// This lets an external ACME client (e.g. Caddy's caddy-dns/rfc2136
// provider) inject `_acme-challenge.<delegated-zone>` TXT records into
// this authoritative server so it can solve DNS-01 for a wildcard cert.
// It is NOT a general dynamic-DNS server: the ONLY thing it will write is
// an `_acme-challenge.*` TXT under a zone we already serve, and only over
// a TSIG-authenticated message. Everything else — unsigned, wrong key,
// other names, other types, other zones — is refused. Writes land in the
// transient ChallengeStore (challenge.go), never in dns_zones/dns_records
// and never through the dbcache reload cycle.
//
// The receiver is OFF unless both --dns-update-tsig-key-name and
// --dns-update-tsig-secret are set; with no key, every UPDATE is refused.

// updatesEnabled reports whether the TSIG-gated UPDATE path is configured.
func (c *DNSController) updatesEnabled() bool {
	return c.tsigKeyName != "" && c.tsigSecret != ""
}

// acceptDynamicUpdate is the server MsgAcceptFunc installed ONLY when the
// RFC2136 receiver is enabled. miekg's default rejects OpcodeUpdate with
// NOTIMP before the handler runs (and its query-oriented section-count
// checks misread an UPDATE's sections), so we replicate the Query/Notify
// rules and additionally accept Update so it reaches handleUpdate. With the
// receiver disabled this func isn't installed, so updates stay NOTIMP.
func acceptDynamicUpdate(dh dns.Header) dns.MsgAcceptAction {
	const qrBit = 1 << 15 // QR is the top bit of the flags word
	if dh.Bits&qrBit != 0 {
		return dns.MsgIgnore // a response, not a request
	}
	opcode := int(dh.Bits>>11) & 0xF
	switch opcode {
	case dns.OpcodeUpdate:
		// Exactly one zone in the question section; the update RRs live in
		// the Ns (update) section and are validated by handleUpdate.
		if dh.Qdcount != 1 {
			return dns.MsgReject
		}
		return dns.MsgAccept
	case dns.OpcodeQuery, dns.OpcodeNotify:
		if dh.Qdcount != 1 {
			return dns.MsgReject
		}
		if dh.Ancount > 1 || dh.Nscount > 1 || dh.Arcount > 2 {
			return dns.MsgReject
		}
		return dns.MsgAccept
	default:
		return dns.MsgRejectNotImplemented
	}
}

// handleUpdate processes an RFC2136 UPDATE. It validates TSIG + scope in a
// first pass (so a single bad RR rejects the whole message with no partial
// writes), then applies. The reply is TSIG-signed with the request's key.
func (c *DNSController) handleUpdate(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)

	// Auth gate. Disabled receiver, an unsigned message, an unknown key,
	// or a failed MAC all collapse to NOTAUTH — we never reveal which.
	if !c.updatesEnabled() {
		c.replyUpdate(w, req, m, dns.RcodeNotAuth)
		return
	}
	tsig := req.IsTsig()
	if tsig == nil || w.TsigStatus() != nil {
		c.replyUpdate(w, req, m, dns.RcodeNotAuth)
		return
	}

	// RFC2136 §2.3: the single Question is the zone (class IN, type SOA).
	snap := c.snap.Load()
	if snap == nil || len(req.Question) != 1 {
		c.replyUpdate(w, req, m, dns.RcodeFormatError)
		return
	}
	zone := strings.ToLower(dns.Fqdn(req.Question[0].Name))
	if snap.zoneFor(zone) == nil {
		// We are not authoritative for this zone.
		c.replyUpdate(w, req, m, dns.RcodeNotZone)
		return
	}

	// Pass 1: validate every update RR is an `_acme-challenge.<served-zone>`
	// TXT op of an accepted class. Reject the whole message otherwise.
	type apply struct {
		owner  string
		delete bool   // true → remove, false → insert
		value  string // "" with delete → drop all values at owner
	}
	ops := make([]apply, 0, len(req.Ns))
	for _, rr := range req.Ns {
		h := rr.Header()
		owner := strings.ToLower(h.Name)
		if !isACMEChallengeName(owner) || snap.zoneFor(owner) == nil {
			c.replyUpdate(w, req, m, dns.RcodeRefused)
			return
		}
		switch h.Class {
		case dns.ClassINET: // §2.5.1 add to RRset
			txt, ok := rr.(*dns.TXT)
			if !ok || h.Rrtype != dns.TypeTXT {
				c.replyUpdate(w, req, m, dns.RcodeRefused)
				return
			}
			ops = append(ops, apply{owner: owner, value: strings.Join(txt.Txt, "")})
		case dns.ClassANY: // §2.5.2 delete RRset / §2.5.3 delete name
			if h.Rrtype != dns.TypeTXT && h.Rrtype != dns.TypeANY {
				c.replyUpdate(w, req, m, dns.RcodeRefused)
				return
			}
			ops = append(ops, apply{owner: owner, delete: true})
		case dns.ClassNONE: // §2.5.4 delete an individual RR
			txt, ok := rr.(*dns.TXT)
			if !ok || h.Rrtype != dns.TypeTXT {
				c.replyUpdate(w, req, m, dns.RcodeRefused)
				return
			}
			ops = append(ops, apply{owner: owner, delete: true, value: strings.Join(txt.Txt, "")})
		default:
			c.replyUpdate(w, req, m, dns.RcodeRefused)
			return
		}
	}

	// Pass 2: apply (validated, so this can't partially fail on policy).
	for _, op := range ops {
		switch {
		case !op.delete:
			c.challenges.Present(op.owner, op.value)
		case op.value != "":
			c.challenges.CleanUp(op.owner, op.value)
		default: // delete-all at owner
			for _, v := range c.challenges.ActiveTXT(op.owner) {
				c.challenges.CleanUp(op.owner, v)
			}
		}
	}
	c.replyUpdate(w, req, m, dns.RcodeSuccess)
}

// replyUpdate sets the rcode, re-signs with the request's TSIG key when the
// request carried a verified one, and writes. Signing only happens when
// TsigStatus passed, so a NOTAUTH reply to an unsigned/bad request goes out
// unsigned (the client can't verify it anyway).
func (c *DNSController) replyUpdate(w dns.ResponseWriter, req, m *dns.Msg, rcode int) {
	m.Rcode = rcode
	if t := req.IsTsig(); t != nil && w.TsigStatus() == nil {
		m.SetTsig(t.Hdr.Name, t.Algorithm, tsigFudgeSeconds, time.Now().Unix())
	}
	if err := w.WriteMsg(m); err != nil {
		c.pu.Logger.Debug("dns update reply write failed", zap.String("err", err.Error()))
	}
}

// tsigFudgeSeconds is the permitted clock skew on TSIG timestamps.
const tsigFudgeSeconds = 300
