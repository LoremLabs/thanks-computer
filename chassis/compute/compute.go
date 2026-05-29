// Package compute is the sandboxed local-compute seam. A resonator's
// `EXEC "op://name"` is resolved at apply time to
// `compute://<alg>/<digest>`; at runtime the processor hands that ref here,
// which loads the content-addressed artifact and runs it on a registered
// Engine.
//
// The engine (a wasm runtime, etc.) is an implementation detail behind the
// Engine interface + registry — the same extension pattern as
// chassis/artifact and chassis/trace. The open core ships a reference engine;
// additional engines self-register via a blank import without changing this
// package or its callers. The language a compute is authored in is a
// build-time concern: one engine runs any conforming artifact.
package compute

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound is returned by a Resolver when no artifact exists for a ref.
var ErrNotFound = errors.New("compute: artifact not found")

// Limits bound a single compute invocation. Zero values mean "engine
// default / unbounded"; chassis config wires real defaults.
type Limits struct {
	MaxMemoryMB int
	MaxWall     time.Duration
	// Now is the wall-clock the guest should observe (e.g. JS Date.now()).
	// The processor sets it to the real clock at the moment the op executes,
	// passed via WithNow. Zero = frozen epoch (engine default; bare unit
	// tests). Tests may inject a fixed time here for reproducibility.
	Now time.Time
	// Stderr receives the guest's diagnostic output (console.* / writes to fd
	// 2). nil discards it. The processor wires this to the chassis logger so
	// console.log is visible.
	Stderr io.Writer
	// MetricsSink, if set, is called once per invocation with the runtime
	// measurements (wall time, memory, exit status). The processor wires it to
	// the trace + logger. nil = no metrics.
	MetricsSink func(Metrics)
}

// Metrics are per-invocation runtime measurements, for observability. (There
// is no instruction "fuel" — wazero doesn't meter instructions; wall time +
// memory + status are what's cheaply available.)
type Metrics struct {
	WallMS      int64
	MemoryBytes uint32
	Status      string // "ok" | "trapped" | "wall-limit"
}

type nowKey struct{}

// WithNow carries the wall-clock the guest should observe — the real time at
// the moment the op runs, stamped by the caller. The generic engine reads it
// from context rather than from any envelope, so the seam stays
// transport-agnostic (and tests can inject a fixed time).
func WithNow(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, nowKey{}, t)
}

func nowFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(nowKey{}).(time.Time)
	return t, ok
}

type logWriterKey struct{}

// WithLogWriter carries a writer for the guest's diagnostic output (console.*).
// The processor wires it to the chassis logger so console.log is visible.
func WithLogWriter(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, logWriterKey{}, w)
}

func logWriterFrom(ctx context.Context) (io.Writer, bool) {
	w, ok := ctx.Value(logWriterKey{}).(io.Writer)
	return w, ok
}

type metricsSinkKey struct{}

// WithMetricsSink carries a callback invoked once per invocation with the
// runtime metrics. The processor wires it to the trace + logger.
func WithMetricsSink(ctx context.Context, sink func(Metrics)) context.Context {
	return context.WithValue(ctx, metricsSinkKey{}, sink)
}

func metricsSinkFrom(ctx context.Context) (func(Metrics), bool) {
	s, ok := ctx.Value(metricsSinkKey{}).(func(Metrics))
	return s, ok
}

// Ref is the parsed form of a `compute://<alg>/<digest>` EXEC value.
type Ref struct {
	Alg    string // digest algorithm, e.g. "sha256"
	Digest string // hex content digest
}

// String renders the canonical `compute://<alg>/<digest>` form.
func (r Ref) String() string { return "compute://" + r.Alg + "/" + r.Digest }

// ParseRef parses a `compute://<alg>/<digest>` value. ok is false for any
// other shape (missing scheme, missing alg, or missing digest).
func ParseRef(s string) (Ref, bool) {
	const scheme = "compute://"
	if !strings.HasPrefix(s, scheme) {
		return Ref{}, false
	}
	alg, digest, found := strings.Cut(strings.TrimPrefix(s, scheme), "/")
	if !found || alg == "" || digest == "" {
		return Ref{}, false
	}
	return Ref{Alg: alg, Digest: digest}, true
}

// StoreRef is the artifact-store key for a compute ref:
// "computes/<alg>/<digest>". The runtime resolver, the admin upload endpoint,
// and activate-time verification all derive the key through this one helper so
// they can never drift.
func (r Ref) StoreRef() string { return "computes/" + r.Alg + "/" + r.Digest }

// refRE matches a quoted compute ref inside txcl, e.g.
// `EXEC "compute://sha256/deadbeef"`. Mirrors the oprefs scanner shape.
var refRE = regexp.MustCompile(`"compute://([a-z0-9]+)/([A-Za-z0-9_-]+)"`)

// ScanRefs returns the distinct compute refs referenced by a txcl resonator.
// Used at activate time to verify every referenced artifact is present before
// the resonator materialises into the runtime ops table.
func ScanRefs(txcl string) []Ref {
	ms := refRE.FindAllStringSubmatch(txcl, -1)
	if len(ms) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]Ref, 0, len(ms))
	for _, m := range ms {
		ref := Ref{Alg: m[1], Digest: m[2]}
		k := ref.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ref)
	}
	return out
}

// Artifact is a resolved, content-addressed compute. Wasm holds the module
// bytes (opaque to this package); Engine names the registered engine that
// runs them.
type Artifact struct {
	Alg    string
	Digest string
	Engine string
	Wasm   []byte
}

// Instance is a loaded, ready-to-invoke compute. Engines load per call unless
// they document concurrency-safe reuse.
type Instance interface {
	// Invoke runs the compute: JSON bytes in, JSON bytes out, under lim.
	Invoke(ctx context.Context, input []byte, lim Limits) ([]byte, error)
	Close(ctx context.Context) error
}

// Engine runs artifacts of one kind (e.g. wasm). Load prepares an artifact
// for invocation; engines may cache compiled forms keyed by digest.
type Engine interface {
	Load(ctx context.Context, art Artifact) (Instance, error)
	Name() string
}

// Resolver maps a content-addressed ref to its artifact. Backed by the
// compute_artifacts store; tests inject a stub. ErrNotFound when absent.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) (Artifact, error)
}

// Runner is the processor-facing seam: resolve a ref and run it. The
// processor holds one and errors loudly if a compute:// op fires while unset.
type Runner interface {
	Run(ctx context.Context, ref Ref, input []byte) ([]byte, error)
}
