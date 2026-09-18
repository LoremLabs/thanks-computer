package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/signedurl"
)

// drive_sign.go is the by-reference read of a drive file: `txco://drive/sign`
// mints a short-lived URL for ONE exact document and the web head serves it
// at /_txc/signed/<token>. It is how a body too large for an envelope
// reaches something outside the chassis — the stack hands a parser a URL,
// the parser streams the bytes — instead of `drive/get` base64-ing the file
// into the run.
//
// The URL is a capability: whoever holds it can read that document until it
// expires, with no other credential. So it names the document's CONTENT
// (sha256) as well as its address, and the endpoint answers 410 when the
// file has since changed: a token never reads bytes that were not there when
// it was signed.

const (
	// driveSignTTLDefault is the lifetime of a URL whose op gave no `ttl`:
	// long enough for a fetch to start behind a queue, short enough that a
	// URL left in a trace is dead before anyone reads the trace.
	driveSignTTLDefault = 10 * time.Minute
	// driveSignTTLMax caps `ttl`. The fetch must START inside the window; a
	// download in progress is not cut off when the token expires.
	driveSignTTLMax = time.Hour
)

// signedURLBase is the public origin signed URLs are minted on: the
// explicit --signed-url-base, else wherever continuation workers are told
// to call back, else this node's own web listener (single-node / dev).
func signedURLBase(conf config.Config) string {
	for _, b := range []string{conf.SignedURLBase, conf.ContinuationCallbackBaseURL} {
		if b = strings.TrimRight(strings.TrimSpace(b), "/"); b != "" {
			return b
		}
	}
	host, port, err := net.SplitHostPort(conf.WebAddr)
	if err != nil {
		host, port = "", strings.TrimPrefix(conf.WebAddr, ":")
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = conf.Fqdn
	}
	if host == "" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// driveSign mints a signed URL for one file of the pinned tenant. WITH
// `collection` + (`path` | `resource_id`), optional `ttl` seconds (default
// 600, at most 3600). Result at `into`: {url, expires_at, ttl, resource_id,
// path, name, size, content_type, sha256}.
//
// The fetcher should check what it received against `sha256` — the URL
// guarantees it, but the fetcher is the one who can prove it.
//
// Charged as the read it authorizes (the document's size), once, here: the
// fetch itself happens outside any run and has nothing to bill.
func driveSign(ctx context.Context, d driveDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := drivePrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	if d.signer == nil {
		return driveErr(into, "txco_drive_sign_unavailable",
			"signed URLs are keyed from the secret store's master key, and this node has none (--secret-master-key)"), nil
	}
	coll, ep, ok := driveCollectionFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	path, id, ep, ok := driveAddress(meta, into)
	if !ok {
		return ep, nil
	}
	ttl := driveSignTTLDefault
	if t := gjson.GetBytes(meta, "ttl"); t.Exists() {
		ttl = time.Duration(t.Int()) * time.Second
		if t.Type != gjson.Number || ttl < time.Second || ttl > driveSignTTLMax {
			return driveErr(into, "txco_drive_invalid_arg",
				"`ttl` is seconds, 1 to "+strconv.Itoa(int(driveSignTTLMax/time.Second))), nil
		}
	}
	res, found, err := driveStatFor(ctx, d, coll.ID, path, id)
	if err != nil {
		return driveStoreErr(into, err), nil
	}
	if !found {
		return driveErr(into, "txco_drive_not_found", "no such resource"), nil
	}
	if res.IsDir() {
		return driveErr(into, "txco_drive_is_directory", "resource is a directory; only a file can be signed"), nil
	}
	now := time.Now().UTC()
	if d.now != nil {
		now = d.now().UTC()
	}
	expires := now.Add(ttl)
	token, err := d.signer.Sign(signedurl.Claims{
		Tenant:     tenant,
		Collection: coll.ID,
		Resource:   res.ResourceID,
		SHA256:     res.ETag,
		Expires:    expires.Unix(),
	})
	if err != nil {
		return driveErr(into, "txco_drive_store", err.Error()), nil
	}
	driveChargeBytes(ctx, res.Size, in)

	out := jsonx.NewObject()
	out.Set(into+".url", d.signBase+signedurl.PathPrefix+token)
	out.Set(into+".expires_at", expires.Format(time.RFC3339))
	out.Set(into+".ttl", int64(ttl/time.Second))
	out.Set(into+".resource_id", res.ResourceID)
	out.Set(into+".path", res.Path)
	out.Set(into+".name", chdrive.BaseOf(res.Path))
	out.Set(into+".size", res.Size)
	out.Set(into+".content_type", res.ContentType)
	out.Set(into+".sha256", res.ETag)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// signedBodyBudget is the write deadline for streaming size bytes: the web
// listener's global write timeout would cut a large download short.
func signedBodyBudget(size int64) time.Duration {
	b := time.Minute + time.Duration(size>>20)*time.Second
	if b > 2*time.Hour {
		b = 2 * time.Hour
	}
	return b
}

// signedHandler serves GET/HEAD /_txc/signed/<token>. No session, no Basic
// auth, no stack: the token is the whole authorization, so every refusal
// that could tell a guesser anything is the same bare 404 — malformed,
// forged, expired, and "that document is gone" are indistinguishable. Only
// a holder of a GENUINE token can learn more (410: the file changed since it
// was signed).
//
// The response is inert wherever it lands: this path answers on every
// hostname the web head serves, so a tenant's document must never render as
// a page on someone's origin. It is always an attachment, never sniffed,
// sandboxed, and never cached.
func signedHandler(store *chdrive.Store, signer *signedurl.Signer, adm admission.Provider, logger *zap.Logger) http.HandlerFunc {
	notFound := func(w http.ResponseWriter) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || signer == nil {
			notFound(w)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, signedurl.PathPrefix)
		c, err := signer.Verify(token, time.Now())
		if err != nil {
			notFound(w)
			return
		}
		ctx := r.Context()
		// The claims are ours (the MAC said so), but the world moves after a
		// token is signed: the collection must still exist and still be this
		// tenant's, and the tenant must still be admitted.
		coll, found, err := store.GetCollectionByID(ctx, c.Collection)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found || coll.Tenant != c.Tenant {
			notFound(w)
			return
		}
		if adm != nil {
			if dec := adm.Decide(c.Tenant); !dec.Admit {
				notFound(w)
				return
			}
		}
		rc, res, err := store.OpenByID(ctx, c.Collection, c.Resource)
		if err != nil {
			if errors.Is(err, chdrive.ErrNotFound) || errors.Is(err, chdrive.ErrIsDirectory) {
				notFound(w)
				return
			}
			w.Header().Set("Retry-After", "1")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		defer rc.Close()
		// The row OpenByID opened is the one compared: no window between a
		// check and the read.
		if res.ETag != c.SHA256 {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "the document changed after this URL was signed", http.StatusGone)
			return
		}
		ct := res.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", "attachment")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
		h.Set("Cache-Control", "private, no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("ETag", `"`+res.ETag+`"`)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(signedBodyBudget(res.Size)))
		if logger != nil {
			// Never the token, never the path: what was read, by whom it was
			// signed for, and how big.
			logger.Info("signed url served",
				zap.String("tenant", c.Tenant),
				zap.String("collection_id", c.Collection),
				zap.String("resource_id", c.Resource),
				zap.Int64("size", res.Size),
				zap.String("method", r.Method),
				zap.Bool("range", r.Header.Get("Range") != ""))
		}
		http.ServeContent(w, r, "", res.UpdatedAt, rc)
	}
}
