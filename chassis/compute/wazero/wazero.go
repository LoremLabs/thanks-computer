// Package wazero is the reference in-process compute engine: it runs a
// content-addressed WASI module on the pure-Go wazero runtime (no CGO). The
// ABI is WASI command style — input JSON arrives on stdin, output JSON is read
// from stdout, log lines go to stderr. Any language that compiles to a
// conforming WASI module (Rust, TinyGo, JS via Javy/QuickJS, …) runs here
// unchanged; the language is a build-time concern.
//
// Two module shapes run here, detected automatically:
//
//   - Self-contained: the whole runtime is in the module (Rust/TinyGo native
//     wasm, or a static `javy build`). Compiled once, instantiated per call.
//   - Dynamically linked Javy: a tiny (~1 KB) module carrying only the op's JS
//     bytecode, importing `invoke`/`cabi_realloc`/`memory` from the shared
//     QuickJS plugin (chassis/compute/javyplugin). The engine links it against
//     that plugin so thousands of ops share one ~1.25 MB engine instead of
//     embedding it per op. See the dynamic-link section below.
//
// Sandbox posture ("restricted WASI"): no filesystem, no network, no env, no
// args. Wall clock is frozen and randomness is a fixed-seed PRNG — deterministic
// rather than denied, since real guest runtimes call clock/random at startup
// (denying breaks them) but exposing the host's would be ambient
// nondeterminism. Monotonic time is the one exception: guest runtimes require
// it to advance, so the host monotonic clock is used (a minor timing channel,
// no other authority). Memory is capped (runtime config) and a per-invocation
// wall-clock deadline kills a runaway guest (CloseOnContextDone inserts
// interrupt checks at loop back-edges and calls).
//
// CAVEAT — guest-language RNGs are NOT made deterministic by the fixed-seed
// random_get above. QuickJS's Math.random() seeds from the monotonic clock,
// not random_get, so it varies across runs (non-reproducible; monotonic-seeded
// ⇒ not crypto-grade). This is harmless: computes run exactly once (no replay).
// Don't assume reproducible randomness from a guest unless you control its seed.
//
// Activate with a blank import:
//
//	import _ "github.com/loremlabs/thanks-computer/chassis/compute/wazero"
package wazero

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/javyplugin"
)

// Name is the registered engine identifier.
const Name = "wazero"

// pagesPerMB is wasm pages (64 KiB each) per megabyte.
const pagesPerMB = 16

// wasmPageBytes is the wasm linear-memory page size.
const wasmPageBytes = 65536

func init() {
	compute.RegisterEngine(Name, func(cfg compute.EngineConfig) (compute.Engine, error) {
		return New(cfg)
	})
}

// engine holds the shared wazero runtime + a compiled-module cache keyed by
// content digest. Compilation is expensive and cached; instantiation is
// per-call and cheap. The compilation cache is shared with the per-call
// runtimes used for dynamically linked modules so the ~1.25 MB plugin compiles
// only once across the whole process.
type engine struct {
	cfg   compute.EngineConfig
	cache wazero.CompilationCache

	rt    wazero.Runtime
	mu    sync.Mutex
	cmods map[string]wazero.CompiledModule

	// Dynamic-link plugin snapshot, built lazily on first dynamically linked
	// invocation (see dynamicInstance). The post-init plugin memory is captured
	// once and restored into a fresh plugin instance per call, which lets us run
	// with the event loop enabled while leaving runtime stdin free for input.
	dynOnce  sync.Once
	dynSnap  []byte
	dynPages uint32
	dynErr   error
}

// New builds the engine. The runtime enables context-deadline interruption
// (for the wall-clock limit) and an optional memory cap.
func New(cfg compute.EngineConfig) (compute.Engine, error) {
	cache := wazero.NewCompilationCache()
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig(cfg, cache))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		_ = cache.Close(ctx)
		return nil, fmt.Errorf("wazero: instantiate WASI: %w", err)
	}
	return &engine{
		cfg:   cfg,
		cache: cache,
		rt:    rt,
		cmods: map[string]wazero.CompiledModule{},
	}, nil
}

