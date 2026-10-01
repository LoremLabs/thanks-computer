package sign

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/ssh"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"strings"
)

func TestDigestToSigTag(t *testing.T) {
	if got := DigestToSigTag("sha256:abc123"); got != "sha256-abc123.sig" {
		t.Errorf("DigestToSigTag = %q", got)
	}
}

func newTestSigner(t *testing.T) Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil) // nil → crypto/rand
	if err != nil {
		t.Fatal(err)
	}
	return NewSigner(priv)
}

// signInto signs (digest, repo) into a fresh in-memory store.
func signInto(t *testing.T, digest, repo, spec string, s Signer) oras.Target {
	t.Helper()
	store := memory.New()
	if _, err := SignArtifact(context.Background(), store, digest, repo, spec, "2026-05-31T00:00:00Z", s); err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	return store
}

// fetchSig pulls the signature artifact for digest back out of store, mirroring
// what source.FetchSignature does in production.
func fetchSig(t *testing.T, store oras.Target, digest string) (man, layer []byte, ann map[string]string) {
	t.Helper()
	ctx := context.Background()
	desc, err := store.Resolve(ctx, DigestToSigTag(digest))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if man, err = content.FetchAll(ctx, store, desc); err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(man, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("want 1 layer, got %d", len(m.Layers))
	}
	if layer, err = content.FetchAll(ctx, store, m.Layers[0]); err != nil {
		t.Fatalf("fetch layer: %v", err)
	}
	ann = map[string]string{}
	for k, v := range m.Layers[0].Annotations {
		ann[k] = v
	}
	for k, v := range m.Annotations {
		ann[k] = v
	}
	return man, layer, ann
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	const digest = "sha256:deadbeef"
	const repo = "registry.thanks.computer/txco/support-basic"
	man, layer, ann := fetchSig(t, signInto(t, digest, repo, "oci://"+repo+":0.1.0", s), digest)

	v := VerifyArtifact(man, layer, ann, digest, repo, "registry.thanks.computer",
		[]TrustedKey{{Name: "me", Pub: s.Pub, KeyID: s.KeyID}})
	if !v.Signed || !v.Trusted {
		t.Fatalf("want signed+trusted, got %+v", v)
	}
	if v.KeyID != s.KeyID {
		t.Errorf("keyid %q != %q", v.KeyID, s.KeyID)
	}
}

func TestVerifyRegistryScopedKey(t *testing.T) {
	s := newTestSigner(t)
	const digest, repo = "sha256:aa", "registry.thanks.computer/txco/x"
	man, layer, ann := fetchSig(t, signInto(t, digest, repo, "", s), digest)
	// Key scoped to a DIFFERENT registry → not trusted here.
	v := VerifyArtifact(man, layer, ann, digest, repo, "registry.thanks.computer",
		[]TrustedKey{{Pub: s.Pub, KeyID: s.KeyID, Registry: "ghcr.io"}})
	if !v.Signed || v.Trusted {
		t.Fatalf("registry-scoped key must not match a different host: %+v", v)
	}
	// Same key, scoped to the matching host → trusted.
	v = VerifyArtifact(man, layer, ann, digest, repo, "registry.thanks.computer",
		[]TrustedKey{{Pub: s.Pub, KeyID: s.KeyID, Registry: "registry.thanks.computer"}})
	if !v.Trusted {
		t.Fatalf("matching-host key should be trusted: %+v", v)
	}
}

