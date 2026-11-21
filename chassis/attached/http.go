package attached

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
)

// The three routes a machine uses. All are public: a caller is either a
// machine with a code or a key, or nobody.
const (
	PathCode   = "/v1/attach/code"   // POST {code}: what the code is for, without spending it
	PathEnroll = "/v1/attach/enroll" // POST, signed: spend the code, enroll the key
	PathLink   = "/v1/attach"        // GET, WebSocket: the link
)

// maxEnrollBody bounds an enrollment's body. It holds a code, a key and a
// few short labels.
const maxEnrollBody = 8 << 10

// enrollSkew is how old an enrollment's signature may be.
const enrollSkew = 5 * time.Minute

// EnrollBody is what a machine posts to enroll.
type EnrollBody struct {
	Code      string `json:"code"`
	PublicKey []byte `json:"public_key"` // the 32-byte Ed25519 key
	OS        string `json:"os,omitempty"`
	Root      string `json:"root,omitempty"`
	Machine   string `json:"machine,omitempty"`
}

// EnrollResult is the answer to an enrollment.
type EnrollResult struct {
	Credential string `json:"credential"`
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
}

// CodeResult is the answer to a peek at a code.
type CodeResult struct {
	Label     string    `json:"label,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HTTP serves the machine-facing routes.
type HTTP struct {
	Svc *Service
	// ClientIP names the caller for the throttle: the socket peer, or what a
	// trusted edge says (edgeproxy.Clients.IP). nil uses RemoteAddr.
	ClientIP func(*http.Request) string

	verifier signature.Verifier
	guesses  *throttle.Throttle
}

// NewHTTP builds the handlers. A caller gets guessesPerMinute tries at a
// code a minute (0: 10).
func NewHTTP(svc *Service, guessesPerMinute int) *HTTP {
	if guessesPerMinute <= 0 {
		guessesPerMinute = 10
	}
	return &HTTP{Svc: svc, verifier: signature.NewVerifier(), guesses: throttle.New(guessesPerMinute, time.Minute)}
}

// Register adds the routes to mux.
func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathCode, h.handleCode)
	mux.HandleFunc("POST "+PathEnroll, h.handleEnroll)
	mux.HandleFunc("GET "+PathLink, h.handleLink)
}

func (h *HTTP) ip(r *http.Request) string {
	if h.ClientIP != nil {
		return h.ClientIP(r)
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// throttled answers 429 and reports true when the caller has guessed at
// codes too often. Both code routes share the budget: a code is a bearer
// secret, and these are the only places one can be tried.
func (h *HTTP) throttled(w http.ResponseWriter, r *http.Request) bool {
	ok, retry := h.guesses.Allow(h.ip(r))
	if ok {
		return false
	}
	w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
	writeErr(w, http.StatusTooManyRequests, "throttled", "too many attempts; try again shortly")
	return true
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (h *HTTP) handleCode(w http.ResponseWriter, r *http.Request) {
	if h.throttled(w, r) {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxEnrollBody))
	if json.Unmarshal(body, &in) != nil || in.Code == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "want {code}")
		return
	}
	info, err := h.Svc.Store.PeekCode(r.Context(), in.Code)
	if errors.Is(err, ErrBadCode) {
		writeErr(w, http.StatusUnauthorized, "bad_code", "the code is not valid")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "provider", "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, CodeResult{Label: info.Label, ExpiresAt: info.ExpiresAt})
}

// handleEnroll spends a code and enrolls the key that signed the request.
//
// The signature is what binds the key to the code, so it has to cover the
// exact bytes this handler acts on:
//
//   - the body is read once, to a small bound, and only those bytes are used
//     from then on;
//   - a request with no Content-Digest is refused. The shared verifier
//     checks the digest only when the header is present, and without it the
//     signature would cover no body at all;
//   - the key the verifier is given is the key parsed from those same bytes,
//     and the signature's keyid must be that key's fingerprint.
//
// So a body altered after signing fails the digest, and a body signed by one
// key cannot name another.
func (h *HTTP) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if h.throttled(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEnrollBody+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}
	if len(body) > maxEnrollBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "the enrollment is too large")
		return
	}
	if len(r.Header.Values("Content-Digest")) == 0 {
		writeErr(w, http.StatusUnauthorized, "unsigned", "an enrollment must carry a Content-Digest and sign it")
		return
	}
	var in EnrollBody
	if json.Unmarshal(body, &in) != nil || in.Code == "" || len(in.PublicKey) != ed25519.PublicKeySize {
		writeErr(w, http.StatusBadRequest, "bad_request", "want {code, public_key}")
		return
	}
	pub := ed25519.PublicKey(in.PublicKey)
	want := Fingerprint(pub)
	r.Body = io.NopCloser(bytes.NewReader(body))
	_, err = h.verifier.Verify(r, func(keyID string) (ed25519.PublicKey, error) {
		if want == "" || keyID != want {
			return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("keyid is not the enrolled key's fingerprint")}
		}
		return pub, nil
	}, signature.VerifyOptions{NotOlderThan: enrollSkew})
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "bad_signature", "the enrollment's signature does not verify")
		return
	}
	cred, err := h.Svc.Enroll(r.Context(), in.Code, Enrollment{PublicKey: pub, OS: in.OS, Root: in.Root, Machine: in.Machine})
	switch {
	case errors.Is(err, ErrBadCode):
		writeErr(w, http.StatusUnauthorized, "bad_code", "the code is not valid")
	case errors.Is(err, ErrNameInUse):
		writeErr(w, http.StatusConflict, "name_in_use", "a workspace of that name already exists; destroy it first, or attach under another name")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "provider", "enrollment failed")
	default:
		writeJSON(w, http.StatusOK, EnrollResult{Credential: cred.ID, Name: cred.Name, Label: cred.Label})
	}
}

// handleLink upgrades to a WebSocket and hands the connection to the hub.
// Nothing is authenticated here: the hub's handshake is the authentication,
// and it happens inside the connection, bound to it.
func (h *HTTP) handleLink(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	// yamux frames are larger than the library's default message bound.
	c.SetReadLimit(maxMessage + 1<<20)
	// The link outlives this request's context by design.
	ctx := context.WithoutCancel(r.Context())
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	_ = h.Svc.Hub.Serve(ctx, conn, r.Host)
	_ = c.CloseNow()
}
