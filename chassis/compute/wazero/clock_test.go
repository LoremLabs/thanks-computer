package wazero

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/compute"
)

// TestRequestTimeSeedsClock: when Limits.Now is set (from the envelope _ts),
// the guest's Date.now() reflects that moment; when unset, the clock is frozen.
func TestRequestTimeSeedsClock(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	dir := t.TempDir()
	js := `function w(v){const b=new TextEncoder().encode(JSON.stringify(v));Javy.IO.writeSync(1,new Uint8Array(b));}
w({now: Date.now()});`
	src := filepath.Join(dir, "c.js")
	out := filepath.Join(dir, "c.wasm")
	if err := os.WriteFile(src, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("javy", "build", src, "-o", out).CombinedOutput(); err != nil {
		t.Fatalf("javy: %v\n%s", err, b)
	}
	wasm, _ := os.ReadFile(out)

	e := newEngine(t)
	inst, _ := e.Load(context.Background(), compute.Artifact{Alg: "sha256", Digest: "clk", Engine: Name, Wasm: wasm})
	defer inst.Close(context.Background())

	want := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	res, err := inst.Invoke(context.Background(), []byte(`{}`), compute.Limits{MaxWall: 5 * time.Second, Now: want})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := gjson.GetBytes(res, "now").Int(); got != want.UnixMilli() {
		t.Fatalf("Date.now() = %d, want %d (%s)", got, want.UnixMilli(), want)
	}

	// Unset Now → frozen epoch.
	res2, err := inst.Invoke(context.Background(), []byte(`{}`), compute.Limits{MaxWall: 5 * time.Second})
	if err != nil {
		t.Fatalf("invoke2: %v", err)
	}
	if got := gjson.GetBytes(res2, "now").Int(); got != 0 {
		t.Fatalf("frozen Date.now() = %d, want 0", got)
	}
}
