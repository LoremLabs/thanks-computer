// Package workspace is the seam behind `workspace://<name>/<verb>` EXEC
// dispatch: an owned, stateful execution environment — a directory on the
// chassis host in dev (chassis/workspace/local), a Fly Sprite in the fleet
// (overlay) — that a tenant's stack addresses by name and runs commands in.
//
// Where a compute:// nano-op is a pure JSON→JSON transform in a no-fs,
// no-network sandbox, a workspace keeps files between calls, has a full
// runtime, and reaches the public internet. It is lambda-shaped in cost —
// woken on exec, put to sleep when idle, destroyed after idle days — and
// its output is data: a non-zero exit is a successful dispatch whose result
// says "exit 3", not a chassis error.
//
// Vocabulary: a Computer executes work; a Workspace is the owned, stateful
// environment that provides one; a Run is one wake→sleep; a Task spans
// runs. Verbs: create, wake, exec, checkpoint, sleep, destroy.
//
// Providers self-register from their package's init() (the compute-engine
// and vector-store idiom); the chassis activates one with
// --workspace-provider plus a blank import. The processor talks to a
// Manager, never to a Provider directly; the Manager owns the identity
// table (Store) when one is wired.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Spec identifies a workspace: the (tenant, stack, name) triple is its
// identity; Runtime and Network are creation-time hints a provider may
// honor ("" = provider default).
type Spec struct {
	Tenant  string
	Stack   string
	Name    string
	Runtime string
	Network string // "public" (default) | "none"
}

// Handle is a provider-side reference to a created workspace. Ref is the
// provider's own id (a sprite name, a local directory); Name echoes
// Spec.Name for logging.
type Handle struct {
	Provider string
	Ref      string
	Name     string
}

// ExecRequest is one command to run inside a woken workspace. Exactly one
// of Command (run via the workspace's shell) or Args (an argv, no shell)
// is set. Cwd is relative to the workspace root. Env is added to the
// provider's base environment verbatim. Timeout mirrors the ctx deadline
// for providers that need it as a value.
type ExecRequest struct {
	Command string
	Args    []string
	Stdin   []byte
	Cwd     string
	Env     map[string]string
	Timeout time.Duration
}

// ExecResult is what a command produced. Exit is the process exit code
// (-1 when unknown: killed by the timeout, or the process never ran).
// Stdout/Stderr are capped at Limits.MaxOutputBytes; the *_Truncated flags
// say when the cap dropped bytes.
type ExecResult struct {
	Exit            int
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	WallMS          int64
}

// Limits are per-exec caps the Manager hands every Computer.
type Limits struct {
	// MaxOutputBytes caps captured stdout and stderr, each. 0 = provider
	// default (1 MiB).
	MaxOutputBytes int64
}

// DefaultMaxOutputBytes is the per-stream capture cap when Limits leaves it
// unset.
const DefaultMaxOutputBytes int64 = 1 << 20

// Status is a provider's view of a workspace's lifecycle state.
type Status struct {
	// State is one of created | running | warm | cold | destroyed.
	State    string
	LastUsed time.Time
}

// Provider is a workspace backend. Create must be idempotent (creating an
// existing workspace returns its handle). Sleep is a no-op where the
// provider sleeps implicitly.
type Provider interface {
	Name() string
	// Capabilities lists what this provider can do: "exec" always;
	// "network" when Spec.Network is honored; "checkpoint" when the
	// provider also implements Checkpointer.
	Capabilities() []string
	Create(ctx context.Context, spec Spec) (Handle, error)
	Wake(ctx context.Context, h Handle) (Computer, error)
	Sleep(ctx context.Context, h Handle) error
	Destroy(ctx context.Context, h Handle) error
	Status(ctx context.Context, h Handle) (Status, error)
}

// Computer is a woken workspace that can run a command.
type Computer interface {
	Exec(ctx context.Context, req ExecRequest, lim Limits) (ExecResult, error)
}

// Checkpointer is the optional capability behind the checkpoint verb. A
// provider advertises it by listing "checkpoint" in Capabilities and
// implementing this interface; the returned id is provider-scoped.
type Checkpointer interface {
	Checkpoint(ctx context.Context, h Handle, comment string) (string, error)
}

// WarmProvider is implemented by providers whose computer stays warm for a
// while after an exec before it sleeps (a fleet machine that idles before
// it parks). The Manager reuses the previous run id while the gap since
// the last exec is within the threshold — one run spans the warm window.
// Not implemented (or 0) means one run per exec.
type WarmProvider interface {
	WarmThreshold() time.Duration
}

