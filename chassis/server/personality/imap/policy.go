package imap

import (
	"encoding/json"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

// Policy vocabulary (§25.9): per mailbox, seven verbs, each one of four
// modes. The chassis has no opinion about folder names; it has one about
// which client mutations a stack hears of, and when.
//
//	deny     refused at the protocol layer (NO [NOPERM]), no round trip
//	local    protocol state only, no event
//	observe  commit, then tell the `_imap` stack (fire-and-forget)
//	stack    ask the `_imap` stack first; absent/false @imap.res.ok ⇒ NO
//
// Resolution: the mailbox's own policy, then the account's default policy,
// then the chassis default — `flags` is local (Apple Mail sets \Seen on
// every open), everything else is observe.

type verb string

const (
	verbAppend  verb = "append"
	verbMoveIn  verb = "move_in"
	verbMoveOut verb = "move_out"
	verbDelete  verb = "delete"
	verbFlags   verb = "flags"
	verbCreate  verb = "create"
	verbRename  verb = "rename"
)

type mode string

const (
	modeDeny    mode = "deny"
	modeLocal   mode = "local"
	modeObserve mode = "observe"
	modeStack   mode = "stack"
)

func parseMode(s string) (mode, bool) {
	switch mode(s) {
	case modeDeny, modeLocal, modeObserve, modeStack:
		return mode(s), true
	}
	return "", false
}

// policyMode resolves one verb for a mailbox. A nil/absent mailbox (a
// top-level CREATE has no parent) resolves from the account alone.
func policyMode(mb *chimap.Mailbox, acct *chimap.Account, v verb) mode {
	if mb != nil {
		if m, ok := lookupMode(mb.Policy, v); ok {
			return m
		}
	}
	if acct != nil {
		if m, ok := lookupMode(acct.Policy, v); ok {
			return m
		}
	}
	if v == verbFlags {
		return modeLocal
	}
	return modeObserve
}

func lookupMode(raw json.RawMessage, v verb) (mode, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	if s, ok := m[string(v)]; ok {
		return parseMode(s)
	}
	return "", false
}

// strictest picks the governing mode when a mutation touches two
// mailboxes (MOVE: move_out on the source, move_in on the destination):
// deny > stack > observe > local.
func strictest(a, b mode) mode {
	rank := map[mode]int{modeLocal: 0, modeObserve: 1, modeStack: 2, modeDeny: 3}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}
