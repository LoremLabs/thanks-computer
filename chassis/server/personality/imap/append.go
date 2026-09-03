package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jhillyerd/enmime/v2"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// Verbatim ingest (client APPEND, or a stack's `from` / `from_sha`): the
// exact RFC 5322 bytes are the retained object. The head stores the
// message and each decoded attachment part in the CAS (each with the
// tenant's sha row), caches ENVELOPE/BODYSTRUCTURE, and builds the
// `_txc.imap.msg` facts the lanes carry — references, never bytes.

// recordContentType is the sha row's content type for a stored record.
const recordContentType = "application/vnd.txco.imap-record+json"

// Verbatim is the result of parsing + measuring raw RFC 5322 bytes.
type Verbatim struct {
	SHA256  string
	Message chimap.Message
	Facts   msgFacts
	parts   [][]byte // decoded attachment bytes, index-aligned with Facts.Parts
}

// ParseVerbatim parses raw bytes into a row + facts. objectKey may be ""
// (a client APPEND). Nothing is stored here; see StoreVerbatim.
func ParseVerbatim(raw []byte, objectKey string, internal time.Time, flags []string) (*Verbatim, error) {
	if len(raw) == 0 {
		return nil, errors.New("imap: empty message")
	}
	if internal.IsZero() {
		internal = time.Now()
	}
	internal = internal.UTC().Truncate(time.Second)
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	env, bs, err := extract(raw)
	if err != nil {
		return nil, err
	}
	envJSON, _ := json.Marshal(env)
	bsJSON, err := EncodeBodyStructure(bs)
	if err != nil {
		return nil, err
	}
	facts := msgFacts{SHA256: sha, Size: int64(len(raw)), Parts: []partFacts{}}
	var partBytes [][]byte
	excerpt := ""
	// Best-effort MIME parse (enmime is forgiving): bodies, headers,
	// attachments. A parse failure still stores the message — the cached
	// ENVELOPE/BODYSTRUCTURE came from go-message above.
	if me, perr := enmime.ReadEnvelope(bytes.NewReader(raw)); perr == nil {
		facts.ID = me.GetHeader("Message-ID")
		if d, derr := me.Date(); derr == nil && !d.IsZero() {
			facts.Date = d.UTC().Format(time.RFC3339)
		}
		facts.Subject = me.GetHeader("Subject")
		facts.From = addrsOf(me, "From")
		facts.To = addrsOf(me, "To")
		facts.Cc = addrsOf(me, "Cc")
		facts.Text = me.Text
		facts.HTML = me.HTML
		facts.Headers = map[string]string{}
		for _, k := range me.GetHeaderKeys() {
			if v := me.GetHeader(k); v != "" {
				facts.Headers[strings.ToLower(k)] = v
			}
		}
		all := append(append([]*enmime.Part{}, me.Attachments...), me.Inlines...)
		for i, p := range all {
			psum := sha256.Sum256(p.Content)
			facts.Parts = append(facts.Parts, partFacts{
				N: i + 1, Name: p.FileName, Type: p.ContentType, Size: int64(len(p.Content)),
				SHA256: hex.EncodeToString(psum[:]),
			})
			partBytes = append(partBytes, p.Content)
		}
		rec := &chimap.Record{Text: me.Text, HTML: me.HTML}
		excerpt = rec.Excerpt(excerptBytes)
	}
	from := ""
	if len(env.From) > 0 {
		from = env.From[0].Addr()
	}
	partsJSON, _ := json.Marshal(facts.Parts)
	return &Verbatim{
		SHA256: sha,
		Facts:  facts,
		parts:  partBytes,
		Message: chimap.Message{
			ObjectKey:     objectKey,
			Kind:          chimap.KindVerbatim,
			SHA256:        sha,
			FormatVersion: 0,
			Size:          int64(len(raw)),
			InternalDate:  internal,
			Flags:         chimap.NormalizeFlags(flags),
			Envelope:      envJSON,
			BodyStructure: bsJSON,
			Subject:       env.Subject,
			FromAddr:      from,
			TextExcerpt:   excerpt,
			Parts:         partsJSON,
		},
	}, nil
}

func addrsOf(me *enmime.Envelope, key string) []addr {
	list, err := me.AddressList(key)
	if err != nil {
		return nil
	}
	out := make([]addr, 0, len(list))
	for _, a := range list {
		out = append(out, addr{Name: a.Name, Addr: a.Address})
	}
	return out
}

// StoreVerbatim writes the message bytes and every part into the CAS,
// recording the tenant's ownership rows (ix may be nil: bytes only). Write
// order CAS → sha rows; the index row is the caller's AppendMessage.
func StoreVerbatim(ctx context.Context, fcas filecas.Store, ix blob.Index, tenant string, raw []byte, v *Verbatim, now time.Time) error {
	if fcas == nil {
		return errors.New("imap: no content store on this node")
	}
	put := func(sha string, data []byte, ctype string) error {
		present, err := fcas.Exists(ctx, sha)
		if err != nil {
			return err
		}
		if !present {
			if err := filecas.PutReader(ctx, fcas, sha, bytes.NewReader(data), int64(len(data))); err != nil {
				return err
			}
		}
		if ix != nil {
			if _, err := ix.PutShaIfAbsent(ctx, tenant, blob.ShaRow{SHA256: sha, Size: int64(len(data)), ContentType: ctype, FirstSeen: now}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := put(v.SHA256, raw, "message/rfc822"); err != nil {
		return fmt.Errorf("imap: store message: %w", err)
	}
	for i, p := range v.Facts.Parts {
		if i >= len(v.parts) || p.SHA256 == "" {
			continue
		}
		ct := p.Type
		if ct == "" {
			ct = "application/octet-stream"
		}
		if err := put(p.SHA256, v.parts[i], ct); err != nil {
			return fmt.Errorf("imap: store part %d: %w", p.N, err)
		}
	}
	return nil
}

// factsOfRecord builds the lane facts for a record row from its record
// (the stack's append never crosses the head, but an observe of a COPY /
// MOVE of a record row wants the same shape).
func factsOfRecordRow(m chimap.Message) msgFacts {
	var parts []partFacts
	_ = json.Unmarshal(m.Parts, &parts)
	if parts == nil {
		parts = []partFacts{}
	}
	var env imap.Envelope
	_ = json.Unmarshal(m.Envelope, &env)
	f := msgFacts{ID: env.MessageID, Subject: m.Subject, SHA256: m.SHA256, Size: m.Size, Parts: parts, Text: m.TextExcerpt}
	if !env.Date.IsZero() {
		f.Date = env.Date.UTC().Format(time.RFC3339)
	}
	for _, a := range env.From {
		f.From = append(f.From, addr{Name: a.Name, Addr: a.Addr()})
	}
	for _, a := range env.To {
		f.To = append(f.To, addr{Name: a.Name, Addr: a.Addr()})
	}
	for _, a := range env.Cc {
		f.Cc = append(f.Cc, addr{Name: a.Name, Addr: a.Addr()})
	}
	return f
}
