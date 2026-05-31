package cli

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// workspaceTrust returns the workspace's trusted signing keys, or an empty
// config when there's no txco.yaml. Mirrors workspaceRegistry (pkgref.go).
func workspaceTrust(dir string) trustConfig {
	if cfg := loadWorkspaceConfig(dir); cfg != nil {
		return cfg.Trust
	}
	return trustConfig{}
}

// loadTrustedKeys assembles the trusted-key set from the workspace `trust:`
// block plus any --key flags (each a pubkey, a .pub path, or base64). A
// malformed config key is skipped with a warning (one bad line shouldn't brick
// trust); a malformed --key flag is a hard error (the user typed it). Deduped
// by key id.
func loadTrustedKeys(root string, keyFlags []string, stderr io.Writer) ([]sign.TrustedKey, error) {
	seen := map[string]bool{}
	var out []sign.TrustedKey
	add := func(name, registry string, pub ed25519.PublicKey) {
		id := sign.KeyIDForPub(pub)
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, sign.TrustedKey{Name: name, Pub: pub, KeyID: id, Registry: registry})
	}
	for _, k := range workspaceTrust(root).Keys {
		pub, err := sign.ParseTrustedKey(k.Pubkey)
		if err != nil {
			fmt.Fprintf(stderr, "warning: skipping trusted key %q: %v\n", k.Name, err)
			continue
		}
		add(k.Name, k.Registry, pub)
	}
	for _, f := range keyFlags {
		pub, err := sign.ParseTrustedKey(f)
		if err != nil {
			return nil, fmt.Errorf("--key %q: %w", f, err)
		}
		add("", "", pub)
	}
	return out, nil
}

// verifyPackageSignature fetches + verifies the signature for an OCI-sourced
// package. dir:/github: sources are inherently unsigned here (blank digest).
func verifyPackageSignature(prov source.Provenance, trusted []sign.TrustedKey) (sign.Verdict, error) {
	if prov.Digest == "" {
		return sign.Verdict{Reason: "source is not an OCI registry (no signature)"}, nil
	}
	ref := source.ParsedRef{Registry: prov.Registry, Namespace: prov.Namespace, Name: prov.Name}
	man, layer, ann, found, err := source.FetchSignature(context.Background(), ref, sign.DigestToSigTag(prov.Digest))
	if err != nil {
		return sign.Verdict{}, err
	}
	if !found {
		return sign.Verdict{Reason: "no signature found"}, nil
	}
	return sign.VerifyArtifact(man, layer, ann, prov.Digest, ref.Repository(), prov.Registry, trusted), nil
}

// enforceSignaturePosture prints the verdict and decides whether to proceed.
// With requireSig, anything short of signed+trusted fails (proceed=false);
// without it, an unverified package only warns. Verified lines go to stdout;
// warnings + failures to stderr.
func enforceSignaturePosture(v sign.Verdict, requireSig bool, stdout, stderr io.Writer) bool {
	switch {
	case v.Signed && v.Trusted:
		fmt.Fprintf(stdout, "  verified: signed by %s\n", v.KeyID)
		return true
	case requireSig:
		fmt.Fprintf(stderr, "  signature required, but %s\n", verdictReason(v))
		return false
	default:
		fmt.Fprintf(stderr, "  warning: %s (use --require-signature to enforce)\n", verdictReason(v))
		return true
	}
}

func verdictReason(v sign.Verdict) string {
	if v.Reason != "" {
		return v.Reason
	}
	if v.Signed {
		return "signed but not trusted"
	}
	return "unsigned"
}