// A key scoped to a namespace vouches for that namespace's repositories and
// no others on the same registry.
func TestVerifyNamespaceScopedKey(t *testing.T) {
	s := newTestSigner(t)
	const host, digest = "registry.thanks.computer", "sha256:aa"
	key := func(ns ...string) []TrustedKey {
		return []TrustedKey{{Name: "onepony", Pub: s.Pub, KeyID: s.KeyID, Registry: host, Namespaces: ns}}
	}
	verify := func(repo string, trusted []TrustedKey) Verdict {
		man, layer, ann := fetchSig(t, signInto(t, digest, repo, "", s), digest)
		return VerifyArtifact(man, layer, ann, digest, repo, host, trusted)
	}
	for _, c := range []struct {
		repo string
		ns   []string
		want bool
	}{
		{host + "/onepony/pony-agent", []string{"onepony"}, true},
		{host + "/onepony/tools/pony-web", []string{"onepony"}, true},       // nested under the namespace
		{host + "/onepony/pony-agent", []string{"txco", "onepony"}, true},   // any of several
		{host + "/onepony/pony-agent", []string{" /onepony/ "}, true},       // slashes and space are trimmed
		{host + "/onepony/tools/pony-web", []string{"onepony/tools"}, true}, // a deeper prefix
		{host + "/txco/hello-world", []string{"onepony"}, false},            // another namespace
		{host + "/oneponyx/pony-agent", []string{"onepony"}, false},         // a prefix of a segment is not the namespace
		{host + "/onepony", []string{"onepony"}, false},                     // the namespace itself is not a repository in it
		{host + "/onepony/pony-agent", []string{""}, false},                 // an empty entry admits nothing
		{host + "/onepony/pony-agent", nil, true},                           // unscoped: every namespace
		{host + "/txco/hello-world", nil, true},
	} {
		v := verify(c.repo, key(c.ns...))
		if !v.Signed || v.Trusted != c.want {
			t.Errorf("%s with namespaces %q: trusted=%v, want %v (%s)", c.repo, c.ns, v.Trusted, c.want, v.Reason)
		}
		if !c.want && !strings.Contains(v.Reason, "not trusted for this namespace") {
			t.Errorf("%s: the reason should say the key is out of scope: %q", c.repo, v.Reason)
		}
	}
	// A second, unscoped entry for the same key still admits the repository:
	// the scope narrows an entry, it does not ban the key.
	both := append(key("onepony"), TrustedKey{Name: "registry", Pub: s.Pub, KeyID: s.KeyID, Registry: host})
	if v := verify(host+"/txco/hello-world", both); !v.Trusted || v.Name != "registry" {
		t.Errorf("an unscoped entry for the same key should match: %+v", v)
	}
}

func TestVerifyUntrustedKey(t *testing.T) {
	s, other := newTestSigner(t), newTestSigner(t)
	const digest, repo = "sha256:deadbeef", "r/n/name"
	man, layer, ann := fetchSig(t, signInto(t, digest, repo, "", s), digest)
	v := VerifyArtifact(man, layer, ann, digest, repo, "",
		[]TrustedKey{{Pub: other.Pub, KeyID: other.KeyID}})
	if !v.Signed || v.Trusted {
		t.Fatalf("want signed-but-untrusted, got %+v", v)
	}
}

func TestVerifyWrongDigest(t *testing.T) {
	s := newTestSigner(t)
	const repo = "r/n/name"
	man, layer, ann := fetchSig(t, signInto(t, "sha256:aaaa", repo, "", s), "sha256:aaaa")
	v := VerifyArtifact(man, layer, ann, "sha256:bbbb", repo, "", []TrustedKey{{Pub: s.Pub, KeyID: s.KeyID}})
	if !v.Signed || v.Trusted {
		t.Fatalf("transplanted digest must not be trusted: %+v", v)
	}
}

func TestVerifyWrongRepo(t *testing.T) {
	s := newTestSigner(t)
	const digest = "sha256:aaaa"
	man, layer, ann := fetchSig(t, signInto(t, digest, "r/n/orig", "", s), digest)
	v := VerifyArtifact(man, layer, ann, digest, "r/n/evil", "", []TrustedKey{{Pub: s.Pub, KeyID: s.KeyID}})
	if !v.Signed || v.Trusted {
		t.Fatalf("transplanted repo must not be trusted: %+v", v)
	}
}

func TestVerifyBadSignature(t *testing.T) {
	s := newTestSigner(t)
	const digest, repo = "sha256:aaaa", "r/n/name"
	man, layer, ann := fetchSig(t, signInto(t, digest, repo, "", s), digest)
	ann[AnnotationSignature] = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)) // all-zero sig
	v := VerifyArtifact(man, layer, ann, digest, repo, "", []TrustedKey{{Pub: s.Pub, KeyID: s.KeyID}})
	if v.Signed {
		t.Fatalf("corrupt signature must not verify: %+v", v)
	}
}

func TestVerifyMalformedPayload(t *testing.T) {
	s := newTestSigner(t)
	v := VerifyArtifact(nil, []byte("{not json"), map[string]string{}, "sha256:x", "r", "", []TrustedKey{{Pub: s.Pub, KeyID: s.KeyID}})
	if v.Signed || v.Reason == "" {
		t.Fatalf("malformed payload should be unsigned with a reason: %+v", v)
	}
}

func TestParseTrustedKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := priv.Public().(ed25519.PublicKey)

	got, err := ParseTrustedKey(base64.StdEncoding.EncodeToString(pub))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("base64: err=%v equal=%v", err, got.Equal(pub))
	}

	sshPub, _ := ssh.NewPublicKey(pub)
	line := string(ssh.MarshalAuthorizedKey(sshPub))
	got, err = ParseTrustedKey(line)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("ssh line: err=%v equal=%v", err, got.Equal(pub))
	}

	if _, err := ParseTrustedKey("not-a-key"); err == nil {
		t.Error("garbage should error")
	}
}
