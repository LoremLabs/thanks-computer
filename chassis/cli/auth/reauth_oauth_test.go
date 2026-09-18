package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recorded struct {
	path string
	body map[string]any
}

// enrolledProfile enrolls a fresh key under a temp TXCO_HOME and returns the
// profile's recorded public key — the machine state a sign-out leaves behind.
func enrolledProfile(t *testing.T, profile string) string {
	t.Helper()
	t.Setenv("TXCO_HOME", t.TempDir())
	var got []map[string]any
	srv := slugEnrollServer(t, &got)
	defer srv.Close()
	res, err := OAuthEnroll(OAuthEnrollOptions{
		EndpointURL: srv.URL, IDToken: "h.p.s", Profile: profile, NewKey: true, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	m, err := LoadMeta(res.MetaPath)
	if err != nil || m.PublicKeyB64 == "" {
		t.Fatalf("meta: %+v err=%v", m, err)
	}
	return m.PublicKeyB64
}

// chassis is a stand-in admin API: reauth answers from `reauth` (nil = the
// route does not exist, an older chassis), enroll from `enroll`.
func chassis(t *testing.T, got *[]recorded, reauth, enroll func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = append(*got, recorded{r.URL.Path, body})
		switch {
		case strings.HasSuffix(r.URL.Path, "/reauth") && reauth != nil:
			reauth(w)
		case strings.HasSuffix(r.URL.Path, "/enroll") && enroll != nil:
			enroll(w)
		default:
			http.NotFound(w, r)
		}
	}))
}

func answer(status int, v map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
}

// TestOAuthReauthPresentsTheProfilesOwnKey — signing back in is one request
// to the enroll endpoint's sibling, carrying the key the profile ALREADY has.
// It must never pick the default key or mint one: a different key is a
// different actor, and the marker sits on this one.
func TestOAuthReauthPresentsTheProfilesOwnKey(t *testing.T) {
	pub := enrolledProfile(t, "onepony")
	var got []recorded
	srv := chassis(t, &got, answer(200, map[string]any{"actor_id": "actor_x", "tenant_slug": "onepony", "signed_in_as": "email:o@example.com"}), nil)
	defer srv.Close()

	res, err := OAuthReauth(OAuthReauthOptions{EnrollEndpointURL: srv.URL + "/auth/oauth/enroll", IDToken: "h.p.s", Profile: "onepony"})
	if err != nil {
		t.Fatalf("OAuthReauth: %v", err)
	}
	if res.TenantSlug != "onepony" || res.ActorID != "actor_x" {
		t.Errorf("result = %+v", res)
	}
	if len(got) != 1 || got[0].path != "/auth/oauth/reauth" {
		t.Fatalf("requests = %+v, want one POST to /auth/oauth/reauth", got)
	}
	if got[0].body["public_key"] != pub || got[0].body["id_token"] != "h.p.s" {
		t.Errorf("reauth body = %v, want the profile's own key %q", got[0].body, pub)
	}
}