// runtimeConfig is the shared runtime configuration: context-deadline
// interruption (wall limit), the shared compilation cache, and the optional
// memory cap.
func runtimeConfig(cfg compute.EngineConfig, cache wazero.CompilationCache) wazero.RuntimeConfig {
	rc := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithCompilationCache(cache)
	if cfg.MaxMemoryMB > 0 {
		rc = rc.WithMemoryLimitPages(uint32(cfg.MaxMemoryMB * pagesPerMB))
	}
	return rc
}

func (e *engine) Name() string { return Name }

func (e *engine) compiled(ctx context.Context, art compute.Artifact) (wazero.CompiledModule, error) {
	key := art.Alg + ":" + art.Digest
	e.mu.Lock()
	defer e.mu.Unlock()
	if cm, ok := e.cmods[key]; ok {
		return cm, nil
	}
	cm, err := e.rt.CompileModule(ctx, art.Wasm)
	if err != nil {
		return nil, fmt.Errorf("wazero: compile %s: %w", key, err)
	}
	e.cmods[key] = cm
	return cm, nil
}

func (e *engine) Load(ctx context.Context, art compute.Artifact) (compute.Instance, error) {
	cm, err := e.compiled(ctx, art)
	if err != nil {
		return nil, err
	}
	if importsPlugin(cm) {
		// Dynamically linked Javy module: it imports the plugin namespace, so it
		// is linked against the shared plugin at invoke time in a fresh per-call
		// runtime (see dynamicInstance.Invoke). Compiling it here (above) only
		// inspects/caches it — compilation does not resolve imports.
		return &dynamicInstance{eng: e, art: art}, nil
	}
	return &instance{rt: e.rt, cm: cm}, nil
}

// Close releases the runtime, compiled modules, and the compilation cache.
func (e *engine) Close(ctx context.Context) error {
	err := e.rt.Close(ctx)
	if cerr := e.cache.Close(ctx); err == nil {
		err = cerr
	}
	return err
}

// importsPlugin reports whether a compiled module imports a function from the
// Javy plugin namespace — i.e. it is a small dynamically linked op rather than
// a self-contained module. Inspecting imports (not a substring scan) is the
// reliable probe: a self-contained javy build *embeds* the plugin, so the
// namespace string also appears in its custom sections, but only a linked
// module actually imports `<namespace>.invoke` et al.
func importsPlugin(cm wazero.CompiledModule) bool {
	for _, f := range cm.ImportedFunctions() {
		if m, _, ok := f.Import(); ok && m == javyplugin.Namespace {
			return true
		}
	}
	return false
}

// ---- self-contained modules ----

type instance struct {
	rt wazero.Runtime
	cm wazero.CompiledModule
}

// Invoke runs the module: input JSON on stdin, output JSON read from stdout.
// A nonzero lim.MaxWall bounds execution; exceeding it kills the guest.
func (i *instance) Invoke(ctx context.Context, input []byte, lim compute.Limits) ([]byte, error) {
	if len(input) == 0 {
		input = []byte("{}")
	}
	var stdout, stderr bytes.Buffer
	mc := moduleConfig(lim, input, &stdout, &stderr)

	rctx, cancel := withWall(ctx, lim)
	if cancel != nil {
		defer cancel()
	}

	start := time.Now()
	var memBytes uint32
	status := "ok"
	if lim.MetricsSink != nil {
		defer func() {
			lim.MetricsSink(compute.Metrics{
				WallMS:      time.Since(start).Milliseconds(),
				MemoryBytes: memBytes,
				Status:      status,
			})
		}()
	}

	mod, err := i.rt.InstantiateModule(rctx, i.cm, mc)
	if mod != nil {
		if m := mod.Memory(); m != nil {
			memBytes = m.Size()
		}
		defer func() { _ = mod.Close(ctx) }()
	}
	if err != nil {
		out, herr := handleRunError(err, rctx, ctx, lim, stderr.Bytes(), &status)
		if herr != nil {
			return nil, herr
		}
		_ = out
	}
	return readOut(stdout.Bytes()), nil
}

