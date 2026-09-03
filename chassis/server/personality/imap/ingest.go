package imap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// excerptBytes bounds the text_excerpt column column-backed SEARCH
// BODY/TEXT reads.
const excerptBytes = 4096

// Ingested is a record turned into everything the store row and the CAS
// need: the canonical bytes (the CAS object, keyed by SHA256), the rendered
// RFC 5322 bytes (for the size + the cached structures — discarded after),
// and the row with ENVELOPE/BODYSTRUCTURE already cached.
type Ingested struct {
	Canonical []byte
	SHA256    string
	Rendered  []byte
	Message   chimap.Message
}

// BuildRecordMessage runs the formatter once over a record and caches what
// FETCH needs. objectKey/domain/internal are the RenderOptions and are
// stored on the row so the render is reproducible on every BODY[] fetch.
func BuildRecordMessage(rec *chimap.Record, objectKey, domain string, internal time.Time, flags []string) (Ingested, error) {
	if internal.IsZero() {
		return Ingested{}, errors.New("imap: internal date required")
	}
	internal = internal.UTC().Truncate(time.Second)
	canonical, sha, err := rec.Canonical()
	if err != nil {
		return Ingested{}, err
	}
	rendered, err := chimap.Render(rec, sha, chimap.RenderOptions{ObjectKey: objectKey, Domain: domain, InternalDate: internal})
	if err != nil {
		return Ingested{}, err
	}
	env, bs, err := extract(rendered)
	if err != nil {
		return Ingested{}, err
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return Ingested{}, err
	}
	bsJSON, err := EncodeBodyStructure(bs)
	if err != nil {
		return Ingested{}, err
	}
	partsJSON, _ := json.Marshal(rec.Parts)
	if len(rec.Parts) == 0 {
		partsJSON = []byte("[]")
	}
	from := ""
	if len(env.From) > 0 {
		from = env.From[0].Addr()
	}
	return Ingested{
		Canonical: canonical,
		SHA256:    sha,
		Rendered:  rendered,
		Message: chimap.Message{
			ObjectKey:     objectKey,
			Kind:          chimap.KindRecord,
			SHA256:        sha,
			FormatVersion: chimap.FormatVersion,
			Size:          int64(len(rendered)),
			InternalDate:  internal,
			Flags:         chimap.NormalizeFlags(flags),
			Envelope:      envJSON,
			BodyStructure: bsJSON,
			Subject:       env.Subject,
			FromAddr:      from,
			TextExcerpt:   rec.Excerpt(excerptBytes),
			Parts:         partsJSON,
		},
	}, nil
}

// extract computes ENVELOPE + BODYSTRUCTURE from rendered bytes with the
// library's own helpers, so what we cache is exactly what a parsing
// backend would have produced.
func extract(raw []byte) (*imap.Envelope, imap.BodyStructure, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	hdr, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, nil, fmt.Errorf("imap: rendered header: %w", err)
	}
	env := imapserver.ExtractEnvelope(hdr)
	bs := imapserver.ExtractBodyStructure(bytes.NewReader(raw))
	return env, bs, nil
}

// renderRow re-derives a record row's RFC 5322 bytes from the CAS object.
// Deterministic per format_version, so the bytes equal what was measured at
// append. domain is the account's (Message-ID fallback).
func renderRow(ctx context.Context, fcas filecas.Store, m chimap.Message, domain string) ([]byte, error) {
	if fcas == nil {
		return nil, errors.New("imap: no content store on this node")
	}
	obj, err := fcas.Get(ctx, m.SHA256)
	if err != nil {
		return nil, fmt.Errorf("imap: object %s: %w", m.SHA256, err)
	}
	switch m.Kind {
	case chimap.KindVerbatim:
		return obj, nil
	case chimap.KindRecord:
		rec, err := chimap.ParseRecord(obj)
		if err != nil {
			return nil, err
		}
		if m.FormatVersion != chimap.FormatVersion {
			return nil, fmt.Errorf("imap: record format v%d not renderable by v%d", m.FormatVersion, chimap.FormatVersion)
		}
		return chimap.Render(rec, m.SHA256, chimap.RenderOptions{ObjectKey: m.ObjectKey, Domain: domain, InternalDate: m.InternalDate})
	}
	return nil, fmt.Errorf("imap: unknown kind %q", m.Kind)
}

// domainOf returns the domain half of a username ("" when malformed).
func domainOf(username string) string {
	at := strings.LastIndex(username, "@")
	if at < 0 || at == len(username)-1 {
		return ""
	}
	return username[at+1:]
}