// RunReporter is implemented by a Computer that can tell whether waking it
// started a new run (the provider reported the environment was not
// running). The Manager mints a new run id when it says so, even inside
// the warm window.
type RunReporter interface {
	NewRun() bool
}

// Config carries backend-selecting options resolved from chassis config. A
// backend reads only the fields it needs (same posture as compute.
// EngineConfig and vector.Config).
type Config struct {
	// LocalRoot is the local provider's root directory
	// (--workspace-local-root): <root>/<tenant>/<stack>/<name>.
	LocalRoot string
	// MaxOutputBytes is the per-stream capture cap (--workspace-max-output-bytes).
	MaxOutputBytes int64
}

// Constructor builds a Provider from resolved config.
type Constructor func(Config) (Provider, error)

// providers maps provider name → constructor. Providers self-register via
// init(); the chassis activates one with a blank import.
var providers = map[string]Constructor{}

// Register adds a provider constructor. Called from a provider package's
// init().
func Register(name string, c Constructor) { providers[name] = c }

// Open constructs the named provider. Unknown name is a startup error
// listing what is available (so a misconfigured --workspace-provider fails
// loudly).
func Open(name string, cfg Config) (Provider, error) {
	c, ok := providers[name]
	if !ok {
		avail := make([]string, 0, len(providers))
		for k := range providers {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("workspace: unknown provider %q (available: %v)", name, avail)
	}
	return c(cfg)
}

// ErrNotAllowed is returned when the configured provider is refused on this
// chassis (the local provider without --workspace-allow-local).
var ErrNotAllowed = errors.New("workspace: provider not enabled on this chassis")

// ErrTimeout is returned by a Computer when the op's context ended before
// the command did. The processor reports it in-band as
// workspace.error.code = "timeout".
var ErrTimeout = errors.New("workspace: exec timed out")

// Error is a coded provider failure. The processor surfaces Code verbatim
// as workspace.error.code, so providers can say "capacity", "bad_request",
// "provider" and the rule author can gate on it.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "workspace: " + e.Code + ": " + e.Message }

// Verbs is the closed vocabulary after the last "/" of a workspace ref.
var Verbs = []string{"create", "wake", "exec", "checkpoint", "sleep", "destroy"}

// IsVerb reports whether s is one of Verbs.
func IsVerb(s string) bool {
	for _, v := range Verbs {
		if s == v {
			return true
		}
	}
	return false
}

// ParseRef parses a `workspace://<name>/<verb>` EXEC value. The verb is the
// segment after the LAST "/", so a name may itself contain "/"
// (`workspace://pony/paris/exec` → name "pony/paris", verb "exec"). ok is
// false for any other shape: missing scheme, empty name, empty verb, or a
// verb outside Verbs. Name validity is ValidateName's job — the name may
// still be replaced by `WITH workspace = …` before it is checked.
func ParseRef(s string) (name, verb string, ok bool) {
	const scheme = "workspace://"
	if !strings.HasPrefix(s, scheme) {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, scheme)
	i := strings.LastIndex(rest, "/")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	name, verb = rest[:i], rest[i+1:]
	if !IsVerb(verb) {
		return "", "", false
	}
	return name, verb, true
}

// segmentRE is one name segment: a DNS-label-shaped token so a name can be
// embedded in a provider's own identifier (a sprite name, a directory).
var segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// MaxNameSegments bounds how deep a "/"-separated name may nest.
const MaxNameSegments = 4

// ValidateName checks a workspace name: 1–4 "/"-separated segments, each
// matching ^[a-z0-9][a-z0-9-]{0,62}$, none containing "--", none equal to a
// verb (so `workspace://exec/exec` cannot be read two ways).
func ValidateName(name string) error {
	if name == "" {
		return errors.New("workspace: empty name")
	}
	segs := strings.Split(name, "/")
	if len(segs) > MaxNameSegments {
		return fmt.Errorf("workspace: name %q has %d segments; max %d", name, len(segs), MaxNameSegments)
	}
	for _, s := range segs {
		switch {
		case !segmentRE.MatchString(s):
			return fmt.Errorf("workspace: name segment %q must match %s", s, segmentRE.String())
		case strings.Contains(s, "--"):
			return fmt.Errorf("workspace: name segment %q must not contain \"--\"", s)
		case IsVerb(s):
			return fmt.Errorf("workspace: name segment %q collides with a verb", s)
		}
	}
	return nil
}

// ProviderName derives a provider-side identifier for (tenant, stack,
// name): a readable prefix `<tenant>-<stack>-<name with / → ->` truncated
// to 40 characters, then "-" and the first 8 hex of
// sha256(tenant\x00stack\x00name) so two identities can never collide after
// truncation. At most 49 characters — a DNS label with room for a
// provider suffix.
func ProviderName(tenant, stack, name string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + stack + "\x00" + name))
	prefix := tenant + "-" + stack + "-" + strings.ReplaceAll(name, "/", "-")
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	prefix = strings.TrimRight(prefix, "-")
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

