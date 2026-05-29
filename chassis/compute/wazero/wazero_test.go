package wazero

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/compute"
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