func (i *instance) Close(context.Context) error { return nil }

// ---- dynamically linked Javy modules ----
//
// A dynamically linked module is just the op's QuickJS bytecode; the engine
// lives in the shared plugin (chassis/compute/javyplugin). Running it:
//
//  1. Once (lazily): instantiate the plugin feeding the SharedConfig on stdin,
//     call initialize-runtime (turns the event loop on), and snapshot its full
//     linear memory. This freezes the configured QuickJS runtime.
//  2. Per call: spin up a fresh runtime (sharing the engine's compilation
//     cache, so the plugin doesn't recompile), instantiate the plugin WITHOUT
//     re-initializing, restore the snapshot into its memory, then instantiate
//     the op module. Its `_start` calls `plugin.invoke`, which runs the JS; the
//     SDK reads input from the plugin's stdin (fd 0) and writes output to its
//     stdout (fd 1). Restoring the snapshot — rather than re-running
//     initialize-runtime — leaves stdin free to carry the request input.
//
// A fresh per-call runtime gives each invocation an isolated module namespace,
// which is necessary because the op module imports the plugin by its fixed
// namespace name (one named plugin instance per runtime). Pooling runtimes is a
// possible future optimization; correctness does not depend on it.

type dynamicInstance struct {
	eng *engine
	art compute.Artifact
}

func (d *dynamicInstance) Close(context.Context) error { return nil }

func (d *dynamicInstance) Invoke(ctx context.Context, input []byte, lim compute.Limits) ([]byte, error) {
	if len(input) == 0 {
		input = []byte("{}")
	}
	snap, pages, err := d.eng.snapshot(ctx)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	var memBytes uint32
	status := "ok"
	if lim.MetricsSink != nil {
		defer func() {
			lim.MetricsSink(compute.Metrics{
				WallMS:      time.Since(start).Milliseconds(),
				MemoryBytes: memBytes,
				Status:      status,
			})
		}()
	}

	rt := wazero.NewRuntimeWithConfig(ctx, runtimeConfig(d.eng.cfg, d.eng.cache))
	defer rt.Close(ctx)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		status = "trapped"
		return nil, fmt.Errorf("wazero: instantiate WASI: %w", err)
	}

	pluginCM, err := rt.CompileModule(ctx, javyplugin.Bytes()) // cache hit after first build
	if err != nil {
		status = "trapped"
		return nil, fmt.Errorf("wazero: compile plugin: %w", err)
	}
	opCM, err := rt.CompileModule(ctx, d.art.Wasm)
	if err != nil {
		status = "trapped"
		return nil, fmt.Errorf("wazero: compile %s:%s: %w", d.art.Alg, d.art.Digest, err)
	}

	var stdout, stderr bytes.Buffer
	// The plugin instance executes the QuickJS engine, so the guest's stdio,
	// clock, and RNG belong to it. It must carry the plugin namespace as its
	// module name so the op module's imports resolve to it. Skip its start
	// functions: we restore the post-init snapshot instead of running
	// initialize-runtime (which would consume stdin, where the input must go).
	pluginCfg := moduleConfig(lim, input, &stdout, &stderr).
		WithName(javyplugin.Namespace).
		WithStartFunctions()
	plugin, err := rt.InstantiateModule(ctx, pluginCM, pluginCfg)
	if err != nil {
		status = "trapped"
		return nil, fmt.Errorf("wazero: instantiate plugin: %w", err)
	}
	defer func() { _ = plugin.Close(ctx) }()

	mem := plugin.Memory()
	if mem == nil {
		status = "trapped"
		return nil, errors.New("wazero: plugin has no memory")
	}
	if cur := mem.Size() / wasmPageBytes; cur < pages {
		if _, ok := mem.Grow(pages - cur); !ok {
			status = "trapped"
			return nil, fmt.Errorf("wazero: grow plugin memory to %d pages (limit too small?)", pages)
		}
	}
	if !mem.Write(0, snap) {
		status = "trapped"
		return nil, errors.New("wazero: restore plugin snapshot")
	}

	rctx, cancel := withWall(ctx, lim)
	if cancel != nil {
		defer cancel()
	}

	// Instantiating the op module runs its _start, which calls plugin.invoke.
	opCfg := wazero.NewModuleConfig().WithName("").WithArgs("compute").
		WithStdin(bytes.NewReader(nil)).WithStdout(io.Discard).WithStderr(io.Discard)
	opMod, err := rt.InstantiateModule(rctx, opCM, opCfg)
	if opMod != nil {
		defer func() { _ = opMod.Close(ctx) }()
	}
	if m := plugin.Memory(); m != nil {
		memBytes = m.Size()
	}
	if err != nil {
		if _, herr := handleRunError(err, rctx, ctx, lim, stderr.Bytes(), &status); herr != nil {
			return nil, herr
		}
	}
	return readOut(stdout.Bytes()), nil
}

