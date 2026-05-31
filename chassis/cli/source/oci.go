package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// TxCo package OCI media types (config = the verbatim manifest bytes; layer =
// one gzip(tar(tree))). artifactType is a distinct identity string.
const (
	MediaTypePackageConfig = "application/vnd.thanks.computer.package.manifest.v1alpha1+yaml"
	MediaTypePackageLayer  = "application/vnd.thanks.computer.package.layer.v1alpha1.tar+gzip"
	ArtifactTypePackage    = "application/vnd.thanks.computer.package.v1alpha1"
)

// Provenance is what a resolvable source reports after Fetch so callers
// (install) can record where bytes actually came from. dir/github sources do
// not implement Resolver, so their provenance stays blank.
type Provenance struct {
	Registry  string // host[:port]
	Namespace string
	Name      string // the repository name in the ref (not necessarily manifest.Name)
	Tag       string
	Digest    string // sha256:... — the manifest digest the pull resolved
	Reference string // oci://host/ns/name@sha256:...
}

// Resolver is OPTIONAL; only ociSource implements it. Callers type-assert.
type Resolver interface {
	Resolved() Provenance
}

// newRepository builds an oras target for an OCI repository reference. The real
// impl talks to a registry with docker-config (or TXCO_OCI_* env) auth; tests
// swap it via SetRepositoryFactory to point at an in-process content store.
var newRepository = func(repository string) (oras.ReadOnlyTarget, error) {
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return nil, err
	}
	if u := os.Getenv("TXCO_OCI_USERNAME"); u != "" {
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Cache:  auth.NewCache(),
			Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
				Username: u,
				Password: os.Getenv("TXCO_OCI_PASSWORD"),
			}),
		}
		return repo, nil
	}
	// Default: docker-config credentials (anonymous for public repos).
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(store),
		}
	}
	return repo, nil
}

// SetRepositoryFactory swaps the repository constructor (for tests). Returns the
// previous value so callers can restore it in t.Cleanup. TESTS ONLY — nothing
// in production should call the real factory with an in-process store.
func SetRepositoryFactory(fn func(string) (oras.ReadOnlyTarget, error)) func(string) (oras.ReadOnlyTarget, error) {
	prev := newRepository
	newRepository = fn
	return prev
}

type ociSource struct {
	ref  ParsedRef
	spec string
	prov Provenance
}

func newOCISource(spec string) (*ociSource, error) {
	r, err := ParseRef(spec)
	if err != nil {
		return nil, err
	}
	return &ociSource{ref: r, spec: spec}, nil
}

func (o *ociSource) Spec() string         { return o.spec }
func (o *ociSource) Resolved() Provenance { return o.prov }

// Fetch pulls the artifact into an in-memory store, records the resolved digest
// as provenance, finds the package layer by media type, and extracts its
// gzip(tar) into destDir.
func (o *ociSource) Fetch(ctx context.Context, destDir string) (int, error) {
	repo, err := newRepository(o.ref.Repository())
	if err != nil {
		return 0, fmt.Errorf("oci: open %s: %w", o.ref.Repository(), err)
	}
	store := memory.New()
	manifestDesc, err := oras.Copy(ctx, repo, o.ref.TagOrDigest(), store, o.ref.TagOrDigest(), oras.DefaultCopyOptions)
	if err != nil {
		return 0, fmt.Errorf("oci: pull %s: %w", o.ref.Reference(), err)
	}
	digest := manifestDesc.Digest.String()
	o.prov = Provenance{
		Registry:  o.ref.Registry,
		Namespace: o.ref.Namespace,
		Name:      o.ref.Name,
		Tag:       o.ref.Tag,
		Digest:    digest,
		Reference: "oci://" + o.ref.WithDigest(digest),
	}

	manBytes, err := content.FetchAll(ctx, store, manifestDesc)
	if err != nil {
		return 0, fmt.Errorf("oci: fetch manifest: %w", err)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(manBytes, &man); err != nil {
		return 0, fmt.Errorf("oci: parse manifest: %w", err)
	}
	var layer *ocispec.Descriptor
	for i := range man.Layers {
		if man.Layers[i].MediaType == MediaTypePackageLayer {
			layer = &man.Layers[i]
			break
		}
	}
	if layer == nil {
		return 0, fmt.Errorf("oci: %s is not a TxCo package (no %s layer)", o.ref.Reference(), MediaTypePackageLayer)
	}
	blob, err := content.FetchAll(ctx, store, *layer)
	if err != nil {
		return 0, fmt.Errorf("oci: fetch layer: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return 0, fmt.Errorf("oci: gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return extractTar(tar.NewReader(gz), false, "", destDir)
}

// --- publish-side helpers (shared by `txco package publish`) ----------------

// tarGzDir builds a gzip(tar) of the regular files under dir, with
// slash-separated relative paths and no synthetic top directory.
func tarGzDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// packPackageArtifact pushes the config (manifest bytes) + single layer into
// dst, packs an OCI manifest referencing them, and tags it. Returns the
// manifest descriptor (its Digest is the artifact's pin).
func packPackageArtifact(ctx context.Context, dst oras.Target, layerBytes, manifestBytes []byte, tag string) (ocispec.Descriptor, error) {
	layerDesc := content.NewDescriptorFromBytes(MediaTypePackageLayer, layerBytes)
	layerDesc.Annotations = map[string]string{ocispec.AnnotationTitle: "package.tar.gz"}
	if err := dst.Push(ctx, layerDesc, bytes.NewReader(layerBytes)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push layer: %w", err)
	}
	cfgDesc := content.NewDescriptorFromBytes(MediaTypePackageConfig, manifestBytes)
	if err := dst.Push(ctx, cfgDesc, bytes.NewReader(manifestBytes)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push config: %w", err)
	}
	manDesc, err := oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, ArtifactTypePackage, oras.PackManifestOptions{
		ConfigDescriptor: &cfgDesc,
		Layers:           []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest: %w", err)
	}
	if tag != "" {
		if err := dst.Tag(ctx, manDesc, tag); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("tag %s: %w", tag, err)
		}
	}
	return manDesc, nil
}

// pushArtifact copies a packed artifact from a local store to a remote
// repository, returning the manifest digest. Used by `txco package publish`.
var newPushRepository = func(repository string) (oras.Target, error) {
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return nil, err
	}
	if u := os.Getenv("TXCO_OCI_USERNAME"); u != "" {
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Cache:  auth.NewCache(),
			Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
				Username: u,
				Password: os.Getenv("TXCO_OCI_PASSWORD"),
			}),
		}
		return repo, nil
	}
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(store),
		}
	}
	return repo, nil
}

