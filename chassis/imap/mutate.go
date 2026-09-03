package imap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// mutate.go holds the mailbox-tree and message mutations the full verb
// set needs (Phase 0b): CREATE / DELETE / RENAME on the tree, COPY / MOVE /
// EXPUNGE on rows, plus the op-side reset and windowed listing. Every
// write commits first and emits Change events after, in the order a
// client must see them (expunges in descending sequence, then appends).

var (
	// ErrMailboxExists is returned by CreateMailbox / RenameMailbox when
	// the target name is already a live mailbox.
	ErrMailboxExists = errors.New("imap: mailbox already exists")
	// ErrMailboxNotFound is returned when the named mailbox is absent.
	ErrMailboxNotFound = errors.New("imap: no such mailbox")
	// ErrINBOX is returned for DELETE / RENAME of INBOX (RFC 3501 §5.1:
	// INBOX always exists).
	ErrINBOX = errors.New("imap: INBOX cannot be deleted or renamed")
)

// Expunged is one row removed by Expunge / MoveMessages: its UID and its
// sequence number at the moment of removal, reported in descending
// sequence order so a client applying them in order stays consistent.
type Expunged struct {
	UID uint32
	Seq uint32
}

// CreateMailbox creates a live mailbox at name with the given role,
// special-use attrs and policy. Parents need not exist (they LIST as
// \Noselect until created). ErrMailboxExists when the name is taken.
func (s *Store) CreateMailbox(ctx context.Context, tenant, username, name, role string, attrs []string, policy json.RawMessage) (Mailbox, error) {
	username = NormalizeUsername(username)
	name = NormalizeMailboxName(name)
	if name == "" {
		return Mailbox{}, errors.New("imap: empty mailbox name")
	}
	if len(policy) > 0 && !json.Valid(policy) {
		return Mailbox{}, errors.New("imap: policy is not valid JSON")
	}
	if _, ok, err := s.GetMailbox(ctx, tenant, username, name); err != nil {
		return Mailbox{}, err
	} else if ok {
		return Mailbox{}, ErrMailboxExists
	}
	now := s.now()
	uidv := uint32(now.Unix())
	if uidv == 0 {
		uidv = 1
	}
	attrsJSON, _ := json.Marshal(normalizeAttrs(attrs))
	id := "mbox_" + hxid.NewTimeSort().String()
	if _, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO imap_mailboxes (id, tenant, username, name, role, attrs, policy, uidvalidity, uidnext, modseq, subscribed, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 0, 1, ?)`),
		id, tenant, username, name, strings.TrimSpace(role), string(attrsJSON), rawOr(policy, "{}"), uidv, now.Format(time.RFC3339)); err != nil {
		if _, ok, gerr := s.GetMailbox(ctx, tenant, username, name); gerr == nil && ok {
			return Mailbox{}, ErrMailboxExists
		}
		return Mailbox{}, fmt.Errorf("imap: insert mailbox: %w", err)
	}
	mb, _, err := s.GetMailbox(ctx, tenant, username, name)
	return mb, err
}

func normalizeAttrs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[strings.ToLower(a)] {
			continue
		}
		seen[strings.ToLower(a)] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// DeleteMailbox soft-deletes a live mailbox (deleted_at). Its rows stay
// (immutable, unreferenced — the blob posture); children keep their names
// and the parent LISTs as \Noselect. INBOX is refused.
func (s *Store) DeleteMailbox(ctx context.Context, tenant, username, name string) (Mailbox, error) {
	username = NormalizeUsername(username)
	name = NormalizeMailboxName(name)
	if name == "INBOX" {
		return Mailbox{}, ErrINBOX
	}
	mb, ok, err := s.GetMailbox(ctx, tenant, username, name)
	if err != nil {
		return Mailbox{}, err
	}
	if !ok {
		return Mailbox{}, ErrMailboxNotFound
	}
	if _, err := s.db.ExecContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`),
		s.now().Format(time.RFC3339), mb.ID); err != nil {
		return Mailbox{}, fmt.Errorf("imap: delete mailbox: %w", err)
	}
	return mb, nil
}

