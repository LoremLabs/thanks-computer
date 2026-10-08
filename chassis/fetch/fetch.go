// Package fetch is the chassis's safe public-HTTP fetcher: one GET of a
// public web resource on a stack's behalf, bounded in every dimension.
//
// It is deliberately narrow. The URL must be http(s) on port 80 or 443
// with no credentials; the host is resolved here and every candidate
// address goes through the chassis egress guard before it is dialed, so
// the address checked is the address connected (no second lookup for DNS
// rebinding to exploit), and every redirect hop is checked the same way.
// There is no proxy, no cookie jar, no authentication, no request body;
// redirects, header bytes, body bytes and the total time are capped.
//
// It knows nothing about what the body is for: callers (txco://html/
// extract) parse it. Errors are *Error values with a stable Code and a
// message that never names an address.
package fetch

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
)

// Defaults for the zero values in Options.
const (
	DefaultMaxBytes       = 3 << 20
	DefaultMaxRedirects   = 3
	DefaultTimeout        = 5 * time.Second
	DefaultMaxHeaderBytes = 64 << 10
)

// Options configure a Fetcher. Guard is required.
type Options struct {
	Guard        egress.Guard
	MaxBytes     int64         // body bytes; one more fails the fetch
	MaxRedirects int           // hops followed; one more fails the fetch
	Timeout      time.Duration // the whole fetch: dial, TLS, headers, body
	// MediaTypes are the Content-Type media types accepted (lowercase,
	// no parameters). Empty accepts any.
	MediaTypes []string
	// Accept is sent as the Accept header.
	Accept string

	// Resolver, Dial and TLSConfig are seams for tests; nil means the
	// system resolver, a guarded TCP dial and the system roots.
	Resolver  Resolver
	Dial      DialFunc
	TLSConfig *tls.Config
}

// Request is one fetch.
type Request struct {
	URL       string
	UserAgent string
}

// Result is a successful fetch: a 2xx response of an accepted media type
// whose body fit.
type Result struct {
	URL         string // as requested (normalized)
	FinalURL    string // after redirects
	Status      int
	ContentType string // the header as sent
	Body        []byte
	Redirects   int
	Bytes       int64 // body bytes read off the wire (compressed, if it was)
}

// Fetcher is safe for concurrent use. Its connection pool is keyed by
// scheme and host, so a reused connection is one dialed (and checked) for
// that same host.
type Fetcher struct {
	opts   Options
	client *http.Client
}

// errRedirectLimit and errRedirectTarget come back through the client's
// url.Error wrapping from CheckRedirect.
var (
	errRedirectLimit  = errors.New("fetch: redirect limit")
	errRedirectTarget = errors.New("fetch: redirect target refused")
)

