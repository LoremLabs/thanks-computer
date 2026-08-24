package cli

// The fast-forward guard for the code deploy path — git's "! [rejected]
// (non-fast-forward)" for stacks. A workspace records which version it last
// synced (`.txco/<stack>.state.json`, written by pull/apply/push/dev and
// kept current by data apply/activate). If the chassis's active version is
// no longer that one — someone deployed, rolled back, or edited in the admin
// UI — pushing the local tree would silently supersede their change, so
// `apply`/`push`/`data apply` refuse unless --force. The chassis enforces the
// same rule under its row lock via ActivateOpts.ExpectedActive (409
// stack_moved), which closes the race after the pre-check. A workspace with
// no baseline (first apply from a fresh checkout, CI) cannot detect a move
// and proceeds — like a clone that never fetched.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

// remoteMove describes a chassis whose active version is not the one this
// workspace last synced.
type remoteMove struct {
	Synced   int64 // the version this workspace last synced
	Active   int64 // the chassis's active version now (0 = none)
	Rollback bool  // active < synced
}

// remoteMoved reports how the chassis moved relative to the workspace's
// baseline, or nil when they agree or no baseline exists.
func remoteMoved(saved *state.State, rec *client.StackRecord) *remoteMove {
	if saved == nil || rec == nil {
		return nil
	}
	var active int64
	if rec.ActiveVersion != nil {
		active = *rec.ActiveVersion
	}
	if active == saved.VersionNumber {
		return nil
	}
	return &remoteMove{Synced: saved.VersionNumber, Active: active, Rollback: active < saved.VersionNumber}
}

// refusedMovedMessage is the operator-facing refusal: what moved, and the
// three ways out (look, take theirs, overwrite).
func refusedMovedMessage(cmd, stack string, m *remoteMove) string {
	what := fmt.Sprintf("chassis active is v%d but this workspace last synced v%d — someone deployed (or edited in the admin UI) since", m.Active, m.Synced)
	if m.Rollback {
		what = fmt.Sprintf("chassis active is v%d, rolled back from the v%d this workspace last synced", m.Active, m.Synced)
	}
	return fmt.Sprintf("%s: %s: refused — %s.\n  `txco diff %s` shows what changed; `txco pull %s --force` takes the chassis version (discards local edits); `%s --force` overwrites the chassis.\n",
		cmd, stack, what, stack, stack, cmd)
}

// isStackMovedErr recognises the chassis-side refusal (the race the
// pre-check can't see).
func isStackMovedErr(err error) bool {
	var he *client.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusConflict && he.Code == "stack_moved"
	}
	return false
}

// expectedActiveFor is the ExpectedActive a deploy should send after its
// pre-check passed: the version it just observed (0 when none), so the
// chassis refuses if the ref moves in the window before the activate.
func expectedActiveFor(rec *client.StackRecord) *int64 {
	var n int64
	if rec != nil && rec.ActiveVersion != nil {
		n = *rec.ActiveVersion
	}
	return &n
}

// recordSyncedVersion bumps the workspace's synced version after a deploy
// that did not change the code tree (data apply, activate): the workspace
// now "knows" v<n>, so its own action never trips the fast-forward guard.
// The code manifest is left as it was (pull's dirty check keys on it).
// Best-effort: a state-write failure never fails the deploy.
func recordSyncedVersion(dir, stack string, n int64) {
	saved, _ := state.Load(dir, stack)
	if saved == nil {
		saved = &state.State{}
	}
	saved.VersionNumber, saved.ParentVersionNumber = n, n
	_ = state.Save(dir, stack, *saved)
}
