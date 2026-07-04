package signer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// signatureLabel is the inner-list label RFC 9421 uses to namespace
// multiple signatures on the same request. v1 always uses a single
// label, "sig1" — matches what httpsign emits on the chassis side.
const signatureLabel = "sig1"

// emptyBodyDigest is sha-256 of zero bytes, base64-encoded per
// RFC 9530. Pinning it as a constant keeps GET / no-body requests
// from rehashing nothing on every call. Same value the chassis-side
// signer uses (chassis/auth/signature/signer.go) — drift here would
// surface as a Content-Digest mismatch at verification time.
const emptyBodyDigest = "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:"

// signParams collects the per-signature parameters that go into both
// the Signature-Input header and the @signature-params line of the
// canonical base. We pass it by value because every field is small
// and immutable for the lifetime of one signature.
type signParams struct {
	KeyID   string
	Created int64
	Nonce   string
}

// computeContentDigest sets req.Header["Content-Digest"] to the
// SHA-256 digest of body, in the structured-fields wrapper RFC 9530
// requires (`sha-256=:<base64>:`). Returns the value it set so the
// caller can also inject it into the canonical base.
func computeContentDigest(req *http.Request, body []byte) string {
	// A caller that STREAMS its body (dataset blob PUT — gigabytes, never
	// in memory) pre-sets Content-Digest from its own streamed sha256 and
	// passes body=nil; sign over the declared value. This is not a
	// trust hole: the digest is a covered component, and the server
	// verifies the actual bytes against it (or against the URL hash the
	// signature also covers) on receipt.
	if v := req.Header.Get("Content-Digest"); v != "" {
		return v
	}
	if len(body) == 0 {
		req.Header.Set("Content-Digest", emptyBodyDigest)
		return emptyBodyDigest
	}
	sum := sha256.Sum256(body)
	v := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	req.Header.Set("Content-Digest", v)
	return v
}

// newNonce produces a 16-byte random nonce, encoded as URL-safe
// base64 without padding. 22 ASCII characters, 128 bits of entropy.
// Matches the shape chassis-side httpsign expects to parse.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// buildSignatureInputValue formats the Signature-Input header value
// MINUS the `sig1=` label prefix — i.e. just the inner list +
// parameters. The same string is used verbatim in the canonical base
// on the `@signature-params` line, and prefixed with `sig1=` when set
// on the request.
//
// Parameter order matches what httpsign typically emits (keyid, alg,
// created, nonce). RFC 9421 doesn't pin order — the verifier parses
// our string back into params — but keeping a consistent order makes
// diffs against reference implementations easier to read.
func buildSignatureInputValue(p signParams) string {
	covered := `("@method" "@path" "@query" "@authority" "content-digest")`
	return covered +
		fmt.Sprintf(`;created=%d`, p.Created) +
		fmt.Sprintf(`;keyid=%q`, p.KeyID) +
		`;alg="ed25519"` +
		fmt.Sprintf(`;nonce=%q`, p.Nonce)
}

// buildSignatureBase returns the byte sequence that the backend
// signs. Per RFC 9421 §2.3, this is one line per covered component
// followed by the @signature-params line. Each line is
// `"<name>": <value>` joined with `\n`, no trailing newline.
//
// The canonical values follow §2.2:
//   - @method  : uppercase request method.
//   - @path    : URL path; "/" if empty.
//   - @query   : "?" + raw query; "?" alone for empty query.
//   - @authority: lowercase Host header (or URL.Host).
//   - content-digest: the header value we computed above.
//   - @signature-params: the params string from buildSignatureInputValue.
//
// digestValue is what computeContentDigest returned for the same
// request; we accept it as a parameter rather than re-reading the
// header so callers can't get the two out of sync.
func buildSignatureBase(req *http.Request, digestValue string, paramsInputValue string) []byte {
	method := strings.ToUpper(req.Method)

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	rawQuery := req.URL.RawQuery
	query := "?" + rawQuery // RFC 9421 §2.2.7: "?" alone for empty query

	authority := strings.ToLower(canonicalAuthority(req))

	var b strings.Builder
	fmt.Fprintf(&b, "\"@method\": %s\n", method)
	fmt.Fprintf(&b, "\"@path\": %s\n", path)
	fmt.Fprintf(&b, "\"@query\": %s\n", query)
	fmt.Fprintf(&b, "\"@authority\": %s\n", authority)
	fmt.Fprintf(&b, "\"content-digest\": %s\n", digestValue)
	fmt.Fprintf(&b, "\"@signature-params\": %s", paramsInputValue)
	return []byte(b.String())
}

// canonicalAuthority returns the Host the chassis will reconstruct
// when it parses the request. http.Request.Host is the on-the-wire
// Host header; URL.Host is what the client URL parser captured.
// Prefer the explicit Host header when present (proxies set it),
// fall back to URL.Host.
func canonicalAuthority(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// nowUnix is a function-typed seam so tests can pin time. Production
// uses time.Now().Unix(). Kept package-level (not on a struct) so the
// canonicalizer stays a pure function for callers.
var nowUnix = func() int64 { return time.Now().Unix() }
