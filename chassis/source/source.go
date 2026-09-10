// Package source is the chassis's outbound source-inlet seam: a cursor-
// bearing watcher that dials an EXTERNAL system a tenant delegated to us
// (today a remote IMAP mailbox; later Kafka/SQS/CDC/webhook-poll), reads
// what is new past a durable cursor, fires each item as a run of the
// tenant's `_source/0` stack, and — once that run succeeds — acknowledges
// the item at the source (marking it read, moving it aside).
//
// This is the OPPOSITE direction from the `imap` personality, which is a
// server other people's mail clients connect INTO. A source connects OUT.
// It is also not an `EXEC` op: an op pulls inside one run, whereas a source
// is a durable subscription the chassis drives on a schedule, so its
// lifecycle (claim → poll → dispatch → ack → advance) lives in a poller
// personality, not on the request path.
//
// The seam is deliberately kind-agnostic. A Kind opens connections to one
// class of external system; a Conn is one live attachment the poller owns
// for the duration of a claim. Adding a second kind (say Kafka) is a new
// Kind implementation that self-registers — no change to the poller, the
// store, the envelope shape, or the cursor plumbing, because the poller
// never interprets a Kind's cursor or item metadata.
//
// Credentials never pass through this package as anything durable: the
// poller materializes the tenant's secret from the encrypted store, hands
// it to Open as a raw []byte, and zeroes it the instant Open returns. A
// Kind must not retain it — the authenticated Conn holds the socket, not
// the password. Nothing here is JSON-serializable in a way that could carry
// a credential into an envelope, trace, or log.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/egress"
)

// Cursor is the durable "how far have I read" marker for one source. It is
// opaque to the poller and the store — only the Kind that produced it reads
// it (for IMAP it is `{"uidvalidity":N,"uid_hwm":M}`). A nil/empty Cursor
// means "from the beginning". Keeping it opaque is what lets a second kind
// with a completely different cursor shape reuse every other moving part.
type Cursor json.RawMessage

// Item is one unit of new input read from a source.
type Item struct {
	// Key is a stable dedup identity for this item within its source,
	// surfaced to the stack as `_txc.source.key` so a run can guard against
	// an at-least-once redelivery (kv/cas, or txco://imap/append object_key).
	// For IMAP it is "<uidvalidity>:<uid>".
	Key string

	// Raw is the item's bytes — RFC 5322 for an IMAP message. The poller
	// hands these to a kind-neutral parser (chassis/mail.ParseMessage for
	// mail) so a fetched message and an LMTP-delivered one produce identical
	// envelope JSON.
	Raw []byte

	// Meta is kind-specific facts the poller stamps under `_txc.source.*`
	// verbatim (it never interprets them) — e.g. `{"uid":42,"flags":[...]}`.
	Meta json.RawMessage

	// Cursor is the cursor to persist AFTER this item's run and ack both
	// succeed. Items arrive in order; the poller advances the stored cursor
	// item-by-item on success and stops at the first failure, so a mid-batch
	// failure never skips an unprocessed item.
	Cursor Cursor
}

// Action is what to do to an item at the source once its run succeeded. It
// is the parsed form of a source's `on_processed` config, overridable per
// run by the stack via `_txc.source.res.action`.
type Action struct {
	Op   string // "none" | "seen" | "move"
	Dest string // destination mailbox/folder for "move"; empty otherwise
}

// ParseAction parses an action spec: "" / "none" (do nothing), "seen" (mark
// read), or "move:<dest>" (move aside to <dest>). A move with no destination
// is an error rather than a silent no-op, since it usually means a typo in
// config that would otherwise leave mail piling up unread.
func ParseAction(spec string) (Action, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "" || spec == "none":
		return Action{Op: "none"}, nil
	case spec == "seen":
		return Action{Op: "seen"}, nil
	case strings.HasPrefix(spec, "move:"):
		dest := strings.TrimSpace(strings.TrimPrefix(spec, "move:"))
		if dest == "" {
			return Action{}, fmt.Errorf("source: move action needs a destination (move:<folder>)")
		}
		return Action{Op: "move", Dest: dest}, nil
	default:
		return Action{}, fmt.Errorf("source: unknown action %q (want none|seen|move:<folder>)", spec)
	}
}

