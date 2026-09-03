package imap

import (
	"encoding/json"
	"errors"

	"github.com/emersion/go-imap/v2"
)

// bsJSON is the on-row form of an imap.BodyStructure. The library type is
// an interface over two structs, so it needs a codec; cached at append,
// decoded on FETCH — FETCH never parses MIME.
type bsJSON struct {
	Multi       bool              `json:"multi,omitempty"`
	Type        string            `json:"type,omitempty"`
	Subtype     string            `json:"subtype,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	ID          string            `json:"id,omitempty"`
	Description string            `json:"description,omitempty"`
	Encoding    string            `json:"encoding,omitempty"`
	Size        uint32            `json:"size,omitempty"`
	Lines       *int64            `json:"lines,omitempty"`
	Children    []bsJSON          `json:"children,omitempty"`
	Disposition *dispJSON         `json:"disposition,omitempty"`
	Language    []string          `json:"language,omitempty"`
	Location    string            `json:"location,omitempty"`
	Message     *msgJSON          `json:"message,omitempty"`
}

type dispJSON struct {
	Value  string            `json:"value"`
	Params map[string]string `json:"params,omitempty"`
}

type msgJSON struct {
	Envelope *imap.Envelope `json:"envelope,omitempty"`
	Body     *bsJSON        `json:"body,omitempty"`
	Lines    int64          `json:"lines"`
}

// EncodeBodyStructure serialises a body structure for the row cache.
func EncodeBodyStructure(bs imap.BodyStructure) ([]byte, error) {
	if bs == nil {
		return nil, errors.New("imap: nil body structure")
	}
	return json.Marshal(toJSON(bs))
}

// DecodeBodyStructure restores a cached body structure.
func DecodeBodyStructure(b []byte) (imap.BodyStructure, error) {
	if len(b) == 0 || string(b) == "{}" {
		return nil, errors.New("imap: no cached body structure")
	}
	var j bsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	return fromJSON(&j), nil
}

func toJSON(bs imap.BodyStructure) bsJSON {
	switch v := bs.(type) {
	case *imap.BodyStructureMultiPart:
		j := bsJSON{Multi: true, Subtype: v.Subtype}
		for _, c := range v.Children {
			j.Children = append(j.Children, toJSON(c))
		}
		if v.Extended != nil {
			j.Params = v.Extended.Params
			j.Disposition = dispToJSON(v.Extended.Disposition)
			j.Language = v.Extended.Language
			j.Location = v.Extended.Location
		}
		return j
	case *imap.BodyStructureSinglePart:
		j := bsJSON{Type: v.Type, Subtype: v.Subtype, Params: v.Params, ID: v.ID,
			Description: v.Description, Encoding: v.Encoding, Size: v.Size}
		if v.Text != nil {
			n := v.Text.NumLines
			j.Lines = &n
		}
		if v.MessageRFC822 != nil {
			m := &msgJSON{Envelope: v.MessageRFC822.Envelope, Lines: v.MessageRFC822.NumLines}
			if v.MessageRFC822.BodyStructure != nil {
				inner := toJSON(v.MessageRFC822.BodyStructure)
				m.Body = &inner
			}
			j.Message = m
		}
		if v.Extended != nil {
			j.Disposition = dispToJSON(v.Extended.Disposition)
			j.Language = v.Extended.Language
			j.Location = v.Extended.Location
		}
		return j
	}
	return bsJSON{}
}

func fromJSON(j *bsJSON) imap.BodyStructure {
	if j.Multi {
		bs := &imap.BodyStructureMultiPart{Subtype: j.Subtype}
		for i := range j.Children {
			bs.Children = append(bs.Children, fromJSON(&j.Children[i]))
		}
		bs.Extended = &imap.BodyStructureMultiPartExt{
			Params: j.Params, Disposition: dispFromJSON(j.Disposition), Language: j.Language, Location: j.Location,
		}
		return bs
	}
	bs := &imap.BodyStructureSinglePart{Type: j.Type, Subtype: j.Subtype, Params: j.Params, ID: j.ID,
		Description: j.Description, Encoding: j.Encoding, Size: j.Size}
	if j.Lines != nil {
		bs.Text = &imap.BodyStructureText{NumLines: *j.Lines}
	}
	if j.Message != nil {
		m := &imap.BodyStructureMessageRFC822{Envelope: j.Message.Envelope, NumLines: j.Message.Lines}
		if j.Message.Body != nil {
			m.BodyStructure = fromJSON(j.Message.Body)
		}
		bs.MessageRFC822 = m
	}
	bs.Extended = &imap.BodyStructureSinglePartExt{
		Disposition: dispFromJSON(j.Disposition), Language: j.Language, Location: j.Location,
	}
	return bs
}

func dispToJSON(d *imap.BodyStructureDisposition) *dispJSON {
	if d == nil {
		return nil
	}
	return &dispJSON{Value: d.Value, Params: d.Params}
}

func dispFromJSON(d *dispJSON) *imap.BodyStructureDisposition {
	if d == nil {
		return nil
	}
	return &imap.BodyStructureDisposition{Value: d.Value, Params: d.Params}
}
