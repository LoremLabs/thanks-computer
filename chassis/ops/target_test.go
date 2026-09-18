package ops

import (
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestVerdictPathTargets — the auth helpers write their own verdict at an
// author-chosen `output_path`. Their documented home (`_txc.computed.*`) and
// the author's own keys keep working; any other reserved `_txc` path is
// refused loud, in every spelling sjson resolves to it. Same trusted-transport
// reasoning as TestCopyRefusesReservedTarget.
func TestVerdictPathTargets(t *testing.T) {
	key := []byte("whsec_test")
	signed := "1690000000.body"
	in := verifyInput(t, signed, hexHMAC256(key, signed))

	allowed := map[string]string{ // output_path → where the verdict must land
		"_txc.computed.stripe_sig": "_txc.computed.stripe_sig",
		"@computed.stripe_sig":     "_txc.computed.stripe_sig",
		".auth.sig_ok":             "auth.sig_ok",
		"sig_ok":                   "sig_ok",
	}
	for outputPath, probe := range allowed {
		meta, _ := sjson.Set(verifyMeta, "output_path", outputPath)
		out, err := HMACVerify(withBagAndMeta(t, "WHSEC", key, meta), "txco://hmac-verify", in, nil)
		if err != nil {
			t.Errorf("output_path=%q: %v", outputPath, err)
			continue
		}
		if !gjson.Get(out.Raw, probe).Bool() {
			t.Errorf("output_path=%q: verdict not at %s: %s", outputPath, probe, out.Raw)
		}
	}

	for _, outputPath := range []string{
		"_txc.tenant", "@tenant", "@imap.account", "@principal", "@route.to",
		`\_txc.tenant`, ":_txc.tenant", "_txc", "@computedx.ok", `@computed.ok\`,
	} {
		meta, _ := sjson.Set(verifyMeta, "output_path", outputPath)
		out, err := HMACVerify(withBagAndMeta(t, "WHSEC", key, meta), "txco://hmac-verify", in, nil)
		if err == nil {
			t.Errorf("hmac-verify output_path=%q: want an error, got %s", outputPath, out.Raw)
		}
		if gjson.Get(out.Raw, "_txc").Exists() {
			t.Errorf("hmac-verify output_path=%q: refused op still wrote under _txc: %s", outputPath, out.Raw)
		}

		smeta, _ := sjson.Set(`{"secrets":{"key":{"secret":"WHSEC"}},"input_path":"signed"}`, "output_path", outputPath)
		if out, err := HMACSign(withBagAndMeta(t, "WHSEC", key, smeta), "txco://hmac-sign", in, nil); err == nil {
			t.Errorf("hmac-sign output_path=%q: want an error, got %s", outputPath, out.Raw)
		}

		emeta, _ := sjson.Set(`{"secrets":{"password":{"secret":"WHSEC"}},"user":"u"}`, "output_path", outputPath)
		if out, err := BasicAuthEncode(withBagAndMeta(t, "WHSEC", key, emeta), "txco://basic-auth-encode", in, nil); err == nil {
			t.Errorf("basic-auth-encode output_path=%q: want an error, got %s", outputPath, out.Raw)
		}

		for _, param := range []string{"output_path", "configured_path"} {
			vmeta, _ := sjson.Set(`{"secrets":{"password":{"secret":"WHSEC"}},"user":"u"}`, param, outputPath)
			if out, err := BasicAuthVerify(withBagAndMeta(t, "WHSEC", key, vmeta), "txco://basic-auth-verify", in, nil); err == nil {
				t.Errorf("basic-auth-verify %s=%q: want an error, got %s", param, outputPath, out.Raw)
			}
		}
	}
}