// snapshot lazily initializes the plugin runtime once and returns the captured
// post-init linear memory (shared, read-only) plus its size in pages.
func (e *engine) snapshot(ctx context.Context) ([]byte, uint32, error) {
	e.dynOnce.Do(func() {
		e.dynSnap, e.dynPages, e.dynErr = buildSnapshot(ctx, e.cache)
	})
	return e.dynSnap, e.dynPages, e.dynErr
}

// buildSnapshot instantiates the plugin with the SharedConfig on stdin, runs
// initialize-runtime, and copies out its full linear memory. It uses its own
// runtime (no memory cap) since init must allocate the QuickJS heap regardless
// of per-op limits; the captured bytes are then restored per call.
func buildSnapshot(ctx context.Context, cache wazero.CompilationCache) ([]byte, uint32, error) {
	rt := wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfig().WithCompilationCache(cache))
	defer rt.Close(ctx)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, 0, fmt.Errorf("wazero: snapshot WASI: %w", err)
	}
	cm, err := rt.CompileModule(ctx, javyplugin.Bytes())
	if err != nil {
		return nil, 0, fmt.Errorf("wazero: compile plugin: %w", err)
	}
	var serr bytes.Buffer
	cfg := wazero.NewModuleConfig().
		WithName(javyplugin.Namespace).
		WithStdin(bytes.NewReader([]byte(javyplugin.ConfigJSON))).
		WithStderr(&serr).
		WithStartFunctions() // reactor: we drive initialize-runtime ourselves
	mod, err := rt.InstantiateModule(ctx, cm, cfg)
	if err != nil {
		return nil, 0, fmt.Errorf("wazero: instantiate plugin: %w", err)
	}
	defer func() { _ = mod.Close(ctx) }()
	if fn := mod.ExportedFunction("initialize-runtime"); fn != nil {
		if _, err := fn.Call(ctx); err != nil {
			return nil, 0, fmt.Errorf("wazero: initialize-runtime: %w (stderr=%q)", err, serr.String())
		}
	} else {
		return nil, 0, errors.New("wazero: plugin missing initialize-runtime export")
	}
	mem := mod.Memory()
	if mem == nil {
		return nil, 0, errors.New("wazero: plugin has no memory")
	}
	size := mem.Size()
	buf, ok := mem.Read(0, size)
	if !ok {
		return nil, 0, errors.New("wazero: read plugin memory")
	}
	snap := make([]byte, len(buf))
	copy(snap, buf)
	return snap, size / wasmPageBytes, nil
}

// ---- shared helpers ----

