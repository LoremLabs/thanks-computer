package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"go.uber.org/zap"
)

// uploadRequest is one git-upload-pack POST, read from pkt-lines: the
// client's wants (with its capabilities on the first), its haves, and
// whether it is done negotiating. go-git's packp has no decoder for the
// haves half of a v0 request (UploadHaves only encodes), so the reader is
// here; it is a page.
type uploadRequest struct {
	wants []plumbing.Hash
	haves []plumbing.Hash
	done  bool
	caps  *capability.List
}

// maxHaves bounds a negotiation: git sends haves in growing batches and
// gives up after 256 in vain, so a legitimate request never nears this.
const maxHaves = 4096

func readUploadRequest(r io.Reader) (*uploadRequest, error) {
	req := &uploadRequest{caps: capability.NewList()}
	s := pktline.NewScanner(r)
	first := true
	for s.Scan() {
		line := bytes.TrimSuffix(s.Bytes(), []byte{'\n'})
		if len(line) == 0 {
			continue // a flush separates wants from haves, and batches
		}
		switch {
		case bytes.HasPrefix(line, []byte("want ")):
			rest := line[len("want "):]
			if len(rest) < 40 || !plumbing.IsHash(string(rest[:40])) {
				return nil, fmt.Errorf("malformed want line")
			}
			req.wants = append(req.wants, plumbing.NewHash(string(rest[:40])))
			if first {
				if i := bytes.IndexByte(rest, ' '); i >= 0 {
					if err := req.caps.Decode(rest[i+1:]); err != nil {
						return nil, fmt.Errorf("malformed capabilities: %w", err)
					}
				}
				first = false
			}
		case bytes.HasPrefix(line, []byte("have ")):
			rest := line[len("have "):]
			if len(rest) < 40 || !plumbing.IsHash(string(rest[:40])) {
				return nil, fmt.Errorf("malformed have line")
			}
			if len(req.haves) >= maxHaves {
				return nil, fmt.Errorf("too many haves")
			}
			req.haves = append(req.haves, plumbing.NewHash(string(rest[:40])))
		case bytes.Equal(line, []byte("done")):
			req.done = true
		case bytes.HasPrefix(line, []byte("deepen")), bytes.HasPrefix(line, []byte("shallow ")),
			bytes.HasPrefix(line, []byte("filter ")):
			return nil, fmt.Errorf("%s is not supported by this server", bytes.Fields(line)[0])
		default:
			return nil, fmt.Errorf("unexpected line in upload request")
		}
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return req, nil
}

// serveUploadPack is a fetch or clone: negotiate what the client lacks,
// then stream one pack of exactly those objects.
//
// Negotiation is the v0 protocol's, stateless (one POST per batch of
// haves, nothing kept between them). With multi_ack_detailed, which the
// head advertises and every modern client requests: every have the
// repository knows is acknowledged `ACK <id> common`; a batch without
// `done` ends with `NAK` and the client sends the next batch, carrying its
// commons forward; the `done` batch ends with `ACK <last common>` (or
// `NAK` when nothing is shared) and the pack follows, built from the wants
// minus everything reachable from the commons. Without multi_ack (a client
// that did not request it) the exchange is plain ACK/NAK and the pack is
// whatever the wants reach minus the haves it still names.
func (c *Controller) serveUploadPack(w http.ResponseWriter, r *http.Request, repo Repo) {
	body, err := c.requestBody(w, r, c.limits.MaxPackBytes)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	req, err := readUploadRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.wants) == 0 {
		http.Error(w, "no wants", http.StatusBadRequest)
		return
	}
	refs, err := repo.Refs(r.Context())
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	// A want must be a ref's tip: the server does not hand out arbitrary
	// objects by id (no allow-*-in-want).
	tips := map[plumbing.Hash]bool{}
	for _, ref := range refs {
		tips[ref.Hash] = true
	}
	for _, h := range req.wants {
		if !tips[h] {
			http.Error(w, "not our ref: "+h.String(), http.StatusForbidden)
			return
		}
	}
	var commons []plumbing.Hash
	for _, h := range req.haves {
		if repo.HasEncodedObject(h) == nil {
			commons = append(commons, h)
		}
	}
	detailed := req.caps.Supports(capability.MultiACKDetailed)

	noCache(w.Header())
	w.Header().Set("Content-Type", "application/x-"+uploadPackService+"-result")
	w.WriteHeader(http.StatusOK)
	enc := pktline.NewEncoder(w)
	if detailed {
		for _, h := range commons {
			if err := enc.Encodef("ACK %s common\n", h); err != nil {
				return
			}
		}
	}
	if !req.done {
		_ = enc.EncodeString("NAK\n")
		return
	}
	if len(commons) > 0 {
		if err := enc.Encodef("ACK %s\n", commons[len(commons)-1]); err != nil {
			return
		}
	} else if err := enc.EncodeString("NAK\n"); err != nil {
		return
	}
	objs, err := revlist.Objects(repo, req.wants, commons)
	if err != nil {
		c.logger().Warn("git upload-pack walk failed", zap.Error(err))
		_ = enc.Encodef("ERR %s\n", "could not walk the objects")
		return
	}
	if _, err := packfile.NewEncoder(w, repo, false).Encode(objs, 10); err != nil {
		c.logger().Debug("git upload-pack not streamed", zap.Error(err))
	}
}
