package git

import (
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"go.uber.org/zap"
)

// serveInfoRefs is reference discovery, GET <repo>/info/refs?service=…:
// the `# service=` line and a flush, then every ref with the capabilities
// on the first line. HEAD and its symref are advertised only for
// upload-pack and only when the head branch exists (a dangling symref
// breaks clone); an empty repository advertises the zero id with
// `capabilities^{}`, which a client clones as empty.
//
// What is advertised is the contract: upload-pack offers multi_ack_detailed
// (without it a stateless client forgets its haves and every fetch is a
// full pack), ofs-delta and the symref; receive-pack offers report-status,
// delete-refs, ofs-delta and atomic (every push here is all-or-nothing
// anyway, so `git push --atomic` may as well work). Not offered, so never
// requested: side-band, thin-pack (the client sends thin packs regardless;
// the staging resolves them), shallow, filter, push-options, protocol v2.
func (c *Controller) serveInfoRefs(w http.ResponseWriter, r *http.Request, repo Repo) {
	service := r.URL.Query().Get("service")
	if service != uploadPackService && service != receivePackService {
		http.Error(w, "smart HTTP only: this server has no dumb protocol (add ?service=git-upload-pack)", http.StatusForbidden)
		return
	}
	refs, err := repo.Refs(r.Context())
	if err != nil {
		c.logger().Warn("git refs failed", zap.Error(err))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	adv := packp.NewAdvRefs()
	adv.Prefix = [][]byte{[]byte("# service=" + service), pktline.Flush}
	caps := adv.Capabilities
	var head *plumbing.Hash
	for _, ref := range refs {
		adv.References[ref.Name.String()] = ref.Hash
		if ref.Name == repo.Head() {
			h := ref.Hash
			head = &h
		}
	}
	switch service {
	case uploadPackService:
		_ = caps.Set(capability.MultiACKDetailed)
		_ = caps.Set(capability.OFSDelta)
		_ = caps.Set(capability.NoProgress)
		if head != nil {
			adv.Head = head
			_ = caps.Set(capability.SymRef, "HEAD:"+repo.Head().String())
		}
	case receivePackService:
		_ = caps.Set(capability.ReportStatus)
		_ = caps.Set(capability.DeleteRefs)
		_ = caps.Set(capability.OFSDelta)
		_ = caps.Set(capability.Atomic)
	}
	_ = caps.Set(capability.ObjectFormat, "sha1")
	_ = caps.Set(capability.Agent, agentName)

	noCache(w.Header())
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	if err := adv.Encode(w); err != nil {
		c.logger().Debug("git advertisement not written", zap.Error(err))
	}
}