// SortedEnv renders an environment map as sorted KEY=VALUE pairs, the shape
// os/exec and the fleet SDKs take. Sorted so two execs with the same env
// produce the same argv (stable traces, stable tests).
func SortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// CappedWriter keeps the first Max bytes written to it and flags the rest
// as dropped. Writes never fail, so a child process never sees EPIPE from
// the cap — it just loses the tail of its output.
type CappedWriter struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

// NewCappedWriter returns a writer that keeps at most max bytes (≤ 0 means
// DefaultMaxOutputBytes).
func NewCappedWriter(max int64) *CappedWriter {
	if max <= 0 {
		max = DefaultMaxOutputBytes
	}
	return &CappedWriter{max: max}
}

func (c *CappedWriter) Write(p []byte) (int, error) {
	room := c.max - int64(c.buf.Len())
	switch {
	case room <= 0:
		c.truncated = true
	case int64(len(p)) > room:
		c.buf.Write(p[:room])
		c.truncated = true
	default:
		c.buf.Write(p)
	}
	return len(p), nil
}

// Bytes returns what was kept.
func (c *CappedWriter) Bytes() []byte { return c.buf.Bytes() }

// Truncated reports whether any byte was dropped.
func (c *CappedWriter) Truncated() bool { return c.truncated }

// entry is what the Manager remembers per identity between execs: the
// provider handle and the run bookkeeping the run-id policy needs.
type entry struct {
	h        Handle
	runID    string
	lastUsed time.Time
}

