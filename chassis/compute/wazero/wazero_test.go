package wazero

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/javyplugin"
)

// fixtureCache holds compiled .wasm bytes keyed by fixture name, so each is
// cross-built (GOOS=wasip1) at most once per test process.
var (
	fixtureMu    sync.Mutex
	fixtureCache = map[string][]byte{}
)

// buildFixture cross-compiles testdata/src/<name> to a WASI module and returns
// its bytes. Uses only the Go toolchain (always present for `go test`); no
// external wasm toolchain required.
func buildFixture(t *testing.T, name string) []byte {
	t.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if b, ok := fixtureCache[name]; ok {
		return b
	}
	out := filepath.Join(t.TempDir(), name+".wasm")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/src/"+name)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture %q: %v\n%s", name, err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	fixtureCache[name] = data
	return data
}

func newEngine(t *testing.T) compute.Engine {
	t.Helper()
	e, err := New(compute.EngineConfig{MaxMemoryMB: 64})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.(interface{ Close(context.Context) error }).Close(context.Background()) })
	return e
}

func TestRegisteredViaInit(t *testing.T) {
	if _, err := compute.OpenEngine(Name, compute.EngineConfig{}); err != nil {
		t.Fatalf("wazero engine not registered: %v", err)
	}
}

func TestTransformRoundTrip(t *testing.T) {
	e := newEngine(t)
	art := compute.Artifact{Alg: "sha256", Digest: "transform", Engine: Name, Wasm: buildFixture(t, "transform")}
	inst, err := e.Load(context.Background(), art)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer inst.Close(context.Background())

	out, err := inst.Invoke(context.Background(), []byte(`{"in":1}`), compute.Limits{MaxWall: 5 * time.Second})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !gjson.GetBytes(out, "computed").Bool() {
		t.Fatalf("output missing computed:true: %s", out)
	}
	if gjson.GetBytes(out, "in").Int() != 1 {
		t.Fatalf("output dropped input field: %s", out)
	}
}