// RenameMailbox renames a live mailbox and its whole subtree. UIDs,
// UIDVALIDITY, roles and rows are untouched — only names change. INBOX
// cannot be renamed; the new name must be free.
func (s *Store) RenameMailbox(ctx context.Context, tenant, username, oldName, newName string) (Mailbox, error) {
	username = NormalizeUsername(username)
	oldName = NormalizeMailboxName(oldName)
	newName = NormalizeMailboxName(newName)
	if oldName == "INBOX" || newName == "INBOX" {
		return Mailbox{}, ErrINBOX
	}
	if newName == "" {
		return Mailbox{}, errors.New("imap: empty mailbox name")
	}
	if newName == oldName {
		mb, ok, err := s.GetMailbox(ctx, tenant, username, oldName)
		if err == nil && !ok {
			err = ErrMailboxNotFound
		}
		return mb, err
	}
	if strings.HasPrefix(newName, oldName+"/") {
		return Mailbox{}, errors.New("imap: cannot move a mailbox under itself")
	}
	mb, ok, err := s.GetMailbox(ctx, tenant, username, oldName)
	if err != nil {
		return Mailbox{}, err
	}
	if !ok {
		return Mailbox{}, ErrMailboxNotFound
	}
	if _, taken, err := s.GetMailbox(ctx, tenant, username, newName); err != nil {
		return Mailbox{}, err
	} else if taken {
		return Mailbox{}, ErrMailboxExists
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET name = ? WHERE id = ?`), newName, mb.ID); err != nil {
		return Mailbox{}, fmt.Errorf("imap: rename: %w", err)
	}
	// Children: replace the old prefix. substr/length/|| are portable
	// across SQLite and Postgres; LIKE is avoided because names may hold
	// its wildcards.
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE imap_mailboxes
		   SET name = ? || substr(name, length(?) + 1)
		 WHERE tenant = ? AND username = ? AND deleted_at IS NULL
		   AND substr(name, 1, length(?) + 1) = ? || '/'`),
		newName, oldName, tenant, username, oldName, oldName); err != nil {
		return Mailbox{}, fmt.Errorf("imap: rename subtree: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, fmt.Errorf("imap: commit: %w", err)
	}
	out, _, err := s.GetMailboxByID(ctx, mb.ID)
	return out, err
}

// UpdateMailbox sets a mailbox's role / attrs / policy (nil = keep).
func (s *Store) UpdateMailbox(ctx context.Context, id string, role *string, attrs []string, policy json.RawMessage) (Mailbox, error) {
	sets := []string{}
	args := []any{}
	if role != nil {
		sets = append(sets, "role = ?")
		args = append(args, strings.TrimSpace(*role))
	}
	if attrs != nil {
		b, _ := json.Marshal(normalizeAttrs(attrs))
		sets = append(sets, "attrs = ?")
		args = append(args, string(b))
	}
	if len(policy) > 0 {
		if !json.Valid(policy) {
			return Mailbox{}, errors.New("imap: policy is not valid JSON")
		}
		sets = append(sets, "policy = ?")
		args = append(args, string(policy))
	}
	if len(sets) > 0 {
		args = append(args, id)
		if _, err := s.db.ExecContext(ctx, s.rb(
			`UPDATE imap_mailboxes SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`), args...); err != nil {
			return Mailbox{}, fmt.Errorf("imap: update mailbox: %w", err)
		}
	}
	mb, ok, err := s.GetMailboxByID(ctx, id)
	if err == nil && !ok {
		err = ErrMailboxNotFound
	}
	return mb, err
}

// ResetMailbox empties a mailbox and bumps UIDVALIDITY — the one sanctioned
// way UIDVALIDITY changes (§25.6). Sessions holding it selected must
// reselect; they are not notified (a reset is an operator act, not a
// client flow).
func (s *Store) ResetMailbox(ctx context.Context, id string) (Mailbox, error) {
	mb, ok, err := s.GetMailboxByID(ctx, id)
	if err != nil {
		return Mailbox{}, err
	}
	if !ok {
		return Mailbox{}, ErrMailboxNotFound
	}
	uidv := uint32(s.now().Unix())
	if uidv <= mb.UIDValidity {
		uidv = mb.UIDValidity + 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM imap_messages WHERE mailbox_id = ?`), id); err != nil {
		return Mailbox{}, fmt.Errorf("imap: reset rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET uidvalidity = ?, uidnext = 1, modseq = modseq + 1 WHERE id = ?`), uidv, id); err != nil {
		return Mailbox{}, fmt.Errorf("imap: reset mailbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, fmt.Errorf("imap: commit: %w", err)
	}
	out, _, err := s.GetMailboxByID(ctx, id)
	return out, err
}

// CopyMessage inserts a new row in dest over the same retained object as
// (src, uid). The object_key travels with the copy unless dest already
// holds that key, in which case the copy is keyless (a client COPY is not
// a stack projection). Returns the dest UID.
func (s *Store) CopyMessage(ctx context.Context, srcID string, uid uint32, destID string) (AppendResult, error) {
	m, ok, err := s.GetMessage(ctx, srcID, uid)
	if err != nil {
		return AppendResult{}, err
	}
	if !ok {
		return AppendResult{}, fmt.Errorf("imap: message %d not found", uid)
	}
	m.MailboxID = destID
	if m.ObjectKey != "" {
		if _, taken, err := s.GetMessageByKey(ctx, destID, m.ObjectKey); err != nil {
			return AppendResult{}, err
		} else if taken {
			m.ObjectKey = ""
		}
	}
	return s.AppendMessage(ctx, destID, m)
}