// String renders an Action back to its spec form (round-trips ParseAction).
func (a Action) String() string {
	switch a.Op {
	case "move":
		return "move:" + a.Dest
	case "seen":
		return "seen"
	default:
		return "none"
	}
}

// OpenParams carries everything a Kind needs to establish one Conn. The
// credential is live only for the duration of the Open call; a Kind must
// authenticate with it and not keep a reference.
type OpenParams struct {
	// Config is the kind-specific declared config blob (the pack line's
	// fields), e.g. `{"host":"imap.fastmail.com","port":993,...}`. It never
	// contains the secret VALUE — only its name, resolved by the poller.
	Config json.RawMessage

	// Cred is the materialized secret value (the mailbox password). It is
	// zeroed by the caller the moment Open returns; do not retain it.
	Cred []byte

	// Guard is the egress policy for the outbound dial. When non-nil a Kind
	// MUST route its connection through it (net.Dialer.Control =
	// egress.DialControl(guard)) so a source cannot be pointed at loopback,
	// link-local, or private address space. A nil Guard means "no policy"
	// (dev/test).
	Guard egress.Guard

	// Logger is the poller's logger, scoped for this source.
	Logger *zap.Logger
}

// Conn is one live attachment to a source, owned by the poller for the
// duration of a single claim. It is NOT safe for concurrent use.
type Conn interface {
	// Poll reads items strictly after cur, up to limit, in ascending order.
	// Each returned Item carries the cursor to persist once that item's run
	// and ack both succeed. `drained` is the cursor to persist only if the
	// WHOLE batch is processed without a failure (or the batch is empty): it
	// carries side-band progress such as an IMAP UIDVALIDITY reset, so an
	// empty or renumbered source is not rescanned forever. drained may be nil.
	Poll(ctx context.Context, cur Cursor, limit int) (items []Item, drained Cursor, err error)

	// Ack applies act to one item at the source. Called by the poller only
	// after that item's run reached a successful terminal. A "none" action
	// is a no-op. An error stops the batch (the cursor does not advance past
	// this item), so Ack must be safe to retry on the next poll.
	Ack(ctx context.Context, it Item, act Action) error

	// Close releases the connection. Always called once per successful Open.
	Close() error
}

// Kind opens connections to one class of external source.
type Kind interface {
	// Name is the kind identity used in a source's `kind` field ("imap").
	Name() string
	// Open dials and authenticates one connection. See OpenParams for the
	// credential lifetime contract.
	Open(ctx context.Context, p OpenParams) (Conn, error)
}

// kinds is the kind registry, populated by each kind package's init() via
// RegisterKind and read by the poller via OpenKind. Not guarded by a lock:
// registration happens only at init (single-threaded), before any Open.
var kinds = map[string]Kind{}

// RegisterKind adds a source kind. Called from a kind package's init(); a
// duplicate name panics at boot (a programming error). The concrete kinds
// live in subpackages (imapsource) and are blank-imported by the binary, so
// a build that omits a kind simply cannot open a source of it.
func RegisterKind(k Kind) {
	name := k.Name()
	if _, dup := kinds[name]; dup {
		panic(fmt.Sprintf("source: duplicate kind %q", name))
	}
	kinds[name] = k
}

// OpenKind returns the registered kind, or (nil, false) if no kind of that
// name is built into this binary.
func OpenKind(name string) (Kind, bool) {
	k, ok := kinds[name]
	return k, ok
}

// Kinds returns the registered kind names (for diagnostics / a "no such
// kind" error that lists what is available).
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for name := range kinds {
		out = append(out, name)
	}
	return out
}
