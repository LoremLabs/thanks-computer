package op

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSDKCryptoMatchesGo: @txco/op/crypto's pure-JS sha256 and hmac, run in the
// real sandbox, agree with Go's crypto/sha256 and crypto/hmac on the padding
// boundaries, UTF-8, an empty input, a large input, and keys shorter than,
// equal to and longer than the 64-byte block. Guards the QuickJS-tuned
// implementation: one wrong rotation passes every smoke test and corrupts
// every msgkey, campaign and signature silently.
func TestSDKCryptoMatchesGo(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
import { sha256, hmac } from "@txco/op/crypto";
export default op(({ input }) => ({
  sha: input.msgs.map((m) => sha256(m)),
  mac: input.keys.map((k) => hmac(k, input.msgs[1])),
}));`)

	r := rand.New(rand.NewSource(7))
	alphabet := []rune("abcdefghij klmnop\n=ÄéΩ🙂")
	var big strings.Builder
	for i := 0; i < 40000; i++ {
		big.WriteRune(alphabet[r.Intn(len(alphabet))])
	}
	msgs := []string{
		"", "abc",
		strings.Repeat("a", 55), strings.Repeat("a", 56), strings.Repeat("a", 63), strings.Repeat("a", 64),
		strings.Repeat("a", 119), strings.Repeat("a", 120),
		"héllo wörld — Ω 🙂", "=== POLICIES · 885fdf76f055 ===\n{{x}}",
		big.String(),
	}
	keys := []string{"k", strings.Repeat("k", 64), strings.Repeat("key!", 30)}

	req, err := json.Marshal(map[string]any{"msgs": msgs, "keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("OPS/site/100/mock-request.json", req, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runTest([]string{"OPS/site/100/hello.txcl"}, &out, &errb); code != 0 {
		t.Fatalf("test code=%d stderr=%s", code, errb.String())
	}
	line := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	var got struct {
		Sha []string `json:"sha"`
		Mac []string `json:"mac"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if len(got.Sha) != len(msgs) || len(got.Mac) != len(keys) {
		t.Fatalf("got %d digests and %d macs, want %d and %d", len(got.Sha), len(got.Mac), len(msgs), len(keys))
	}
	for i, m := range msgs {
		sum := sha256.Sum256([]byte(m))
		if want := hex.EncodeToString(sum[:]); got.Sha[i] != want {
			t.Errorf("sha256 vector %d (%d bytes): got %s, want %s", i, len(m), got.Sha[i], want)
		}
	}
	for i, k := range keys {
		mac := hmac.New(sha256.New, []byte(k))
		mac.Write([]byte(msgs[1]))
		if want := hex.EncodeToString(mac.Sum(nil)); got.Mac[i] != want {
			t.Errorf("hmac key %d (%d bytes): got %s, want %s", i, len(k), got.Mac[i], want)
		}
	}
}