// MoveMessages copies each uid into dest and removes it from src, in one
// transaction per message (a crash between leaves a copy — never a loss).
// Returns src→dest UID pairs plus the src expunges in descending sequence.
func (s *Store) MoveMessages(ctx context.Context, srcID string, uids []uint32, destID string) (map[uint32]uint32, []Expunged, error) {
	moved := map[uint32]uint32{}
	if len(uids) == 0 {
		return moved, nil, nil
	}
	// Copy first (each emits its dest append), then expunge in one pass so
	// the src sequence numbers are reported consistently.
	for _, uid := range uids {
		res, err := s.CopyMessage(ctx, srcID, uid, destID)
		if err != nil {
			return moved, nil, err
		}
		moved[uid] = res.UID
	}
	exp, err := s.removeRows(ctx, srcID, uids, false)
	return moved, exp, err
}

// Expunge removes the rows flagged \Deleted (restricted to uids when
// non-empty) and returns them in descending sequence order.
func (s *Store) Expunge(ctx context.Context, mailboxID string, uids []uint32) ([]Expunged, error) {
	return s.removeRows(ctx, mailboxID, uids, true)
}

// removeRows deletes rows (all of `uids`, or — with onlyDeleted — those
// among them, or all rows when uids is empty, carrying \Deleted), computing
// each doomed row's pre-removal sequence number in one snapshot so the
// EXPUNGE responses are correct when applied top-down.
func (s *Store) removeRows(ctx context.Context, mailboxID string, uids []uint32, onlyDeleted bool) ([]Expunged, error) {
	heads, err := s.ListMessageHeads(ctx, mailboxID)
	if err != nil {
		return nil, err
	}
	want := map[uint32]bool{}
	for _, u := range uids {
		want[u] = true
	}
	var doomed []Expunged
	for i, h := range heads {
		if len(uids) > 0 && !want[h.UID] {
			continue
		}
		if onlyDeleted && !HasFlag(h.Flags, `\Deleted`) {
			continue
		}
		doomed = append(doomed, Expunged{UID: h.UID, Seq: uint32(i) + 1})
	}
	if len(doomed) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, d := range doomed {
		if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM imap_messages WHERE mailbox_id = ? AND uid = ?`), mailboxID, d.UID); err != nil {
			return nil, fmt.Errorf("imap: expunge %d: %w", d.UID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE imap_mailboxes SET modseq = modseq + 1 WHERE id = ?`), mailboxID); err != nil {
		return nil, fmt.Errorf("imap: bump modseq: %w", err)
	}
	var total uint32
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ?`), mailboxID).Scan(&total); err != nil {
		return nil, fmt.Errorf("imap: count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("imap: commit: %w", err)
	}
	// Descending sequence: removing seq 5 then seq 2 keeps 2 valid.
	sort.Slice(doomed, func(i, j int) bool { return doomed[i].Seq > doomed[j].Seq })
	changes := make([]Change, 0, len(doomed))
	for _, d := range doomed {
		changes = append(changes, Change{MailboxID: mailboxID, Kind: ChangeExpunge, UID: d.UID, Seq: d.Seq, Total: total, Origin: originFrom(ctx)})
	}
	s.emit(changes)
	return doomed, nil
}

// ListMessages is the windowed read for txco://imap/messages: rows with
// uid > after, in UID order, at most limit, optionally only those carrying
// ANY of flags. next is the last uid returned when the window was full.
func (s *Store) ListMessages(ctx context.Context, mailboxID string, after uint32, limit int, flags []string) ([]Message, uint32, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT `+messageCols+` FROM imap_messages
		 WHERE mailbox_id = ? AND uid > ? ORDER BY uid`), mailboxID, after)
	if err != nil {
		return nil, 0, fmt.Errorf("imap: list messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	var next uint32
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("imap: scan message: %w", err)
		}
		if len(flags) > 0 {
			hit := false
			for _, f := range flags {
				if HasFlag(m.Flags, f) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		if len(out) == limit {
			next = out[len(out)-1].UID
			break
		}
		out = append(out, m)
	}
	return out, next, rows.Err()
}

// CountByMailbox returns (messages, unseen) for a mailbox.
func (s *Store) CountByMailbox(ctx context.Context, mailboxID string) (uint32, uint32, error) {
	heads, err := s.ListMessageHeads(ctx, mailboxID)
	if err != nil {
		return 0, 0, err
	}
	var unseen uint32
	for _, h := range heads {
		if !HasFlag(h.Flags, `\Seen`) {
			unseen++
		}
	}
	return uint32(len(heads)), unseen, nil
}

// ensure sql import stays used on every build.
var _ = sql.ErrNoRows
