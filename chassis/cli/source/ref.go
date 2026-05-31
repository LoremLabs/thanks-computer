package source

import (
	"fmt"
	"strings"
)

// ParsedRef is a decomposed OCI package reference.
type ParsedRef struct {
	Registry  string // host[:port]
	Namespace string // path between host and the final segment (may be multi-segment or "")
	Name      string // final path segment (the repository name)
	Tag       string // tag label (empty when pinned by digest)
	Digest    string // "sha256:..." (empty when referenced by tag)
}

// ParseRef decomposes an OCI reference. Accepts an optional `oci://` (or `oci:`)
// scheme prefix. Forms:
//
//	host/ns/name:tag
//	host/ns/name@sha256:...
//	host/name:tag
//	localhost:5000/ns/name:tag      (host port preserved)
//	host/ns/sub/name:tag            (multi-segment namespace)
//
// A reference with neither tag nor digest defaults to tag "latest".
func ParseRef(s string) (ParsedRef, error) {
	orig := s
	s = strings.TrimPrefix(s, "oci://")
	s = strings.TrimPrefix(s, "oci:")
	s = strings.TrimSpace(s)
	if s == "" {
		return ParsedRef{}, fmt.Errorf("empty OCI reference")
	}

	// Digest pin (terminal `@sha256:...`).
	var digest string
	if at := strings.LastIndex(s, "@"); at >= 0 {
		digest = s[at+1:]
		s = s[:at]
		if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
			return ParsedRef{}, fmt.Errorf("invalid digest in %q (want @sha256:<hex>)", orig)
		}
	}

	// Split host[:port] from the repository path on the FIRST slash, so a host
	// port colon is never mistaken for a tag separator.
	slash := strings.Index(s, "/")
	if slash < 0 {
		return ParsedRef{}, fmt.Errorf("OCI reference %q needs a registry host and a name", orig)
	}
	host, path := s[:slash], s[slash+1:]
	if host == "" || path == "" {
		return ParsedRef{}, fmt.Errorf("OCI reference %q needs a registry host and a name", orig)
	}

	// Tag = a colon in the FINAL path segment.
	var tag string
	if digest == "" {
		if colon := strings.LastIndex(path, ":"); colon >= 0 && !strings.Contains(path[colon+1:], "/") {
			tag = path[colon+1:]
			path = path[:colon]
		}
	}

	name := path
	namespace := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		namespace, name = path[:i], path[i+1:]
	}
	if name == "" {
		return ParsedRef{}, fmt.Errorf("OCI reference %q has an empty name", orig)
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return ParsedRef{Registry: host, Namespace: namespace, Name: name, Tag: tag, Digest: digest}, nil
}

// Repository is the host/namespace/name string (no tag/digest) — what
// remote.NewRepository takes.
func (r ParsedRef) Repository() string {
	if r.Namespace == "" {
		return r.Registry + "/" + r.Name
	}
	return r.Registry + "/" + r.Namespace + "/" + r.Name
}

// TagOrDigest is the reference label to resolve within the repository (the
// srcRef for oras.Copy).
func (r ParsedRef) TagOrDigest() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Reference is the full canonical reference (repository plus :tag or @digest).
func (r ParsedRef) Reference() string {
	if r.Digest != "" {
		return r.Repository() + "@" + r.Digest
	}
	return r.Repository() + ":" + r.Tag
}

// WithDigest returns the repository pinned to a resolved digest.
func (r ParsedRef) WithDigest(digest string) string {
	return r.Repository() + "@" + digest
}