// moduleConfig builds the restricted-WASI module config: anonymous name,
// deterministic clock/RNG (host monotonic excepted), input on stdin, output on
// stdout, and the guest's diagnostics teed to lim.Stderr when provided. Used
// for self-contained modules and for the plugin instance of a dynamically
// linked op (which is where its QuickJS guest actually runs).
func moduleConfig(lim compute.Limits, input []byte, stdout, stderr *bytes.Buffer) wazero.ModuleConfig {
	walltime := frozenWalltime
	if !lim.Now.IsZero() {
		sec, nsec := lim.Now.Unix(), int32(lim.Now.Nanosecond())
		walltime = func() (int64, int32) { return sec, nsec }
	}
	var stderrW io.Writer = stderr
	if lim.Stderr != nil {
		stderrW = io.MultiWriter(stderr, lim.Stderr)
	}
	rng := &detReader{state: 0x123456789abcdef}
	return wazero.NewModuleConfig().
		WithName("").
		WithArgs("compute").
		WithStdin(bytes.NewReader(input)).
		WithStdout(stdout).
		WithStderr(stderrW).
		WithRandSource(rng).
		WithWalltime(walltime, sys.ClockResolution(1)).
		WithSysNanotime()
	// No WithFS / WithEnv: filesystem, env, network stay absent.
}

// withWall returns a context bounded by lim.MaxWall (and its cancel), or the
// input context and a nil cancel when no wall limit is set.
func withWall(ctx context.Context, lim compute.Limits) (context.Context, context.CancelFunc) {
	if lim.MaxWall > 0 {
		return context.WithTimeout(ctx, lim.MaxWall)
	}
	return ctx, nil
}

// handleRunError classifies an InstantiateModule error shared by both run
// paths. A clean proc_exit(0) is success (returns nil, nil). A fired wall
// deadline is "wall-limit". Otherwise it's a guest trap: surface the guest's
// stderr (the real JS/runtime error) when present, else the wasm trap. It
// updates *status for metrics.
func handleRunError(err error, rctx, ctx context.Context, lim compute.Limits, guestStderr []byte, status *string) ([]byte, error) {
	var ee *sys.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 0:
		return nil, nil // clean exit; caller reads stdout
	case rctx.Err() != nil && ctx.Err() == nil:
		*status = "wall-limit"
		return nil, fmt.Errorf("compute: wall-clock limit exceeded after %s", lim.MaxWall)
	default:
		*status = "trapped"
		if s := trim(guestStderr); len(s) > 0 {
			return nil, fmt.Errorf("compute: guest error: %s", s)
		}
		return nil, fmt.Errorf("compute: guest trapped: %w", err)
	}
}

// readOut normalizes a guest's stdout into the returned bytes: empty ⇒ "{}",
// otherwise a defensive copy (the buffer is reused/freed after the call).
func readOut(out []byte) []byte {
	if len(out) == 0 {
		return []byte("{}")
	}
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp
}

// frozenWalltime makes the guest's wall clock deterministic — a fixed epoch,
// so time-of-day reads (e.g. JS Date.now) are constant and reproducible.
//
// Monotonic time (nanotime) is deliberately NOT frozen: real guest runtimes
// (the Go wasip1 runtime, and JS engines) require an advancing monotonic clock
// for scheduling/GC and will fatally error on a zero/non-advancing one. We use
// the host monotonic clock (WithSysNanotime). The residual leak — a guest can
// measure elapsed time — is a minor, accepted timing channel in v1; it carries
// no wall-clock, entropy, filesystem, env, or network authority.
func frozenWalltime() (sec int64, nsec int32) { return 0, 0 }

// detReader is an infinite, deterministic byte source (splitmix64) feeding
// WASI random_get. Fixed seed ⇒ reproducible; not cryptographic — a sandboxed
// pure transform has no business needing real entropy in v1.
type detReader struct{ state uint64 }

func (d *detReader) Read(p []byte) (int, error) {
	for i := range p {
		d.state += 0x9e3779b97f4a7c15
		z := d.state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		p[i] = byte(z)
	}
	return len(p), nil
}

func trim(b []byte) []byte {
	const max = 512
	if len(b) > max {
		return b[:max]
	}
	return b
}