// Manager is what the processor drives: it owns the provider, the identity
// table (when a Store is wired), and turns "run this in workspace X" into
// lookup → create-if-needed → wake → exec, minting run ids along the way.
// Without a Store the identity lives in a per-process cache (dev);
// with one it survives restarts and is shared across nodes.
type Manager struct {
	prov  Provider
	lim   Limits
	store *Store
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// NewManager builds a Manager over a Provider with per-exec limits. store
// may be nil (no durable identity — the per-process cache only).
func NewManager(p Provider, lim Limits, store *Store) *Manager {
	if lim.MaxOutputBytes <= 0 {
		lim.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return &Manager{prov: p, lim: lim, store: store, now: func() time.Time { return time.Now().UTC() }, cache: map[string]entry{}}
}

// Provider exposes the backend (for capability checks and logging).
func (m *Manager) Provider() Provider { return m.prov }

// Store exposes the identity table, if any (admin listings).
func (m *Manager) Store() *Store { return m.store }

func identityKey(spec Spec) string {
	return spec.Tenant + "\x00" + spec.Stack + "\x00" + spec.Name
}

// lookup returns the entry for spec: the cache, then the identity table,
// then Create (+ record). A stored row for a different provider or in the
// destroyed state is treated as absent — the workspace is recreated on
// this provider and the row rewritten in place.
func (m *Manager) lookup(ctx context.Context, spec Spec) (entry, error) {
	key := identityKey(spec)
	m.mu.Lock()
	e, ok := m.cache[key]
	m.mu.Unlock()
	if ok {
		return e, nil
	}
	if m.store != nil {
		row, err := m.store.Get(ctx, spec.Tenant, spec.Stack, spec.Name)
		if err != nil {
			return entry{}, &Error{Code: "provider", Message: err.Error()}
		}
		if row != nil && row.Status != StatusDestroyed && row.Provider == m.prov.Name() {
			e = entry{h: Handle{Provider: row.Provider, Ref: row.ProviderRef, Name: row.Name}, runID: row.RunID, lastUsed: row.LastUsedAt}
			m.remember(key, e)
			return e, nil
		}
	}
	h, err := m.prov.Create(ctx, spec)
	if err != nil {
		return entry{}, err
	}
	e = entry{h: h}
	if m.store != nil {
		now := m.now()
		if err := m.store.Upsert(ctx, Row{
			Tenant: spec.Tenant, Stack: spec.Stack, Name: spec.Name,
			Provider: h.Provider, ProviderRef: h.Ref, Runtime: spec.Runtime, Network: spec.Network,
			Status: StatusCreated, CreatedAt: now, LastUsedAt: now,
		}); err != nil {
			return entry{}, &Error{Code: "provider", Message: err.Error()}
		}
	}
	m.remember(key, e)
	return e, nil
}

func (m *Manager) remember(key string, e entry) {
	m.mu.Lock()
	m.cache[key] = e
	m.mu.Unlock()
}

func (m *Manager) forget(spec Spec) {
	m.mu.Lock()
	delete(m.cache, identityKey(spec))
	m.mu.Unlock()
}

// runIDFor applies the run-id policy: reuse the last run id while the
// provider's warm window has not elapsed and the woken computer does not
// report a fresh start; otherwise mint a new one. Providers without a warm
// window (local) get one run per exec.
func (m *Manager) runIDFor(e entry, comp Computer) string {
	var threshold time.Duration
	if wp, ok := m.prov.(WarmProvider); ok {
		threshold = wp.WarmThreshold()
	}
	if e.runID != "" && threshold > 0 && m.now().Sub(e.lastUsed) <= threshold {
		if rr, ok := comp.(RunReporter); !ok || !rr.NewRun() {
			return e.runID
		}
	}
	return hxid.New().String()
}

// Exec runs req in the workspace spec names: lookup/create, wake, exec,
// record. It returns the provider handle and the run id alongside the
// result so the processor can stamp provenance. A non-nil error is a
// transport or provider failure — never a non-zero exit, which is data in
// ExecResult.
func (m *Manager) Exec(ctx context.Context, spec Spec, req ExecRequest) (ExecResult, Handle, string, error) {
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return ExecResult{Exit: -1}, Handle{}, "", err
	}
	comp, err := m.prov.Wake(ctx, e.h)
	if err != nil {
		// A handle that will not wake (destroyed out from under us) is
		// dropped so the next exec recreates rather than retrying a corpse.
		m.forget(spec)
		return ExecResult{Exit: -1}, e.h, "", err
	}
	runID := m.runIDFor(e, comp)
	res, err := comp.Exec(ctx, req, m.lim)
	now := m.now()
	e.runID, e.lastUsed = runID, now
	m.remember(identityKey(spec), e)
	if m.store != nil {
		// Best-effort bookkeeping: the exec already happened; a failed
		// touch costs one stale last_used_at, not the result.
		_ = m.store.Touch(ctx, ID(spec.Tenant, spec.Stack, spec.Name), now, runID, StatusRunning)
	}
	return res, e.h, runID, err
}

// Create makes sure the workspace exists (idempotent) and returns its
// handle without waking it.
func (m *Manager) Create(ctx context.Context, spec Spec) (Handle, error) {
	e, err := m.lookup(ctx, spec)
	return e.h, err
}

// Wake warms the workspace ahead of a task's first exec and returns the
// run id that exec will run under.
func (m *Manager) Wake(ctx context.Context, spec Spec) (Handle, string, error) {
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return Handle{}, "", err
	}
	comp, err := m.prov.Wake(ctx, e.h)
	if err != nil {
		m.forget(spec)
		return e.h, "", err
	}
	runID := m.runIDFor(e, comp)
	now := m.now()
	e.runID, e.lastUsed = runID, now
	m.remember(identityKey(spec), e)
	if m.store != nil {
		_ = m.store.Touch(ctx, ID(spec.Tenant, spec.Stack, spec.Name), now, runID, StatusRunning)
	}
	return e.h, runID, nil
}

// Sleep asks the provider to park the workspace. A no-op on providers that
// sleep implicitly.
func (m *Manager) Sleep(ctx context.Context, spec Spec) error {
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return err
	}
	return m.prov.Sleep(ctx, e.h)
}

// Checkpoint snapshots the workspace through the provider's Checkpointer
// and records the returned reference. Providers without the capability
// report unsupported.
func (m *Manager) Checkpoint(ctx context.Context, spec Spec, comment string) (string, error) {
	cp, ok := m.prov.(Checkpointer)
	if !ok {
		return "", &Error{Code: "unsupported", Message: "provider " + m.prov.Name() + " has no checkpoint capability"}
	}
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return "", err
	}
	ref, err := cp.Checkpoint(ctx, e.h, comment)
	if err != nil {
		return "", err
	}
	if m.store != nil {
		_ = m.store.SetCheckpoint(ctx, ID(spec.Tenant, spec.Stack, spec.Name), ref)
	}
	return ref, nil
}

// Destroy removes the workspace, marks its row, and forgets its handle;
// the next exec starts fresh.
func (m *Manager) Destroy(ctx context.Context, spec Spec) error {
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return err
	}
	m.forget(spec)
	derr := m.prov.Destroy(ctx, e.h)
	if m.store != nil {
		_ = m.store.MarkDestroyed(ctx, ID(spec.Tenant, spec.Stack, spec.Name), m.now())
	}
	return derr
}

// Status reports the provider's view of the workspace.
func (m *Manager) Status(ctx context.Context, spec Spec) (Status, error) {
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return Status{}, err
	}
	return m.prov.Status(ctx, e.h)
}
