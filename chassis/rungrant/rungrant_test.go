package rungrant

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/signedurl"
)

func testSigner(t *testing.T, fill byte, ver int) *Signer {
	t.Helper()
	s, err := New(bytes.Repeat([]byte{fill}, 32), ver)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testClaims(now time.Time) Claims {
	return Claims{
		Grant: "rgr_7Hq", Tenant: "tnt_acme", Principal: "service:research",
		Run: "task-42", Generation: 3, Depth: 1, Trace: "tr_1", Expires: now.Add(time.Minute).Unix(),
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := testSigner(t, 1, 1)
	now := time.Unix(1_800_000_000, 0)
	in := testClaims(now)
	tok, err := s.Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix) || strings.ContainsAny(tok, "/+= \n") {
		t.Errorf("token is not one header-safe word starting %q: %s", Prefix, tok)
	}
	got, err := s.Verify(tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	in.KeyVersion = 1
	if got != in {
		t.Errorf("claims = %+v, want %+v", got, in)
	}
	// The last second it is valid, and the first it is not.
	if _, err := s.Verify(tok, now.Add(59*time.Second)); err != nil {
		t.Errorf("at expiry-1s: %v", err)
	}
	if _, err := s.Verify(tok, now.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("at expiry: %v, want ErrExpired", err)
	}
	// Depth and Trace are optional.
	in.Depth, in.Trace = 0, ""
	if tok, err := s.Sign(in); err != nil {
		t.Errorf("sign without depth and trace: %v", err)
	} else if got, err := s.Verify(tok, now); err != nil || got.Depth != 0 || got.Trace != "" {
		t.Errorf("verify without depth and trace: %+v err=%v", got, err)
	}
}

func TestVerifyRefusals(t *testing.T) {
	s := testSigner(t, 1, 1)
	now := time.Unix(1_800_000_000, 0)
	claims := testClaims(now)
	tok, _ := s.Sign(claims)
	parts := strings.Split(tok, ".")

	// A forger who rewrites the claims keeps the old MAC.
	evil := claims
	evil.Tenant, evil.Principal = "tnt_victim", "service:admin"
	other, _ := testSigner(t, 9, 1).Sign(evil)
	forged := strings.Split(other, ".")[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]

	for name, bad := range map[string]string{
		"empty":            "",
		"garbage":          "not-a-token",
		"two parts":        parts[0] + "." + parts[1],
		"four parts":       tok + ".x",
		"wrong version":    "rg2." + parts[1] + "." + parts[2],
		"a signed URL tag": "v1." + parts[1] + "." + parts[2],
		"claims swapped":   forged,
		"mac truncated":    parts[0] + "." + parts[1] + "." + parts[2][:10],
		"mac not base64":   parts[0] + "." + parts[1] + ".!!!",
		"padded base64":    parts[0] + "." + parts[1] + "=." + parts[2],
		"oversized":        tok + strings.Repeat("A", maxTokenLen),
		"another key":      func() string { o, _ := testSigner(t, 2, 1).Sign(claims); return o }(),
		"another key generation": func() string {
			o, _ := testSigner(t, 1, 2).Sign(claims)
			return o
		}(),
	} {
		if _, err := s.Verify(bad, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// A forged token is invalid even when it is also expired: the MAC is
	// checked first, so a guesser learns nothing from the difference.
	if _, err := s.Verify(forged, now.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Errorf("forged and expired: %v, want ErrInvalid", err)
	}
}

func TestSignRefusesIncompleteClaims(t *testing.T) {
	s := testSigner(t, 1, 1)
	now := time.Unix(1_800_000_000, 0)
	for name, mut := range map[string]func(*Claims){
		"no grant":            func(c *Claims) { c.Grant = "" },
		"no tenant":           func(c *Claims) { c.Tenant = "" },
		"no principal":        func(c *Claims) { c.Principal = "" },
		"no run":              func(c *Claims) { c.Run = "" },
		"no generation":       func(c *Claims) { c.Generation = 0 },
		"negative generation": func(c *Claims) { c.Generation = -1 },
		"negative depth":      func(c *Claims) { c.Depth = -1 },
		"no expiry":           func(c *Claims) { c.Expires = 0 },
		"too long":            func(c *Claims) { c.Run = strings.Repeat("r", maxTokenLen) },
	} {
		c := testClaims(now)
		mut(&c)
		if tok, err := s.Sign(c); err == nil {
			t.Errorf("%s: signed %q", name, tok)
		}
	}
	if _, err := New(make([]byte, 16), 1); err == nil {
		t.Error("a 16-byte key was accepted")
	}
}

type fixedKey struct{}

func (fixedKey) Key() []byte  { return bytes.Repeat([]byte{7}, 32) }
func (fixedKey) Version() int { return 1 }

// The two token kinds share a master key and a form. What keeps a signed URL
// from being presented as a run grant, or the reverse, is the derivation
// label — so this test derives both keys the way the chassis does.
func TestTokensOfAnotherKindDoNotVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runKey, ver, err := secrets.DeriveKey(fixedKey{}, KeyLabel)
	if err != nil {
		t.Fatal(err)
	}
	urlKey, _, err := secrets.DeriveKey(fixedKey{}, signedurl.KeyLabel)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(runKey, urlKey) {
		t.Fatal("the run grant key and the signed URL key are the same key")
	}
	runs, _ := New(runKey, ver)
	urls, _ := signedurl.New(urlKey, ver)

	urlTok, err := urls.Sign(signedurl.Claims{Tenant: "acme", Collection: "dc_1", Resource: "dr_1", SHA256: "aa", Expires: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Verify(urlTok, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a signed URL presented as a run grant: %v", err)
	}
	// Even with its tag rewritten to ours.
	if _, err := runs.Verify(Prefix+strings.TrimPrefix(urlTok, "v1."), now); !errors.Is(err, ErrInvalid) {
		t.Errorf("a re-tagged signed URL presented as a run grant: %v", err)
	}
	runTok, err := runs.Sign(testClaims(now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := urls.Verify(runTok, now); !errors.Is(err, signedurl.ErrInvalid) {
		t.Errorf("a run grant presented as a signed URL: %v", err)
	}
	if _, err := urls.Verify("v1."+strings.TrimPrefix(runTok, Prefix), now); !errors.Is(err, signedurl.ErrInvalid) {
		t.Errorf("a re-tagged run grant presented as a signed URL: %v", err)
	}
}
