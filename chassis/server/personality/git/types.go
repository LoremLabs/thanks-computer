package git

import gitcore "github.com/loremlabs/thanks-computer/chassis/git"

// The storage seam is the protocol core's (chassis/git); the head uses it
// under its own names.
type (
	Limits      = gitcore.Limits
	Repo        = gitcore.Repo
	Staging     = gitcore.Staging
	RepoLoader  = gitcore.RepoLoader
	RefUpdate   = gitcore.RefUpdate
	Actor       = gitcore.Actor
	ErrRefMoved = gitcore.ErrRefMoved
)

// NewMemRepos is the in-memory loader (tests; the prototype).
var NewMemRepos = gitcore.NewMemRepos
