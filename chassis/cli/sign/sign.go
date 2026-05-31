package sign

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

// Signer holds a loaded ed25519 signing key and its derived id.
type Signer struct {
	Priv  ed25519.PrivateKey
	Pub   ed25519.PublicKey
	KeyID string
}

// NewSigner derives the public key + key id from a private key.
func NewSigner(priv ed25519.PrivateKey) Signer {
	pub := priv.Public().(ed25519.PublicKey)
	return Signer{Priv: priv, Pub: pub, KeyID: KeyIDForPub(pub)}
}

// SignArtifact builds the signature payload for (digest, repository), signs it
// with s, packs a single-layer OCI artifact carrying the payload + signature
// annotations into dst, tags it sha256-<hex>.sig, and returns that tag. dst is
// any oras.Target (an in-memory store in tests, a remote repo in production);
// the caller copies it to the registry. signedAt is supplied by the caller
// (RFC3339 UTC) so this stays deterministic and testable.
func SignArtifact(ctx context.Context, dst oras.Target, digest, repository, spec, signedAt string, s Signer) (string, error) {
	p := Payload{
		Spec:       spec,
		Repository: repository,
		Digest:     digest,
		KeyID:      s.KeyID,
		SignedAt:   signedAt,
	}
	payloadBytes, err := p.Bytes()
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	sig := ed25519.Sign(s.Priv, payloadBytes)
	ann := map[string]string{
		AnnotationSignature: base64.StdEncoding.EncodeToString(sig),
		AnnotationKeyID:     s.KeyID,
		AnnotationPubKey:    base64.StdEncoding.EncodeToString(s.Pub),
	}

	layerDesc := content.NewDescriptorFromBytes(MediaTypeSignaturePayload, payloadBytes)
	layerDesc.Annotations = ann // also on the layer, so verify can read either place
	if err := dst.Push(ctx, layerDesc, bytes.NewReader(payloadBytes)); err != nil {
		return "", fmt.Errorf("push payload: %w", err)
	}
	manDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, ArtifactTypeSignature, oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{layerDesc},
		ManifestAnnotations: ann,
	})
	if err != nil {
		return "", fmt.Errorf("pack signature manifest: %w", err)
	}
	tag := DigestToSigTag(digest)
	if err := dst.Tag(ctx, manDesc, tag); err != nil {
		return "", fmt.Errorf("tag %s: %w", tag, err)
	}
	return tag, nil
}
