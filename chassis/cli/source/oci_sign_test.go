package source

import (
	"bytes"
	"context"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// TestPushFetchSignatureRoundTrip exercises the signature transport primitives
// against an in-process OCI store (both factories point at it), with no
// crypto/sign dependency — just bytes + annotations + the tag convention.
func TestPushFetchSignatureRoundTrip(t *testing.T) {
	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevPush := SetPushRepositoryFactory(func(string) (oras.Target, error) { return store, nil })
	prevPull := SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { SetPushRepositoryFactory(prevPush); SetRepositoryFactory(prevPull) })

	ref := ParsedRef{Registry: "registry.thanks.computer", Namespace: "txco", Name: "support-basic"}
	const sigTag = "sha256-deadbeef.sig"
	payload := []byte(`{"digest":"sha256:deadbeef"}`)
	ann := map[string]string{"computer.thanks.signature": "AAA", "computer.thanks.signature.keyid": "SHA256:abc"}

	// Build a minimal sig-shaped artifact (one layer + manifest annotations) and push it.
	err = PushSignature(context.Background(), ref, func(dst oras.Target) (string, error) {
		ld := content.NewDescriptorFromBytes("application/vnd.thanks.computer.signature.payload.v1alpha1+json", payload)
		ld.Annotations = ann
		if err := dst.Push(context.Background(), ld, bytes.NewReader(payload)); err != nil {
			return "", err
		}
		md, err := oras.PackManifest(context.Background(), dst, oras.PackManifestVersion1_1,
			"application/vnd.thanks.computer.signature.v1alpha1",
			oras.PackManifestOptions{Layers: []ocispec.Descriptor{ld}, ManifestAnnotations: ann})
		if err != nil {
			return "", err
		}
		return sigTag, dst.Tag(context.Background(), md, sigTag)
	})
	if err != nil {
		t.Fatalf("PushSignature: %v", err)
	}

	man, layer, gotAnn, found, err := FetchSignature(context.Background(), ref, sigTag)
	if err != nil || !found {
		t.Fatalf("FetchSignature: found=%v err=%v", found, err)
	}
	if !bytes.Equal(layer, payload) {
		t.Errorf("layer roundtrip mismatch: %q", layer)
	}
	if gotAnn["computer.thanks.signature.keyid"] != "SHA256:abc" {
		t.Errorf("annotations not returned: %+v", gotAnn)
	}
	if len(man) == 0 {
		t.Error("manifest bytes empty")
	}

	// A missing tag is "unsigned", not an error.
	_, _, _, found, err = FetchSignature(context.Background(), ref, "sha256-absent.sig")
	if err != nil || found {
		t.Fatalf("missing tag should be found=false,err=nil; got found=%v err=%v", found, err)
	}
}