func TestMetricsSinkFires(t *testing.T) {
	e := newEngine(t)
	art := compute.Artifact{Alg: "sha256", Digest: "transform", Engine: Name, Wasm: buildFixture(t, "transform")}
	inst, err := e.Load(context.Background(), art)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer inst.Close(context.Background())

	var got compute.Metrics
	var fired bool
	_, err = inst.Invoke(context.Background(), []byte(`{"in":1}`), compute.Limits{
		MaxWall:     5 * time.Second,
		MetricsSink: func(m compute.Metrics) { got = m; fired = true },
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fired {
		t.Fatal("metrics sink not called")
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	if got.MemoryBytes == 0 {
		t.Fatalf("memory_bytes = 0, want > 0 (the guest allocated memory)")
	}
}

func TestCompiledModuleCached(t *testing.T) {
	e := newEngine(t).(*engine)
	art := compute.Artifact{Alg: "sha256", Digest: "transform", Engine: Name, Wasm: buildFixture(t, "transform")}
	if _, err := e.Load(context.Background(), art); err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	if _, err := e.Load(context.Background(), art); err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	e.mu.Lock()
	n := len(e.cmods)
	e.mu.Unlock()
	if n != 1 {
		t.Fatalf("compiled-module cache has %d entries, want 1 (same digest reused)", n)
	}
}

// TestWallClockLimitKillsRunaway is the decisive sandbox test: an infinite
// loop must be terminated at the wall-clock limit, not run forever.
func TestWallClockLimitKillsRunaway(t *testing.T) {
	e := newEngine(t)
	art := compute.Artifact{Alg: "sha256", Digest: "loop", Engine: Name, Wasm: buildFixture(t, "loop")}
	inst, err := e.Load(context.Background(), art)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer inst.Close(context.Background())

	start := time.Now()
	_, err = inst.Invoke(context.Background(), []byte(`{}`), compute.Limits{MaxWall: 200 * time.Millisecond})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runaway guest returned no error; wall-clock limit did not fire")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("guest ran %s before being killed; preemption too slow/ineffective", elapsed)
	}
}

// TestJSFixtureOnWazero is the JS-on-wazero spike, now realized: it loads a
// committed QuickJS/Javy-compiled WASI module (testdata/src/js/transform.js,
// built with `javy build`) and asserts it round-trips on the same engine and
// restricted-WASI config as the native-wasm fixtures. This proves JS is
// first-class on the in-process engine — QuickJS boots under the frozen
// walltime + fixed-seed RNG and speaks the stdin→stdout JSON ABI via Javy IO.
// The .wasm is committed so the test is hermetic (no javy needed in CI);
// regenerate with: javy build testdata/src/js/transform.js -o testdata/src/js/transform.js.wasm
func TestJSFixtureOnWazero(t *testing.T) {
	wasm, err := os.ReadFile("testdata/src/js/transform.js.wasm")
	if err != nil {
		t.Fatalf("read JS fixture (regenerate with `javy build`): %v", err)
	}
	e := newEngine(t)
	art := compute.Artifact{Alg: "sha256", Digest: "js-transform", Engine: Name, Wasm: wasm}
	inst, err := e.Load(context.Background(), art)
	if err != nil {
		t.Fatalf("Load JS: %v", err)
	}
	defer inst.Close(context.Background())

	out, err := inst.Invoke(context.Background(), []byte(`{"in":42}`), compute.Limits{MaxWall: 5 * time.Second})
	if err != nil {
		t.Fatalf("Invoke JS: %v", err)
	}
	if !gjson.GetBytes(out, "computed").Bool() {
		t.Fatalf("JS output missing computed:true: %s", out)
	}
	if gjson.GetBytes(out, "lang").String() != "js" {
		t.Fatalf("JS output missing lang:js: %s", out)
	}
	if gjson.GetBytes(out, "in").Int() != 42 {
		t.Fatalf("JS compute dropped input field: %s", out)
	}
}

// legacyNamespace is the namespace of the plugin earlier chassis versions
// built against; linked.js.wasm imports it, so the linked-op tests below also
// cover an op that has not been rebuilt since the plugin changed.
const legacyNamespace = "javy-default-plugin-v3"

// linkedFixture loads the committed dynamically linked JS fixture
// (testdata/src/js/linked.js, built against the legacy plugin) on e, checking
// it takes the linked path against that plugin.
func linkedFixture(t *testing.T, e *engine) compute.Instance {
	t.Helper()
	return dynamicFixture(t, e, "linked", legacyNamespace)
}

// dynamicFixture loads testdata/src/js/<name>.js.wasm on e and checks it is
// a dynamically linked op importing the plugin namespace ns.
func dynamicFixture(t *testing.T, e *engine, name, ns string) compute.Instance {
	t.Helper()
	wasm, err := os.ReadFile("testdata/src/js/" + name + ".js.wasm")
	if err != nil {
		t.Fatalf("read %s fixture (regenerate with `javy build … -C dynamic`): %v", name, err)
	}
	inst, err := e.Load(context.Background(), compute.Artifact{Alg: "sha256", Digest: "js-" + name, Engine: Name, Wasm: wasm})
	if err != nil {
		t.Fatalf("Load %s: %v", name, err)
	}
	d, ok := inst.(*dynamicInstance)
	if !ok {
		t.Fatalf("%s fixture loaded as %T, want *dynamicInstance", name, inst)
	}
	if d.ps.plugin.Namespace != ns {
		t.Fatalf("%s fixture linked against %q, want %q", name, d.ps.plugin.Namespace, ns)
	}
	return inst
}

// idleLinkers counts the idle linkers across every plugin's pool.
func idleLinkers(e *engine) int {
	e.dynMu.Lock()
	defer e.dynMu.Unlock()
	n := 0
	for _, ps := range e.dyn {
		ps.linkMu.Lock()
		n += len(ps.linkers)
		ps.linkMu.Unlock()
	}
	return n
}

// TestLinkedOpReusesLinker: calls in a row share one linker, and each still
// starts from a fresh plugin instance (the guest's global counter reads 1).
func TestLinkedOpReusesLinker(t *testing.T) {
	e := newEngine(t).(*engine)
	inst := linkedFixture(t, e)
	lim := compute.Limits{MaxWall: 5 * time.Second}
	for i := 1; i <= 3; i++ {
		out, err := inst.Invoke(context.Background(), []byte(fmt.Sprintf(`{"in":%d}`, i)), lim)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if gjson.GetBytes(out, "in").Int() != int64(i) || !gjson.GetBytes(out, "linked").Bool() {
			t.Fatalf("call %d: output %s", i, out)
		}
		if runs := gjson.GetBytes(out, "runs").Int(); runs != 1 {
			t.Fatalf("call %d: runs = %d, want 1 (state carried over from an earlier call)", i, runs)
		}
		if n := idleLinkers(e); n != 1 {
			t.Fatalf("after call %d: %d idle linkers, want 1", i, n)
		}
	}
}

// TestLinkedOpFailureDropsLinker: a linker whose call failed is closed, not
// kept, and the next call runs clean on a new one.
func TestLinkedOpFailureDropsLinker(t *testing.T) {
	e := newEngine(t).(*engine)
	inst := linkedFixture(t, e)
	lim := compute.Limits{MaxWall: 5 * time.Second}

	if _, err := inst.Invoke(context.Background(), []byte(`{"throw":true}`), lim); err == nil {
		t.Fatal("throwing guest returned no error")
	}
	if n := idleLinkers(e); n != 0 {
		t.Fatalf("after a guest error: %d idle linkers, want 0", n)
	}
	if _, err := inst.Invoke(context.Background(), []byte(`{"spin":true}`), compute.Limits{MaxWall: 200 * time.Millisecond}); err == nil {
		t.Fatal("spinning guest returned no error; wall-clock limit did not fire")
	}
	if n := idleLinkers(e); n != 0 {
		t.Fatalf("after a wall-limit kill: %d idle linkers, want 0", n)
	}
	out, err := inst.Invoke(context.Background(), []byte(`{"in":7}`), lim)
	if err != nil || gjson.GetBytes(out, "in").Int() != 7 || gjson.GetBytes(out, "runs").Int() != 1 {
		t.Fatalf("call after failures: out=%s err=%v", out, err)
	}
	if n := idleLinkers(e); n != 1 {
		t.Fatalf("after a clean call: %d idle linkers, want 1", n)
	}
}

// TestLinkedOpConcurrent: calls in flight together each get a linker of
// their own, and the pool keeps no more than maxIdleLinkers afterwards.
func TestLinkedOpConcurrent(t *testing.T) {
	e := newEngine(t).(*engine)
	inst := linkedFixture(t, e)
	const n = 3 * maxIdleLinkers
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := inst.Invoke(context.Background(), []byte(fmt.Sprintf(`{"in":%d}`, i)), compute.Limits{MaxWall: 10 * time.Second})
			if err == nil && (gjson.GetBytes(out, "in").Int() != int64(i) || gjson.GetBytes(out, "runs").Int() != 1) {
				err = fmt.Errorf("call %d: output %s", i, out)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := idleLinkers(e); got < 1 || got > maxIdleLinkers {
		t.Fatalf("%d idle linkers, want 1..%d", got, maxIdleLinkers)
	}
}

// TestCloseDropsLinkers: Close releases the idle linkers, and a linker handed
// back afterwards isn't kept.
func TestCloseDropsLinkers(t *testing.T) {
	e := newEngine(t).(*engine)
	inst := linkedFixture(t, e)
	if _, err := inst.Invoke(context.Background(), []byte(`{}`), compute.Limits{MaxWall: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	ps := inst.(*dynamicInstance).ps
	l, err := e.takeLinker(context.Background(), ps)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.giveLinker(context.Background(), ps, l, true)
	if n := idleLinkers(e); n != 0 {
		t.Fatalf("after Close: %d idle linkers, want 0", n)
	}
}

// A cancelled caller must not poison the plugin snapshot: the build runs
// under a context of its own, and a failed build is tried again. (On prod,
// 2026-10-27, the first nano-op on a fresh node ran under a cancelled
// request and every nano-op on that node then failed until a restart.)
func TestSnapshotSurvivesCancelledCaller(t *testing.T) {
	e := newEngine(t).(*engine)
	ps, err := e.pluginState(javyplugin.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	snap, pages, err := e.snapshot(cancelled, ps)
	if err != nil || len(snap) == 0 || pages == 0 {
		t.Fatalf("snapshot under a cancelled caller: %d bytes, %d pages, %v", len(snap), pages, err)
	}
	again, _, err := e.snapshot(context.Background(), ps)
	if err != nil || len(again) != len(snap) {
		t.Fatalf("second snapshot: %d bytes, %v (want the first, kept)", len(again), err)
	}
}

// TestNativeCryptoFixture: an op built against the current plugin reaches
// the plugin's natives, and they agree with Go's crypto — on a plain
// Uint8Array, on a view with an offset, and for HMAC — while refusing what
// is not bytes.
func TestNativeCryptoFixture(t *testing.T) {
	e := newEngine(t).(*engine)
	inst := dynamicFixture(t, e, "native", javyplugin.Namespace)
	msg := "héllo wörld — Ω 🙂 " + strings.Repeat("x", 1000)
	key := strings.Repeat("key!", 30) // longer than the 64-byte block
	in := fmt.Sprintf(`{"msg":%q,"key":%q}`, msg, key)
	out, err := inst.Invoke(context.Background(), []byte(in), compute.Limits{MaxWall: 5 * time.Second})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := gjson.GetBytes(out, "native").String(); got != "object" {
		t.Fatalf("__txco_native is %q, want object: %s", got, out)
	}
	if got := gjson.GetBytes(out, "version").Int(); got != 1 {
		t.Fatalf("__txco_native.version = %d, want 1", got)
	}
	sum := sha256.Sum256([]byte(msg))
	if want := hex.EncodeToString(sum[:]); gjson.GetBytes(out, "sha").String() != want {
		t.Errorf("sha256: got %s, want %s", gjson.GetBytes(out, "sha").String(), want)
	}
	if want := hex.EncodeToString(sum[:]); gjson.GetBytes(out, "shaView").String() != want {
		t.Errorf("sha256 of a view: got %s, want %s (offset/length not honoured)", gjson.GetBytes(out, "shaView").String(), want)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	if want := hex.EncodeToString(mac.Sum(nil)); gjson.GetBytes(out, "mac").String() != want {
		t.Errorf("hmac: got %s, want %s", gjson.GetBytes(out, "mac").String(), want)
	}
	if got := gjson.GetBytes(out, "notBytes").String(); !strings.Contains(got, "Error") {
		t.Errorf("sha256(string) did not throw: %q", got)
	}
}

// TestBothPluginsServeTogether: an op built against the legacy plugin and one
// built against the current plugin run on the same engine, each on its own
// plugin's linkers.
func TestBothPluginsServeTogether(t *testing.T) {
	e := newEngine(t).(*engine)
	legacy := linkedFixture(t, e)
	current := dynamicFixture(t, e, "native", javyplugin.Namespace)
	lim := compute.Limits{MaxWall: 5 * time.Second}
	for i := 0; i < 2; i++ {
		out, err := legacy.Invoke(context.Background(), []byte(`{"in":1}`), lim)
		if err != nil || !gjson.GetBytes(out, "linked").Bool() {
			t.Fatalf("legacy op: out=%s err=%v", out, err)
		}
		out, err = current.Invoke(context.Background(), []byte(`{"msg":"abc"}`), lim)
		if err != nil || gjson.GetBytes(out, "sha").String() != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
			t.Fatalf("current op: out=%s err=%v", out, err)
		}
	}
	e.dynMu.Lock()
	n := len(e.dyn)
	e.dynMu.Unlock()
	if n != 2 {
		t.Fatalf("%d plugin states, want 2", n)
	}
	if got := idleLinkers(e); got != 2 {
		t.Fatalf("%d idle linkers, want 2 (one per plugin)", got)
	}
}

// TestUnknownPluginNamespaceIsClear: a module importing a plugin this chassis
// does not vendor (built by a newer txco, say) fails at Load with an error
// that names the namespace and the fix, instead of a bare link error later.
func TestUnknownPluginNamespaceIsClear(t *testing.T) {
	wasm, err := os.ReadFile("testdata/src/js/linked.js.wasm")
	if err != nil {
		t.Fatal(err)
	}
	// Same length as the legacy namespace, so the import's length prefix holds.
	other := []byte("javy-default-plugin-v9")
	if len(other) != len(legacyNamespace) {
		t.Fatal("test namespace must match the legacy one in length")
	}
	wasm = bytes.ReplaceAll(wasm, []byte(legacyNamespace), other)
	e := newEngine(t)
	_, err = e.Load(context.Background(), compute.Artifact{Alg: "sha256", Digest: "js-future", Engine: Name, Wasm: wasm})
	if err == nil {
		t.Fatal("Load accepted a module importing an unknown plugin")
	}
	for _, want := range []string{string(other), javyplugin.Namespace, "txco apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
