package attached

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func (r *rig) serve(guesses int) *httptest.Server {
	mux := http.NewServeMux()
	NewHTTP(r.svc, guesses).Register(mux)
	srv := httptest.NewServer(mux)
	r.t.Cleanup(srv.Close)
	return srv
}

func apiCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	if err != nil {
		return "!" + err.Error()
	}
	return ""
}

// The whole path a real machine takes: read the code, enroll a key with a
// signed request, dial the WebSocket, prove the key, and serve an exec.
func TestEnrollDialAndExecOverHTTP(t *testing.T) {
	r := newRig(t)
	srv := r.serve(100)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	code, _, err := r.svc.Invite(ctx, testSpec, "Rain's computer", 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := PeekCode(ctx, nil, srv.URL, code)
	if err != nil || info.Label != "Rain's computer" {
		t.Fatalf("peek = %+v, %v", info, err)
	}
	_, key := newKey(t)
	out, err := EnrollKey(ctx, nil, srv.URL, key, EnrollBody{Code: code, OS: "darwin", Root: "~/projects/onepony", Machine: "MacBook"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != testSpec.Name || out.Label != "Rain's computer" || !strings.HasPrefix(out.Credential, "atc-") {
		t.Fatalf("enroll = %+v", out)
	}

	conn, host, err := Dial(ctx, nil, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	up := make(chan Welcome, 1)
	link := &Link{Credential: out.Credential, Key: key, Host: host, Machine: "MacBook", Root: "~/projects/onepony",
		Handler: handlerFunc{request: echo}, OnWelcome: func(w Welcome) { up <- w }}
	linkErr := make(chan error, 1)
	go func() { linkErr <- link.Run(ctx, conn) }()
	select {
	case <-up:
	case err := <-linkErr:
		t.Fatalf("the link did not come up: %v", err)
	}
	res, err := r.exec(testSpec, "pwd", 5*time.Second)
	if err != nil || string(res.Stdout) != "ran: pwd" {
		t.Fatalf("exec = %+v, %v", res, err)
	}
	// A large reply crosses the WebSocket whole.
	big := strings.Repeat("0123456789", 90_000) // 900 KB
	link.Handler = handlerFunc{request: func(context.Context, Header) Reply {
		b, _ := json.Marshal(ExecReply{Stdout: big})
		return Reply{OK: true, Payload: b}
	}}
	if res, err := r.exec(testSpec, "big", 20*time.Second); err != nil || string(res.Stdout) != big {
		t.Fatalf("a large reply: %d bytes, %v", len(res.Stdout), err)
	}
	// The code is spent.
	if _, err := EnrollKey(ctx, nil, srv.URL, key, EnrollBody{Code: code}); apiCode(err) != "bad_code" {
		t.Fatalf("the code a second time: %v", err)
	}
}

// signedEnroll builds an enrollment request the way EnrollKey does, and lets
// the test spoil it before it is sent.
func signedEnroll(t *testing.T, base string, key ed25519.PrivateKey, keyID string, in EnrollBody, spoil func(req *http.Request, body []byte) []byte) (int, string) {
	t.Helper()
	body, _ := json.Marshal(in)
	req, err := http.NewRequest(http.MethodPost, base+PathEnroll, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := signature.NewSigner().Sign(req, signature.ActorPrivateKey{KeyID: keyID, PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	if spoil != nil {
		body = spoil(req, body)
	}
	req.Body = http.NoBody
	if body != nil {
		req.Body = nopCloser{bytes.NewReader(body)}
		req.ContentLength = int64(len(body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Error.Code
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// Every way of sending an enrollment whose signature does not cover exactly
// what the chassis would act on. Each is refused, and none spends the code.
func TestEnrollBindsTheSignatureToTheBody(t *testing.T) {
	r := newRig(t)
	srv := r.serve(1000)
	ctx := context.Background()
	code, _, err := r.svc.Invite(ctx, testSpec, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	pub, key := newKey(t)
	otherPub, otherKey := newKey(t)
	good := EnrollBody{Code: code, PublicKey: pub, Root: "~/a"}

	cases := []struct {
		name       string
		key        ed25519.PrivateKey
		keyID      string
		in         EnrollBody
		spoil      func(*http.Request, []byte) []byte
		wantStatus int
		wantCode   string
	}{
		{"the body changed after signing", key, Fingerprint(pub), good,
			func(_ *http.Request, body []byte) []byte {
				return bytes.Replace(body, []byte(`"~/a"`), []byte(`"~/b"`), 1)
			}, 401, "bad_signature"},
		{"the key swapped after signing", key, Fingerprint(pub), good,
			func(_ *http.Request, _ []byte) []byte {
				b, _ := json.Marshal(EnrollBody{Code: code, PublicKey: otherPub, Root: "~/a"})
				return b
			}, 401, "bad_signature"},
		{"no Content-Digest", key, Fingerprint(pub), good,
			func(req *http.Request, body []byte) []byte {
				req.Header.Del("Content-Digest")
				return body
			}, 401, "unsigned"},
		{"no signature at all", key, Fingerprint(pub), good,
			func(req *http.Request, body []byte) []byte {
				req.Header.Del("Signature")
				req.Header.Del("Signature-Input")
				return body
			}, 401, "bad_signature"},
		{"signed by a key other than the one it enrolls", otherKey, Fingerprint(otherPub), good, nil, 401, "bad_signature"},
		{"a keyid that is not the key's fingerprint", key, "SHA256:somebody-else", good, nil, 401, "bad_signature"},
		{"too large", key, Fingerprint(pub), EnrollBody{Code: code, PublicKey: pub, Root: strings.Repeat("x", maxEnrollBody)}, nil, 413, "too_large"},
	}
	for _, c := range cases {
		status, got := signedEnroll(t, srv.URL, c.key, c.keyID, c.in, c.spoil)
		if status != c.wantStatus || got != c.wantCode {
			t.Errorf("%s: HTTP %d %q, want %d %q", c.name, status, got, c.wantStatus, c.wantCode)
		}
	}
	// After all of that the code is unspent, and nothing was enrolled.
	if _, err := r.store.PeekCode(ctx, code); err != nil {
		t.Fatalf("a refused enrollment spent the code: %v", err)
	}
	if enrolled, _, _ := r.store.Enrolled(ctx, testSpec); enrolled {
		t.Fatal("a refused enrollment attached the name")
	}
	// And the honest request still works.
	if status, got := signedEnroll(t, srv.URL, key, Fingerprint(pub), good, nil); status != 200 {
		t.Fatalf("the honest enrollment: HTTP %d %q", status, got)
	}
}

func TestEnrollOverAProvidersNameIs409(t *testing.T) {
	r := newRig(t)
	srv := r.serve(100)
	ctx := context.Background()
	code, _, err := r.svc.Invite(ctx, testSpec, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ws.Upsert(ctx, workspace.Row{Tenant: testSpec.Tenant, Stack: testSpec.Stack, Name: testSpec.Name, Provider: "sprites", ProviderRef: "s"}); err != nil {
		t.Fatal(err)
	}
	_, key := newKey(t)
	_, err = EnrollKey(ctx, nil, srv.URL, key, EnrollBody{Code: code})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Code != "name_in_use" {
		t.Fatalf("err = %v", err)
	}
}

func TestCodeGuessesAreThrottled(t *testing.T) {
	r := newRig(t)
	srv := r.serve(3)
	ctx := context.Background()
	var last error
	for i := 0; i < 5; i++ {
		_, last = PeekCode(ctx, nil, srv.URL, "one-two-three-four-five-six-seven-eight")
		if i < 3 && apiCode(last) != "bad_code" {
			t.Fatalf("guess %d: %v", i, last)
		}
	}
	if apiCode(last) != "throttled" {
		t.Fatalf("the fifth guess: %v, want throttled", last)
	}
	// Enrollments draw on the same budget.
	_, key := newKey(t)
	if _, err := EnrollKey(ctx, nil, srv.URL, key, EnrollBody{Code: "x"}); apiCode(err) != "throttled" {
		t.Fatalf("an enrollment after the guesses: %v", err)
	}
}