// SetPushRepositoryFactory swaps the push-target constructor (tests only).
func SetPushRepositoryFactory(fn func(string) (oras.Target, error)) func(string) (oras.Target, error) {
	prev := newPushRepository
	newPushRepository = fn
	return prev
}

// Publish packs the package tree at dir into a single-layer OCI artifact
// (config = the verbatim txco.package.yaml bytes; layer = gzip(tar(tree))) and
// pushes it to ref, returning the resolved manifest digest (sha256:...).
func Publish(ctx context.Context, dir string, ref ParsedRef) (string, error) {
	layer, err := tarGzDir(dir)
	if err != nil {
		return "", fmt.Errorf("oci: build layer: %w", err)
	}
	// The config blob is the manifest verbatim (filename mirrors manifest.FileName).
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "txco.package.yaml"))
	if err != nil {
		return "", fmt.Errorf("oci: read manifest: %w", err)
	}
	local := memory.New()
	manDesc, err := packPackageArtifact(ctx, local, layer, manifestBytes, ref.TagOrDigest())
	if err != nil {
		return "", err
	}
	repo, err := newPushRepository(ref.Repository())
	if err != nil {
		return "", fmt.Errorf("oci: open %s: %w", ref.Repository(), err)
	}
	if _, err := oras.Copy(ctx, local, ref.TagOrDigest(), repo, ref.TagOrDigest(), oras.DefaultCopyOptions); err != nil {
		return "", fmt.Errorf("oci: push %s: %w", ref.Reference(), err)
	}
	return manDesc.Digest.String(), nil
}

// --- signature transport (used by `txco package publish --sign` + verify) ----
//
// These move bytes only: the crypto + artifact shape live in chassis/cli/sign,
// the trust policy in the CLI layer. Both reuse the same repo factories as
// package push/pull, so the in-process test seam covers signing end-to-end.

// PushSignature uploads a signature artifact to ref's repository. `build` packs
// the artifact into an in-memory store and returns the tag it was given
// (sha256-<hex>.sig); that exact tag is copied to the remote.
func PushSignature(ctx context.Context, ref ParsedRef, build func(dst oras.Target) (string, error)) error {
	local := memory.New()
	tag, err := build(local)
	if err != nil {
		return err
	}
	repo, err := newPushRepository(ref.Repository())
	if err != nil {
		return fmt.Errorf("oci: open %s: %w", ref.Repository(), err)
	}
	if _, err := oras.Copy(ctx, local, tag, repo, tag, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("oci: push signature %s: %w", tag, err)
	}
	return nil
}

// FetchSignature pulls the signature artifact at sigTag from ref's repository. A
// missing tag yields found=false with nil error (the package is simply
// unsigned); a transport failure yields a non-nil error. Returns the manifest
// bytes, the single payload layer's bytes, and the merged manifest+layer
// annotations (manifest wins on conflict).
func FetchSignature(ctx context.Context, ref ParsedRef, sigTag string) (manifestBytes, layerBytes []byte, ann map[string]string, found bool, err error) {
	repo, err := newRepository(ref.Repository())
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("oci: open %s: %w", ref.Repository(), err)
	}
	store := memory.New()
	manifestDesc, err := oras.Copy(ctx, repo, sigTag, store, sigTag, oras.DefaultCopyOptions)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil, nil, false, nil
		}
		return nil, nil, nil, false, fmt.Errorf("oci: fetch signature %s: %w", sigTag, err)
	}
	manifestBytes, err = content.FetchAll(ctx, store, manifestDesc)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("oci: fetch signature manifest: %w", err)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &man); err != nil {
		return nil, nil, nil, false, fmt.Errorf("oci: parse signature manifest: %w", err)
	}
	ann = map[string]string{}
	if len(man.Layers) == 1 {
		if layerBytes, err = content.FetchAll(ctx, store, man.Layers[0]); err != nil {
			return nil, nil, nil, false, fmt.Errorf("oci: fetch signature payload: %w", err)
		}
		for k, v := range man.Layers[0].Annotations {
			ann[k] = v
		}
	}
	for k, v := range man.Annotations {
		ann[k] = v
	}
	return manifestBytes, layerBytes, ann, true, nil
}
