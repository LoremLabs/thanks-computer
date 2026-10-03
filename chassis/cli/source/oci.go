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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

// CredentialProvider answers a registry's Bearer challenge with a token it
// obtained some other way than the registry's own token realm. It is given
// the registry (host[:port]) and the scopes the operation in flight needs
// (`repository:<name>:pull,push`). handled=false means "not mine": the
// caller falls back to docker-config credentials. A token must be fresh on
// every call — the client asks again exactly when the last one was refused.
type CredentialProvider func(ctx context.Context, hostport string, scopes []string) (token string, handled bool, err error)

// credentialProvider is set by the CLI layer (SetCredentialProvider), which
// owns profiles and the admin client that this package cannot import.
var credentialProvider CredentialProvider

// SetCredentialProvider installs the token provider consulted for a Bearer
// challenge, returning the previous one (tests restore it in t.Cleanup).
func SetCredentialProvider(p CredentialProvider) CredentialProvider {
	prev := credentialProvider
	credentialProvider = p
	return prev
}

// LoopbackHost reports whether a registry or service host[:port] is this
// machine: `localhost` or a loopback ADDRESS. Such a registry is reached
// over plain HTTP, as docker does. A name that merely should resolve to
// loopback (`x.localhost`) does not count: where it resolves is the
// resolver's decision, and plain HTTP must not depend on it.
func LoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerChallenges remembers, per registry host, whether its last 401 asked
// for a Bearer token. The credential function is told only the host, never
// the challenge, and a token provider must not answer a Basic challenge:
// that one wants the docker-config username and password.
type bearerChallenges struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen map[string]bool
}

func (b *bearerChallenges) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		scheme, _, _ := strings.Cut(strings.TrimSpace(resp.Header.Get("Www-Authenticate")), " ")
		b.mu.Lock()
		b.seen[req.URL.Host] = strings.EqualFold(scheme, "Bearer")
		b.mu.Unlock()
	}
	return resp, err
}

func (b *bearerChallenges) bearer(host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen[host]
}

// newAuthClient is the one place a registry client's credentials are
// decided, for pulls and pushes alike. In order:
//
//  1. TXCO_OCI_USERNAME / TXCO_OCI_PASSWORD, when set — an explicit override.
//  2. The credential provider, for a Bearer challenge it handles: a registry
//     whose token service is a chassis (`txco package publish` to a tenant's
//     own namespace, signed in as that tenant).
//  3. docker-config credentials.
//  4. Anonymous.
//
// Credentials are asked for only after a 401, so pulling a public package
// consults none of them.
func newAuthClient(registryHost string) *auth.Client {
	if u := os.Getenv("TXCO_OCI_USERNAME"); u != "" {
		return &auth.Client{
			Client: retry.DefaultClient,
			Cache:  auth.NewCache(),
			Credential: auth.StaticCredential(registryHost, auth.Credential{
				Username: u,
				Password: os.Getenv("TXCO_OCI_PASSWORD"),
			}),
		}
	}
	var docker auth.CredentialFunc
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		docker = credentials.Credential(store)
	}
	challenges := &bearerChallenges{base: retry.DefaultClient.Transport, seen: map[string]bool{}}
	return &auth.Client{
		Client: &http.Client{Transport: challenges},
		Cache:  auth.NewCache(),
		Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
			if p := credentialProvider; p != nil && challenges.bearer(hostport) {
				token, handled, err := p(ctx, hostport, auth.GetAllScopesForHost(ctx, hostport))
				if err != nil {
					return auth.EmptyCredential, err
				}
				if handled {
					return auth.Credential{AccessToken: token}, nil
				}
			}
			if docker != nil {
				return docker(ctx, hostport)
			}
			return auth.EmptyCredential, nil
		},
	}
}

// newRemoteRepository opens a registry repository with newAuthClient's
// credentials; a registry on this machine is spoken to over plain HTTP.
func newRemoteRepository(repository string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = LoopbackHost(repo.Reference.Registry)
	repo.Client = newAuthClient(repo.Reference.Registry)
	return repo, nil
}

// newRepository builds an oras target for an OCI repository reference. The real
// impl talks to a registry with newAuthClient's credentials; tests
// swap it via SetRepositoryFactory to point at an in-process content store.
var newRepository = func(repository string) (oras.ReadOnlyTarget, error) {
	return newRemoteRepository(repository)
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

// excludedPackageDir reports whether an entry must never ship in a published
// package layer: the VCS dir and the local build cache. Matched by base name so
// it holds at any depth. This is the single source of truth for publish-time
// exclusion — applied in tarGzDir, which every publish path funnels through.
func excludedPackageDir(name string) bool {
	return name == ".git" || name == ".txco"
}

// PackageFileMode is the only mode a package file has: 0755 when its source
// is executable by anyone, else 0644. Packing records it and every install
// path restores it, so a program shipped in a stack (run from
// $TXCO_STACK_DIR) stays runnable; no other bit (setuid, group write) travels.
func PackageFileMode(m fs.FileMode) fs.FileMode {
	if m&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// tarGzDir builds a gzip(tar) of the regular files under dir, with
// slash-separated relative paths and no synthetic top directory. Each file's
// mode is PackageFileMode of its source. `.git` and
// `.txco` are skipped wherever they appear, so neither the author's VCS history
// nor the build cache rides along — and the rule holds whether we're packing
// the author's own tree (the common path) or a prebuild staging copy.
func tarGzDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && excludedPackageDir(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // a `.git` gitlink file (submodule) or stray `.txco`
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     int64(PackageFileMode(info.Mode())),
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
	return newRemoteRepository(repository)
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