// TestOAuthReauthWrongAccount — the chassis refusing the account is a typed
// error naming the account the user signed in with.
func TestOAuthReauthWrongAccount(t *testing.T) {
	enrolledProfile(t, "onepony")
	for _, code := range []string{"identity_not_enrolled", "identity_mismatch"} {
		var got []recorded
		srv := chassis(t, &got, answer(403, map[string]any{"error": code, "detail": map[string]any{"signed_in_as": "email:wrong@example.com"}}), nil)
		_, err := OAuthReauth(OAuthReauthOptions{EnrollEndpointURL: srv.URL + "/auth/oauth/enroll", IDToken: "h.p.s", Profile: "onepony"})
		srv.Close()
		var mm *ReauthMismatchError
		if !errors.As(err, &mm) || mm.SignedInAs != "email:wrong@example.com" {
			t.Errorf("%s: err = %v, want ReauthMismatchError naming the account", code, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: a refusal must not fall back to enroll: %+v", code, got)
		}
	}
}

// TestOAuthReauthOlderChassis — a chassis without /reauth clears the marker
// in enroll, which is idempotent for a key it already knows. The fallback
// presents the same key, sends no tenant slug, and treats "name your new
// space" as the wrong account rather than answering it.
func TestOAuthReauthOlderChassis(t *testing.T) {
	pub := enrolledProfile(t, "onepony")

	var got []recorded
	srv := chassis(t, &got, nil, answer(200, map[string]any{"chassis_url": "https://c", "tenant_slug": "onepony", "actor_id": "actor_x", "key_id": "key_x"}))
	res, err := OAuthReauth(OAuthReauthOptions{EnrollEndpointURL: srv.URL + "/auth/oauth/enroll", IDToken: "h.p.s", Profile: "onepony"})
	srv.Close()
	if err != nil || res.TenantSlug != "onepony" {
		t.Fatalf("fallback: res=%+v err=%v", res, err)
	}
	if len(got) != 2 || got[1].path != "/auth/oauth/enroll" || got[1].body["public_key"] != pub {
		t.Fatalf("requests = %+v, want reauth then enroll with the same key", got)
	}
	if _, sent := got[1].body["tenant_slug"]; sent {
		t.Errorf("the fallback must never name a tenant: %v", got[1].body)
	}

	got = nil
	srv = chassis(t, &got, nil, answer(409, map[string]any{"error": "tenant_slug_required", "detail": map[string]any{"suggested_tenant_slug": "matt"}}))
	_, err = OAuthReauth(OAuthReauthOptions{EnrollEndpointURL: srv.URL + "/auth/oauth/enroll", IDToken: "h.p.s", Profile: "onepony"})
	srv.Close()
	var mm *ReauthMismatchError
	if !errors.As(err, &mm) {
		t.Errorf("an offer to create a space means the wrong account, got err = %v", err)
	}
	if len(got) != 2 {
		t.Errorf("must not resubmit with the suggested slug: %+v", got)
	}
}

// TestOAuthReauthKeyNotEnrolled — a new chassis saying it does not know the
// key is an answer, not a missing route: no fallback.
func TestOAuthReauthKeyNotEnrolled(t *testing.T) {
	enrolledProfile(t, "onepony")
	var got []recorded
	srv := chassis(t, &got, answer(404, map[string]any{"error": "key_not_enrolled"}), answer(200, map[string]any{"tenant_slug": "x"}))
	defer srv.Close()
	if _, err := OAuthReauth(OAuthReauthOptions{EnrollEndpointURL: srv.URL + "/auth/oauth/enroll", IDToken: "h.p.s", Profile: "onepony"}); err == nil {
		t.Fatal("want an error")
	}
	if len(got) != 1 {
		t.Errorf("requests = %+v, want no enroll fallback", got)
	}
}

func TestReauthEndpointAndExistingKeyChoice(t *testing.T) {
	for in, want := range map[string]string{
		"https://admin.example/auth/oauth/enroll":  "https://admin.example/auth/oauth/reauth",
		"https://admin.example/auth/oauth/enroll/": "https://admin.example/auth/oauth/reauth",
		"http://localhost:8081/auth/oauth":         "http://localhost:8081/auth/oauth/reauth",
	} {
		if got := reauthEndpoint(in); got != want {
			t.Errorf("reauthEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("TXCO_HOME", t.TempDir())
	def, _ := KeyPath("onepony")
	for name, tc := range map[string]struct {
		m         *Meta
		wantKey   string
		wantAgent bool
	}{
		"explicit path": {&Meta{KeySource: SourceFile, KeyPath: "/k/onepony.ed25519"}, "/k/onepony.ed25519", false},
		"default path":  {&Meta{}, def, false},
		"ssh-agent":     {&Meta{KeySource: SourceSSHAgent}, "", true},
		"no profile":    {nil, "", false},
	} {
		if k, a := ExistingKeyChoice(tc.m, "onepony"); k != tc.wantKey || a != tc.wantAgent {
			t.Errorf("%s: ExistingKeyChoice = (%q, %v), want (%q, %v)", name, k, a, tc.wantKey, tc.wantAgent)
		}
	}
}

// TestLoginSignsBackInAndRetries — `txco ui` on a machine the admin UI signed
// out: the chassis answers reauth_required, the CLI signs the user back in
// itself (the installed Reauthenticate hook, for THIS profile), and asks again.
// The user asked to open the UI; they are not sent to find another command.
func TestLoginSignsBackInAndRetries(t *testing.T) {
	enrolledProfile(t, "onepony")

	signedOut := true
	var bootstraps int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/auth/browser/bootstrap") {
			http.NotFound(w, r)
			return
		}
		bootstraps++
		if signedOut {
			answer(403, map[string]any{"error": "reauth_required"})(w)
			return
		}
		answer(200, map[string]any{"url": "https://admin.example/auth/browser/consume?t=once"})(w)
	}))
	defer srv.Close()

	var hookProfile string
	prev := Reauthenticate
	t.Cleanup(func() { Reauthenticate = prev })
	Reauthenticate = func(profile string, noOpen bool, _, _ io.Writer) int {
		hookProfile, signedOut = profile, false
		return 0
	}

	var out, errb strings.Builder
	if code := runLogin([]string{"--no-open", "--profile", "onepony", "--url", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("runLogin = %d, stderr=%q", code, errb.String())
	}
	if hookProfile != "onepony" || bootstraps != 2 {
		t.Errorf("hook profile = %q, bootstraps = %d; want onepony and one retry", hookProfile, bootstraps)
	}
	if !strings.Contains(out.String(), "consume?t=once") {
		t.Errorf("expected the sign-in URL after the retry, got %q", out.String())
	}

	// The wrong account: the hook fails, and so does the command — with the
	// hook's own explanation, not a second "run txco login".
	signedOut, bootstraps = true, 0
	Reauthenticate = func(string, bool, io.Writer, io.Writer) int { return 1 }
	out.Reset()
	errb.Reset()
	if code := runLogin([]string{"--no-open", "--profile", "onepony", "--url", srv.URL}, &out, &errb); code != 1 || bootstraps != 1 {
		t.Errorf("failed sign-in: code = %d, bootstraps = %d; want 1 and no retry", code, bootstraps)
	}

	// No hook installed: the instructions name the profile to sign in.
	Reauthenticate = nil
	errb.Reset()
	if code := runLogin([]string{"--no-open", "--profile", "onepony", "--url", srv.URL}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "txco login --profile onepony") {
		t.Errorf("no hook: code = %d stderr = %q", code, errb.String())
	}
}