// New builds a Fetcher.
func New(opts Options) (*Fetcher, error) {
	if opts.Guard == nil {
		return nil, errors.New("fetch: Options.Guard is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxRedirects < 0 {
		opts.MaxRedirects = 0
	} else if opts.MaxRedirects == 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Dial == nil {
		opts.Dial = guardedDial(opts.Guard)
	}
	d := &dialer{guard: opts.Guard, resolver: opts.Resolver, dial: opts.Dial}
	tr := &http.Transport{
		Proxy:                  nil, // never the environment's proxy
		DialContext:            d.DialContext,
		ForceAttemptHTTP2:      true,
		DisableCompression:     true,
		MaxIdleConns:           64,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    opts.Timeout,
		ResponseHeaderTimeout:  opts.Timeout,
		MaxResponseHeaderBytes: DefaultMaxHeaderBytes,
		TLSClientConfig:        opts.TLSConfig,
	}
	maxRedirects := opts.MaxRedirects
	client := &http.Client{
		Transport: tr,
		Jar:       nil,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errRedirectLimit
			}
			if checkParsed(req.URL) != nil {
				return errRedirectTarget
			}
			// Nothing of the caller's travels: no cookies, no auth.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
	return &Fetcher{opts: opts, client: client}, nil
}

// Get fetches one URL. The deadline is the earlier of ctx's and the
// Fetcher's Timeout.
func (f *Fetcher) Get(ctx context.Context, r Request) (*Result, error) {
	u, ferr := CheckURL(r.URL)
	if ferr != nil {
		return nil, ferr
	}
	ctx, cancel := context.WithTimeout(ctx, f.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errf(CodeInvalidURL, "the URL can't be requested")
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	if f.opts.Accept != "" {
		req.Header.Set("Accept", f.opts.Accept)
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer resp.Body.Close()

	res := &Result{
		URL:         u.String(),
		FinalURL:    resp.Request.URL.String(),
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Redirects:   redirects(resp),
	}
	fail := func(e *Error) (*Result, error) {
		e.Status, e.ContentType = res.Status, res.ContentType
		return nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(errf(CodeHTTPError, "the server answered %d", resp.StatusCode))
	}
	if !f.acceptable(res.ContentType) {
		if res.ContentType == "" {
			return fail(errf(CodeUnsupportedType, "the server sent no Content-Type"))
		}
		return fail(errf(CodeUnsupportedType, "the server sent a Content-Type this op does not read"))
	}
	if resp.ContentLength > f.opts.MaxBytes {
		return fail(errf(CodeResponseTooLarge, "the body is larger than %d bytes", f.opts.MaxBytes))
	}

	wire := &countingReader{r: io.LimitReader(resp.Body, f.opts.MaxBytes+1)}
	var body io.Reader = wire
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		// Asked for identity and got gzip anyway: decode under the same
		// cap, applied to the decoded bytes.
		zr, zerr := gzip.NewReader(wire)
		if zerr != nil {
			return fail(classifyBody(ctx, zerr, CodeConnectionFailed, "the gzip body does not decode"))
		}
		defer zr.Close()
		body = zr
	default:
		return fail(errf(CodeUnsupportedEncoding, "the server sent a Content-Encoding this op does not decode"))
	}
	buf, rerr := io.ReadAll(io.LimitReader(body, f.opts.MaxBytes+1))
	res.Bytes = wire.n
	if rerr != nil {
		e := classifyBody(ctx, rerr, CodeConnectionFailed, "the body could not be read")
		e.Bytes = wire.n
		return fail(e)
	}
	if int64(len(buf)) > f.opts.MaxBytes || wire.n > f.opts.MaxBytes {
		e := errf(CodeResponseTooLarge, "the body is larger than %d bytes", f.opts.MaxBytes)
		e.Bytes = wire.n
		return fail(e)
	}
	res.Body = buf
	return res, nil
}

func (f *Fetcher) acceptable(contentType string) bool {
	if len(f.opts.MediaTypes) == 0 {
		return true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	for _, want := range f.opts.MediaTypes {
		if mt == want {
			return true
		}
	}
	return false
}

// redirects counts the hops behind a response.
func redirects(resp *http.Response) int {
	n := 0
	for r := resp.Request; r != nil && r.Response != nil; r = r.Response.Request {
		n++
	}
	return n
}

// classify maps a client.Do error to an Error.
func classify(ctx context.Context, err error) *Error {
	switch {
	case errors.Is(err, errRedirectLimit):
		return errf(CodeRedirectLimit, "more than the allowed number of redirects")
	case errors.Is(err, errRedirectTarget):
		return errf(CodeInvalidURL, "a redirect pointed at a URL this op can't fetch")
	case isDenied(err):
		return errf(CodeDestinationDenied, "the destination is not permitted")
	case isTimeout(ctx, err):
		return errf(CodeTimeout, "the fetch did not finish in time")
	case isDNS(err):
		return errf(CodeDNSFailed, "the host could not be resolved")
	}
	return errf(CodeConnectionFailed, "the connection failed")
}

func classifyBody(ctx context.Context, err error, code, msg string) *Error {
	if isTimeout(ctx, err) {
		return errf(CodeTimeout, "the fetch did not finish in time")
	}
	return errf(code, "%s", msg)
}

func isTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
